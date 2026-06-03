package driver

import (
	"context"
	"fmt"
	"os"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"
)

const (
	vniPoolName        = "vni-pool"
	vniLeaseName       = "cxi-vni-pool-leader"
	vniSliceMaxDevices = 128
)

// VNIPoolConfig holds configuration for the cluster-wide VNI pool publisher.
type VNIPoolConfig struct {
	DriverName    string
	KubeClient    kubernetes.Interface
	Namespace     string
	Identity      string // node name — unique per DaemonSet pod
	DaemonSetName string
	VNIStart      int
	VNIEnd        int
}

// StartVNIPool starts leader election in a background goroutine. The elected
// leader creates a resourceslice.Controller that publishes VNI devices as a
// cluster-wide pool (allNodes: true) owned by the DaemonSet. Non-leaders
// simply wait. Returns immediately.
func StartVNIPool(ctx context.Context, cfg VNIPoolConfig) {
	go runVNILeaderElection(ctx, cfg)
}

func runVNILeaderElection(ctx context.Context, cfg VNIPoolConfig) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      vniLeaseName,
			Namespace: cfg.Namespace,
		},
		Client: cfg.KubeClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		ReleaseOnCancel: true,
		Name:            vniLeaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leadCtx context.Context) {
				runVNIController(leadCtx, cfg)
			},
			OnStoppedLeading: func() {},
			OnNewLeader:      func(_ string) {},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cxi-vni-pool: create leader elector: %v\n", err)
		return
	}

	le.Run(ctx)
}

func runVNIController(ctx context.Context, cfg VNIPoolConfig) {
	ds, err := cfg.KubeClient.AppsV1().DaemonSets(cfg.Namespace).Get(ctx, cfg.DaemonSetName, metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cxi-vni-pool: get DaemonSet %s/%s: %v\n", cfg.Namespace, cfg.DaemonSetName, err)
		return
	}

	ctrl, err := resourceslice.StartController(ctx, resourceslice.Options{
		DriverName: cfg.DriverName,
		KubeClient: cfg.KubeClient,
		Owner: &resourceslice.Owner{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
			Name:       ds.Name,
			UID:        ds.UID,
		},
		Resources: buildVNIResources(cfg.VNIStart, cfg.VNIEnd),
		ErrorHandler: func(_ context.Context, err error, msg string) {
			fmt.Fprintf(os.Stderr, "cxi-vni-pool: %s: %v\n", msg, err)
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cxi-vni-pool: start controller: %v\n", err)
		return
	}
	defer ctrl.Stop()

	<-ctx.Done()
}

// buildVNIResources constructs DriverResources with one device per VNI in
// [start, end], chunked into slices of at most vniSliceMaxDevices each.
// The pool has no NodeSelector and no owner.Kind==Node, so the controller
// sets allNodes: true on every slice — VNIs are fabric-wide.
func buildVNIResources(start, end int) *resourceslice.DriverResources {
	devices := make([]resourceapi.Device, 0, end-start+1)
	for vni := start; vni <= end; vni++ {
		devices = append(devices, resourceapi.Device{
			Name: fmt.Sprintf("vni-%d", vni),
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"vni":  {IntValue: ptr.To(int64(vni))},
				"type": {StringValue: ptr.To("vni")},
			},
		})
	}

	var slices []resourceslice.Slice
	for len(devices) > 0 {
		n := min(vniSliceMaxDevices, len(devices))
		// Copy the chunk so each Slice owns its own backing array rather than
		// aliasing a different portion of the shared devices slice.
		chunk := make([]resourceapi.Device, n)
		copy(chunk, devices[:n])
		slices = append(slices, resourceslice.Slice{Devices: chunk})
		devices = devices[n:]
	}

	return &resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			vniPoolName: {Slices: slices},
		},
	}
}
