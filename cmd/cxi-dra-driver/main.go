package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
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

		// Blessed system services: svc_ids the driver must never delete, and
		// whose reserved resources are deducted from each device's usable
		// ceiling. Comma-separated, applied uniformly to all devices on this
		// node. Set from `cxi_service list` to cover pre-existing system
		// services (e.g. the default service and the Ethernet service).
		systemSvcIDs = flag.String("system-svc-ids", "",
			"Comma-separated svc_ids of blessed system services: never deleted, and their reserved resources are deducted from the per-device usable ceiling")

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

	sysSvcIDs, err := parseSvcIDs(*systemSvcIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: --system-svc-ids: %v\n", err)
		os.Exit(1)
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
		SystemSvcIDs:         sysSvcIDs,
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

	nriPlugin, err := nri.New(d.Store(), mock, d.TriggerRepublish, d.RemovePending, binder.TriggerRetry, d.RemoveOccupancy, d.RecordSvcID, sysSvcIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to create NRI plugin: %v\n", err)
		os.Exit(1)
	}

	// Give the driver's orphan-service reaper its view of services still owned
	// by live pods. Must happen before d.Run starts the ticker that drives the
	// reaper; until it is wired the reaper stays inert.
	d.SetLiveSvcIDsFunc(nriPlugin.LiveSvcIDs)

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

// parseSvcIDs parses a comma-separated list of non-negative svc_ids. An empty
// or whitespace-only string yields a nil slice (no blessed services).
func parseSvcIDs(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	ids := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid svc_id %q: %w", p, err)
		}
		if id < 0 {
			return nil, fmt.Errorf("svc_id must be non-negative, got %d", id)
		}
		ids = append(ids, id)
	}
	return ids, nil
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
