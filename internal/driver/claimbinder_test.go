package driver

import (
	"context"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
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
		driverName:        testDriver,
		nodeName:          nodePool,
		devices:           cxi.MockDevices(4), // cxi0..cxi3
		store:             NewAllocationStore(),
		kubeClient:        client,
		notify:            make(chan struct{}, 1),
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]types.UID),
		claimToOccupancy:  make(map[types.UID]occupancyKey),
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
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Errorf("occupancy[ns1][cxi0] = %q, want %q", got, "c1")
	}
}

// Two claims from the same namespace allocated to the same device: the first
// gets Ready=True; the second must get Unschedulable=True with reason
// NamespaceDeviceOccupied so the scheduler reallocates it elsewhere. SHS14
// would reject the second cxil_svc_set_vni_range with -EEXIST since both
// claims share the namespace's VNI.
func TestClaimBinder_EvaluateClaim_SameNsSameDeviceUnschedulable(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, _, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	binder.evaluateClaim(context.Background(), second)

	// First claim still Ready=True.
	if cond := conditionFor(t, client, first, "cxi0", "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("first Ready = %+v, want Status=True", cond)
	}
	// Second claim Unschedulable=True with the occupancy reason.
	cond := conditionFor(t, client, second, "cxi0", "Unschedulable")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second Unschedulable = %+v, want Status=True", cond)
	}
	if cond.Reason != "NamespaceDeviceOccupied" {
		t.Errorf("second Unschedulable reason = %q, want NamespaceDeviceOccupied", cond.Reason)
	}
	// Second must NOT have been granted Ready=True.
	if cond := conditionFor(t, client, second, "cxi0", "Ready"); cond != nil && cond.Status == metav1.ConditionTrue {
		t.Errorf("second was granted Ready=True; SHS14 collision not enforced")
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
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Errorf("occupancy[ns1][cxi0] = %q, want %q", got, "c1")
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi1"]; got != "c2" {
		t.Errorf("occupancy[ns1][cxi1] = %q, want %q", got, "c2")
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

// After RemoveOccupancy frees a (ns, dev) slot (modelling the NRI
// StopPodSandbox → DestroyService → RemoveOccupancy callback), a second
// claim from the same namespace must be grantable on that device. This is
// the unblocking path that prevents permanent stuck-Unschedulable loops.
func TestClaimBinder_EvaluateClaim_GrantsAfterRemoveOccupancy(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	// Simulate the pod stopping: NRI fires RemovePending then RemoveOccupancy.
	d.RemovePending(types.UID("c1"))
	d.RemoveOccupancy("ns1", "cxi0")

	binder.evaluateClaim(context.Background(), second)

	cond := conditionFor(t, client, second, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second after RemoveOccupancy: Ready = %+v, want Status=True", cond)
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c2" {
		t.Errorf("occupancy[ns1][cxi0] after second grant = %q, want %q", got, "c2")
	}
}

// When the scheduler clears Status.Allocation (e.g. PostFilter after a
// PreBind failure, or Unschedulable=True deallocating the claim), the binder
// must release BOTH pending and the (namespace, device) occupancy slot. NRI's
// StopPodSandbox cannot be relied on: the pod may never have been admitted,
// so NRI never sees it. Without this cleanup the slot leaks and the
// namespace stays pinned to the device in active_namespaces, eventually
// blocking the namespace from every device on the node as the scheduler
// keeps retrying. This test reproduces the leak by re-evaluating the same
// claim with Allocation=nil after the original grant, then verifies that
// another claim in the same namespace is immediately grantable on the same
// device.
func TestClaimBinder_EvaluateClaim_AllocationNilCleansOccupancy(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Fatalf("setup: occupancy[ns1][cxi0] = %q, want %q", got, "c1")
	}

	// Simulate the scheduler deallocating: Status.Allocation cleared.
	// Mutating an unrelated copy of the original keeps the harness's
	// stored fake-client object intact while letting us drive the binder
	// with the deallocated state, matching the watch event the binder
	// would see in production after PostFilter clears Allocation.
	deallocated := first.DeepCopy()
	deallocated.Status.Allocation = nil
	binder.evaluateClaim(context.Background(), deallocated)

	if got, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; ok {
		t.Errorf("ns1/cxi0 slot still present after Allocation=nil; held by %q (this is the leak)", got)
	}
	if _, ok := d.claimToOccupancy["c1"]; ok {
		t.Errorf("claimToOccupancy[c1] still present after Allocation=nil")
	}
	if _, ok := d.pending["c1"]; ok {
		t.Errorf("pending[c1] still present after Allocation=nil")
	}

	// Proof-of-life: a second claim in the same namespace on the same
	// device must now grant cleanly. If the cleanup had been incomplete,
	// checkAndAddPending would see ns1 still occupying cxi0 (by c1) and
	// refuse the second grant with occupied=true.
	binder.evaluateClaim(context.Background(), second)
	cond := conditionFor(t, client, second, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second claim after Allocation=nil cleanup: Ready = %+v, want Status=True", cond)
	}
}

// A ResourceClaim deletion event must release pending AND the (namespace,
// device) occupancy slot. A claim can be deleted before NRI's
// StopPodSandbox fires — force-delete, the pod was never admitted, the
// scheduler deallocated during a retry — so the binder's watch.Deleted
// handler is the sole reliable cleanup path. Without it, the slot leaks
// and the namespace stays pinned to this device in active_namespaces.
func TestClaimBinder_OnClaimDeleted_CleansOccupancy(t *testing.T) {
	first := makeClaim("c1", "ns1", "first", "cxi0")
	second := makeClaim("c2", "ns1", "second", "cxi0")
	binder, d, client := newClaimBinderHarness(t, first, second)

	binder.evaluateClaim(context.Background(), first)
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Fatalf("setup: occupancy[ns1][cxi0] = %q, want %q", got, "c1")
	}

	// Simulate the watch.Deleted event for c1.
	binder.onClaimDeleted(first)

	if got, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; ok {
		t.Errorf("ns1/cxi0 slot still present after onClaimDeleted; held by %q (this is the leak)", got)
	}
	if _, ok := d.claimToOccupancy["c1"]; ok {
		t.Errorf("claimToOccupancy[c1] still present after onClaimDeleted")
	}
	if _, ok := d.pending["c1"]; ok {
		t.Errorf("pending[c1] still present after onClaimDeleted")
	}

	// Proof-of-life: the second claim must now grant on the same device.
	binder.evaluateClaim(context.Background(), second)
	cond := conditionFor(t, client, second, "cxi0", "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("second claim after onClaimDeleted cleanup: Ready = %+v, want Status=True", cond)
	}
}
