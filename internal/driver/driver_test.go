package driver

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// newDriverForTest returns a Driver with just the pending/occupancy maps
// initialised — the only state checkAndAddPending and RemoveOccupancy need.
// The other Driver fields (devices, kubeClient, store, etc.) are not touched
// by these unit tests. The notify channel is left nil; TriggerRepublish is
// safe to call on a nil channel because the select{} in it always falls
// through to the default branch when the send case is on a nil channel.
func newDriverForTest() *Driver {
	return &Driver{
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]int),
	}
}

// newDriverForTestWithNotify returns a Driver with the notify channel
// initialised so tests can assert that TriggerRepublish was called by reading
// from the channel non-blockingly.
func newDriverForTestWithNotify() *Driver {
	d := newDriverForTest()
	d.notify = make(chan struct{}, 1)
	return d
}

// drainNotify removes any pending republish signal so a subsequent
// notifyFired check measures only signals emitted after this call.
func drainNotify(d *Driver) {
	select {
	case <-d.notify:
	default:
	}
}

// notifyFired returns true if at least one republish signal is waiting on
// the channel. Non-blocking so the test does not deadlock when the producer
// did not signal.
func notifyFired(d *Driver) bool {
	select {
	case <-d.notify:
		return true
	default:
		return false
	}
}

// fits represents resources small enough to never fail the capacity check
// against any reasonable hwAvail; used when the test is about occupancy or
// pending logic, not resource fit.
var fits = cxi.ResourceSet{TXQs: 1, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1}

// hwAvailPlenty is a hardware capacity well above the resources requested in
// these tests, so the capacity branch never fires.
var hwAvailPlenty = cxi.ResourceSet{TXQs: 1000, TGQs: 1000, EQs: 1000, CTs: 1000, PTEs: 1000, ACs: 1000, LEs: 1000, TLEs: 1000}

