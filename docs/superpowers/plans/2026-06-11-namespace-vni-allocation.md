# Namespace-Scoped VNI Allocation Implementation Plan

> **STATUS: COMPLETED — DO NOT TREAT AS LIVE INSTRUCTIONS.** Implemented and
> merged. Current behaviour: `internal/driver/vniallocator.go` runs a
> leader-elected namespace controller that assigns one VNI per namespace via
> the `cxi.slingshot.hpe.com/vni` annotation; `prepareClaim` reads that
> annotation and programs the CXI service. Templates have been renamed
> `cxi-quarter`/`cxi-half` → `cxi-small`/`cxi-medium`/`cxi-large`/`cxi-whole`
> and now also carry the required `cdi_profiles` field. Retained for
> historical reference; do not follow the checkboxes below as if they were
> unfinished work.

---

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace DRA-allocated VNIs (a ResourceSlice pool + per-pod `cxi-vni` ResourceClaim) with a controller that auto-assigns one unique VNI per namespace, written as a namespace annotation and read by the node driver at prepare time — so every pod in a namespace shares a VNI for RDMA while each pod still gets its own DRA-scheduled CXI device.

**Tenancy model:** A namespace == one cluster *allocation* (a tenancy boundary). A job never spans namespaces, but a namespace may run several concurrent jobs; those jobs belong to the same allocation and intentionally share the namespace's VNI (they are not fabric-isolated from each other, which is acceptable within one allocation). The VNI's isolation boundary therefore lines up with the allocation boundary.

**Architecture:** A single leader (reusing the existing leader-election scaffolding) runs a namespace controller that watches all namespaces and assigns the lowest free VNI in `[VNIStart, VNIEnd]` to every namespace not in a system skip-list, persisting it as the annotation `cxi.slingshot.hpe.com/vni`. Allocation state is derived entirely from the live namespaces' annotations (crash-safe; deletion frees the VNI). The node-side `prepareClaim` looks up the claim's namespace annotation instead of reading a VNI from the claim. Each pod then carries exactly **one** ResourceClaim (the CXI device), which eliminates the `DRAPrioritizedList` scoring bug (`number of allocations N is smaller than number of claims`) at its root.

**Tech Stack:** Go, `k8s.io/client-go` (informers, workqueue, leaderelection), `k8s.io/api/resource/v1` (DRA), kubeletplugin DRA node API.

---

## Background: why this change

The kube-scheduler DRA plugin's `computeScore` (gated by the `DRAPrioritizedList` feature gate, in `pkg/scheduler/framework/plugins/dynamicresources/dynamicresources.go`) iterates a pod's claims and positionally matches the claims *needing allocation this cycle* against the per-node `allocationResults`. A pod that mixes a **pre-allocated shared claim** (a shared VNI claim, allocated when the first pod scheduled) with a **fresh template claim** (the per-pod CXI device) breaks that positional assumption and returns `number of allocations N is smaller than number of claims needing allocation`. Collapsing VNI to a namespace annotation removes the shared claim entirely: each pod has exactly one claim, so the mismatch cannot occur.

## File Structure

**Create:**
- `internal/driver/vniallocator.go` — pure free-VNI selection + the namespace controller (watch/reconcile/annotate), run by the leader.
- `internal/driver/vniallocator_test.go` — tests for the pure selection function and the reconcile annotate path (fake clientset).

**Modify:**
- `internal/driver/vnipool.go` — repurpose: keep leader-election scaffolding, swap the ResourceSlice publisher for the namespace controller; rename `VNIPoolConfig`→`NamespaceVNIConfig`, `StartVNIPool`→`StartNamespaceVNIAllocator`. Delete `buildVNIResources`, `vniPoolName`, `vniSliceMaxDevices`.
- `internal/driver/prepare.go` — replace VNI-from-claim extraction with a namespace-annotation lookup; thread `ctx`; delete `vniFromClaims`, `isVNIClaim`, `vniFromDeviceName`.
- `internal/driver/prepare_test.go` — drop VNI-from-claim tests; add namespace-lookup tests.
- `internal/driver/claimbinder.go` — remove `vniPoolName` references in `findOurDevice`; rewrite the type-doc comment (no more merged claim requirement).
- `internal/driver/resourceslice.go` — update the `buildDevices` comment that references the merged CXI+VNI template.
- `cmd/cxi-dra-driver/main.go` — call `StartNamespaceVNIAllocator`; keep `--vni-range-start/end` validation and `--pod-namespace` (used for the lease).
- `deploy/kubernetes/clusterrole.yaml` — add `namespaces` get/list/watch (+ patch for the leader); rename the lease Role/RoleBinding.
- `deploy/kubernetes/test/resourceclaim.yaml` — templates become CXI-only (`cxi-quarter`, `cxi-half`); drop the `vni` request and `cxi-vni` class.
- `deploy/kubernetes/test/replicaset.yaml` + `replicaset-2node.yaml` — reference `cxi-quarter` (not `cxi-quarter-with-vni`).

