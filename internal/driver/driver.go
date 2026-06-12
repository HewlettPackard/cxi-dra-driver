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

// Config holds all configuration for a Driver instance.
type Config struct {
	NodeName        string
	PodUID          string
	DriverName      string
	CDIRoot         string
	PluginDir       string
	RegistrarDir    string
	KubeClient      kubernetes.Interface
	MockDevices     []*cxi.Device
	Overhead        cxi.ResourceSet
	RefreshInterval time.Duration
	CDIEnvConfig         cdi.CDIEnvConfig
	EnableCDIEnv         bool   // if false, env/nccl/ofi CDI specs are not written and not injected into pods
	ContainerdConfigPath string // path for the containerd CDI config drop-in; empty = skip
}

// Driver implements kubeletplugin.DRAPlugin for CXI devices.
type Driver struct {
	nodeName        string
	podUID          string
	driverName      string
	cdiRoot         string
	pluginDir       string
	registrarDir    string
	kubeClient      kubernetes.Interface
	mockDevices     []*cxi.Device
	devices         []*cxi.Device
	store           *AllocationStore
	overhead        cxi.ResourceSet // admin-configured per-device reserved overhead
	refreshInterval time.Duration   // how often to republish ResourceSlice with fresh avail attrs
	cdiEnvConfig         cdi.CDIEnvConfig
	enableCDIEnv         bool   // if false, env/nccl/ofi CDI specs are not written and not injected into pods
	containerdConfigPath string // path for the containerd CDI config drop-in; empty = skip
	notify               chan struct{} // signals an immediate ResourceSlice republish

	pendingMu sync.Mutex
	pending   map[types.UID]pendingEntry // claimUID → in-flight grant not yet backed by a service
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
	// VNI on the same device with -EEXIST from cxil_svc_set_vni_range.
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
	claimToOccupancy map[types.UID]occupancyKey
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
		nodeName:          cfg.NodeName,
		podUID:            cfg.PodUID,
		driverName:        cfg.DriverName,
		cdiRoot:           cfg.CDIRoot,
		pluginDir:         cfg.PluginDir,
		registrarDir:      cfg.RegistrarDir,
		kubeClient:        cfg.KubeClient,
		mockDevices:       cfg.MockDevices,
		store:             NewAllocationStore(),
		overhead:          cfg.Overhead,
		refreshInterval:   cfg.RefreshInterval,
		cdiEnvConfig:         cfg.CDIEnvConfig,
		enableCDIEnv:         cfg.EnableCDIEnv,
		containerdConfigPath: cfg.ContainerdConfigPath,
		notify:            make(chan struct{}, 1),
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]types.UID),
		claimToOccupancy:  make(map[types.UID]occupancyKey),
	}
}

// Store returns the allocation store, so it can be passed to the NRI plugin.
func (d *Driver) Store() *AllocationStore {
	return d.store
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

// RemovePending removes the in-flight pending grant for a claim. Called by
// the NRI plugin after a CXI service is successfully created — at that point
// the hardware service accounts for the resources and the pending entry would
// otherwise double-subtract from effective capacity.
//
// Does NOT touch nsDeviceOccupancy. The service is alive on the device until
// DestroyService runs, so the namespace continues to occupy this device for
// the purpose of SHS14 per-device VNI uniqueness. RemoveOccupancy is the
// only safe decrement point — see its docs and the nsDeviceOccupancy field
// comment.
func (d *Driver) RemovePending(claimUID types.UID) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	delete(d.pending, claimUID)
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
	delete(d.claimToOccupancy, claimUID)
	return true
}

// RemoveOccupancy releases the (namespace, deviceName) slot. Called by the
// NRI plugin in StopPodSandbox after a successful DestroyService: at that
// point the kernel no longer holds the VNI on the device and another claim
// in the same namespace can be placed here without triggering -EEXIST from
// cxil_svc_set_vni_range.
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
// Idempotent: claims with no recorded slot are no-ops. Triggers an
// immediate republish on real state change so the freed slot becomes
// visible to other claims in the namespace immediately, not at the next
// 10s periodic refresh.
func (d *Driver) RemoveOccupancyForClaim(claimUID types.UID) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	key, ok := d.claimToOccupancy[claimUID]
	if !ok {
		return
	}
	if d.removeOccupancyLocked(key.namespace, key.deviceName) {
		d.TriggerRepublish()
	}
}