func TestCheckAndAddPending_GrantsWhenFreeAndFits(t *testing.T) {
	d := newDriverForTest()
	granted, occupied := d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	if !granted || occupied {
		t.Fatalf("checkAndAddPending = (granted=%v, occupied=%v), want (true,false)", granted, occupied)
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != 1 {
		t.Errorf("nsDeviceOccupancy[ns1][cxi0] = %d, want 1", got)
	}
	if _, ok := d.pending["c1"]; !ok {
		t.Errorf("pending[c1] not recorded after grant")
	}
}

// Two claims from the same namespace must not co-locate on the same device.
// The first grants; the second receives occupied=true so the ClaimBinder can
// signal Unschedulable=True and the scheduler retries on another device.
func TestCheckAndAddPending_SameNamespaceSameDeviceRejects(t *testing.T) {
	d := newDriverForTest()
	if _, _ = d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty); d.nsDeviceOccupancy["ns1"]["cxi0"] != 1 {
		t.Fatalf("setup: first grant did not increment occupancy")
	}
	granted, occupied := d.checkAndAddPending("c2", "ns1", "cxi0", fits, hwAvailPlenty)
	if granted || !occupied {
		t.Fatalf("second claim in same ns+dev: got (granted=%v,occupied=%v), want (false,true)", granted, occupied)
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != 1 {
		t.Errorf("occupancy must not be re-incremented on refusal; got %d, want 1", got)
	}
	if _, ok := d.pending["c2"]; ok {
		t.Errorf("pending[c2] must not be recorded on occupancy refusal")
	}
}

// A different namespace on the same device must be allowed: SHS14's VNI
// uniqueness is per-namespace-VNI, not per-device.
func TestCheckAndAddPending_DifferentNamespaceSameDeviceGrants(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	granted, occupied := d.checkAndAddPending("c2", "ns2", "cxi0", fits, hwAvailPlenty)
	if !granted || occupied {
		t.Fatalf("different ns same device: got (granted=%v,occupied=%v), want (true,false)", granted, occupied)
	}
	if got := d.nsDeviceOccupancy["ns2"]["cxi0"]; got != 1 {
		t.Errorf("ns2 occupancy = %d, want 1", got)
	}
}

// Same namespace on a different device must be allowed: the SHS14 constraint
// is per-(namespace,device), so spreading a namespace across devices is
// exactly the intended behaviour.
func TestCheckAndAddPending_SameNamespaceDifferentDeviceGrants(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	granted, occupied := d.checkAndAddPending("c2", "ns1", "cxi1", fits, hwAvailPlenty)
	if !granted || occupied {
		t.Fatalf("same ns different device: got (granted=%v,occupied=%v), want (true,false)", granted, occupied)
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi1"]; got != 1 {
		t.Errorf("ns1/cxi1 occupancy = %d, want 1", got)
	}
}

// When raw hwAvail can't fit the request, neither pending nor occupancy is
// touched — occupied=false signals "capacity, not namespace" so the
// ClaimBinder picks the right Unschedulable path (DeviceAtCapacity).
func TestCheckAndAddPending_OverHardwareDoesNotIncrement(t *testing.T) {
	d := newDriverForTest()
	tooBig := cxi.ResourceSet{TXQs: 2000} // exceeds hwAvailPlenty.TXQs=1000
	granted, occupied := d.checkAndAddPending("c1", "ns1", "cxi0", tooBig, hwAvailPlenty)
	if granted || occupied {
		t.Fatalf("over hardware: got (granted=%v,occupied=%v), want (false,false)", granted, occupied)
	}
	if _, ok := d.pending["c1"]; ok {
		t.Errorf("pending[c1] must not be recorded on capacity refusal")
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != 0 {
		t.Errorf("occupancy must not be incremented on capacity refusal; got %d", got)
	}
}

// Occupancy is checked BEFORE capacity, so an occupancy refusal must not
// depend on requested vs hwAvail. If both constraints would fail, the
// caller sees occupied=true (the more specific/actionable signal).
func TestCheckAndAddPending_OccupancyTakesPrecedenceOverCapacity(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	tooBig := cxi.ResourceSet{TXQs: 2000} // would also fail capacity
	granted, occupied := d.checkAndAddPending("c2", "ns1", "cxi0", tooBig, hwAvailPlenty)
	if granted || !occupied {
		t.Fatalf("occupancy first: got (granted=%v,occupied=%v), want (false,true)", granted, occupied)
	}
}

// RemoveOccupancy decrements the count and drops the entry on zero. Calls for
// unknown (ns, dev) are no-ops so NRI can retry without state corruption.
func TestRemoveOccupancy(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.checkAndAddPending("c2", "ns1", "cxi1", fits, hwAvailPlenty)

	// First decrement on cxi0 should drop the entry and leave cxi1.
	d.RemoveOccupancy("ns1", "cxi0")
	if _, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; ok {
		t.Errorf("nsDeviceOccupancy[ns1][cxi0] still present after RemoveOccupancy")
	}
	if got := d.nsDeviceOccupancy["ns1"]["cxi1"]; got != 1 {
		t.Errorf("ns1/cxi1 occupancy = %d, want 1 (RemoveOccupancy must not affect unrelated entries)", got)
	}

	// Idempotent: a second call for the same (ns, dev) is a no-op.
	d.RemoveOccupancy("ns1", "cxi0")
	if _, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; ok {
		t.Errorf("idempotent RemoveOccupancy unexpectedly re-created entry")
	}

	// Removing the last device for a namespace prunes the namespace bucket.
	d.RemoveOccupancy("ns1", "cxi1")
	if _, ok := d.nsDeviceOccupancy["ns1"]; ok {
		t.Errorf("namespace bucket should be pruned when empty; ns1 still present")
	}

	// Unknown ns/dev is harmless.
	d.RemoveOccupancy("does-not-exist", "neither-does-this")
}

// After a namespace device is freed by RemoveOccupancy, a subsequent claim
// from the same namespace must be grantable on that device. This is the
// post-DestroyService unblocking path.
func TestCheckAndAddPending_GrantableAfterRemoveOccupancy(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)

	// Simulate the NRI StopPodSandbox cleanup: pending stays in place until
	// CreateService runs and calls RemovePending; here we exercise the
	// post-RemovePending state where only occupancy remains.
	d.RemovePending("c1")
	d.RemoveOccupancy("ns1", "cxi0")

	granted, occupied := d.checkAndAddPending("c2", "ns1", "cxi0", fits, hwAvailPlenty)
	if !granted || occupied {
		t.Fatalf("after RemoveOccupancy: got (granted=%v,occupied=%v), want (true,false)", granted, occupied)
	}
}

// RemovePending must not touch nsDeviceOccupancy — the service is alive on
// the device until DestroyService runs. Premature decrement would let another
// claim be granted on the same (ns, device) and then collide at CreateService.
func TestRemovePending_DoesNotDecrementOccupancy(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.RemovePending("c1")
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != 1 {
		t.Errorf("occupancy after RemovePending = %d, want 1 (RemovePending must NOT touch occupancy)", got)
	}
}

// An unoccupied device returns the empty string. The CEL selector relies on
// this — `"".contains(",foo,")` returns false, so empty active_namespaces
// never excludes any device.
func TestActiveNamespacesFor_Empty(t *testing.T) {
	d := newDriverForTest()
	if got := d.activeNamespacesFor("cxi0"); got != "" {
		t.Errorf("activeNamespacesFor(cxi0) on empty driver = %q, want %q", got, "")
	}
}

// A single occupant produces ",<NS>," with both leading and trailing commas
// so the CEL selector's `.contains(",<NS>,")` is an exact-match (not a
// prefix match). Without the commas, namespace "foo" would falsely match
// "foobar".
func TestActiveNamespacesFor_SingleNamespace(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)

	got := d.activeNamespacesFor("cxi0")
	want := ",ns1,"
	if got != want {
		t.Errorf("activeNamespacesFor(cxi0) = %q, want %q", got, want)
	}
}

