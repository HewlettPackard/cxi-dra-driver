package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// pendingEntry records a grant that has been issued to the ClaimBinder
// (Ready=True set) but for which the NRI plugin has not yet created the
// hardware service. The resources are held in the pending map so that
// subsequent binder evaluations on the same device see the reduced capacity
// and do not double-grant.
type pendingEntry struct {
	deviceName string
	resources  cxi.ResourceSet
}

// pendingKey identifies one in-flight grant. A claim that allocates more than
// one CXI device (a count>1 request) holds one pending entry per allocation
// result. Results on one shared device (a VF-mode parent) differ only by
// ShareID, so it is part of the key.
type pendingKey struct {
	claimUID   types.UID
	deviceName string
	shareID    string
}

// Config holds all configuration for a Driver instance.
type Config struct {
	NodeName             string
	PodUID               string
	DriverName           string
	CDIRoot              string
	PluginDir            string
	RegistrarDir         string
	KubeClient           kubernetes.Interface
	MockDevices          []*cxi.Device
	SystemSvcIDs         []int // blessed system-service svc_ids; their reserved resources are excluded from each device's usable ceiling
	VFOnly               bool  // discover and publish SR-IOV VFs only (--vf-only); the default publishes PFs only
	VNIRangeStart        int   // --vni-range-start; with VNIRangeEnd, checked against each parent's VNI set at startup (0 = unset)
	VNIRangeEnd          int   // --vni-range-end
	RefreshInterval      time.Duration
	CDIEnvConfig         cdi.CDIEnvConfig // host libs and env vars for the env spec; zero value when --enable-cdi-env is unset
	ContainerdConfigPath string           // path for the containerd CDI config drop-in; empty = skip
	CNIHintsDir          string           // host dir for per-pod CNI hint files; empty disables the hint-file write/delete
}

// Driver implements kubeletplugin.DRAPlugin for CXI devices.
type Driver struct {
	nodeName             string
	podUID               string
	driverName           string
	cdiRoot              string
	pluginDir            string
	registrarDir         string
	kubeClient           kubernetes.Interface
	mockDevices          []*cxi.Device
	devices              []*cxi.Device
	store                *AllocationStore
	systemSvcIDs         []int         // blessed system-service svc_ids passed to cxi.Discover; reserved resources excluded from each device's usable ceiling
	vfOnly               bool          // passed to cxi.Discover: publish SR-IOV VFs only
	vniRangeStart        int           // cluster VNI range, for the parent coverage warning
	vniRangeEnd          int           // 0 with vniRangeStart when unset
	refreshInterval      time.Duration // how often the orphan-service reaper runs
	cdiEnvConfig         cdi.CDIEnvConfig
	containerdConfigPath string // path for the containerd CDI config drop-in; empty = skip
	cniHintsDir          string // host dir for per-pod CNI hint files; empty disables the hint-file write/delete

	pendingMu sync.Mutex
	pending   map[pendingKey]pendingEntry // (claim, device, share) → in-flight grant not yet backed by a service

	// parents are the VF-mode published devices (empty in PF mode). parentLocks
	// holds one *sync.Mutex per parent name: Prepare holds it across VF
	// selection, store.Set and the hint write, so concurrent Prepares under one
	// parent never pick the same VF.
	parents     parentSet
	parentLocks sync.Map

	// recoveredBusy marks VFs found at startup with a pod child but no hint
	// file: busy for selection until the reaper has removed the child.
	recoveredMu   sync.Mutex
	recoveredBusy map[string]string // VF name → reason

	// Orphan-service reaper state. The reaper runs on the same Run ticker as
	// the periodic ResourceSlice republish and destroys CXI services whose
	// owning pod is gone. All three fields are
	// touched solely by reapOrphanServices, which runs only from the single
	// Run goroutine, so they need no additional locking.
	//
	// reapSvcMgr issues the DeleteService calls: the driver's own stateless
	// ServiceManager (CGo-backed in production), independent of the NRI
	// plugin's manager so the reaper stays self-contained.
	//
	// liveSvcIDs returns the svc_ids the NRI plugin still owns for live pods,
	// grouped by device, so the reaper never deletes a service backing a
	// running pod. nil until main wires plugin.LiveSvcIDs via
	// SetLiveSvcIDsFunc; while nil the reaper is inert.
	//
	// reaperMisses counts the consecutive cycles each (device, svc_id) has
	// looked orphaned; a service is destroyed only after reapGraceCycles
	// confirmations so a pod mid-start is never reaped.
	reapSvcMgr   cxi.ServiceManager
	liveSvcIDs   func() map[string]map[int]bool
	reaperMisses map[reapKey]int

	// reaperCycle counts reap passes that scanned services; reaperWarned records the
	// cycle each unattributable VF child was last reported on, so the warning
	// is not repeated every cycle.
	reaperCycle  int
	reaperWarned map[reapKey]int

	// logFn replaces the stdout logger when set; for tests.
	logFn func(format string, args ...any)
}

