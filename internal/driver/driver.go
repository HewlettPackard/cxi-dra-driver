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
		}
	}
}

// HandleError is called by the kubelet plugin helper for background errors
// (e.g. ResourceSlice controller failures).
func (d *Driver) HandleError(_ context.Context, err error, msg string) {
	d.logf("background error: %s: %v", msg, err)
}