**Delete:**
- `deploy/kubernetes/deviceclass-vni.yaml` — the `cxi-vni` DeviceClass is no longer used.

**Unchanged (reused):** `internal/driver/vni.go` (`ValidVNI`, `ValidateVNIRange` still validate the configured range and parsed annotation values).

---

## Constants & naming (used throughout)

```go
// annotation written on each namespace by the allocator and read by the node driver
const vniAnnotationKey = "cxi.slingshot.hpe.com/vni"

// lease used by the single-writer allocator leader
const vniAllocatorLeaseName = "cxi-vni-allocator-leader"
```

Default skip-list (namespaces that never get a VNI): `kube-system`, `kube-public`, `kube-node-lease`.

---

### Task 1: Pure free-VNI selection

**Files:**
- Create: `internal/driver/vniallocator.go`
- Test: `internal/driver/vniallocator_test.go`

- [ ] **Step 1: Write the failing test**

```go
package driver

import "testing"

func TestNextFreeVNI(t *testing.T) {
	tests := []struct {
		name     string
		assigned map[int]bool
		start    int
		end      int
		wantVNI  int
		wantOK   bool
	}{
		{"empty range picks start", map[int]bool{}, 10, 12, 10, true},
		{"skips assigned", map[int]bool{10: true, 11: true}, 10, 12, 12, true},
		{"gap in middle", map[int]bool{10: true, 12: true}, 10, 12, 11, true},
		{"exhausted", map[int]bool{10: true, 11: true, 12: true}, 10, 12, 0, false},
		{"single free", map[int]bool{}, 5, 5, 5, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vni, ok := nextFreeVNI(tc.assigned, tc.start, tc.end)
			if ok != tc.wantOK || (ok && vni != tc.wantVNI) {
				t.Fatalf("nextFreeVNI(%v,%d,%d) = (%d,%v), want (%d,%v)",
					tc.assigned, tc.start, tc.end, vni, ok, tc.wantVNI, tc.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/driver/ -run TestNextFreeVNI -v`
Expected: FAIL — `undefined: nextFreeVNI`

- [ ] **Step 3: Write minimal implementation** (top of new `internal/driver/vniallocator.go`)

```go
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

const (
	vniAnnotationKey      = "cxi.slingshot.hpe.com/vni"
	vniAllocatorLeaseName = "cxi-vni-allocator-leader"
)

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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/driver/ -run TestNextFreeVNI -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/driver/vniallocator.go internal/driver/vniallocator_test.go
git commit -m "driver: add nextFreeVNI pure allocator helper"
```

---

### Task 2: Namespace VNI controller (watch + reconcile + annotate)

**Files:**
- Modify: `internal/driver/vniallocator.go` (append)
- Test: `internal/driver/vniallocator_test.go` (append)

The controller is single-writer (only the leader runs it), so a single workqueue worker serializing reconciles makes the "build assigned set, pick free VNI, patch" sequence race-free. The assigned set is read from the informer cache (the lister) on every reconcile, so a crash/restart rebuilds state from the live namespaces' annotations.

- [ ] **Step 1: Write the failing test** (append to `vniallocator_test.go`)

