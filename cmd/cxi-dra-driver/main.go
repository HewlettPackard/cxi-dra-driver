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

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
	"github.com/slingshot/cxi-dra-driver/internal/driver"
	"github.com/slingshot/cxi-dra-driver/internal/nri"
)

const defaultDriverName = "cxi.slingshot.hpe.com"

func main() {
	var (
		nodeName      = flag.String("node-name", os.Getenv("NODE_NAME"), "Kubernetes node name (defaults to $NODE_NAME)")
		podUID        = flag.String("pod-uid", os.Getenv("POD_UID"), "Pod UID for rolling-update socket uniqueness (defaults to $POD_UID)")
		podNamespace  = flag.String("pod-namespace", os.Getenv("POD_NAMESPACE"), "Namespace this pod runs in, used for leader election lease (defaults to $POD_NAMESPACE)")
		driverName    = flag.String("driver-name", defaultDriverName, "DRA driver name (DNS subdomain)")
		cdiRoot       = flag.String("cdi-root", "/var/run/cdi", "Directory for CDI spec files")
		pluginDir     = flag.String("plugin-dir", kubeletplugin.KubeletPluginsDir+"/"+defaultDriverName, "Kubelet plugin data directory")
		registrarDir  = flag.String("registrar-dir", kubeletplugin.KubeletRegistryDir, "Kubelet plugin registrar directory")
		kubeconfig    = flag.String("kubeconfig", "", "Path to kubeconfig (omit for in-cluster config)")
		mockDevices   = flag.Int("mock-devices", 0, "Generate N synthetic CXI devices instead of real discovery (for kind/local testing)")
		vniRangeStart = flag.Int("vni-range-start", 0, "First VNI in the cluster-wide pool used by the namespace VNI allocator (0 disables the allocator)")
		vniRangeEnd   = flag.Int("vni-range-end", 0, "Last VNI in the cluster-wide pool (inclusive)")

		refreshInterval = flag.Duration("resource-refresh-interval", 10*time.Second,
			"How often to republish the ResourceSlice with refreshed available-capacity attributes")

		// Admin overhead: resources to deduct from hardware totals when computing
		// the usable ceiling for outright rejection. Applied uniformly to all
		// devices on this node. Set these to cover pre-existing system services.
		reservedTXQs     = flag.Int64("reserved-txqs", 0, "TXQs reserved by pre-existing system services (deducted from usable ceiling)")
		reservedTGQs     = flag.Int64("reserved-tgqs", 0, "TGQs reserved by pre-existing system services")
		reservedEQs      = flag.Int64("reserved-eqs", 0, "EQs reserved by pre-existing system services")
		reservedCTs      = flag.Int64("reserved-cts", 0, "CTs reserved by pre-existing system services")
		reservedPTEs     = flag.Int64("reserved-ptes", 0, "PTEs reserved by pre-existing system services")
		reservedACs      = flag.Int64("reserved-acs", 0, "ACs reserved by pre-existing system services")
		reservedLEPools  = flag.Int64("reserved-le-pools", 0, "LE pools reserved by pre-existing system services (1 pool = 1024 LEs)")
		reservedTLEPools = flag.Int64("reserved-tle-pools", 0, "TLE pools reserved by pre-existing system services (1 pool = 512 TLEs)")

		// CDI env spec configuration. When --enable-cdi-env is true, --env-config
		// must point to a JSON file (typically mounted from a ConfigMap) that
		// lists the library bind-mounts and env vars for the env/nccl/ofi specs.
		enableCDIEnv         = flag.Bool("enable-cdi-env", false, "Write env/nccl/ofi CDI specs and inject their device IDs into pods")
		containerdConfigPath = flag.String("containerd-config-path", "", "Path to write the containerd CDI config drop-in; empty disables")
		envConfigPath        = flag.String("env-config", "", "Path to the JSON CDI env config file (required when --enable-cdi-env)")
	)
	flag.Parse()

	driver.SetDriverName(*driverName)

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

	if *reservedTXQs < 0 || *reservedTGQs < 0 || *reservedEQs < 0 || *reservedCTs < 0 ||
		*reservedPTEs < 0 || *reservedACs < 0 || *reservedLEPools < 0 || *reservedTLEPools < 0 {
		fmt.Fprintln(os.Stderr, "error: --reserved-* values must be non-negative")
		os.Exit(1)
	}

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

	var mockDevList []*cxi.Device
	if mock {
		mockDevList = cxi.MockDevices(*mockDevices)
	}

	var envConfig cdi.CDIEnvConfig
	if *enableCDIEnv {
		if *envConfigPath == "" {
			fmt.Fprintln(os.Stderr, "error: --env-config is required when --enable-cdi-env is set")
			os.Exit(1)
		}
		envConfig, err = cdi.LoadConfig(*envConfigPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}

	d := driver.New(driver.Config{
		NodeName:             *nodeName,
		PodUID:               *podUID,
		DriverName:           *driverName,
		CDIRoot:              *cdiRoot,
		PluginDir:            *pluginDir,
		RegistrarDir:         *registrarDir,
		KubeClient:           kubeClient,
		MockDevices:          mockDevList,
		Overhead:             overhead,
		RefreshInterval:      *refreshInterval,
		EnableCDIEnv:         *enableCDIEnv,
		ContainerdConfigPath: *containerdConfigPath,
		CDIEnvConfig:         envConfig,
	})

	// Start the cluster-wide namespace→VNI allocator if a VNI range is
	// configured. Exactly one pod (the leader) assigns VNIs to namespaces; the
	// rest participate in leader election and take over if the leader dies.
	if *vniRangeStart != 0 || *vniRangeEnd != 0 {
		if err := driver.ValidateVNIRange(*vniRangeStart, *vniRangeEnd); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		if *podNamespace == "" {
			fmt.Fprintln(os.Stderr, "error: --pod-namespace or $POD_NAMESPACE must be set when the namespace VNI allocator is enabled")
			os.Exit(1)
		}
		driver.StartNamespaceVNIAllocator(ctx, driver.NamespaceVNIConfig{
			KubeClient: kubeClient,
			Namespace:  *podNamespace,
			Identity:   fmt.Sprintf("%s/%s", *nodeName, *podUID),
			VNIStart:   *vniRangeStart,
			VNIEnd:     *vniRangeEnd,
		})
	}

	binder := driver.NewClaimBinder(d, kubeClient, *driverName, *nodeName)

	nriPlugin, err := nri.New(d.Store(), mock, d.TriggerRepublish, d.RemovePending, binder.TriggerRetry, d.RemoveOccupancy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create NRI plugin: %v\n", err)
		os.Exit(1)
	}

	nriErrCh := make(chan error, 1)
	go func() {
		nriErrCh <- nriPlugin.Run(ctx)
	}()

	binderErrCh := make(chan error, 1)
	go func() {
		binderErrCh <- binder.Run(ctx)
	}()

	if err := d.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: driver exited: %v\n", err)
		os.Exit(1)
	}

	if err := <-nriErrCh; err != nil {
		fmt.Fprintf(os.Stderr, "error: NRI plugin exited: %v\n", err)
		os.Exit(1)
	}

	if err := <-binderErrCh; err != nil {
		fmt.Fprintf(os.Stderr, "error: claim binder exited: %v\n", err)
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
