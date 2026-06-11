package driver

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const vniAnnotationKey = "cxi.slingshot.hpe.com/vni"

// nextFreeVNI returns the lowest VNI in [start, end] that is not present in
// assigned, or (0, false) if the range is exhausted.
func nextFreeVNI(assigned map[int]bool, start, end int) (int, bool) {
	for vni := start; vni <= end; vni++ {
		if !assigned[vni] {
			return vni, true
		}
	}
	return 0, false
}

// namespaceVNIController assigns a unique VNI to every namespace not in skip.
// Only the elected leader runs one; a single workqueue worker serializes
// reconciles so the assigned-set scan and the patch cannot race each other.
type namespaceVNIController struct {
	client kubernetes.Interface
	start  int
	end    int
	skip   map[string]bool
}

func defaultVNISkip() map[string]bool {
	return map[string]bool{
		"kube-system":     true,
		"kube-public":     true,
		"kube-node-lease": true,
	}
}

// assignedVNIs returns the set of VNIs currently annotated across all
// namespaces. The controller is single-writer (leader only) and namespaces are
// low-cardinality with rare events, so a live LIST per reconcile is fine and
// keeps the assigned-set scan trivially consistent with the patch that follows.
func (c *namespaceVNIController) assignedVNIs(ctx context.Context) (map[int]bool, error) {
	list, err := c.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	assigned := make(map[int]bool)
	for i := range list.Items {
		if v, ok := list.Items[i].Annotations[vniAnnotationKey]; ok {
			if n, perr := strconv.Atoi(v); perr == nil {
				assigned[n] = true
			}
		}
	}
	return assigned, nil
}

// reconcile ensures the named namespace has a valid VNI annotation. It is a
// no-op for skip-listed namespaces and for namespaces that already carry a
// valid annotation.
func (c *namespaceVNIController) reconcile(ctx context.Context, name string) error {
	if c.skip[name] {
		return nil
	}
	ns, err := c.client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil // deleted; its VNI is freed implicitly
	}
	if err != nil {
		return fmt.Errorf("getting namespace %s: %w", name, err)
	}
	if v, ok := ns.Annotations[vniAnnotationKey]; ok {
		if n, perr := strconv.Atoi(v); perr == nil && ValidVNI(n) {
			return nil // already assigned and valid
		}
	}

	assigned, err := c.assignedVNIs(ctx)
	if err != nil {
		return err
	}
	vni, ok := nextFreeVNI(assigned, c.start, c.end)
	if !ok {
		return fmt.Errorf("VNI range [%d,%d] exhausted; cannot assign to namespace %s", c.start, c.end, name)
	}

	patch := []byte(fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}}}`, vniAnnotationKey, strconv.Itoa(vni)))
	if _, err := c.client.CoreV1().Namespaces().Patch(
		ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		// Conflict / lost race: returning the error requeues the key.
		return fmt.Errorf("patching namespace %s with VNI %d: %w", name, vni, err)
	}
	return nil
}

// runNamespaceVNIAllocator runs the controller until ctx is cancelled. It is
// invoked from OnStartedLeading so only the leader assigns VNIs. The informer
// only delivers namespace Add/Update events into the queue; assignedVNIs reads
// namespaces via a live List, so no lister is needed.
func runNamespaceVNIAllocator(ctx context.Context, cfg NamespaceVNIConfig) {
	factory := informers.NewSharedInformerFactory(cfg.KubeClient, 10*time.Minute)
	nsInformer := factory.Core().V1().Namespaces()

	c := &namespaceVNIController{
		client: cfg.KubeClient,
		start:  cfg.VNIStart,
		end:    cfg.VNIEnd,
		skip:   defaultVNISkip(),
	}

	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[string]())
	defer queue.ShutDown()

	_, _ = nsInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if ns, ok := obj.(*corev1.Namespace); ok {
				queue.Add(ns.Name)
			}
		},
		UpdateFunc: func(_, obj interface{}) {
			if ns, ok := obj.(*corev1.Namespace); ok {
				queue.Add(ns.Name)
			}
		},
		// No DeleteFunc: a deleted namespace frees its VNI implicitly the next
		// time assignedVNIs scans the (now smaller) namespace set.
	})

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), nsInformer.Informer().HasSynced) {
		fmt.Fprintln(os.Stderr, "cxi-vni-allocator: namespace cache failed to sync")
		return
	}

	// Shut the queue down when leadership is lost (ctx cancelled) so the worker
	// loop's blocking Get() returns and this goroutine exits instead of leaking.
	go func() {
		<-ctx.Done()
		queue.ShutDown()
	}()

	for {
		key, shutdown := queue.Get()
		if shutdown {
			return
		}
		if err := c.reconcile(ctx, key); err != nil {
			fmt.Fprintf(os.Stderr, "cxi-vni-allocator: %v\n", err)
			queue.AddRateLimited(key)
		} else {
			queue.Forget(key)
		}
		queue.Done(key)
	}
}
