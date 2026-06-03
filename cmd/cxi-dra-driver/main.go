package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
	"github.com/slingshot/cxi-dra-driver/internal/driver"
	"github.com/slingshot/cxi-dra-driver/internal/nri"
)

const defaultDriverName = "cxi.slingshot.hpe.com"

func main() {
	var (
		nodeName     = flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name (defaults to $NODE_NAME)")
		podUID       = flag.String("pod-uid", os.Getenv("POD_UID"), "Pod UID for rolling-update socket uniqueness (defaults to $POD_UID)")
		driverName   = flag.String("driver-name", defaultDriverName, "DRA driver name (DNS subdomain)")
		cdiRoot      = flag.String("cdi-root", "/var/run/cdi", "Directory for CDI spec files")
		pluginDir    = flag.String("plugin-dir", kubeletplugin.KubeletPluginsDir+"/"+defaultDriverName, "Kubelet plugin data directory")
		registrarDir = flag.String("registrar-dir", kubeletplugin.KubeletRegistryDir, "Kubelet plugin registrar directory")
		kubeconfig   = flag.String("kubeconfig", "", "Path to kubeconfig (omit for in-cluster config)")
		mockDevices  = flag.Int("mock-devices", 0, "Generate N synthetic CXI devices instead of real discovery (for kind/local testing)")

		refreshInterval = flag.Duration("resource-refresh-interval", 30*time.Second,
			"How often to republish the ResourceSlice with refreshed available-capacity attributes")

		// Admin overhead: resources to deduct from hardware totals when computing
		// the usable ceiling for outright rejection. Applied uniformly to all
		// devices on this node. Set these to cover pre-existing system services.
		reservedTXQs    = flag.Int64("reserved-txqs", 0, "TXQs reserved by pre-existing system services (deducted from usable ceiling)")
		reservedTGQs    = flag.Int64("reserved-tgqs", 0, "TGQs reserved by pre-existing system services")
		reservedEQs     = flag.Int64("reserved-eqs", 0, "EQs reserved by pre-existing system services")
		reservedCTs     = flag.Int64("reserved-cts", 0, "CTs reserved by pre-existing system services")
		reservedPTEs    = flag.Int64("reserved-ptes", 0, "PTEs reserved by pre-existing system services")
		reservedACs     = flag.Int64("reserved-acs", 0, "ACs reserved by pre-existing system services")
		reservedLEPools = flag.Int64("reserved-le-pools", 0, "LE pools reserved by pre-existing system services (1 pool = 1024 LEs)")
		reservedTLEPools = flag.Int64("reserved-tle-pools", 0, "TLE pools reserved by pre-existing system services (1 pool = 512 TLEs)")

		// Default resources allocated when the user's claim has no config.
		// Roughly one quarter of hardware capacity; LEs and TLEs default to
		// zero to avoid pool exhaustion from pre-existing services.
		defaultTXQs    = flag.Int64("default-txqs", 256, "Default TXQs per service when claim has no resource config")
		defaultTGQs    = flag.Int64("default-tgqs", 128, "Default TGQs per service")
		defaultEQs     = flag.Int64("default-eqs", 511, "Default EQs per service")
		defaultCTs     = flag.Int64("default-cts", 511, "Default CTs per service")
		defaultPTEs    = flag.Int64("default-ptes", 512, "Default PTEs per service")
		defaultACs     = flag.Int64("default-acs", 255, "Default ACs per service")
		defaultLEPools  = flag.Int64("default-le-pools", 0, "Default LE pools per service (0 = none; 1 pool = 1024 LEs)")
		defaultTLEPools = flag.Int64("default-tle-pools", 0, "Default TLE pools per service (0 = none; 1 pool = 512 TLEs)")
	)
	flag.Parse()

	kubeClient, err := buildKubeClient(*kubeconfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to build kube client: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *nodeName == "" {
		fmt.Fprintln(os.Stderr, "error: --node-name or $NODE_NAME must be set")
		os.Exit(1)
	}

	mock := *mockDevices > 0

	overhead := cxi.ResourceSet{
		TXQs: *reservedTXQs,
		TGQs: *reservedTGQs,
		EQs:  *reservedEQs,
		CTs:  *reservedCTs,
		PTEs: *reservedPTEs,
		ACs:  *reservedACs,
		LEs:  *reservedLEPools * cxi.LEPoolSize,
		TLEs: *reservedTLEPools * cxi.TLEPoolSize,
	}

	defaults := cxi.ResourceSet{
		TXQs: *defaultTXQs,
		TGQs: *defaultTGQs,
		EQs:  *defaultEQs,
		CTs:  *defaultCTs,
		PTEs: *defaultPTEs,
		ACs:  *defaultACs,
		LEs:  *defaultLEPools * cxi.LEPoolSize,
		TLEs: *defaultTLEPools * cxi.TLEPoolSize,
	}

	var mockDevList []*cxi.Device
	if mock {
		mockDevList = cxi.MockDevices(*mockDevices)
	}

	d := driver.New(
		*nodeName, *podUID, *driverName, *cdiRoot, *pluginDir, *registrarDir,
		kubeClient, mockDevList,
		// TODO: replace StaticVNIAllocator with a cluster-wide VNI pool allocator.
		&driver.StaticVNIAllocator{VNI: 1},
		driver.Config{
			Overhead:        overhead,
			Defaults:        defaults,
			RefreshInterval: *refreshInterval,
		},
	)

	nriPlugin, err := nri.New(d.Store(), mock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create NRI plugin: %v\n", err)
		os.Exit(1)
	}

	nriErrCh := make(chan error, 1)
	go func() {
		nriErrCh <- nriPlugin.Run(ctx)
	}()

	if err := d.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: driver exited: %v\n", err)
		os.Exit(1)
	}

	if err := <-nriErrCh; err != nil {
		fmt.Fprintf(os.Stderr, "error: NRI plugin exited: %v\n", err)
		os.Exit(1)
	}
}

func buildKubeClient(kubeconfig string) (kubernetes.Interface, error) {
	var cfg *rest.Config
	var err error
	if kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}
