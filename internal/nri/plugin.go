package nri

import (
	"context"
	"fmt"
	"maps"
	"os"
	"sync"
	"syscall"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
	"github.com/slingshot/cxi-dra-driver/internal/driver"
)

// podState tracks per-pod service creation state so that the CXI service is
// created exactly once per pod (in RunPodSandbox) and torn down when the pod
// sandbox stops (in StopPodSandbox).
type podState struct {
	netnsInode uint64
	// svcIDs maps deviceName -> allocated svc_id, injected as FI_CXI_SVC_ID
	svcIDs map[string]int
}

// Plugin is an NRI plugin that creates CXI services when pod sandboxes start.
// It reads pod → device mappings from the AllocationStore, which is populated
// by the DRA driver in PrepareResourceClaims before containers are created.
//
// Hooks used:
//   - RunPodSandbox   — network namespace is available; create CXI service here.
//   - CreateContainer — inject FI_CXI_SVC_ID env vars into each container.
//   - StopPodSandbox  — tear down CXI service when the pod sandbox stops.
type Plugin struct {
	stub             stub.Stub
	store            *driver.AllocationStore
	svcMgr           cxi.ServiceManager
	republish        func()                            // triggers immediate ResourceSlice update
	onServiceCreated func(types.UID)                   // called with claimUID after service creation
	onServiceDeleted func()                            // called after any service is deleted, triggers binder retry
	onOccupancyFreed func(namespace, deviceName string) // called per-(ns,dev) after DestroyService to release per-namespace occupancy
	mu               sync.Mutex
	podStates        map[types.UID]*podState
	deviceMu         sync.Map // map[string]*sync.Mutex — one mutex per device name
}

// New creates a new NRI plugin.
//
// republish is called after each service create/delete so the ResourceSlice
// immediately reflects updated avail_*.
//
// onServiceCreated(claimUID) is called after a service is successfully created
// so the driver can remove the pending entry for that claim.
//
// onServiceDeleted is called after any service is deleted so the ClaimBinder
// can immediately re-evaluate claims waiting in Ready=False.
//
// onOccupancyFreed(namespace, deviceName) is called for each (ns, device)
// pair after a successful DestroyService so the driver releases its
// per-namespace device-occupancy slot. The ClaimBinder uses this slot to
// enforce SHS14's per-device VNI uniqueness constraint: two claims from
// the same namespace cannot co-locate on the same device. See the
// nsDeviceOccupancy field on Driver for details.
func New(
	store *driver.AllocationStore,
	mock bool,
	republish func(),
	onServiceCreated func(types.UID),
	onServiceDeleted func(),
	onOccupancyFreed func(namespace, deviceName string),
) (*Plugin, error) {
	var svcMgr cxi.ServiceManager
	if mock {
		svcMgr = cxi.MockServiceManager{}
	} else {
		svcMgr = cxi.NewServiceManager()
	}
	p := &Plugin{
		store:            store,
		svcMgr:           svcMgr,
		republish:        republish,
		onServiceCreated: onServiceCreated,
		onServiceDeleted: onServiceDeleted,
		onOccupancyFreed: onOccupancyFreed,
		podStates:        make(map[types.UID]*podState),
	}
	var err error
	p.stub, err = stub.New(p,
		stub.WithPluginName("cxi-dra-driver"),
		stub.WithPluginIdx("00"),
	)
	if err != nil {
		return nil, fmt.Errorf("creating NRI stub: %w", err)
	}
	return p, nil
}

// Run starts the NRI plugin and blocks until ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if err := p.stub.Run(ctx); err != nil {
		return fmt.Errorf("NRI plugin exited: %w", err)
	}
	return nil
}