// Multiple occupants are sorted lexicographically so the published string is
// deterministic across reconciles — preventing spurious ResourceSlice
// republishes when only the iteration order of the map changes.
func TestActiveNamespacesFor_MultipleNamespacesSorted(t *testing.T) {
	d := newDriverForTest()
	// Insert in non-alphabetic order to verify sorting.
	d.checkAndAddPending("c1", "zeta", "cxi0", fits, hwAvailPlenty)
	d.checkAndAddPending("c2", "alpha", "cxi0", fits, hwAvailPlenty)
	d.checkAndAddPending("c3", "mu", "cxi0", fits, hwAvailPlenty)

	got := d.activeNamespacesFor("cxi0")
	want := ",alpha,mu,zeta,"
	if got != want {
		t.Errorf("activeNamespacesFor(cxi0) = %q, want %q", got, want)
	}
}

// Each device's list is scoped to that device only — a namespace occupying
// cxi0 does NOT appear in the active_namespaces of cxi1. Otherwise CEL would
// exclude a free device because of unrelated occupancy elsewhere on the node.
func TestActiveNamespacesFor_OnlyForRequestedDevice(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.checkAndAddPending("c2", "ns2", "cxi1", fits, hwAvailPlenty)

	if got := d.activeNamespacesFor("cxi0"); got != ",ns1," {
		t.Errorf("cxi0 = %q, want %q", got, ",ns1,")
	}
	if got := d.activeNamespacesFor("cxi1"); got != ",ns2," {
		t.Errorf("cxi1 = %q, want %q", got, ",ns2,")
	}
	if got := d.activeNamespacesFor("cxi2"); got != "" {
		t.Errorf("cxi2 = %q, want empty", got)
	}
}

// After RemoveOccupancy frees the slot, the namespace is removed from the
// active_namespaces string for that device. This is the path that unblocks
// scheduling for another claim in the same namespace.
func TestActiveNamespacesFor_AfterRemoveOccupancy(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.checkAndAddPending("c2", "ns2", "cxi0", fits, hwAvailPlenty)

	if got := d.activeNamespacesFor("cxi0"); got != ",ns1,ns2," {
		t.Fatalf("setup: cxi0 = %q, want %q", got, ",ns1,ns2,")
	}

	d.RemoveOccupancy("ns1", "cxi0")
	if got := d.activeNamespacesFor("cxi0"); got != ",ns2," {
		t.Errorf("after removing ns1: cxi0 = %q, want %q", got, ",ns2,")
	}

	d.RemoveOccupancy("ns2", "cxi0")
	if got := d.activeNamespacesFor("cxi0"); got != "" {
		t.Errorf("after removing ns2: cxi0 = %q, want empty", got)
	}
}

// A grant from checkAndAddPending must trigger a ResourceSlice republish so
// the new pending entry (reduced avail_*) and the 0→1 occupancy transition
// (active_namespaces gains this namespace) are visible to the scheduler.
func TestCheckAndAddPending_GrantTriggersRepublish(t *testing.T) {
	d := newDriverForTestWithNotify()
	drainNotify(d)

	granted, _ := d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	if !granted {
		t.Fatalf("setup: grant failed unexpectedly")
	}
	if !notifyFired(d) {
		t.Errorf("checkAndAddPending grant did not TriggerRepublish; scheduler will not see the active_namespaces or avail_* change until the periodic refresh")
	}
}

// An occupancy refusal must NOT trigger a republish: no state changed, so
// publishing an identical ResourceSlice would be wasted work.
func TestCheckAndAddPending_OccupiedDoesNotTriggerRepublish(t *testing.T) {
	d := newDriverForTestWithNotify()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	drainNotify(d) // clear the first grant's signal

	_, occupied := d.checkAndAddPending("c2", "ns1", "cxi0", fits, hwAvailPlenty)
	if !occupied {
		t.Fatalf("setup: expected occupied=true for second claim in same ns+dev")
	}
	if notifyFired(d) {
		t.Errorf("occupied refusal triggered a republish; ResourceSlice would be churned without any attribute change")
	}
}

