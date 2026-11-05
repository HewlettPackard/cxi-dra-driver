package nri

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
	"github.com/slingshot/cxi-dra-driver/internal/netsysfs"
)

// podState tracks per-pod service creation state so that the CXI service is
// created exactly once per pod (in RunPodSandbox) and torn down when the pod
// sandbox stops (in StopPodSandbox).
type podState struct {
	// svcIDs maps deviceName -> allocated svc_id. Used to tear the service
	// down in StopPodSandbox and injected as SLINGSHOT_SVC_IDS.
	svcIDs map[string]int

	// netSysfsDir is the host path of the per-pod synthetic /sys/class/net tree
	// (see internal/netsysfs). It is bind-mounted over the container's
	// /sys/class/net in CreateContainer and removed in StopPodSandbox. Empty
	// when HSN sysfs injection is disabled or no device has a paired HSN.
	netSysfsDir string
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
	stub              stub.Stub
	store             *driver.AllocationStore
	svcMgr            cxi.ServiceManager
	systemSvcIDs      map[int]bool    // blessed system-service svc_ids (--system-svc-ids); destroyService refuses to delete these
	onServiceCreated  func(types.UID) // called with claimUID after service creation
	onServiceDeleted  func()          // called after any service is deleted, triggers binder retry
	recordSvcID       func(ctx context.Context, alloc driver.PodAllocation, svcID int)
	availableFor      func(driver.PodAllocation) (cxi.ResourceSet, error) // live capacity an allocation must fit
	serviceOptionsFor func(driver.PodAllocation) (cxi.ServiceOptions, error)
	netSysfsRoot      string // root dir for per-pod synthetic /sys/class/net trees; "" disables HSN sysfs injection
	mu                sync.Mutex
	podStates         map[types.UID]*podState
	deviceMu          sync.Map // map[string]*sync.Mutex — one mutex per device name
}

// New creates a new NRI plugin.
//
// onServiceCreated(claimUID) is called after a service is successfully created
// so the driver can remove the pending entry for that claim.
//
// onServiceDeleted is called after any service is deleted so the ClaimBinder
// can immediately re-evaluate claims waiting in Ready=False.
//
// recordSvcID is called per allocation after CreateService succeeds, to
// publish the VNI, svc_id and backing VF on the claim's status for operator
// introspection (`kubectl describe resourceclaim`). Best-effort; informational only.
//
// availableFor returns the live capacity an allocation must fit before its
// service is created: the parent's remaining budget in VF mode, the PF's free
// resources otherwise.
//
// serviceOptionsFor supplies the discovered parent and sibling VF list for VF
// child creation, or empty options for unchanged PF service behavior.
//
// systemSvcIDs are the blessed system-service svc_ids (--system-svc-ids). The
// plugin never deletes a service whose id is in this set — see destroyService.
// In mock mode the mock svc_id counter is seeded past the highest blessed id so
// generated ids never collide with the reserved system services.
//
// netSysfsRoot is the host directory under which per-pod synthetic
// /sys/class/net trees are built (see internal/netsysfs). When empty — or in
// mock mode — HSN sysfs injection is disabled.
func New(
	store *driver.AllocationStore,
	mock bool,
	onServiceCreated func(types.UID),
	onServiceDeleted func(),
	recordSvcID func(ctx context.Context, alloc driver.PodAllocation, svcID int),
	availableFor func(driver.PodAllocation) (cxi.ResourceSet, error),
	serviceOptionsFor func(driver.PodAllocation) (cxi.ServiceOptions, error),
	systemSvcIDs []int,
	netSysfsRoot string,
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
		store:             store,
		svcMgr:            svcMgr,
		systemSvcIDs:      blessed,
		onServiceCreated:  onServiceCreated,
		onServiceDeleted:  onServiceDeleted,
		recordSvcID:       recordSvcID,
		availableFor:      availableFor,
		serviceOptionsFor: serviceOptionsFor,
		netSysfsRoot:      netSysfsRoot,
		podStates:         make(map[types.UID]*podState),
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

// getDeviceLock returns the mutex for a lock key, creating it if needed.
// Serialising RunPodSandbox calls per key makes the fit-check + service
// creation atomic with respect to other concurrent pod starts drawing on the
// same budget.
func (p *Plugin) getDeviceLock(name string) *sync.Mutex {
	v, _ := p.deviceMu.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// lockKey is the budget an allocation draws on: its parent in VF mode (sibling
// VFs share it), its device otherwise.
func lockKey(alloc driver.PodAllocation) string {
	if alloc.ParentName != "" {
		return alloc.ParentName
	}
	return alloc.DeviceName
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

		// Per-budget mutex: serialise fit-check + CreateService so that
		// concurrent pod starts on the same device (or parent) don't race.
		mu := p.getDeviceLock(lockKey(alloc))
		mu.Lock()

		avail, err := p.availableFor(alloc)
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
		options, err := p.serviceOptionsFor(alloc)
		if err != nil {
			mu.Unlock()
			rollback()
			return fmt.Errorf("resolving CXI service parent for %s: %w", alloc.DeviceName, err)
		}
		svcID, err := p.svcMgr.CreateService(alloc.DeviceName, inode, alloc.VNI, alloc.Resources, options)
		mu.Unlock()

		if err != nil {
			rollback()
			return fmt.Errorf("creating CXI service for %s: %w", alloc.DeviceName, err)
		}
		state.svcIDs[alloc.DeviceName] = svcID

		// Publish VNI, svc_id and the VF on the claim's status for operator introspection.
		if alloc.ClaimUID != "" {
			p.recordSvcID(ctx, alloc, svcID)
		}
	}

	// Build the per-pod synthetic /sys/class/net tree so stock libfabric can
	// discover the allocated HSN device(s). It is bind-mounted into each
	// container in CreateContainer. A hard error here means the HSN metadata
	// libfabric needs is unavailable, so roll the services back and let the
	// kubelet retry rather than start a pod where libfabric cannot bootstrap.
	netDir, err := p.buildNetSysfs(podUID, pid, allocs)
	if err != nil {
		rollback()
		return fmt.Errorf("pod %s: preparing HSN sysfs: %w", podUID, err)
	}
	state.netSysfsDir = netDir

	p.mu.Lock()
	p.podStates[podUID] = state
	p.mu.Unlock()

	// Remove the pending entry now that the hardware service owns the resources.
	if claimUID := p.store.ClaimForPod(podUID); claimUID != "" {
		p.onServiceCreated(claimUID)
	}
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
	netSysfsDir := state.netSysfsDir
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

	// Give the container its own tmpfs at /dev/shm instead of a host directory
	// shared with other tenants. Like an emptyDir with medium Memory, it has no
	// tmpfs size cap (the container's memory cgroup limits it) and disappears
	// with the container. It replaces any existing /dev/shm mount, including
	// the runtime's default.
	adj.AddMount(&api.Mount{
		Source:      "shm",
		Destination: "/dev/shm",
		Type:        "tmpfs",
		Options:     []string{"nosuid", "nodev", "noexec", "mode=1777"},
	})

	// Overmount the container's /sys/class/net with the per-pod synthetic tree
	// so stock libfabric discovers the HSN device(s). libfabric's link gate
	// (cxip_if.c:142) has no env bypass and is satisfied by operstate=up in the
	// tree.
	//
	// libfabric also applies an AMA gate (cxip_if.c:580): it drops any device
	// whose netdev lacks a recognized addr_assign_type/address unless
	// CXIP_SKIP_AMA_CHECK=1 is set. This tree does not synthesize a MAC, so that
	// bypass is required — but it is deliberately NOT injected here. Env belongs
	// in the CDI env config (see internal/cdi/env.go), so the operator controls
	// it declaratively rather than having NRI hardcode it.
	if netSysfsDir != "" {
		adj.AddMount(&api.Mount{
			Source:      netSysfsDir,
			Destination: "/sys/class/net",
			Options:     []string{"bind", "ro"},
		})
	}

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
	netSysfsDir := state.netSysfsDir
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
	}

	// Remove the per-pod synthetic /sys/class/net tree. netSysfsDir is
	// <root>/<podUID>/net, so drop the whole <podUID> directory.
	if netSysfsDir != "" {
		if err := os.RemoveAll(filepath.Dir(netSysfsDir)); err != nil {
			fmt.Printf("cxi-nri: warning: removing net sysfs dir for pod %s: %v\n", podUID, err)
		}
	}

	// Best-effort store cleanup: UnprepareResourceClaims may have already
	// removed this entry. Delete is idempotent.
	p.store.Delete(podUID)

	// Tell the binder to re-evaluate Ready=False claims — capacity just freed.
	p.onServiceDeleted()
	return nil
}

