package driver

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
// and do not double-grant. The namespace is recorded so that two claims from
// the same namespace are prevented from being granted on the same device —
// see nsDeviceOccupancy on Driver.
type pendingEntry struct {
	namespace  string
	deviceName string
	resources  cxi.ResourceSet
}

// pendingKey identifies one in-flight grant. A claim that allocates more than
// one CXI device (a count>1 request) holds one pending entry per device, so
// the map is keyed by (claim, device) rather than claim alone.
type pendingKey struct {
	claimUID   types.UID
	deviceName string
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
	RefreshInterval      time.Duration
	CDIEnvConfig         cdi.CDIEnvConfig
	EnableCDIEnv         bool   // if false, env/nccl/ofi CDI specs are not written and not injected into pods
	ContainerdConfigPath string // path for the containerd CDI config drop-in; empty = skip
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
	refreshInterval      time.Duration // how often to republish ResourceSlice with fresh avail attrs
	cdiEnvConfig         cdi.CDIEnvConfig
	enableCDIEnv         bool          // if false, env/nccl/ofi CDI specs are not written and not injected into pods
	containerdConfigPath string        // path for the containerd CDI config drop-in; empty = skip
	notify               chan struct{} // signals an immediate ResourceSlice republish

	pendingMu sync.Mutex
	pending   map[pendingKey]pendingEntry // (claim, device) → in-flight grant not yet backed by a service
	// nsDeviceOccupancy records which claim currently holds each
	// (namespace, device) slot. Keyed namespace → device → claim UID.
	// At most one entry per (namespace, device) is ever present:
	// checkAndAddPending rejects a second grant in the same namespace on
	// the same device with occupied=true, so two claims from one namespace
	// cannot simultaneously occupy one device.
	//
	// Used by the ClaimBinder to enforce the SHS14 per-device VNI
	// uniqueness constraint: every claim in a namespace shares the
	// namespace's VNI, and the kernel rejects two services with the same
	// VNI on the same device with -EEXIST from cxil_alloc_svc (the RX-
	// profile vni_overlap_test rejects a second service whose VNI matches
	// an existing one on the device).
	// Also drives the active_namespaces ResourceSlice attribute that
	// pre-filters scheduling via the admission-injected CEL selector — see
	// activeNamespacesFor.
	//
	// Lifecycle:
	//   - written in checkAndAddPending when Ready=True is granted.
	//   - cleared in RemoveOccupancy (by namespace/device, called by the
	//     NRI plugin after DestroyService on StopPodSandbox) or in
	//     RemoveOccupancyForClaim (by claim UID, called by the ClaimBinder
	//     when the scheduler clears Status.Allocation or a claim is
	//     deleted before NRI fires StopPodSandbox).
	//
	// The slot is released only after the service is destroyed (NOT on
	// RemovePending) because the service holds the VNI on the device until
	// then. Releasing earlier risks another claim being granted on the
	// same (namespace, device) pair, which would then collide at
	// CreateService.
	nsDeviceOccupancy map[string]map[string]types.UID
	// claimToOccupancy is the reverse index of nsDeviceOccupancy: given a
	// claim UID, find which (namespace, device) slot it holds. Lets the
	// claim binder release the slot via RemoveOccupancyForClaim without
	// having to know the device name — which it doesn't, once the
	// scheduler has cleared Status.Allocation. The two maps are kept in
	// lockstep by removeOccupancyLocked and checkAndAddPending; the binder
	// holds no occupancy state of its own.
	//
	// A claim that allocates more than one CXI device (count>1) holds one
	// slot per device, so the value is a slice. Single-device claims — the
	// common case — hold a one-element slice.
	claimToOccupancy map[types.UID][]occupancyKey

	// Orphan-service reaper state. The reaper runs on the same Run ticker as
	// the reconciler and destroys CXI services whose owning pod is gone — the
	// gap the reconciler cannot close, since it only repairs in-memory
	// occupancy and never the hardware service itself. All three fields are
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
}

