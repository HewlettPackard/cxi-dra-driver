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
	CDIEnvConfig    cdi.CDIEnvConfig
	EnableCDIEnv    bool // if false, env/nccl/ofi CDI specs are not written and not injected into pods
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
	cdiEnvConfig    cdi.CDIEnvConfig
	enableCDIEnv    bool // if false, env/nccl/ofi CDI specs are not written and not injected into pods
	notify          chan struct{}    // signals an immediate ResourceSlice republish

	pendingMu sync.Mutex
	pending   map[types.UID]pendingEntry // claimUID → in-flight grant not yet backed by a service
	// nsDeviceOccupancy counts how many active (pending or service-live) claims
	// from a namespace currently occupy a particular device. Used by the
	// ClaimBinder to enforce the SHS14 per-device VNI uniqueness constraint:
	// every claim in a namespace shares the namespace's VNI, and the kernel
	// rejects two services with the same VNI on the same device with -EEXIST
	// from cxil_svc_set_vni_range. The map is keyed namespace → device → count.
	//
	// Lifecycle:
	//   - incremented in checkAndAddPending when Ready=True is granted.
	//   - decremented in RemoveOccupancy when the NRI plugin completes
	//     DestroyService on StopPodSandbox.
	//
	// Decrement happens only after the service is destroyed (NOT on
	// RemovePending) because the service holds the VNI on the device until
	// then. Decrementing earlier risks another claim being granted on the
	// same (namespace, device) pair, which would then collide at CreateService.
	nsDeviceOccupancy map[string]map[string]int
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
		cdiEnvConfig:      cfg.CDIEnvConfig,
		enableCDIEnv:      cfg.EnableCDIEnv,
		notify:            make(chan struct{}, 1),
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]int),
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

// RemoveOccupancy decrements the (namespace, deviceName) occupancy count.
// Called by the NRI plugin in StopPodSandbox after a successful DestroyService:
// at that point the kernel no longer holds the VNI on the device and another
// claim in the same namespace can be placed here without triggering -EEXIST
// from cxil_svc_set_vni_range.
//
// Multiple calls do not underflow: when the count reaches zero, the entry is
// dropped from the index. Calls for an unknown (namespace, deviceName) pair
// are no-ops, which makes the path idempotent if NRI fires StopPodSandbox
// twice for the same sandbox (e.g. retry on transient runtime error).
//
// On a 1→0 transition this triggers an immediate ResourceSlice republish so
// the device's active_namespaces attribute drops this namespace and the
// MutatingAdmissionPolicy-injected CEL selector lets other claims in this
// namespace target this device again.
//
// Limitation: if a pod is force-deleted before RunPodSandbox runs (so NRI
// never fires StopPodSandbox), the occupancy increment from Ready=True
// leaks until the driver process restarts. This is rare in practice; the
// only known triggers are `kubectl delete pod --force --grace-period=0` on
// a not-yet-started pod or a hard node reboot during pod admission.
func (d *Driver) RemoveOccupancy(namespace, deviceName string) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	devs := d.nsDeviceOccupancy[namespace]
	if devs == nil {
		return
	}
	if devs[deviceName] <= 1 {
		delete(devs, deviceName)
		if len(devs) == 0 {
			delete(d.nsDeviceOccupancy, namespace)
		}
		// 1→0 transition: the active_namespaces attribute for this device
		// just changed. Republish so the admission-injected CEL selector
		// sees the freed slot and other claims in this namespace can now
		// be scheduled on this device.
		d.TriggerRepublish()
		return
	}
	devs[deviceName]--
}

// checkAndAddPending atomically validates a grant request against two
// constraints:
//
//  1. (namespace, deviceName) must not already be occupied by another claim
//     from the same namespace. This enforces the SHS14 per-device VNI
//     uniqueness constraint (see the nsDeviceOccupancy field docs).
//  2. requested must fit within (hwAvail − sum of pending grants on this
//     device).
//
// hwAvail must be the raw value from ReadAvailableCapacity — do NOT subtract
// overhead before calling, as ReadAvailableCapacity already reflects all
// running service consumption including system services.
//
// On a successful grant, the entry is recorded in the pending map and the
// occupancy count for (namespace, deviceName) is incremented.
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

	// SHS14 enforces VNI uniqueness per device kernel-side. Every claim in
	// the same namespace shares the namespace VNI, so two claims from the
	// same namespace cannot co-locate on the same device.
	if d.nsDeviceOccupancy[namespace][deviceName] > 0 {
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
		return false, false
	}

	d.pending[claimUID] = pendingEntry{
		namespace:  namespace,
		deviceName: deviceName,
		resources:  requested,
	}
	if d.nsDeviceOccupancy[namespace] == nil {
		d.nsDeviceOccupancy[namespace] = make(map[string]int)
	}
	d.nsDeviceOccupancy[namespace][deviceName]++
	// Grant always corresponds to a 0→1 transition for this (namespace,
	// device) pair — the early guard above returned `occupied` if the count
	// was non-zero. Trigger an immediate ResourceSlice republish so:
	//   * avail_* reflects the new pending grant
	//   * active_namespaces gains this namespace
	// The admission-injected CEL selector then steers other claims in this
	// namespace away from this device.
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
		if devs[deviceName] > 0 {
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
		if d.cdiEnvConfig.AWSofiNCCLPrefix != "" {
			if err := cdi.WriteAWSofiNCCLSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
				return fmt.Errorf("writing CDI aws-ofi-nccl spec: %w", err)
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
