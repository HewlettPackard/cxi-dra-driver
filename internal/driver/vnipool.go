package driver

import (
	"context"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

const (
	// vniPoolName is retained only until prepare.go and claimbinder.go stop
	// referencing it (later tasks); the VNI ResourceSlice pool is no longer
	// published.
	vniPoolName           = "vni-pool"
	vniAllocatorLeaseName = "cxi-vni-allocator-leader"
)

// NamespaceVNIConfig configures the cluster-wide namespace→VNI allocator.
type NamespaceVNIConfig struct {
	KubeClient kubernetes.Interface
	Namespace  string // namespace for the leader-election lease
	Identity   string // unique per DaemonSet pod
	VNIStart   int
	VNIEnd     int
}

// StartNamespaceVNIAllocator starts leader election in a background goroutine.
// The elected leader runs the namespace VNI controller; non-leaders wait and
// take over if the leader dies. Returns immediately.
func StartNamespaceVNIAllocator(ctx context.Context, cfg NamespaceVNIConfig) {
	go runVNIAllocatorLeaderElection(ctx, cfg)
}

func runVNIAllocatorLeaderElection(ctx context.Context, cfg NamespaceVNIConfig) {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      vniAllocatorLeaseName,
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
		Name:            vniAllocatorLeaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leadCtx context.Context) {
				runNamespaceVNIAllocator(leadCtx, cfg)
			},
			OnStoppedLeading: func() {},
			OnNewLeader:      func(_ string) {},
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cxi-vni-allocator: create leader elector: %v\n", err)
		return
	}

	le.Run(ctx)
}
