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
	notify          chan struct{}    // signals an immediate ResourceSlice republish

	pendingMu sync.Mutex
	pending   map[types.UID]pendingEntry // claimUID → in-flight grant not yet backed by a service
}

func New(cfg Config) *Driver {
	return &Driver{
		nodeName:        cfg.NodeName,
		podUID:          cfg.PodUID,
		driverName:      cfg.DriverName,
		cdiRoot:         cfg.CDIRoot,
		pluginDir:       cfg.PluginDir,
		registrarDir:    cfg.RegistrarDir,
		kubeClient:      cfg.KubeClient,
		mockDevices:     cfg.MockDevices,
		store:           NewAllocationStore(),
		overhead:        cfg.Overhead,
		refreshInterval: cfg.RefreshInterval,
		cdiEnvConfig:    cfg.CDIEnvConfig,
		notify:          make(chan struct{}, 1),
		pending:         make(map[types.UID]pendingEntry),
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
func (d *Driver) RemovePending(claimUID types.UID) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	delete(d.pending, claimUID)
}

// checkAndAddPending atomically checks whether requested fits within effective
// available capacity for deviceName and, if so, records the grant in the
// pending map. hwAvail must be the raw value from ReadAvailableCapacity —
// do NOT subtract overhead before calling, as ReadAvailableCapacity already
// reflects all running service consumption including system services.
// Returns true if the grant was recorded, false if capacity was insufficient.
func (d *Driver) checkAndAddPending(claimUID types.UID, deviceName string, requested, hwAvail cxi.ResourceSet) bool {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
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
	d.pending[claimUID] = pendingEntry{deviceName: deviceName, resources: requested}
	return true
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