The file already has `import "testing"` from Task 1 — merge these imports into a single import block (don't add a second one):

```go
import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReconcileNamespaceAssignsVNI(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
		// pre-existing assignment must be respected, not reassigned
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:        "team-b",
			Annotations: map[string]string{vniAnnotationKey: "10"},
		}},
		// system namespace must be skipped
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	c := &namespaceVNIController{
		client: client,
		start:  10,
		end:    20,
		skip:   defaultVNISkip(),
	}

	// team-a is unassigned; lowest free VNI given team-b=10 is 11.
	if err := c.reconcile(context.Background(), "team-a"); err != nil {
		t.Fatalf("reconcile team-a: %v", err)
	}
	ns, _ := client.CoreV1().Namespaces().Get(context.Background(), "team-a", metav1.GetOptions{})
	if got := ns.Annotations[vniAnnotationKey]; got != "11" {
		t.Fatalf("team-a VNI = %q, want %q", got, "11")
	}

	// kube-system must remain unassigned.
	if err := c.reconcile(context.Background(), "kube-system"); err != nil {
		t.Fatalf("reconcile kube-system: %v", err)
	}
	sys, _ := client.CoreV1().Namespaces().Get(context.Background(), "kube-system", metav1.GetOptions{})
	if _, ok := sys.Annotations[vniAnnotationKey]; ok {
		t.Fatalf("kube-system unexpectedly got a VNI")
	}

	// re-reconciling team-b must not change its VNI.
	if err := c.reconcile(context.Background(), "team-b"); err != nil {
		t.Fatalf("reconcile team-b: %v", err)
	}
	b, _ := client.CoreV1().Namespaces().Get(context.Background(), "team-b", metav1.GetOptions{})
	if got := b.Annotations[vniAnnotationKey]; got != "10" {
		t.Fatalf("team-b VNI changed to %q, want 10", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/driver/ -run TestReconcileNamespaceAssignsVNI -v`
Expected: FAIL — `undefined: namespaceVNIController` / `defaultVNISkip`

- [ ] **Step 3: Write minimal implementation** (append to `vniallocator.go`)

```go
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
```

Imports needed by Task 2 code (add only these — Task 3 adds the informer/workqueue/leaderelection imports later; do not add them now or the build fails on unused imports):

```go
import (
	"context"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)
```

(`reconcile` calls `ValidVNI` from `vni.go` — same package, no import.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/driver/ -run TestReconcileNamespaceAssignsVNI -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/driver/vniallocator.go internal/driver/vniallocator_test.go
git commit -m "driver: add namespace VNI controller reconcile logic"
```

---

### Task 3: Wire the controller into leader election (run loop)

**Files:**
- Modify: `internal/driver/vniallocator.go` (append the runner)
- Modify: `internal/driver/vnipool.go` (replace publisher wiring)

- [ ] **Step 1: Append the run loop to `vniallocator.go`**

```go
// runNamespaceVNIAllocator runs the controller until ctx is cancelled. It is
// invoked from OnStartedLeading so only the leader assigns VNIs.
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
		// time assignedVNIs() scans the (now smaller) namespace set.
	})

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), nsInformer.Informer().HasSynced) {
		fmt.Fprintln(os.Stderr, "cxi-vni-allocator: namespace cache failed to sync")
		return
	}

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
```

The informer is used only to deliver namespace Add/Update events into the queue; `assignedVNIs` reads namespaces via a live `client.List`, so no lister/adapter is needed. Imports this run loop adds to `vniallocator.go` (on top of Task 2's): `"os"`, `"time"`, `corev1 "k8s.io/api/core/v1"`, `"k8s.io/client-go/informers"`, `"k8s.io/client-go/tools/cache"`, `"k8s.io/client-go/util/workqueue"`. (`context`, `fmt` already present from Task 2.)

- [ ] **Step 2: Replace the publisher wiring in `vnipool.go`**

Replace the entire file body with the leader-election scaffolding pointed at the new runner. Final `vnipool.go`:

```go
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
```

- [ ] **Step 3: Build to verify it compiles**

Run: `go build ./...`
Expected: success (no references to `buildVNIResources`, `vniPoolName`, `vniSliceMaxDevices`, `VNIPoolConfig`, `StartVNIPool` remain — those are fixed in Tasks 4–5).
If the build fails on those names, that's expected until Tasks 4–5 land; proceed.

- [ ] **Step 4: Commit**

```bash
git add internal/driver/vniallocator.go internal/driver/vnipool.go
git commit -m "driver: run namespace VNI allocator under leader election"
```

---

### Task 4: Node-side namespace VNI lookup in prepare

**Files:**
- Modify: `internal/driver/prepare.go`
- Test: `internal/driver/prepare_test.go`

The node driver already holds `d.kubeClient`. `prepareClaim` resolves the VNI once per claim from `claim.Namespace` (the pod and its ephemeral claim share a namespace), applies it to all device allocations, then stores per pod. A bounded retry covers the race where a pod is prepared before the allocator has annotated its namespace.

- [ ] **Step 1: Write the failing test** (replace the old VNI-from-claim tests in `prepare_test.go`)

```go
func TestNamespaceVNI_ReadsAnnotation(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "team-a",
			Annotations: map[string]string{vniAnnotationKey: "42"},
		},
	})
	d := &Driver{kubeClient: client}
	vni, err := d.namespaceVNI(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("namespaceVNI: %v", err)
	}
	if vni != 42 {
		t.Fatalf("vni = %d, want 42", vni)
	}
}