// occupancyKey identifies one namespace's hold on one device. Used as the
// value type of claimToOccupancy so the claim binder can release a slot
// without re-deriving the (namespace, device) pair from external state.
type occupancyKey struct {
	namespace  string
	deviceName string
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
		refreshInterval:      cfg.RefreshInterval,
		cdiEnvConfig:         cfg.CDIEnvConfig,
		enableCDIEnv:         cfg.EnableCDIEnv,
		containerdConfigPath: cfg.ContainerdConfigPath,
		notify:               make(chan struct{}, 1),
		pending:              make(map[pendingKey]pendingEntry),
		nsDeviceOccupancy:    make(map[string]map[string]types.UID),
		claimToOccupancy:     make(map[types.UID][]occupancyKey),
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

// TriggerRepublish schedules an immediate ResourceSlice republish. If one is
// already pending this is a no-op — the buffered channel holds at most one
// signal.
func (d *Driver) TriggerRepublish() {
	select {
	case d.notify <- struct{}{}:
	default:
	}
}

// RemovePending removes all in-flight pending grants for a claim. Called by
// the NRI plugin after a pod's CXI services are successfully created — at that
// point the hardware services account for the resources and the pending
// entries would otherwise double-subtract from effective capacity. A claim
// that allocated more than one device has one pending entry per device; all of
// them are cleared here.
//
// Does NOT touch nsDeviceOccupancy. The service is alive on the device until
// DestroyService runs, so the namespace continues to occupy this device for
// the purpose of SHS14 per-device VNI uniqueness. RemoveOccupancy is the
// only safe decrement point — see its docs and the nsDeviceOccupancy field
// comment.
func (d *Driver) RemovePending(claimUID types.UID) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	for k := range d.pending {
		if k.claimUID == claimUID {
			delete(d.pending, k)
		}
	}
}

// removeOccupancyLocked releases the (namespace, deviceName) slot, if any,
// and clears the matching reverse-index entry. Returns true iff state
// actually changed — the caller uses that to decide whether to call
// TriggerRepublish. Caller MUST hold pendingMu.
//
// Centralising the cleanup here keeps nsDeviceOccupancy and
// claimToOccupancy strictly in lockstep: every code path that releases a
// slot goes through this function.
func (d *Driver) removeOccupancyLocked(namespace, deviceName string) bool {
	devs := d.nsDeviceOccupancy[namespace]
	if devs == nil {
		return false
	}
	claimUID, ok := devs[deviceName]
	if !ok {
		return false
	}
	delete(devs, deviceName)
	if len(devs) == 0 {
		delete(d.nsDeviceOccupancy, namespace)
	}
	// Drop this (namespace, device) slot from the claim's slot list, keeping
	// any other devices the claim still holds. Delete the claim's entry only
	// once its last slot is released.
	keys := d.claimToOccupancy[claimUID]
	kept := keys[:0]
	for _, k := range keys {
		if k.namespace == namespace && k.deviceName == deviceName {
			continue
		}
		kept = append(kept, k)
	}
	if len(kept) == 0 {
		delete(d.claimToOccupancy, claimUID)
	} else {
		d.claimToOccupancy[claimUID] = kept
	}
	return true
}

// RemoveOccupancy releases the (namespace, deviceName) slot. Called by the
// NRI plugin in StopPodSandbox after a successful DestroyService: at that
// point the kernel no longer holds the VNI on the device and another claim
// in the same namespace can be placed here without triggering -EEXIST from
// cxil_alloc_svc (whose RX-profile vni_overlap_test would otherwise reject
// the new service).
//
// Idempotent: calls for an unknown (namespace, deviceName) pair are no-ops,
// so the path is safe if NRI fires StopPodSandbox twice for the same sandbox
// (e.g. retry on transient runtime error).
//
// On a real state change this triggers an immediate ResourceSlice republish
// so the device's active_namespaces attribute drops this namespace and the
// MutatingAdmissionPolicy-injected CEL selector lets other claims in this
// namespace target this device again. (Refusal is a no-op so an idempotent
// retry does not churn the ResourceSlice.)
//
// Limitation: if a pod is force-deleted before RunPodSandbox runs (so NRI
// never fires StopPodSandbox), this path cannot fire — the ClaimBinder's
// RemoveOccupancyForClaim covers that case via the watch.Deleted event.
func (d *Driver) RemoveOccupancy(namespace, deviceName string) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	if d.removeOccupancyLocked(namespace, deviceName) {
		d.TriggerRepublish()
	}
}