// getDeviceLock returns the per-device mutex, creating it if needed.
// Serialising RunPodSandbox calls per device makes the fit-check + service
// creation atomic with respect to other concurrent pod starts on the same device.
func (p *Plugin) getDeviceLock(name string) *sync.Mutex {
	v, _ := p.deviceMu.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// RunPodSandbox is called by containerd when the pod sandbox is created and
// the network namespace is ready — before any containers start.
//
// The BindingConditions gate (ClaimBinder) is the primary capacity check.
// The per-device fit-check here is a secondary safety net for the narrow
// window between Ready=True being granted and the service being created.
// On failure the pod sandbox creation is retried by the kubelet.
func (p *Plugin) RunPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := types.UID(pod.GetUid())
	allocs := p.store.Get(podUID)
	if len(allocs) == 0 {
		return nil
	}

	pid := pod.GetPid()
	if pid == 0 {
		return fmt.Errorf("pod %s has no PID in sandbox — cannot resolve netns inode", podUID)
	}
	inode, err := netnsInode(pid)
	if err != nil {
		return fmt.Errorf("resolving netns inode for pod %s (pid %d): %w", podUID, pid, err)
	}

	netnsPath := netnsFromPod(pod)
	state := &podState{netnsInode: inode, svcIDs: make(map[string]int)}

	// Track services created so far for rollback on partial failure.
	createdDevices := make([]string, 0, len(allocs))
	rollback := func() {
		for _, devName := range createdDevices {
			if err := p.svcMgr.DeleteService(devName, inode); err != nil {
				fmt.Printf("cxi-nri: warning: rollback delete service %s: %v\n", devName, err)
			}
		}
	}

	for _, alloc := range allocs {
		if !driver.ValidVNI(alloc.VNI) {
			rollback()
			return fmt.Errorf("refusing CXI service for %s: VNI %d out of range [%d, %d]",
				alloc.DeviceName, alloc.VNI, driver.MinVNI, driver.MaxVNI)
		}

		// Per-device mutex: serialise fit-check + CreateService so that
		// concurrent pod starts on the same device don't race each other.
		mu := p.getDeviceLock(alloc.DeviceName)
		mu.Lock()

		avail, err := cxi.ReadAvailableCapacity(alloc.DeviceName)
		if err != nil {
			mu.Unlock()
			rollback()
			return fmt.Errorf("pod %s device %s: reading capacity: %w", podUID, alloc.DeviceName, err)
		}
		if !alloc.Resources.Fits(avail) {
			mu.Unlock()
			rollback()
			return fmt.Errorf("pod %s device %s: requested (%s) does not fit available (%s) — will retry",
				podUID, alloc.DeviceName, alloc.Resources, avail)
		}

		fmt.Printf("cxi-nri: creating service: pod=%s device=%s hsn=%s netns=%s inode=%d vni=%d\n",
			podUID, alloc.DeviceName, alloc.HSNIface, netnsPath, inode, alloc.VNI)
		svcID, err := p.svcMgr.CreateService(alloc.DeviceName, inode, alloc.VNI, alloc.Resources)
		mu.Unlock()

		if err != nil {
			rollback()
			return fmt.Errorf("creating CXI service for %s: %w", alloc.DeviceName, err)
		}
		createdDevices = append(createdDevices, alloc.DeviceName)
		state.svcIDs[alloc.DeviceName] = svcID
	}

	p.mu.Lock()
	p.podStates[podUID] = state
	p.mu.Unlock()

	// Remove the pending entry now that the hardware service owns the resources.
	if claimUID := p.store.ClaimForPod(podUID); claimUID != "" {
		p.onServiceCreated(claimUID)
	}

	// Update ResourceSlice immediately so avail_* reflects the new service.
	p.republish()
	return nil
}