func TestNamespaceVNI_MissingAnnotationErrors(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
	})
	d := &Driver{kubeClient: client}
	_, err := d.namespaceVNI(context.Background(), "team-a")
	if err == nil {
		t.Fatalf("expected error for namespace without VNI annotation")
	}
}
```

Add imports to `prepare_test.go` if absent: `"context"`, `corev1 "k8s.io/api/core/v1"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `"k8s.io/client-go/kubernetes/fake"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/driver/ -run TestNamespaceVNI -v`
Expected: FAIL — `d.namespaceVNI undefined`

- [ ] **Step 3: Implement `namespaceVNI` and rewrite the VNI parts of `prepare.go`**

Add to `prepare.go`:

```go
// namespaceVNI returns the VNI assigned to the namespace by the allocator,
// read from the vniAnnotationKey annotation. It retries briefly to tolerate
// the race where a pod is prepared before the allocator has annotated a
// freshly created namespace.
func (d *Driver) namespaceVNI(ctx context.Context, namespace string) (int, error) {
	const (
		attempts = 10
		delay    = time.Second
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		ns, err := d.kubeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			lastErr = fmt.Errorf("getting namespace %s: %w", namespace, err)
		} else if v, ok := ns.Annotations[vniAnnotationKey]; ok {
			vni, perr := strconv.Atoi(v)
			if perr != nil {
				return 0, fmt.Errorf("namespace %s has invalid VNI annotation %q: %w", namespace, v, perr)
			}
			if !ValidVNI(vni) {
				return 0, fmt.Errorf("namespace %s VNI %d out of range [%d,%d]", namespace, vni, MinVNI, MaxVNI)
			}
			return vni, nil
		} else {
			lastErr = fmt.Errorf("namespace %s has no %s annotation yet", namespace, vniAnnotationKey)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
	return 0, fmt.Errorf("VNI not available for namespace %s after %d attempts: %w", namespace, attempts, lastErr)
}
```

Ensure `prepare.go` imports include `"context"`, `"time"`, `"strconv"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"` (keep existing ones).

Change `prepareClaim`'s signature to take `ctx` and replace the per-pod VNI map with a single namespace lookup. New `prepareClaim` (replacing lines ~110–208):