// RemoveOccupancyForClaim releases whichever (namespace, device) slot the
// given claim holds, if any. Called by the ClaimBinder when:
//
//   - the scheduler clears Status.Allocation (e.g. PostFilter after a
//     PreBind failure or an Unschedulable=True signal), and
//   - a ResourceClaim is deleted (watch.Deleted event).
//
// Both cases cannot rely on the NRI StopPodSandbox callback to clean up:
// the scheduler may de-allocate before NRI has done (or will ever do)
// anything with the pod. Without this, the slot would leak in
// nsDeviceOccupancy and the active_namespaces attribute would keep
// excluding the device for this namespace.
//
// Idempotent: claims with no recorded slot are no-ops. A claim that allocated
// more than one device releases every slot it holds. Triggers an immediate
// republish on real state change so the freed slots become visible to other
// claims in the namespace immediately, not at the next 10s periodic refresh.
func (d *Driver) RemoveOccupancyForClaim(claimUID types.UID) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	keys, ok := d.claimToOccupancy[claimUID]
	if !ok {
		return
	}
	// Copy first: removeOccupancyLocked mutates the slice stored under this
	// claim, so ranging over the live slice while it shrinks is unsafe.
	changed := false
	for _, key := range append([]occupancyKey(nil), keys...) {
		if d.removeOccupancyLocked(key.namespace, key.deviceName) {
			changed = true
		}
	}
	if changed {
		d.TriggerRepublish()
	}
}

// checkAndAddPending atomically validates a grant request for one
// (claim, device) against two constraints:
//
//  1. (namespace, deviceName) must not already be occupied by a DIFFERENT
//     claim from the same namespace. This enforces the SHS14 per-device VNI
//     uniqueness constraint (see the nsDeviceOccupancy field docs).
//  2. requested must fit within (hwAvail − sum of pending grants on this
//     device, excluding this claim's own prior entry for this device).
//
// A claim that allocates more than one device calls this once per device; the
// grants are independent because each targets a distinct (namespace, device)
// slot. Two distinct devices in one namespace at the same VNI do not violate
// SHS14 (uniqueness is per device), so the occupancy guard only fires for a
// different claim on the same device.
//
// hwAvail must be the raw value from ReadAvailableCapacity — do NOT subtract
// the device's reserved set before calling, as ReadAvailableCapacity already
// reflects all running service consumption including the blessed system
// services (--system-svc-ids).
//
// Idempotent: if THIS claim already holds (namespace, deviceName), the call is
// a no-op grant — re-evaluation does not double-count or churn the slice.
// Stale slots from a subset reallocation (claim dropped a device without an
// intervening Allocation=nil) are released by reconcileClaimDevices before the
// per-device loop, not here.
//
// Return values:
//
//	(true, false)  — granted; occupancy and pending updated.
//	(false, true)  — refused because the namespace already occupies this
//	                 device via another claim. The ClaimBinder turns this
//	                 into Unschedulable=True so the scheduler retries on a
//	                 different device.
//	(false, false) — refused because effective capacity is insufficient.
//	                 The ClaimBinder distinguishes "raw hardware fits but
//	                 pending grants are blocking" (Ready=False, retry) from
//	                 "raw hardware itself cannot fit" (Unschedulable) by
//	                 a second check against hwAvail alone.
func (d *Driver) checkAndAddPending(
	claimUID types.UID,
	namespace, deviceName string,
	requested, hwAvail cxi.ResourceSet,
) (granted, occupied bool) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	// Idempotent re-grant: this claim already holds this exact (ns, device)
	// slot. Nothing to change — re-evaluation of an already-granted device
	// (e.g. while another device of the same claim is still settling) must
	// not release-and-readd the slot or double-count its pending entry.
	if held, ok := d.nsDeviceOccupancy[namespace][deviceName]; ok && held == claimUID {
		return true, false
	}

	// SHS14 enforces VNI uniqueness per device kernel-side. Every claim in
	// the same namespace shares the namespace VNI, so a DIFFERENT claim from
	// the same namespace cannot co-locate on this device.
	if _, exists := d.nsDeviceOccupancy[namespace][deviceName]; exists {
		return false, true
	}

	// Capacity check excludes this claim's own prior pending entry for this
	// device (there is none — the idempotent guard above returned early if it
	// held the slot), so the sum reflects only OTHER claims on the device.
	var pendingSum cxi.ResourceSet
	for _, e := range d.pending {
		if e.deviceName == deviceName {
			pendingSum = pendingSum.Add(e.resources)
		}
	}
	effective := hwAvail.Sub(pendingSum).ClampToZero()
	if !requested.Fits(effective) {
		return false, false
	}

	d.pending[pendingKey{claimUID: claimUID, deviceName: deviceName}] = pendingEntry{
		namespace:  namespace,
		deviceName: deviceName,
		resources:  requested,
	}
	if d.nsDeviceOccupancy[namespace] == nil {
		d.nsDeviceOccupancy[namespace] = make(map[string]types.UID)
	}
	d.nsDeviceOccupancy[namespace][deviceName] = claimUID
	d.claimToOccupancy[claimUID] = append(d.claimToOccupancy[claimUID],
		occupancyKey{namespace: namespace, deviceName: deviceName})
	// Grant corresponds to a 0→1 transition on (namespace, device). Trigger an
	// immediate ResourceSlice republish so:
	//   * avail_* reflects the new pending grant
	//   * active_namespaces gains this namespace on this device
	// The admission-injected CEL selector then steers other claims in this
	// namespace away from this device.
	d.TriggerRepublish()
	return true, false
}