// CreateContainer injects FI_CXI_SVC_ID env vars. The service was already
// created in RunPodSandbox.
func (p *Plugin) CreateContainer(_ context.Context, pod *api.PodSandbox, container *api.Container) (*api.ContainerAdjustment, []*api.ContainerUpdate, error) {
	podUID := types.UID(pod.GetUid())
	allocs := p.store.Get(podUID)
	if len(allocs) == 0 {
		return nil, nil, nil
	}

	p.mu.Lock()
	state, exists := p.podStates[podUID]
	p.mu.Unlock()
	if !exists {
		return nil, nil, nil
	}

	p.mu.Lock()
	svcIDs := make(map[string]int, len(state.svcIDs))
	maps.Copy(svcIDs, state.svcIDs)
	inode := state.netnsInode
	p.mu.Unlock()

	adj := &api.ContainerAdjustment{}
	for devName, svcID := range svcIDs {
		adj.AddEnv(fmt.Sprintf("FI_CXI_SVC_ID_%s", devName), fmt.Sprintf("%d", svcID))
	}
	if len(svcIDs) == 1 {
		for _, svcID := range svcIDs {
			adj.AddEnv("FI_CXI_SVC_ID", fmt.Sprintf("%d", svcID))
		}
	}

	if isMockManager(p.svcMgr) {
		for _, alloc := range allocs {
			hostPath := cxi.MockServicePath(alloc.DeviceName, inode)
			containerPath := fmt.Sprintf("/run/cxi/%s-service.yaml", alloc.DeviceName)
			adj.AddMount(&api.Mount{
				Source:      hostPath,
				Destination: containerPath,
				Options:     []string{"bind", "ro"},
			})
		}
	}

	return adj, nil, nil
}

// StopPodSandbox tears down CXI services and notifies the binder that
// capacity has been freed.
func (p *Plugin) StopPodSandbox(_ context.Context, pod *api.PodSandbox) error {
	podUID := types.UID(pod.GetUid())
	allocs := p.store.Get(podUID)
	if len(allocs) == 0 {
		return nil
	}

	p.mu.Lock()
	state, exists := p.podStates[podUID]
	if !exists {
		p.mu.Unlock()
		return nil
	}
	delete(p.podStates, podUID)
	inode := state.netnsInode
	p.mu.Unlock()

	for _, alloc := range allocs {
		fmt.Printf("cxi-nri: deleting service: pod=%s device=%s inode=%d\n",
			podUID, alloc.DeviceName, inode)
		if err := p.svcMgr.DeleteService(alloc.DeviceName, inode); err != nil {
			fmt.Printf("cxi-nri: warning: deleting service for %s: %v\n", alloc.DeviceName, err)
		}
		// Release the per-namespace occupancy slot for this device. We do
		// this even if DeleteService returned an error: an undeletable
		// service may indicate the kernel state diverged from our records,
		// but holding the occupancy slot forever would permanently bar the
		// namespace from this device on this driver process. NRI is the
		// sole decrement point — see Driver.RemoveOccupancy.
		p.onOccupancyFreed(pod.GetNamespace(), alloc.DeviceName)
	}

	p.store.Delete(podUID)

	// Tell the binder to re-evaluate Ready=False claims — capacity just freed.
	p.onServiceDeleted()
	// Update ResourceSlice immediately so avail_* reflects freed capacity.
	p.republish()
	return nil
}

// netnsFromPod extracts the network namespace path from the pod sandbox.
func netnsFromPod(pod *api.PodSandbox) string {
	for _, ns := range pod.GetLinux().GetNamespaces() {
		if ns.GetType() == "network" {
			return ns.GetPath()
		}
	}
	return ""
}

// netnsInode returns the nsfs inode of the pod's network namespace via
// /proc/<pid>/ns/net + fstat. stat() on a bind-mount returns the wrong inode.
func netnsInode(pid uint32) (uint64, error) {
	path := fmt.Sprintf("/proc/%d/ns/net", pid)
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, fmt.Errorf("fstat %s: %w", path, err)
	}
	return uint64(st.Ino), nil
}

func isMockManager(svcMgr cxi.ServiceManager) bool {
	_, ok := svcMgr.(cxi.MockServiceManager)
	return ok
}