```go
func (d *Driver) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("ResourceClaim %s/%s has no allocation", claim.Namespace, claim.Name),
		}
	}

	var kubeletDevices []kubeletplugin.Device
	var podAllocs []PodAllocation

	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != d.driverName {
			continue
		}

		requested, err := d.resolveResources(claim, alloc)
		if err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: resolving resources: %w",
					claim.Namespace, claim.Name, alloc.Device, err),
			}
		}

		ceiling := d.usableCeiling(alloc.Device)
		if !requested.Fits(ceiling) {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: requested resources (%s) exceed usable ceiling (%s) — request will never fit on this hardware",
					claim.Namespace, claim.Name, alloc.Device, requested, ceiling),
			}
		}

		cdiIDs := []string{cdi.DeviceID(d.driverName, alloc.Device)}
		if d.enableCDIEnv {
			cdiIDs = append(cdiIDs, cdi.EnvDeviceID(d.driverName))
			if d.cdiEnvConfig.NCCLPrefix != "" {
				cdiIDs = append(cdiIDs, cdi.NCCLDeviceID(d.driverName))
			}
			if d.cdiEnvConfig.AWSofiNCCLPrefix != "" {
				cdiIDs = append(cdiIDs, cdi.AWSofiNCCLDeviceID(d.driverName))
			}
		}

		kubeletDevices = append(kubeletDevices, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: cdiIDs,
		})

		pa := d.podAllocationBase(alloc.Device)
		pa.Resources = requested
		podAllocs = append(podAllocs, pa)
	}

	// The VNI is namespace-scoped: a namespace is one cluster allocation, so
	// every pod in it (across one or more jobs of that allocation) shares the
	// VNI and can form RDMA connections. The claim and the pods that reserved
	// it share claim.Namespace.
	vni, err := d.namespaceVNI(ctx, claim.Namespace)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	for i := range podAllocs {
		podAllocs[i].VNI = vni
	}

	var preparedPods []types.UID
	rollback := func() {
		for _, podUID := range preparedPods {
			d.store.Delete(podUID)
		}
		d.store.DeleteClaim(claim.UID)
	}
	_ = rollback // retained for symmetry; no per-pod failure path remains
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		podUID := types.UID(ref.UID)
		d.store.Set(podUID, podAllocs)
		d.store.SetClaimPod(claim.UID, podUID)
		preparedPods = append(preparedPods, podUID)
	}

	return kubeletplugin.PrepareResult{Devices: kubeletDevices}
}
```

Then delete the now-unused helpers `vniFromClaims`, `isVNIClaim`, `vniFromDeviceName` from `prepare.go`, and update `PrepareResourceClaims` to drop the first-pass VNI extraction and pass `ctx` through:

```go
func (d *Driver) PrepareResourceClaims(
	ctx context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		result[claim.UID] = d.prepareClaim(ctx, claim)
	}
	return result, nil
}
```