// reconcileClaimDevices releases any pending grant and occupancy slot the
// claim holds for a device NOT in currentDevices. It covers a subset
// reallocation: the scheduler can move a claim from device set {A,B} to {A,C}
// without our ever seeing Allocation=nil in between, which would otherwise
// leak B's slot in nsDeviceOccupancy and keep the namespace pinned to B via
// active_namespaces. Called by the ClaimBinder before it (re)evaluates the
// claim's current device set. Triggers a single republish on any change.
func (d *Driver) reconcileClaimDevices(claimUID types.UID, currentDevices map[string]bool) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	changed := false

	// Release occupancy slots for devices the claim no longer holds. Copy the
	// slice first: removeOccupancyLocked mutates it.
	for _, key := range append([]occupancyKey(nil), d.claimToOccupancy[claimUID]...) {
		if currentDevices[key.deviceName] {
			continue
		}
		if d.removeOccupancyLocked(key.namespace, key.deviceName) {
			changed = true
		}
	}
	// Drop pending entries for the same dropped devices so they don't inflate
	// the per-device pendingSum or linger after the service is gone.
	for k := range d.pending {
		if k.claimUID == claimUID && !currentDevices[k.deviceName] {
			delete(d.pending, k)
			changed = true
		}
	}

	if changed {
		d.TriggerRepublish()
	}
}

// hasPending reports whether there is an active in-flight pending grant for
// the given (claim, device).
func (d *Driver) hasPending(claimUID types.UID, deviceName string) bool {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	_, ok := d.pending[pendingKey{claimUID: claimUID, deviceName: deviceName}]
	return ok
}

// effectiveAvail returns the current available capacity for a device adjusted
// for in-flight pending grants:
//
//	ReadAvailableCapacity(deviceName) − pending grants
//
// Note: the device's reserved set is NOT subtracted here. ReadAvailableCapacity
// already reflects the actual consumption of all running services (including
// the blessed system services named by --system-svc-ids). The reserved set is
// only used to form the prepare-time usable ceiling — see usableCeiling.
// Used by buildDevices to publish accurate avail_* attributes in the
// ResourceSlice so the scheduler can make informed placement decisions.
func (d *Driver) effectiveAvail(deviceName string) cxi.ResourceSet {
	avail, err := cxi.ReadAvailableCapacity(deviceName)
	if err != nil {
		d.logf("warning: reading available capacity for %s: %v", deviceName, err)
		avail = cxi.ResourceSet{}
	}
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	var pendingSum cxi.ResourceSet
	for _, e := range d.pending {
		if e.deviceName == deviceName {
			pendingSum = pendingSum.Add(e.resources)
		}
	}
	return avail.Sub(pendingSum).ClampToZero()
}

