package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
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

// defaultLdconfigSrc, defaultLinkerSrc and defaultLibcSrc match this repo's
// Dockerfile, which copies the image's own ldconfig, dynamic linker and libc
// there under arch-neutral names so one set of defaults serves amd64 and arm64.
const (
	defaultLdconfigSrc = "/ldconfig-runtime/ldconfig"
	defaultLinkerSrc   = "/ldconfig-runtime/ld.so"
	defaultLibcSrc     = "/ldconfig-runtime/libc.so.6"
)

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

		// Root directory for the per-pod synthetic /sys/class/net trees the NRI
		// plugin bind-mounts into pods so stock libfabric can discover the HSN
		// device. Must be a host path mounted writable at the same path in this
		// container so containerd can resolve it as a bind-mount source.
		netnsSysfsRoot = flag.String("netns-sysfs-root", "/var/run/cxi/netns",
			"Host directory for per-pod synthetic /sys/class/net trees (empty disables HSN sysfs injection)")

		// Root directory for the per-pod CNI hint files this driver writes at
		// PrepareResourceClaims time and removes at UnprepareResourceClaims time.
		// This is the read-side contract for the Slingshot CNI plugin,
		// invoked locally by containerd/CRI with no access to this
		// driver's in-process AllocationStore. See docs/cni/cnidra-cni-hint-contract.md.
		cniHintsDir = flag.String("cni-hints-dir", "/var/run/cxi/net-hints",
			"Host directory for per-pod CNI hint files (empty disables the hint-file write/delete)")

		// Optional host install of the slingshot-cni plugin binary bundled in
		// this image, mirroring how cxi-cdi-hook is installed below. Opt-in:
		// using the CNI is one of several ways a pod can reach the HSN
		// (hostNetwork, future SR-IOV VFs), so the default leaves the node's
		// CNI_PATH untouched. The directory must be a hostPath mounted at the
		// same path in this container.
		cniInstallDir = flag.String("cni-install-dir", "",
			"Host CNI plugin directory (e.g. /opt/cni/bin) to install the bundled slingshot-cni binary into at startup; empty disables")
		cniSrc = flag.String("cni-src", "/slingshot-cni", "Path to the slingshot-cni binary bundled in this image")

		refreshInterval = flag.Duration("resource-refresh-interval", 10*time.Second,
			"How often the orphan-service reaper checks for CXI services whose pod is gone")

		// Blessed system services: svc_ids the driver must never delete, and
		// whose reserved resources are deducted from each device's usable
		// ceiling. Comma-separated, applied uniformly to all devices on this
		// node. Set from `cxi_service list` to cover pre-existing system
		// services (e.g. the default service and the Ethernet service).
		systemSvcIDs = flag.String("system-svc-ids", "",
			"Comma-separated svc_ids of blessed system services: never deleted, and their reserved resources are deducted from the per-device usable ceiling")

		// SR-IOV mode switch. PFs and VFs are never published together: the
		// default publishes PFs and skips VFs; --vf-only publishes VFs and skips
		// PFs before any libcxi read, so the PF is never opened.
		vfOnly = flag.Bool("vf-only", false, "Discover and publish SR-IOV VFs only (PFs are skipped); the default publishes PFs only")

		// CDI env spec configuration. The env spec (kdreg2/gdrdrv/xpmem nodes, /dev/shm)
		// is always attached to every CXI device; when --enable-cdi-env is true,
		// --env-config must point to a JSON file (typically mounted from a
		// ConfigMap) listing the host libraries and env vars it also injects.
		enableCDIEnv         = flag.Bool("enable-cdi-env", false, "Inject the host libfabric/libcxi libs and env vars from --env-config via the env CDI spec")
		containerdConfigPath = flag.String("containerd-config-path", "", "Path to write the containerd CDI config drop-in; empty disables")
		envConfigPath        = flag.String("env-config", "", "Path to the JSON CDI env config file (required when --enable-cdi-env)")

		// cxi-cdi-hook is the createContainer hook the env CDI spec
		// uses to refresh ldconfig's cache. It ships baked into this image at
		// cdiHookSrc and must be installed onto the host at cdiHookPath — CDI
		// createContainer hooks run in the runtime (host) namespace, so the
		// binary has to exist on the node filesystem, not just this image.
		cdiHookSrc  = flag.String("cdi-hook-src", "/cxi-cdi-hook", "Path to the cxi-cdi-hook binary bundled in this image")
		cdiHookPath = flag.String("cdi-hook-path", cdi.DefaultHookPath, "Host path to install cxi-cdi-hook at; referenced by generated CDI hooks")

		// cxi-cdi-hook's update-ldcache subcommand execs a self-contained
		// ldconfig rather than depending on one being pre-installed on the
		// node. That ldconfig, and the dynamic linker + libc it needs, are
		// bundled in this image and installed onto the host alongside
		// cxi-cdi-hook. The src paths default to the Dockerfile's layout.
		cdiLdconfigDir       = flag.String("cdi-ldconfig-dir", cdi.DefaultLdconfigDir, "Host directory to install the ldconfig runtime (ldconfig, its dynamic linker, and libc) into; referenced by generated CDI hooks")
		cdiLdconfigSrc       = flag.String("cdi-ldconfig-src", defaultLdconfigSrc, "Path to the ldconfig binary bundled in this image")
		cdiLdconfigLinkerSrc = flag.String("cdi-ldconfig-linker-src", defaultLinkerSrc, "Path to the dynamic linker bundled in this image, used to run the bundled ldconfig")
		cdiLdconfigLibcSrc   = flag.String("cdi-ldconfig-libc-src", defaultLibcSrc, "Path to the libc bundled in this image, used to run the bundled ldconfig")
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
	// The ids are PF svc_ids; a VF numbers its services on its own, so they would
	// match pod children on a VF and keep the NRI plugin and reaper from deleting them.
	if *vfOnly && len(sysSvcIDs) > 0 {
		log.Printf("warning: ignoring --system-svc-ids %v with --vf-only: PF svc_ids do not identify services on a VF", sysSvcIDs)
		sysSvcIDs = nil
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
		if err := cdi.InstallHook(*cdiHookSrc, *cdiHookPath); err != nil {
			fmt.Fprintf(os.Stderr, "error: installing cxi-cdi-hook: %v\n", err)
			os.Exit(1)
		}
		if err := installLdconfigRuntime(*cdiLdconfigDir, *cdiLdconfigSrc, *cdiLdconfigLinkerSrc, *cdiLdconfigLibcSrc); err != nil {
			fmt.Fprintf(os.Stderr, "error: installing ldconfig runtime: %v\n", err)
			os.Exit(1)
		}
		envConfig.HookPath = *cdiHookPath
		envConfig.LdconfigDir = *cdiLdconfigDir
	}

	if *cniInstallDir != "" {
		if *cniHintsDir == "" {
			fmt.Fprintln(os.Stderr, "error: --cni-install-dir requires --cni-hints-dir; slingshot-cni cannot operate without hint files")
			os.Exit(1)
		}
		dest := filepath.Join(*cniInstallDir, "slingshot-cni")
		if err := cdi.InstallHook(*cniSrc, dest); err != nil {
			fmt.Fprintf(os.Stderr, "error: installing slingshot-cni: %v\n", err)
			os.Exit(1)
		}
		log.Printf("installed slingshot-cni at %s", dest)
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
		VFOnly:               *vfOnly,
		RefreshInterval:      *refreshInterval,
		ContainerdConfigPath: *containerdConfigPath,
		CDIEnvConfig:         envConfig,
		CNIHintsDir:          *cniHintsDir,
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

	nriPlugin, err := nri.New(d.Store(), mock, d.RemovePending, binder.TriggerRetry, d.RecordSvcID, sysSvcIDs, *netnsSysfsRoot)
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

// installLdconfigRuntime installs the ldconfig binary, its dynamic linker,
// and libc — all baked into this image — into dir on the host, alongside
// cxi-cdi-hook. cxi-cdi-hook execs ldconfig through this linker with an
// explicit --library-path pointing at dir, so update-ldcache never depends
// on the node itself having ldconfig (or a compatible glibc) installed.
func installLdconfigRuntime(dir, ldconfigSrc, linkerSrc, libcSrc string) error {
	files := map[string]string{
		ldconfigSrc: "ldconfig",
		linkerSrc:   "ld.so",
		libcSrc:     "libc.so.6",
	}
	for src, name := range files {
		if err := cdi.InstallHook(src, filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("installing %s: %w", name, err)
		}
	}
	return nil
}
