package driver

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

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
	vniAllocator    VNIAllocator
	overhead        cxi.ResourceSet // admin-configured per-device reserved overhead
	defaults        cxi.ResourceSet // default resources when claim has no config
	refreshInterval time.Duration   // how often to republish ResourceSlice with fresh avail attrs
}

// Config holds admin-configured driver parameters passed from flags.
type Config struct {
	Overhead        cxi.ResourceSet
	Defaults        cxi.ResourceSet
	RefreshInterval time.Duration
}

func New(
	nodeName, podUID, driverName, cdiRoot, pluginDir, registrarDir string,
	kubeClient kubernetes.Interface,
	mockDevices []*cxi.Device,
	vniAllocator VNIAllocator,
	cfg Config,
) *Driver {
	return &Driver{
		nodeName:        nodeName,
		podUID:          podUID,
		driverName:      driverName,
		cdiRoot:         cdiRoot,
		pluginDir:       pluginDir,
		registrarDir:    registrarDir,
		kubeClient:      kubeClient,
		mockDevices:     mockDevices,
		store:           NewAllocationStore(),
		vniAllocator:    vniAllocator,
		overhead:        cfg.Overhead,
		defaults:        cfg.Defaults,
		refreshInterval: cfg.RefreshInterval,
	}
}

// Store returns the allocation store, so it can be passed to the NRI plugin.
func (d *Driver) Store() *AllocationStore {
	return d.store
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
		}
	}
}

// HandleError is called by the kubelet plugin helper for background errors
// (e.g. ResourceSlice controller failures).
func (d *Driver) HandleError(_ context.Context, err error, msg string) {
	d.logf("background error: %s: %v", msg, err)
}