// A 1→0 RemoveOccupancy must trigger a republish so the active_namespaces
// attribute drops this namespace and other claims in it become schedulable
// on this device again. Without this, claims would wait for the next 10s
// periodic refresh to see the freed slot.
func TestRemoveOccupancy_1To0TriggersRepublish(t *testing.T) {
	d := newDriverForTestWithNotify()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	drainNotify(d) // clear the grant's republish signal

	d.RemoveOccupancy("ns1", "cxi0")
	if !notifyFired(d) {
		t.Errorf("RemoveOccupancy 1→0 did not TriggerRepublish; scheduler will not see the freed slot until the periodic refresh")
	}
}

// A N→N-1 (N>=2) RemoveOccupancy must NOT trigger a republish: the namespace
// is still present in active_namespaces (count is positive), so the attribute
// string is unchanged and publishing it would be no-op churn. This case
// arises if NRI ever increments the count >1 (currently it cannot — checkAndAddPending
// rejects same-ns same-dev so count is always at most 1 — but the property
// is still required for the 1→0 trigger to be a meaningful signal rather
// than a noisy "any decrement").
func TestRemoveOccupancy_NToNMinusOneDoesNotTriggerRepublish(t *testing.T) {
	d := newDriverForTestWithNotify()
	// Bypass checkAndAddPending and set the count directly to model the
	// hypothetical N>=2 case.
	d.nsDeviceOccupancy["ns1"] = map[string]int{"cxi0": 3}
	drainNotify(d)

	d.RemoveOccupancy("ns1", "cxi0")
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != 2 {
		t.Fatalf("setup: count after decrement = %d, want 2", got)
	}
	if notifyFired(d) {
		t.Errorf("RemoveOccupancy 3→2 triggered a republish; only 1→0 transitions change the active_namespaces string")
	}
}

// RemoveOccupancy on an unknown (ns, dev) must NOT trigger a republish: no
// state changed, so there is nothing to republish.
func TestRemoveOccupancy_UnknownDoesNotTriggerRepublish(t *testing.T) {
	d := newDriverForTestWithNotify()
	drainNotify(d)

	d.RemoveOccupancy("does-not-exist", "neither-does-this")
	if notifyFired(d) {
		t.Errorf("RemoveOccupancy on unknown (ns, dev) triggered a republish; should have been a no-op")
	}
}

// buildDevices must include the active_namespaces attribute for every device,
// even unoccupied ones (as an empty string). The admission-injected CEL
// selector relies on this — if the attribute is missing on any CXI device
// the selector's `.contains()` call would error and the device would be
// excluded from scheduling entirely (CEL errors are not silently false).
func TestBuildDevices_AlwaysPublishesActiveNamespaces(t *testing.T) {
	d := &Driver{
		nodeName:          "test-node",
		devices:           cxi.MockDevices(3), // cxi0..cxi2
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]int),
	}

	devices := d.buildDevices()
	if len(devices) != 3 {
		t.Fatalf("buildDevices returned %d devices, want 3", len(devices))
	}
	for _, dev := range devices {
		attr, ok := dev.Attributes["active_namespaces"]
		if !ok {
			t.Errorf("device %s missing active_namespaces attribute; CEL selector would error", dev.Name)
			continue
		}
		if attr.StringValue == nil {
			t.Errorf("device %s active_namespaces.StringValue is nil; want pointer to empty string", dev.Name)
			continue
		}
		if *attr.StringValue != "" {
			t.Errorf("device %s active_namespaces = %q, want empty (no occupancy was set)", dev.Name, *attr.StringValue)
		}
	}
}

// buildDevices encodes occupied devices with the sorted comma-padded list and
// leaves unoccupied devices empty. End-to-end check that the helper, the
// publish path, and the data structure are all wired correctly.
func TestBuildDevices_PublishesPerDeviceOccupancy(t *testing.T) {
	d := &Driver{
		nodeName:          "test-node",
		devices:           cxi.MockDevices(3), // cxi0..cxi2
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]int),
	}
	// cxi0: occupied by zeta and alpha (sorted output expected)
	// cxi1: occupied by mu only
	// cxi2: unoccupied
	d.nsDeviceOccupancy["zeta"] = map[string]int{"cxi0": 1}
	d.nsDeviceOccupancy["alpha"] = map[string]int{"cxi0": 1}
	d.nsDeviceOccupancy["mu"] = map[string]int{"cxi1": 1}

	devices := d.buildDevices()
	want := map[string]string{
		"cxi0": ",alpha,zeta,",
		"cxi1": ",mu,",
		"cxi2": "",
	}
	for _, dev := range devices {
		got := ptr.Deref(dev.Attributes["active_namespaces"].StringValue, "<nil>")
		if w, ok := want[dev.Name]; ok && got != w {
			t.Errorf("device %s active_namespaces = %q, want %q", dev.Name, got, w)
		}
	}
}
