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
//   - RunPodSandbox  — network namespace is available; create CXI service here
//     so the cgroup device policy is set before any container starts.
//   - CreateContainer — inject FI_CXI_SVC_ID env vars into each container.
//   - StopPodSandbox  — tear down CXI service when the pod sandbox stops.
type Plugin struct {
	stub      stub.Stub
	store     *driver.AllocationStore
	svcMgr    cxi.ServiceManager
	mu        sync.Mutex
	podStates map[types.UID]*podState
}

// New creates a new NRI plugin.
func New(store *driver.AllocationStore, mock bool) (*Plugin, error) {
	var svcMgr cxi.ServiceManager
	if mock {
		svcMgr = cxi.MockServiceManager{}
	} else {
		svcMgr = cxi.NewServiceManager()
	}
	p := &Plugin{
		store:     store,
		svcMgr:    svcMgr,
		podStates: make(map[types.UID]*podState),
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

// RunPodSandbox is called by containerd when the pod sandbox is created and
// the network namespace is ready — before any containers start. This is the
// correct hook to create the CXI service, matching the pattern used by the
// containerd network-device-injector reference plugin.
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

	for _, alloc := range allocs {
		// Defensive bound check at the hardware boundary: an out-of-range VNI
		// would silently truncate to uint16 in libcxi and could scope the
		// service to the wrong fabric network.
		if !driver.ValidVNI(alloc.VNI) {
			return fmt.Errorf("refusing to create CXI service for %s: VNI %d out of range [%d, %d]",
				alloc.DeviceName, alloc.VNI, driver.MinVNI, driver.MaxVNI)
		}
		fmt.Printf("cxi-nri: creating CXI service: pod=%s device=%s hsn=%s netns=%s inode=%d vni=%d\n",
			podUID, alloc.DeviceName, alloc.HSNIface, netnsPath, inode, alloc.VNI)
		svcID, err := p.svcMgr.CreateService(alloc.DeviceName, inode, alloc.VNI, alloc.Resources)
		if err != nil {
			return fmt.Errorf("creating CXI service for %s: %w", alloc.DeviceName, err)
		}
		state.svcIDs[alloc.DeviceName] = svcID
	}

	p.mu.Lock()
	p.podStates[podUID] = state
	p.mu.Unlock()

	return nil
}

// CreateContainer is called by containerd before each container in a pod is
// started. The CXI service was already created in RunPodSandbox; here we just
// inject the FI_CXI_SVC_ID env vars so libfabric uses the pre-allocated service.
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

	// Inject FI_CXI_SVC_ID per device. If there is exactly one CXI device also
	// set the bare FI_CXI_SVC_ID that libfabric reads by default.
	for devName, svcID := range svcIDs {
		envKey := fmt.Sprintf("FI_CXI_SVC_ID_%s", devName)
		adj.AddEnv(envKey, fmt.Sprintf("%d", svcID))
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

// StopPodSandbox is called by containerd when the pod sandbox stops.
// We tear down the CXI service here, matching the reference plugin pattern.
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
		fmt.Printf("cxi-nri: deleting CXI service: pod=%s device=%s inode=%d\n",
			podUID, alloc.DeviceName, inode)
		if err := p.svcMgr.DeleteService(alloc.DeviceName, inode); err != nil {
			fmt.Printf("cxi-nri: warning: deleting CXI service for %s: %v\n", alloc.DeviceName, err)
		}
	}

	p.store.Delete(podUID)
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

// netnsInode returns the nsfs inode of the pod's network namespace.
// We read it via /proc/<pid>/ns/net rather than stat-ing the netns bind-mount
// path, because stat() on a bind-mount of a nsfs file from a different mount
// namespace returns the inode of the bind-mount entry on the underlying
// filesystem, not the kernel nsfs inode that the CXI driver uses to scope
// services. /proc/<pid>/ns/net resolves directly into nsfs and fstat gives
// the correct inode.
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

// isMockManager reports whether svcMgr is the mock implementation.
func isMockManager(svcMgr cxi.ServiceManager) bool {
	_, ok := svcMgr.(cxi.MockServiceManager)
	return ok
}
