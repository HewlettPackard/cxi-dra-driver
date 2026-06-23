package nri

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
	// svcIDs maps deviceName -> allocated svc_id. Used to tear the service
	// down in StopPodSandbox and injected as SLINGSHOT_SVC_IDS.
	svcIDs map[string]int
}

// Plugin is an NRI plugin that creates CXI services when pod sandboxes start.
// It reads pod → device mappings from the AllocationStore, which is populated
// by the DRA driver in PrepareResourceClaims before containers are created.
//
// Hooks used:
//   - RunPodSandbox   — network namespace is available; create CXI service here.
//   - CreateContainer — inject SLINGSHOT_DEVICES, SLINGSHOT_VNIS, SLINGSHOT_SVC_IDS env vars into each container.
//   - StopPodSandbox  — tear down CXI service when the pod sandbox stops.
type Plugin struct {
	stub             stub.Stub
	store            *driver.AllocationStore
	svcMgr           cxi.ServiceManager
	systemSvcIDs     map[int]bool                       // blessed system-service svc_ids (--system-svc-ids); destroyService refuses to delete these
	republish        func()                             // triggers immediate ResourceSlice update
	onServiceCreated func(types.UID)                    // called with claimUID after service creation
	onServiceDeleted func()                             // called after any service is deleted, triggers binder retry
	onOccupancyFreed func(namespace, deviceName string) // called per-(ns,dev) after DestroyService to release per-namespace occupancy
	recordSvcID      func(ctx context.Context, claimUID types.UID, claimNs, claimName, deviceName string, vni, svcID int)
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
//
// recordSvcID is called per (claim, device) after CreateService succeeds, to
// publish the VNI and svc_id on the claim's status for operator introspection
// (`kubectl describe resourceclaim`). Best-effort; informational only.
//
// systemSvcIDs are the blessed system-service svc_ids (--system-svc-ids). The
// plugin never deletes a service whose id is in this set — see destroyService.
// In mock mode the mock svc_id counter is seeded past the highest blessed id so
// generated ids never collide with the reserved system services.
func New(
	store *driver.AllocationStore,
	mock bool,
	republish func(),
	onServiceCreated func(types.UID),
	onServiceDeleted func(),
	onOccupancyFreed func(namespace, deviceName string),
	recordSvcID func(ctx context.Context, claimUID types.UID, claimNs, claimName, deviceName string, vni, svcID int),
	systemSvcIDs []int,
) (*Plugin, error) {
	var svcMgr cxi.ServiceManager
	if mock {
		svcMgr = cxi.MockServiceManager{}
		// Seed the mock counter past the highest blessed id so mock-created
		// services never reuse a reserved system-service svc_id.
		var maxID int
		for _, id := range systemSvcIDs {
			if id > maxID {
				maxID = id
			}
		}
		cxi.SeedMockSvcIDCounter(int64(maxID))
	} else {
		svcMgr = cxi.NewServiceManager()
	}
	blessed := make(map[int]bool, len(systemSvcIDs))
	for _, id := range systemSvcIDs {
		blessed[id] = true
	}
	p := &Plugin{
		store:            store,
		svcMgr:           svcMgr,
		systemSvcIDs:     blessed,
		republish:        republish,
		onServiceCreated: onServiceCreated,
		onServiceDeleted: onServiceDeleted,
		onOccupancyFreed: onOccupancyFreed,
		recordSvcID:      recordSvcID,
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
// If containerd closes the NRI connection (e.g. during its own restart or a
// transient handshake failure), the stub is reconnected with exponential
// backoff. The NRI stub explicitly supports being restarted via Run()/Start().
func (p *Plugin) Run(ctx context.Context) error {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		if err := p.stub.Run(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Printf("cxi-nri: NRI disconnected (%v), reconnecting in %s\n", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		return nil
	}
}

// getDeviceLock returns the per-device mutex, creating it if needed.
// Serialising RunPodSandbox calls per device makes the fit-check + service
// creation atomic with respect to other concurrent pod starts on the same device.
func (p *Plugin) getDeviceLock(name string) *sync.Mutex {
	v, _ := p.deviceMu.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// destroyService deletes the service identified by (devName, svcID), unless
// svcID is a blessed system service (--system-svc-ids), in which case it logs
// and returns nil without touching the kernel. The blessed services are owned
// by the system and never created by this driver, so a stale in-memory svc_id
// mapping must never tear one down. DRA-pod svc_ids never collide with blessed
// ids in normal operation (the kernel assigns fresh ids in production; mock
// seeds its counter past the blessed set), so this guard only fires on genuine
// state corruption.
func (p *Plugin) destroyService(devName string, svcID int) error {
	if p.systemSvcIDs[svcID] {
		fmt.Printf("cxi-nri: refusing to delete blessed system service svc_id=%d on %s\n", svcID, devName)
		return nil
	}
	return p.svcMgr.DeleteService(devName, svcID)
}

// LiveSvcIDs returns the svc_ids the plugin currently owns, grouped by device:
// result[deviceName][svcID] is true for every service created for a pod sandbox
// that has not yet stopped (i.e. still present in podStates). The driver's
// orphan-service reaper consults this set so it never deletes a service that
// belongs to a live pod — in particular one whose StopPodSandbox has not run
// yet, or one created so recently that the kernel has not finished recording
// its pod netns. The returned map is a fresh copy and safe to read without
// holding p.mu.
func (p *Plugin) LiveSvcIDs() map[string]map[int]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]map[int]bool)
	for _, state := range p.podStates {
		for devName, svcID := range state.svcIDs {
			byDev := out[devName]
			if byDev == nil {
				byDev = make(map[int]bool)
				out[devName] = byDev
			}
			byDev[svcID] = true
		}
	}
	return out
}

// RunPodSandbox is called by containerd when the pod sandbox is created and
// the network namespace is ready — before any containers start.
//
// The BindingConditions gate (ClaimBinder) is the primary capacity check.
// The per-device fit-check here is a secondary safety net for the narrow
// window between Ready=True being granted and the service being created.
// On failure the pod sandbox creation is retried by the kubelet.
func (p *Plugin) RunPodSandbox(ctx context.Context, pod *api.PodSandbox) error {
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
	state := &podState{svcIDs: make(map[string]int)}

	// Roll back any services already created if a later device fails. svcIDs
	// holds exactly the services created so far, keyed by device name.
	rollback := func() {
		for devName, svcID := range state.svcIDs {
			if err := p.destroyService(devName, svcID); err != nil {
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
		state.svcIDs[alloc.DeviceName] = svcID

		// Publish VNI + svc_id on the claim's status for operator introspection.
		if alloc.ClaimUID != "" {
			p.recordSvcID(ctx, alloc.ClaimUID, alloc.ClaimNamespace, alloc.ClaimName,
				alloc.DeviceName, alloc.VNI, svcID)
		}
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

// CreateContainer injects SLINGSHOT_DEVICES, SLINGSHOT_VNIS, and
// SLINGSHOT_SVC_IDS env vars into every container. The service was already
// created in RunPodSandbox. All three variables are comma-separated lists
// aligned by index — position N in each list refers to the same device.
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
	p.mu.Unlock()

	// Build aligned comma-separated lists. Position N in SLINGSHOT_DEVICES,
	// SLINGSHOT_VNIS, and SLINGSHOT_SVC_IDS all refer to the same device.
	devNames := make([]string, 0, len(allocs))
	vnis := make([]string, 0, len(allocs))
	svcIDList := make([]string, 0, len(allocs))
	for _, alloc := range allocs {
		devNames = append(devNames, alloc.DeviceName)
		vnis = append(vnis, strconv.Itoa(alloc.VNI))
		svcIDList = append(svcIDList, strconv.Itoa(svcIDs[alloc.DeviceName]))
	}

	adj := &api.ContainerAdjustment{}
	adj.AddEnv("SLINGSHOT_DEVICES", strings.Join(devNames, ","))
	adj.AddEnv("SLINGSHOT_VNIS", strings.Join(vnis, ","))
	adj.AddEnv("SLINGSHOT_SVC_IDS", strings.Join(svcIDList, ","))

	if isMockManager(p.svcMgr) {
		for _, alloc := range allocs {
			hostPath := cxi.MockServicePath(alloc.DeviceName, svcIDs[alloc.DeviceName])
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

	// Gate teardown on podStates, NOT the AllocationStore. The DRA
	// UnprepareResourceClaims hook also deletes this pod's store entry, and
	// the two hooks have no ordering guarantee: if Unprepare runs first,
	// p.store.Get(podUID) would be empty here and we would skip DeleteService,
	// leaking the CXI service (and its VNI) on the device. podStates is owned
	// solely by this plugin and is the authoritative record of the svc_ids
	// created for the pod, keyed by device name.
	p.mu.Lock()
	state, exists := p.podStates[podUID]
	if !exists {
		p.mu.Unlock()
		return nil
	}
	delete(p.podStates, podUID)
	svcIDs := state.svcIDs
	p.mu.Unlock()

	// podStates[podUID] is set only after RunPodSandbox created a service for
	// every allocated device, so svcIDs covers exactly the pod's devices —
	// no need to consult the store for the device list or namespace.
	for deviceName, svcID := range svcIDs {
		fmt.Printf("cxi-nri: deleting service: pod=%s device=%s svc_id=%d\n",
			podUID, deviceName, svcID)
		if err := p.destroyService(deviceName, svcID); err != nil {
			fmt.Printf("cxi-nri: warning: deleting service for %s: %v\n", deviceName, err)
		}
		// Release the per-namespace occupancy slot for this device. We do
		// this even if DeleteService returned an error: an undeletable
		// service may indicate the kernel state diverged from our records,
		// but holding the occupancy slot forever would permanently bar the
		// namespace from this device on this driver process. NRI is the
		// hot path; the ClaimBinder watch (on Allocation=nil or claim
		// delete) and the driver's polling reconciler cover the
		// scheduler-side and hardware-drift cases respectively — see
		// Driver.RemoveOccupancy, RemoveOccupancyForClaim, and
		// reconcileOccupancyFromHardware.
		p.onOccupancyFreed(pod.GetNamespace(), deviceName)
	}

	// Best-effort store cleanup: UnprepareResourceClaims may have already
	// removed this entry. Delete is idempotent.
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
