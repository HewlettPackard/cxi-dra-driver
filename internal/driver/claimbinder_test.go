package driver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/HewlettPackard/cxi-dra-driver/internal/cxi"
)

// nodePool matches the Pool field used by cxiResult in prepare_test.go; the
// ClaimBinder treats Pool as the node identifier and only acts on results
// whose Pool matches its configured nodeName.
const nodePool = "node-cxi"

// makeClaim builds a ResourceClaim allocated to (driver, nodePool, device)
// with the standard cxiConfig opaque parameters and the given UID/namespace.
func makeClaim(uid types.UID, namespace, name, device string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			UID:       uid,
			Namespace: namespace,
			Name:      name,
		},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{Config: cxiConfig()},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{cxiResult(device)},
				},
			},
		},
	}
}

// newClaimBinderHarness wires a Driver, fake clientset, and ClaimBinder so
// that evaluateClaim can run end-to-end: hwAvail comes from the cxi stub
// (full device capacity), setCondition writes through the fake client's
// UpdateStatus, and we can fetch the claim back to inspect conditions.
func newClaimBinderHarness(t *testing.T, claims ...*resourceapi.ResourceClaim) (*ClaimBinder, *Driver, *fake.Clientset) {
	t.Helper()
	objs := make([]any, 0, len(claims))
	for _, c := range claims {
		objs = append(objs, c)
	}
	client := fake.NewSimpleClientset(toRuntimeObjects(objs)...)
	d := &Driver{
		driverName: testDriver,
		nodeName:   nodePool,
		devices:    cxi.MockDevices(4), // cxi0..cxi3
		store:      NewAllocationStore(),
		kubeClient: client,
		pending:    make(map[pendingKey]pendingEntry),
	}
	binder := NewClaimBinder(d, client, testDriver, nodePool)
	return binder, d, client
}

// toRuntimeObjects adapts the []any in the harness to the
// fake.NewSimpleClientset variadic, avoiding a type-assertion loop in every
// caller.
func toRuntimeObjects(in []any) []runtime.Object {
	out := make([]runtime.Object, 0, len(in))
	for _, obj := range in {
		if ro, ok := obj.(runtime.Object); ok {
			out = append(out, ro)
		}
	}
	return out
}

// conditionFor fetches the latest claim and returns the named condition for
// the matching (driver, pool, device) entry in AllocatedDeviceStatus, or nil
// if not found. Uses the real RV-aware Get/Update path so we catch any
// drift between evaluateClaim's writes and what the API server would see.
func conditionFor(t *testing.T, client *fake.Clientset, claim *resourceapi.ResourceClaim, device, condType string) *metav1.Condition {
	t.Helper()
	got, err := client.ResourceV1().ResourceClaims(claim.Namespace).Get(
		context.Background(), claim.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("fetching claim %s/%s: %v", claim.Namespace, claim.Name, err)
	}
	for _, dev := range got.Status.Devices {
		if dev.Driver != testDriver || dev.Pool != nodePool || dev.Device != device {
			continue
		}
		for i, c := range dev.Conditions {
			if c.Type == condType {
				return &dev.Conditions[i]
			}
		}
	}
	return nil
}

// A single claim on a free device → Ready=True with reason
// DeviceCapacityAvailable. This is the baseline that every other test builds
// on; if it fails, the harness is broken.
func TestClaimBinder_EvaluateClaim_GrantsReady(t *testing.T) {
	claim := makeClaim("c1", "ns1", "claim-1", "cxi0")
	binder, d, client := newClaimBinderHarness(t, claim)

	binder.evaluateClaim(context.Background(), claim)

	cond := conditionFor(t, client, claim, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %+v, want Status=True", cond)
	}
	if cond.Reason != "DeviceCapacityAvailable" {
		t.Errorf("Ready reason = %q, want DeviceCapacityAvailable", cond.Reason)
	}
	if !hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1, cxi0] not recorded after grant")
	}
}

// A single claim allocating two devices (a count: 2 request) must drive BOTH
// devices' Ready binding conditions to True — the scheduler will not bind the
// pod until every allocated device is Ready. Before the multi-device fix the
// binder only evaluated the first result, leaving cxi1 stuck in
// BindingConditionsPending forever. Both devices' pending grants must be held
// by the claim; two distinct devices at the same namespace VNI do not violate
// SHS14.
func TestClaimBinder_EvaluateClaim_MultiDeviceGrantsAllReady(t *testing.T) {
	claim := makeClaim("c1", "ns1", "claim-1", "cxi0")
	// Add a second allocated device to the same claim.
	claim.Status.Allocation.Devices.Results = append(
		claim.Status.Allocation.Devices.Results, cxiResult("cxi1"))
	binder, d, client := newClaimBinderHarness(t, claim)

	binder.evaluateClaim(context.Background(), claim)

	for _, dev := range []string{"cxi0", "cxi1"} {
		cond := conditionFor(t, client, claim, dev, "Ready")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("%s Ready condition = %+v, want Status=True", dev, cond)
		}
		if !hasPend(d, "c1", dev) {
			t.Errorf("pending[c1, %s] not recorded", dev)
		}
	}
}