(If `strings` is now unused in `prepare.go` after deleting `vniFromDeviceName`, remove it from the imports.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/driver/ -run 'TestNamespaceVNI|TestPrepare' -v`
Expected: PASS. Fix any remaining references to removed helpers in `prepare_test.go` by deleting those tests.

- [ ] **Step 5: Commit**

```bash
git add internal/driver/prepare.go internal/driver/prepare_test.go
git commit -m "driver: resolve VNI from namespace annotation at prepare time"
```

---

### Task 5: Drop VNI from the scheduler path (claimbinder + cmd wiring)

**Files:**
- Modify: `internal/driver/claimbinder.go`
- Modify: `internal/driver/resourceslice.go`
- Modify: `cmd/cxi-dra-driver/main.go`

- [ ] **Step 1: Remove `vniPoolName` from `findOurDevice`** (`claimbinder.go` ~line 298)

Replace:

```go
		if alloc.Pool == vniPoolName || alloc.Pool != b.nodeName {
			continue
		}
```

with:

```go
		if alloc.Pool != b.nodeName {
			continue
		}
```

- [ ] **Step 2: Rewrite the `ClaimBinder` type-doc comment** (`claimbinder.go` lines ~47–56)

Replace the paragraph beginning `// IMPORTANT: pods using this driver must use a merged ResourceClaimTemplate ...` through the `number of allocations 1 is smaller than number of claims 2` sentence with:

```go
// Pods carry exactly one ResourceClaim: the CXI device. The VNI is assigned
// per namespace (see NamespaceVNIConfig) and read at prepare time, so it is
// not part of any ResourceClaim. When Unschedulable=True is set on a full CXI
// device the scheduler deallocates that single claim and retries on another
// device — there is no second (VNI) claim to leave dangling.
```

Also remove the now-stale merged-template paragraph in `evaluateClaim`'s Case 3 comment (`claimbinder.go` ~lines 234–238) — replace it with:

```go
		// Case 3: hardware is truly at capacity — the request cannot fit even
		// without counting any pending grants. Signal Unschedulable so the DRA
		// scheduler deallocates this (single, CXI-only) claim and retries on a
		// different device.
```

- [ ] **Step 3: Update the `buildDevices` comment** (`resourceslice.go` lines ~72–79)

Replace the `BindingFailureConditions ... see ClaimBinder type comment for why separate claims cause Score errors.` block with:

```go
			// BindingConditions: the scheduler will not bind a pod until the
			// ClaimBinder sets Ready=True, confirming capacity is available.
			// BindingFailureConditions: if the ClaimBinder sets Unschedulable=True
			// (device hardware truly full), the scheduler deallocates this single
			// CXI claim and retries on a different device. VNIs are namespace-scoped
			// and not part of the claim, so there is nothing else to deallocate.
```

- [ ] **Step 4: Update `cmd/cxi-dra-driver/main.go`**

Replace the `driver.StartVNIPool(...)` block (lines ~147–156) with:

```go
		driver.StartNamespaceVNIAllocator(ctx, driver.NamespaceVNIConfig{
			KubeClient: kubeClient,
			Namespace:  *podNamespace,
			Identity:   fmt.Sprintf("%s/%s", *nodeName, *podUID),
			VNIStart:   *vniRangeStart,
			VNIEnd:     *vniRangeEnd,
		})
```

Update the comment above the `if *vniRangeStart != 0 || *vniRangeEnd != 0 {` guard to describe the namespace allocator instead of the ResourceSlice publisher:

```go
	// Start the cluster-wide namespace→VNI allocator if a VNI range is
	// configured. Exactly one pod (the leader) assigns VNIs to namespaces; the
	// rest participate in leader election and take over if the leader dies.
```

- [ ] **Step 5: Build and run the full driver test suite**

Run: `go build ./... && go test ./internal/driver/... -v`
Expected: build succeeds; all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/driver/claimbinder.go internal/driver/resourceslice.go cmd/cxi-dra-driver/main.go
git commit -m "driver: remove VNI from DRA scheduler path"
```

---

### Task 6: RBAC and manifests

**Files:**
- Modify: `deploy/kubernetes/clusterrole.yaml`
- Modify: `deploy/kubernetes/test/resourceclaim.yaml`
- Modify: `deploy/kubernetes/test/replicaset.yaml`, `deploy/kubernetes/test/replicaset-2node.yaml`
- Delete: `deploy/kubernetes/deviceclass-vni.yaml`

- [ ] **Step 1: Add namespace permissions to the ClusterRole**

In `deploy/kubernetes/clusterrole.yaml`, add this rule under `rules:` (the node driver needs `get` to read the annotation; the leader needs `list`/`watch` for the informer and `patch` to annotate):

```yaml
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["get", "list", "watch", "patch"]
```

Rename the leader-election Role/RoleBinding from `cxi-vni-pool-leader` to `cxi-vni-allocator-leader` and update the `resourceNames` to match the new lease name. The Role becomes:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: cxi-vni-allocator-leader
  namespace: kube-system
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["create"]
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    resourceNames: ["cxi-vni-allocator-leader"]
    verbs: ["get", "update"]
```

And the RoleBinding `metadata.name` and `roleRef.name` both become `cxi-vni-allocator-leader`.

- [ ] **Step 2: Make the test templates CXI-only**

Rewrite `deploy/kubernetes/test/resourceclaim.yaml` so each template has only the CXI device request (drop the `vni` request and the `cxi-vni` class), and rename `cxi-quarter-with-vni`→`cxi-quarter`, `cxi-half-with-vni`→`cxi-half`. The quarter template becomes:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: cxi-quarter
  namespace: default
spec:
  spec:
    devices:
      requests:
        - name: cxi
          exactly:
            deviceClassName: cxi-device
      config:
        - requests: ["cxi"]
          opaque:
            driver: cxi.slingshot.hpe.com
            parameters:
              txqs: 256
              tgqs: 128
              eqs: 511
              cts: 511
              ptes: 512
              acs: 255
              le_pools: 4
              tle_pools: 1
```

Apply the same shape to `cxi-half` (keep its existing larger parameter values; just drop the `vni` request). Replace the file's header comment with a note that the VNI is now assigned per namespace by the allocator and read at prepare time — no VNI request is needed.

- [ ] **Step 3: Point the ReplicaSets at the renamed template**

In `replicaset.yaml` and `replicaset-2node.yaml`, change `resourceClaimTemplateName: cxi-quarter-with-vni` to `resourceClaimTemplateName: cxi-quarter`. Update the surrounding comments that mention "merged cxi-quarter-with-vni claim (CXI device + fabric VNI ...)" to describe a CXI-only claim with a namespace-assigned VNI.

- [ ] **Step 4: Delete the VNI DeviceClass**

```bash
git rm deploy/kubernetes/deviceclass-vni.yaml
```

- [ ] **Step 5: Validate manifests parse**

Run: `kubectl apply --dry-run=client -f deploy/kubernetes/clusterrole.yaml -f deploy/kubernetes/test/resourceclaim.yaml -f deploy/kubernetes/test/replicaset.yaml`
Expected: all resources report `(dry run)` with no schema errors. (If no cluster is reachable, run `kubectl apply --dry-run=client --validate=false` to at least confirm YAML parses.)

- [ ] **Step 6: Commit**

```bash
git add deploy/kubernetes/clusterrole.yaml deploy/kubernetes/test/resourceclaim.yaml deploy/kubernetes/test/replicaset.yaml deploy/kubernetes/test/replicaset-2node.yaml
git rm deploy/kubernetes/deviceclass-vni.yaml
git commit -m "deploy: namespace-scoped VNI — RBAC, CXI-only templates, drop cxi-vni class"
```

---

### Task 7: End-to-end verification on the kind cluster

**Files:** none (verification only). Use the project's `/build` skill to deploy.

- [ ] **Step 1: Build and deploy** via the `/build` skill (kind cluster), ensuring `--vni-range-start`/`--vni-range-end` are set on the DaemonSet.

- [ ] **Step 2: Verify namespaces get VNIs**

Run: `kubectl get ns -o custom-columns=NAME:.metadata.name,VNI:.metadata.annotations.cxi\.slingshot\.hpe\.com/vni`
Expected: `default` and other non-system namespaces show a VNI in range; `kube-system`/`kube-public`/`kube-node-lease` show none.

- [ ] **Step 3: Reproduce the original failure scenario**

Run: `kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml && kubectl apply -f deploy/kubernetes/test/replicaset.yaml`
Watch: `kubectl get pods -l app=cxi-phase3-test -w`
Expected: pods reach `Running` with **no** `number of allocations ... smaller than number of claims` Score error. Two pods in the same namespace share the namespace's VNI.

- [ ] **Step 4: Confirm shared VNI in the service files**

Run: `kubectl exec <pod-a> -- cat /run/cxi/cxi0-service.yaml` and the same for `<pod-b>`.
Expected: both show the **same** VNI (the namespace's), while each pod has its own CXI device/service.

- [ ] **Step 5: Commit any manifest tweaks discovered during verification**, then stop — implementation complete.

---

## Self-Review

**Spec coverage:**
- Auto-allocate unique VNI per namespace → Tasks 1–3 (pure selection, reconcile, leader-run controller).
- Every namespace except system skip-list → `defaultVNISkip()` + `reconcile` skip check (Task 2).
- Read namespace VNI at CXI service creation → Task 4 (`namespaceVNI` in `prepareClaim`).
- One claim per pod / kill the Score bug → Tasks 4–6 remove the VNI claim, template request, and `cxi-vni` class.
- Free VNI on namespace deletion → no `DeleteFunc`; `assignedVNIs` rescans live namespaces (Tasks 2–3 comments).
- RBAC for namespace read/annotate → Task 6.

**Placeholder scan:** No TBD/TODO; every code step shows full code; deletions name exact files/symbols.

**Type consistency:** `vniAnnotationKey`, `nextFreeVNI`, `namespaceVNIController{client,lister,start,end,skip}`, `reconcile(ctx,name)`, `NamespaceVNIConfig{KubeClient,Namespace,Identity,VNIStart,VNIEnd}`, `StartNamespaceVNIAllocator`, `d.namespaceVNI(ctx,namespace)` are used identically across tasks.

## Open items for the implementer to confirm during review
- **Lease rename:** Task 3/6 rename the lease `cxi-vni-pool-leader`→`cxi-vni-allocator-leader`. If any existing cluster has the old lease, it is harmless (new name, new object). Keep the old name instead if you prefer zero RBAC churn — just be consistent across `vniallocator.go` and `clusterrole.yaml`.
- **Retry budget in `namespaceVNI`:** 10×1s. Tune if pod-startup-before-namespace-annotation is observed to need longer.
