package driver

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// newDriverForTest returns a Driver with just the pending/occupancy maps
// initialised — the only state checkAndAddPending and RemoveOccupancy need.
// The other Driver fields (devices, kubeClient, store, etc.) are not touched
// by these unit tests.
func newDriverForTest() *Driver {
	return &Driver{
		pending:           make(map[types.UID]pendingEntry),
		nsDeviceOccupancy: make(map[string]map[string]int),
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