// activeNamespacesFor returns a sorted, comma-padded string of namespaces
// that currently hold a pending grant or a live service on the given device.
//
// Format:
//
//	""              when no namespaces occupy the device
//	",ns1,"         single namespace
//	",ns1,ns2,..."  multiple namespaces, lexicographically sorted
//
// The leading and trailing commas let the admission-injected CEL selector use
// string.contains(",<NS>,") for exact, non-prefix matching:
// ",foo,bar," does not match ",foobar," because the `,` framing is required
// on both sides of the namespace name.
//
// Published as the active_namespaces attribute on each device's ResourceSlice
// entry. The MutatingAdmissionPolicy injects a per-claim CEL selector that
// excludes devices whose active_namespaces already contains the claim's
// namespace — pre-filtering at scheduling time so the DRA scheduler never
// picks a device that would later be rejected by SHS14's per-device VNI
// uniqueness constraint. See nsDeviceOccupancy for the underlying state.
func (d *Driver) activeNamespacesFor(deviceName string) string {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	var names []string
	for ns, devs := range d.nsDeviceOccupancy {
		if _, ok := devs[deviceName]; ok {
			names = append(names, ns)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "," + strings.Join(names, ",") + ","
}

// logf prints a formatted driver log line to stdout.
func (d *Driver) logf(format string, args ...any) {
	fmt.Printf("cxi-dra-driver: "+format+"\n", args...)
}

// Run discovers devices, writes CDI specs, starts the kubelet plugin, and
// publishes a ResourceSlice. Refreshes available-capacity attributes on the
// configured interval. Blocks until ctx is cancelled.
func (d *Driver) Run(ctx context.Context) error {
	if d.mockDevices != nil {
		d.devices = d.mockDevices
	} else {
		devices, err := cxi.Discover(d.systemSvcIDs)
		if err != nil {
			return fmt.Errorf("discovering CXI devices: %w", err)
		}
		d.devices = devices
	}

	for _, dev := range d.devices {
		if err := cdi.WriteSpec(d.cdiRoot, d.driverName, dev); err != nil {
			return fmt.Errorf("writing CDI spec for %s: %w", dev.Name, err)
		}
	}

	if d.enableCDIEnv {
		if err := cdi.WriteEnvSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
			return fmt.Errorf("writing CDI env spec: %w", err)
		}
		if d.cdiEnvConfig.NCCLPrefix != "" {
			if err := cdi.WriteNCCLSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
				return fmt.Errorf("writing CDI NCCL spec: %w", err)
			}
		}
		if len(d.cdiEnvConfig.AWSofiMounts) > 0 {
			if err := cdi.WriteAWSofiNCCLSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
				return fmt.Errorf("writing CDI aws-ofi-nccl spec: %w", err)
			}
		}
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
	helper, err := kubeletplugin.Start(ctx, d, opts...)
	if err != nil {
		return fmt.Errorf("starting kubelet plugin: %w", err)
	}
	defer helper.Stop()

	publish := func() error {
		return helper.PublishResources(ctx, d.buildDriverResources())
	}

	if err := publish(); err != nil {
		return fmt.Errorf("publishing resources: %w", err)
	}

	// Periodically republish with refreshed available-capacity attributes.
	// Also responds to immediate republish signals from the ClaimBinder (after
	// granting Ready=True) and from the NRI plugin (after service create/delete).
	//
	// The ticker also drives reconcileOccupancyFromHardware, which closes
	// the gap between in-memory occupancy state and actual hardware
	// services for drift cases the event path cannot cover (driver
	// restart, missed watch events, force-deleted pods that bypassed
	// NRI). The reconciler runs before publish so any state change it
	// makes is reflected in the same ResourceSlice update — and if it
	// triggers an extra republish via the notify channel, the next loop
	// iteration absorbs it harmlessly.
	//
	// reapOrphanServices runs right after the reconciler. Where the
	// reconciler only repairs in-memory occupancy, the reaper destroys the
	// underlying CXI services whose owning pod is gone — leaks the event
	// path (NRI StopPodSandbox) never cleaned up because the pod was
	// force-deleted, the driver restarted, or a service was created out of
	// band. It deletes only after several consecutive cycles confirm the
	// service's pod netns is gone, and never touches blessed, kernel-system,
	// or NRI-tracked services.
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
			d.reconcileOccupancyFromHardware(ctx)
			d.reapOrphanServices()
			if err := publish(); err != nil {
				d.logf("warning: refreshing resource slice: %v", err)
			}
		case <-d.notify:
			if err := publish(); err != nil {
				d.logf("warning: refreshing resource slice (event-driven): %v", err)
			}
		}
	}
}

// HandleError is called by the kubelet plugin helper for background errors
// (e.g. ResourceSlice controller failures).
func (d *Driver) HandleError(_ context.Context, err error, msg string) {
	d.logf("background error: %s: %v", msg, err)
}