// buildNetSysfs constructs the per-pod synthetic /sys/class/net tree that lets
// stock libfabric discover the allocated HSN device(s), returning the host path
// to bind-mount over the container's /sys/class/net (or "" when injection is
// disabled or no allocated device has a paired HSN interface).
//
// HSN metadata (the device's PCI BDF and link speed) is read from the host's own
// /sys/class/net/<hsn>; a failure there is fatal because libfabric cannot
// bootstrap without it. Replicating the pod's real interfaces is best-effort: if
// the netns enumeration fails the tree is built from the HSN entries alone (the
// pod loses visibility of its own NICs under /sys/class/net but libfabric still
// works), and the failure is logged rather than aborting the sandbox.
//
// Injection is skipped entirely in mock mode (the mock HSN interfaces have no
// host sysfs) and when no root directory is configured.
func (p *Plugin) buildNetSysfs(podUID types.UID, pid uint32, allocs []driver.PodAllocation) (string, error) {
	if p.netSysfsRoot == "" || isMockManager(p.svcMgr) {
		return "", nil
	}

	var hsns []netsysfs.HSN
	for _, alloc := range allocs {
		if alloc.HSNIface == "" {
			fmt.Printf("cxi-nri: warning: pod %s device %s has no paired HSN interface; skipping its sysfs injection\n",
				podUID, alloc.DeviceName)
			continue
		}
		hsn, err := netsysfs.HostHSNForPod(alloc.HSNIface, alloc.PodIfName)
		if err != nil {
			return "", fmt.Errorf("reading host HSN %s for device %s: %w", alloc.HSNIface, alloc.DeviceName, err)
		}
		hsns = append(hsns, hsn)
	}
	if len(hsns) == 0 {
		return "", nil
	}

	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	ifaces, err := netsysfs.PodInterfaces(netnsPath)
	if err != nil {
		fmt.Printf("cxi-nri: warning: enumerating pod %s interfaces via %s: %v; building HSN-only sysfs tree\n",
			podUID, netnsPath, err)
	}

	dir := filepath.Join(p.netSysfsRoot, string(podUID), "net")
	if err := netsysfs.Build(dir, ifaces, hsns); err != nil {
		return "", fmt.Errorf("building net sysfs tree at %s: %w", dir, err)
	}
	fmt.Printf("cxi-nri: built net sysfs for pod %s at %s (%d hsn, %d replicated ifaces)\n",
		podUID, dir, len(hsns), len(ifaces))
	return dir, nil
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
	defer func() { _ = f.Close() }()
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