// Two claims from the same namespace allocated to the same device: with the
// SHS14 duplicate-VNI constraint lifted (kernel now permits multiple services
// sharing a VNI on one device), BOTH claims must be granted Ready=True. The
// namespace's VNI is reused for co-tenant services on the device; the driver
// no longer refuses the second grant. Both claims hold a pending grant on the
// device, independent of one another.
func TestClaimBinder_EvaluateClaim_SameNsSameDeviceGrants(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	binder.evaluateClaim(context.Background(), second)

	// Both claims Ready=True: same-namespace co-location on one device.
	for _, c := range []*resourceapi.ResourceClaim{first, second} {
		cond := conditionFor(t, client, c, "cxi0", "Ready")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Fatalf("claim %s Ready = %+v, want Status=True", c.Name, cond)
		}
	}
	// Neither claim may be marked Unschedulable.
	for _, c := range []*resourceapi.ResourceClaim{first, second} {
		if cond := conditionFor(t, client, c, "cxi0", "Unschedulable"); cond != nil && cond.Status == metav1.ConditionTrue {
			t.Errorf("claim %s Unschedulable=True; duplicate-VNI co-location must be permitted", c.Name)
		}
	}
	// Both claims hold a pending grant on cxi0: same-namespace co-location.
	if !hasPend(d, "c1", "cxi0") || !hasPend(d, "c2", "cxi0") {
		t.Errorf("both c1 and c2 must hold a pending grant on cxi0")
	}
}

// Same namespace, different devices: both should be granted Ready=True. This
// is the happy path that Option B's whole point is to make possible — Option
// B enforces the spread, but only across devices; co-tenancy on the same
// node across devices is intentional.
func TestClaimBinder_EvaluateClaim_SameNsDifferentDeviceGrants(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi1")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	binder.evaluateClaim(context.Background(), second)

	for _, c := range []*resourceapi.ResourceClaim{first, second} {
		dev := c.Status.Allocation.Devices.Results[0].Device
		cond := conditionFor(t, client, c, dev, "Ready")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("claim %s/%s Ready on %s = %+v, want Status=True", c.Namespace, c.Name, dev, cond)
		}
	}
	if !hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1, cxi0] not recorded")
	}
	if !hasPend(d, "c2", "cxi1") {
		t.Errorf("pending[c2, cxi1] not recorded")
	}
}

// Different namespaces on the same device must coexist: SHS14's VNI
// uniqueness is per-VNI-per-device, and different namespaces have different
// VNIs, so co-tenancy is safe and Option B must not block it.
func TestClaimBinder_EvaluateClaim_DifferentNsSameDeviceGrants(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns2", "second", "cxi0")
	binder, _, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	binder.evaluateClaim(context.Background(), second)

	for _, c := range []*resourceapi.ResourceClaim{first, second} {
		cond := conditionFor(t, client, c, "cxi0", "Ready")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			t.Errorf("claim %s/%s Ready = %+v, want Status=True", c.Namespace, c.Name, cond)
		}
	}
}

// RemovePending frees a claim's in-flight grant (modelling NRI creating the
// service and calling RemovePending). A fresh same-namespace claim on the same
// device still grants Ready=True — same-namespace co-location is permitted now
// that SHS14's duplicate-VNI constraint is lifted.
func TestClaimBinder_EvaluateClaim_GrantsAfterRemovePending(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	// Simulate the pod's service being created: NRI fires RemovePending.
	d.RemovePending(types.UID("c1"))

	if hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1, cxi0] still present after RemovePending")
	}

	binder.evaluateClaim(context.Background(), second)

	cond := conditionFor(t, client, second, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second after RemovePending: Ready = %+v, want Status=True", cond)
	}
	if !hasPend(d, "c2", "cxi0") {
		t.Errorf("pending[c2, cxi0] not recorded after second grant")
	}
}