func New(cfg Config) *Driver {
	return &Driver{
		nodeName:             cfg.NodeName,
		podUID:               cfg.PodUID,
		driverName:           cfg.DriverName,
		cdiRoot:              cfg.CDIRoot,
		pluginDir:            cfg.PluginDir,
		registrarDir:         cfg.RegistrarDir,
		kubeClient:           cfg.KubeClient,
		mockDevices:          cfg.MockDevices,
		store:                NewAllocationStore(),
		systemSvcIDs:         cfg.SystemSvcIDs,
		vfOnly:               cfg.VFOnly,
		vniRangeStart:        cfg.VNIRangeStart,
		vniRangeEnd:          cfg.VNIRangeEnd,
		refreshInterval:      cfg.RefreshInterval,
		cdiEnvConfig:         cfg.CDIEnvConfig,
		containerdConfigPath: cfg.ContainerdConfigPath,
		cniHintsDir:          cfg.CNIHintsDir,
		pending:              make(map[pendingKey]pendingEntry),
		recoveredBusy:        make(map[string]string),
		reapSvcMgr:           cxi.NewServiceManager(),
		reaperMisses:         make(map[reapKey]int),
	}
}

// Store returns the allocation store, so it can be passed to the NRI plugin.
func (d *Driver) Store() *AllocationStore {
	return d.store
}

// SetLiveSvcIDsFunc injects the source of currently-live svc_ids — the NRI
// plugin's LiveSvcIDs — consulted by the orphan-service reaper so it never
// destroys a service backing a running pod. Must be called before Run; until
// it is, the reaper does nothing. Wired in main after the NRI plugin is
// constructed, which breaks the construction cycle: the plugin needs the
// driver's store, and the driver needs the plugin's live-svc-id view.
func (d *Driver) SetLiveSvcIDsFunc(fn func() map[string]map[int]bool) {
	d.liveSvcIDs = fn
}

// RemovePending removes all in-flight pending grants for a claim. Called by
// the NRI plugin after a pod's CXI services are successfully created — at that
// point the hardware services account for the resources and the pending
// entries would otherwise double-subtract from effective capacity. A claim
// that allocated more than one device has one pending entry per device; all of
// them are cleared here.
func (d *Driver) RemovePending(claimUID types.UID) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	for k := range d.pending {
		if k.claimUID == claimUID {
			delete(d.pending, k)
		}
	}
}

// checkAndAddPending atomically validates a grant request for one allocation
// result (claim, device, share) against capacity:
//
//	requested must fit within (hwAvail − sum of pending grants on this
//	device, excluding this result's own prior entry).
//
// A claim that allocates more than one device calls this once per result.
//
// Multiple claims from the same namespace MAY co-locate on one device: the
// SHS14 VNI-per-device-uniqueness constraint has been lifted, so there is no
// namespace-occupancy rejection — only capacity gates a grant.
//
// hwAvail must be the raw value from ReadAvailableCapacity — do NOT subtract
// the device's reserved set before calling, as ReadAvailableCapacity already
// reflects all running service consumption including the blessed system
// services (--system-svc-ids).
//
// Idempotent: if THIS claim already holds a pending grant for (device), the
// call is a no-op grant — re-evaluation does not double-count.
//
// Returns true when granted (pending updated); false when effective capacity
// is insufficient. The ClaimBinder distinguishes "raw hardware fits but
// pending grants are blocking" (Ready=False, retry) from "raw hardware itself
// cannot fit" (Unschedulable) by a second check against hwAvail alone.
func (d *Driver) checkAndAddPending(
	claimUID types.UID,
	deviceName, shareID string,
	requested, hwAvail cxi.ResourceSet,
) (granted bool) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	key := pendingKey{claimUID: claimUID, deviceName: deviceName, shareID: shareID}

	// Idempotent re-grant: this result already holds a pending entry. Nothing
	// to change — re-evaluation of an already-granted device
	// (e.g. while another device of the same claim is still settling) must
	// not double-count its pending entry.
	if _, ok := d.pending[key]; ok {
		return true
	}

	// Capacity check. The sum reflects all in-flight pending grants on this
	// device, including any from other claims in the same namespace (which
	// may now co-locate here). This claim has no prior pending entry for the
	// device — the idempotent guard above returned early if it held one.
	var pendingSum cxi.ResourceSet
	for _, e := range d.pending {
		if e.deviceName == deviceName {
			pendingSum = pendingSum.Add(e.resources)
		}
	}
	effective := hwAvail.Sub(pendingSum).ClampToZero()
	if !requested.Fits(effective) {
		return false
	}

	d.pending[key] = pendingEntry{
		deviceName: deviceName,
		resources:  requested,
	}
	return true
}

// reconcileClaimDevices releases any pending grant the claim holds for an
// allocation result NOT in current (keyed by device and ShareID). It covers a
// subset reallocation: the scheduler can move a claim from device set {A,B} to
// {A,C} without our ever seeing Allocation=nil in between, which would
// otherwise leak B's pending grant and understate B's effective capacity.
// Called by the ClaimBinder before it (re)evaluates the claim's current
// results.
func (d *Driver) reconcileClaimDevices(claimUID types.UID, current map[pendingKey]bool) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	// Drop pending entries for results the claim no longer holds so they
	// don't inflate the per-device pendingSum or linger after the service is
	// gone.
	for k := range d.pending {
		if k.claimUID == claimUID && !current[pendingKey{deviceName: k.deviceName, shareID: k.shareID}] {
			delete(d.pending, k)
		}
	}
}