// checkAndAddPending atomically validates a grant request against two
// constraints:
//
//  1. (namespace, deviceName) must not already be occupied by another claim
//     from the same namespace. This enforces the SHS14 per-device VNI
//     uniqueness constraint (see the nsDeviceOccupancy field docs).
//  2. requested must fit within (hwAvail − sum of pending grants on this
//     device, excluding any prior in-flight entry for this same claim UID).
//
// hwAvail must be the raw value from ReadAvailableCapacity — do NOT subtract
// overhead before calling, as ReadAvailableCapacity already reflects all
// running service consumption including system services.
//
// Atomic reallocation: if the same claim UID previously held a slot (the
// scheduler can re-allocate without our seeing Allocation=nil in between,
// e.g. PostFilter clears + next PreFilter re-allocates within one watch
// event), the prior slot and prior pending entry are released first so they
// do not leak and the capacity check does not double-count.
//
// On a successful grant the new entry is recorded in pending and the
// (namespace, deviceName) slot is taken in nsDeviceOccupancy +
// claimToOccupancy. A single TriggerRepublish covers both the prior release
// (if any) and the new grant.
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

	// Defensive cleanup: if the scheduler re-allocated this claim to a new
	// device without our seeing Allocation=nil in between, the prior slot
	// and prior pending entry are stale. Release them up front so they
	// don't (a) leak in nsDeviceOccupancy, (b) over-block other grants on
	// the old device, or (c) inflate the pendingSum below. Track whether
	// the release was a real state change so a subsequent refusal still
	// triggers a republish.
	priorReleased := false
	if oldKey, ok := d.claimToOccupancy[claimUID]; ok {
		priorReleased = d.removeOccupancyLocked(oldKey.namespace, oldKey.deviceName)
	}
	delete(d.pending, claimUID)

	// SHS14 enforces VNI uniqueness per device kernel-side. Every claim in
	// the same namespace shares the namespace VNI, so two claims from the
	// same namespace cannot co-locate on the same device.
	if _, exists := d.nsDeviceOccupancy[namespace][deviceName]; exists {
		if priorReleased {
			d.TriggerRepublish()
		}
		return false, true
	}

	var pendingSum cxi.ResourceSet
	for _, e := range d.pending {
		if e.deviceName == deviceName {
			pendingSum = pendingSum.Add(e.resources)
		}
	}
	effective := hwAvail.Sub(pendingSum).ClampToZero()
	if !requested.Fits(effective) {
		if priorReleased {
			d.TriggerRepublish()
		}
		return false, false
	}

	d.pending[claimUID] = pendingEntry{
		namespace:  namespace,
		deviceName: deviceName,
		resources:  requested,
	}
	if d.nsDeviceOccupancy[namespace] == nil {
		d.nsDeviceOccupancy[namespace] = make(map[string]types.UID)
	}
	d.nsDeviceOccupancy[namespace][deviceName] = claimUID
	d.claimToOccupancy[claimUID] = occupancyKey{namespace: namespace, deviceName: deviceName}
	// Grant corresponds to a 0→1 transition on (namespace, device) — the
	// early guard above returned `occupied` if a different claim held the
	// slot, and the defensive release at the top cleared any prior hold by
	// this claim. Trigger an immediate ResourceSlice republish so:
	//   * avail_* reflects the new pending grant
	//   * active_namespaces gains this namespace on this device
	// The admission-injected CEL selector then steers other claims in this
	// namespace away from this device. The single TriggerRepublish here
	// covers both the new grant and any prior release.
	d.TriggerRepublish()
	return true, false
}

// hasPending reports whether there is an active in-flight pending grant for
// the given claim.
func (d *Driver) hasPending(claimUID types.UID) bool {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	_, ok := d.pending[claimUID]
	return ok
}

// effectiveAvail returns the current available capacity for a device adjusted
// for in-flight pending grants:
//
//	ReadAvailableCapacity(deviceName) − pending grants
//
// Note: overhead is NOT subtracted here. ReadAvailableCapacity already
// reflects the actual consumption of all running services (including the
// pre-configured system services that --reserved-*-pools represents).
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
		devices, err := cxi.Discover()
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
		if err := cdi.WriteContainerdCDIConfig(d.containerdConfigPath, []string{d.cdiRoot}); err != nil {
			return fmt.Errorf("writing containerd CDI config: %w", err)
		}
		pid, err := cdi.FindContainerdPID("/proc")
		if err != nil {
			return fmt.Errorf("finding containerd PID: %w", err)
		}
		if err := cdi.SignalContainerd(pid); err != nil {
			return fmt.Errorf("signalling containerd: %w", err)
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