// When the scheduler clears Status.Allocation (e.g. PostFilter after a
// PreBind failure, or Unschedulable=True deallocating the claim), the binder
// must release the claim's pending grant. NRI's StopPodSandbox cannot be
// relied on: the pod may never have been admitted, so NRI never sees it.
// Without this cleanup the pending grant leaks and understates the device's
// effective capacity. This test reproduces the leak by re-evaluating the same
// claim with Allocation=nil after the original grant, then verifies that
// another claim on the same device is immediately grantable.
func TestClaimBinder_EvaluateClaim_AllocationNilCleansPending(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	if !hasPend(d, "c1", "cxi0") {
		t.Fatalf("setup: pending[c1, cxi0] not recorded")
	}

	// Simulate the scheduler deallocating: Status.Allocation cleared.
	// Mutating an unrelated copy of the original keeps the harness's
	// stored fake-client object intact while letting us drive the binder
	// with the deallocated state, matching the watch event the binder
	// would see in production after PostFilter clears Allocation.
	deallocated := first.DeepCopy()
	deallocated.Status.Allocation = nil
	binder.evaluateClaim(context.Background(), deallocated)

	if hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1] still present after Allocation=nil (this is the leak)")
	}

	// Proof-of-life: a second claim on the same device grants cleanly. The
	// real assertion above is the leak check: c1's pending grant must be gone
	// so the device's effective capacity is not understated.
	binder.evaluateClaim(context.Background(), second)
	cond := conditionFor(t, client, second, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second claim after Allocation=nil cleanup: Ready = %+v, want Status=True", cond)
	}
}

// A ResourceClaim deletion event must release the claim's pending grant. A
// claim can be deleted before NRI's StopPodSandbox fires — force-delete, the
// pod was never admitted, the scheduler deallocated during a retry — so the
// binder's watch.Deleted handler is the sole reliable cleanup path. Without
// it, the pending grant leaks and understates the device's capacity.
func TestClaimBinder_OnClaimDeleted_CleansPending(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	if !hasPend(d, "c1", "cxi0") {
		t.Fatalf("setup: pending[c1, cxi0] not recorded")
	}

	// Simulate the watch.Deleted event for c1.
	binder.onClaimDeleted(first)

	if hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1] still present after onClaimDeleted (this is the leak)")
	}

	// Proof-of-life: the second claim must now grant on the same device.
	binder.evaluateClaim(context.Background(), second)
	cond := conditionFor(t, client, second, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second claim after onClaimDeleted cleanup: Ready = %+v, want Status=True", cond)
	}
}

// The ClaimBinder's watch is what sees the scheduler add a pod to ReservedFor.
func TestClaimBinder_EvaluateClaim_RecordsSharedClaimPods(t *testing.T) {
	claim := sharedClaim("pod-a")
	binder, d, _ := newClaimBinderHarness(t, claim)
	d.cniHintsDir = t.TempDir()
	d.kubeClient = newTestDriver().kubeClient // carries the namespace VNI annotation
	if res := d.prepareClaim(context.Background(), claim); res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}

	binder.evaluateClaim(context.Background(), sharedClaim("pod-a", "pod-b"))

	if got := readHintFile(t, d.cniHintsDir, "pod-b"); len(got) != 1 || got[0].Device != "cxi0" {
		t.Fatalf("pod-b hint = %+v, want one cxi0 entry", got)
	}
}

// A failed hint write must not leave the pod marked as recorded: the claim
// stays in the binder's retry set and the next pass writes the hint.
func TestClaimBinder_SharedClaimHintWriteRetried(t *testing.T) {
	claim := sharedClaim("pod-a", "pod-b")
	binder, d, _ := newClaimBinderHarness(t, claim)
	d.kubeClient = newTestDriver().kubeClient // carries the namespace VNI annotation
	d.cniHintsDir = t.TempDir()
	if res := d.prepareClaim(context.Background(), sharedClaim("pod-a")); res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}

	// A regular file in the path makes the hints directory uncreatable.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	goodDir := d.cniHintsDir
	d.cniHintsDir = filepath.Join(blocker, "hints")

	binder.evaluateClaim(context.Background(), claim)

	if d.store.HasClaimPod("claim-1", "pod-b") {
		t.Fatal("pod-b marked recorded although its hint write failed")
	}
	binder.mu.Lock()
	_, queued := binder.falseClaims[claim.UID]
	binder.mu.Unlock()
	if !queued {
		t.Fatal("claim not queued for retry after a failed hint write")
	}

	d.cniHintsDir = goodDir
	binder.reEvaluateFalse(context.Background())

	if got := readHintFile(t, goodDir, "pod-b"); len(got) != 1 || got[0].Device != "cxi0" {
		t.Fatalf("pod-b hint after retry = %+v, want one cxi0 entry", got)
	}
}