// hasPending reports whether there is an active in-flight pending grant for
// the given allocation result.
func (d *Driver) hasPending(claimUID types.UID, deviceName, shareID string) bool {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	_, ok := d.pending[pendingKey{claimUID: claimUID, deviceName: deviceName, shareID: shareID}]
	return ok
}

// logf prints a formatted driver log line to stdout.
func (d *Driver) logf(format string, args ...any) {
	if d.logFn != nil {
		d.logFn(format, args...)
		return
	}
	fmt.Printf("cxi-dra-driver: "+format+"\n", args...)
}

// Run discovers devices, writes CDI specs, starts the kubelet plugin, and
// publishes a ResourceSlice, then runs the orphan-service reaper on the
// configured interval. Blocks until ctx is cancelled.
func (d *Driver) Run(ctx context.Context) error {
	if d.mockDevices != nil {
		d.devices = d.mockDevices
	} else {
		devices, err := cxi.Discover(cxi.DiscoverOptions{SystemSvcIDs: d.systemSvcIDs, VFOnly: d.vfOnly})
		if err != nil {
			return fmt.Errorf("discovering CXI devices: %w", err)
		}
		d.devices = devices
	}
	d.setupParents()

	for _, dev := range d.programmableDevices() {
		if err := cdi.WriteSpec(d.cdiRoot, d.driverName, dev); err != nil {
			return fmt.Errorf("writing CDI spec for %s: %w", dev.Name, err)
		}
	}

	// The env spec is attached to every CXI device, so it is always written:
	// it carries the kdreg2/gdrdrv/xpmem nodes even when no host libs are
	// configured.
	if err := cdi.WriteEnvSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
		return fmt.Errorf("writing CDI env spec: %w", err)
	}

	// Write containerd CDI config drop-in and reload containerd AFTER all spec
	// files are on disk — containerd's CDI cache must see the complete set.
	if d.containerdConfigPath != "" {
		changed, err := cdi.WriteContainerdCDIConfig(d.containerdConfigPath, []string{d.cdiRoot})
		if err != nil {
			return fmt.Errorf("writing containerd CDI config: %w", err)
		}
		// Only reload containerd when the drop-in actually changed. The file
		// lives on a persistent hostPath, so an image-only DaemonSet rollout
		// finds identical content; SIGHUPing containerd in that case is a
		// gratuitous reload that tears down the CRI/NRI subsystems and can
		// briefly flip the node into NotReady. Reload only on genuine change
		// (first install or a config edit).
		if changed {
			pid, err := cdi.FindContainerdPID("/proc")
			if err != nil {
				return fmt.Errorf("finding containerd PID: %w", err)
			}
			if err := cdi.SignalContainerd(pid); err != nil {
				return fmt.Errorf("signalling containerd: %w", err)
			}
		}
	}

	opts := []kubeletplugin.Option{
		kubeletplugin.KubeClient(d.kubeClient),
		kubeletplugin.NodeName(d.nodeName),
		kubeletplugin.DriverName(d.driverName),
		kubeletplugin.PluginDataDirectoryPath(d.pluginDir),
		kubeletplugin.RegistrarDirectoryPath(d.registrarDir),
	}
	if d.podUID != "" {
		opts = append(opts, kubeletplugin.RollingUpdate(types.UID(d.podUID)))
	}
	// Rebuild which VFs are taken before kubelet can call Prepare.
	d.recoverVFState(ctx)
	helper, err := kubeletplugin.Start(ctx, d, opts...)
	if err != nil {
		return fmt.Errorf("starting kubelet plugin: %w", err)
	}
	defer helper.Stop()

	// The slice holds only static attributes and capacities, so it is
	// published once; the helper's controller keeps it in sync from then on.
	if err := helper.PublishResources(ctx, d.buildDriverResources()); err != nil {
		return fmt.Errorf("publishing resources: %w", err)
	}

	// reapOrphanServices runs on the ticker. It destroys the underlying
	// CXI services whose owning pod is gone — leaks the event path (NRI
	// StopPodSandbox) never cleaned up because the pod was force-deleted, the
	// driver restarted, or a service was created out of band. It deletes only
	// after several consecutive cycles confirm the service's pod netns is
	// gone, and never touches blessed, kernel-system, or NRI-tracked services.
	if d.refreshInterval <= 0 {
		return fmt.Errorf("--resource-refresh-interval must be > 0 (got %s)", d.refreshInterval)
	}
	ticker := time.NewTicker(d.refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			d.reapOrphanServices()
		}
	}
}

// HandleError is called by the kubelet plugin helper for background errors
// (e.g. ResourceSlice controller failures).
func (d *Driver) HandleError(_ context.Context, err error, msg string) {
	d.logf("background error: %s: %v", msg, err)
}
