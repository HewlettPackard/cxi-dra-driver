package driver

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// newDriverForTest returns a Driver with just the pending map initialised —
// the only state checkAndAddPending, RemovePending, and reconcileClaimDevices
// need. The other Driver fields (devices, kubeClient, store, etc.) are not
// touched by these unit tests.
func newDriverForTest() *Driver {
	return &Driver{
		pending: make(map[pendingKey]pendingEntry),
	}
}

// fits represents resources small enough to never fail the capacity check
// against any reasonable hwAvail; used when the test is about pending logic,
// not resource fit.
var fits = cxi.ResourceSet{TXQs: 1, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1}

// hasPend reports whether the (claim, device) pending grant is recorded.
func hasPend(d *Driver, claim types.UID, device string) bool {
	_, ok := d.pending[pendingKey{claimUID: claim, deviceName: device}]
	return ok
}

// hwAvailPlenty is a hardware capacity well above the resources requested in
// these tests, so the capacity branch never fires.
var hwAvailPlenty = cxi.ResourceSet{TXQs: 1000, TGQs: 1000, EQs: 1000, CTs: 1000, PTEs: 1000, ACs: 1000, LEs: 1000, TLEs: 1000}

func TestCheckAndAddPending_GrantsWhenFreeAndFits(t *testing.T) {
	d := newDriverForTest()
	if granted := d.checkAndAddPending("c1", "cxi0", "", fits, hwAvailPlenty); !granted {
		t.Fatalf("checkAndAddPending = false, want granted")
	}
	if !hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1] not recorded after grant")
	}
}

// Two claims MAY now co-locate on the same device: SHS14 permits multiple
// services sharing one VNI per device, so nothing but capacity gates a grant.
// Both claims are granted and both hold a pending entry.
func TestCheckAndAddPending_TwoClaimsSameDeviceGrant(t *testing.T) {
	d := newDriverForTest()
	if granted := d.checkAndAddPending("c1", "cxi0", "", fits, hwAvailPlenty); !granted {
		t.Fatalf("setup: first grant failed")
	}
	if granted := d.checkAndAddPending("c2", "cxi0", "", fits, hwAvailPlenty); !granted {
		t.Fatalf("second claim on same device: got false, want granted")
	}
	if !hasPend(d, "c1", "cxi0") || !hasPend(d, "c2", "cxi0") {
		t.Errorf("both c1 and c2 should hold a pending entry for cxi0")
	}
}

// When raw hwAvail can't fit the request, no pending entry is recorded. The
// ClaimBinder distinguishes DeviceAtCapacity from the pending-blocked case by
// re-checking hwAvail alone.
func TestCheckAndAddPending_OverHardwareDoesNotIncrement(t *testing.T) {
	d := newDriverForTest()
	tooBig := cxi.ResourceSet{TXQs: 2000} // exceeds hwAvailPlenty.TXQs=1000
	if granted := d.checkAndAddPending("c1", "cxi0", "", tooBig, hwAvailPlenty); granted {
		t.Fatalf("over hardware: got granted, want refused")
	}
	if hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1] must not be recorded on capacity refusal")
	}
}

// Capacity accounts for existing pending grants: a second claim on the same
// device that no longer fits after the first grant's pending subtraction is
// refused, and must not record a pending entry.
func TestCheckAndAddPending_PendingBlocksSecondGrant(t *testing.T) {
	d := newDriverForTest()
	// hwAvail with exactly room for one "big" grant.
	hw := cxi.ResourceSet{TXQs: 100, TGQs: 100, EQs: 100, CTs: 100, PTEs: 100, ACs: 100, LEs: 100, TLEs: 100}
	big := cxi.ResourceSet{TXQs: 60}
	if granted := d.checkAndAddPending("c1", "cxi0", "", big, hw); !granted {
		t.Fatalf("setup: first grant failed")
	}
	// Second 60 would need 120 > 100 available; refused.
	if granted := d.checkAndAddPending("c2", "cxi0", "", big, hw); granted {
		t.Fatalf("second grant should be refused: pending sum exceeds capacity")
	}
	if hasPend(d, "c2", "cxi0") {
		t.Errorf("c2 must not record a pending entry on capacity refusal")
	}
}

// RemovePending clears every pending entry for a claim (a count>1 claim holds
// one per device). Called by NRI after the pod's services are created.
func TestRemovePending(t *testing.T) {
	d := newDriverForTest()
	d.checkAndAddPending("c1", "cxi0", "", fits, hwAvailPlenty)
	d.checkAndAddPending("c1", "cxi1", "", fits, hwAvailPlenty)
	d.checkAndAddPending("c2", "cxi0", "", fits, hwAvailPlenty)

	d.RemovePending("c1")

	if hasPend(d, "c1", "cxi0") || hasPend(d, "c1", "cxi1") {
		t.Errorf("RemovePending(c1) left a pending entry for c1")
	}
	if !hasPend(d, "c2", "cxi0") {
		t.Errorf("RemovePending(c1) removed an unrelated pending entry for c2")
	}
}

// Subset reallocation: the scheduler can move a claim from cxi0 to cxi1
// without the binder observing Status.Allocation=nil in between.
// reconcileClaimDevices must release the pending entry for the dropped device.
func TestReconcileClaimDevices_ReleasesDroppedDevicePending(t *testing.T) {
	d := newDriverForTest()

	d.checkAndAddPending("c1", "cxi0", "", fits, hwAvailPlenty)
	if !hasPend(d, "c1", "cxi0") {
		t.Fatalf("setup: pending[c1, cxi0] not recorded")
	}

	// Binder observes c1 now allocated to cxi1 only: reconcile to {cxi1}
	// releases the stale cxi0 pending, then the new device is granted.
	d.reconcileClaimDevices("c1", map[pendingKey]bool{{deviceName: "cxi1"}: true})
	if hasPend(d, "c1", "cxi0") {
		t.Errorf("stale pending[c1, cxi0] still present after reconcile to {cxi1}")
	}

	if granted := d.checkAndAddPending("c1", "cxi1", "", fits, hwAvailPlenty); !granted {
		t.Fatalf("reallocation grant on cxi1 failed")
	}
	if !hasPend(d, "c1", "cxi1") {
		t.Errorf("pending[c1, cxi1] not recorded after reallocation grant")
	}
}

// Re-evaluation of the SAME claim on the SAME device must be idempotent: the
// claim already holds a pending entry, so nothing is double-counted.
func TestCheckAndAddPending_SameClaimReEvaluationIsIdempotent(t *testing.T) {
	d := newDriverForTest()

	if granted := d.checkAndAddPending("c1", "cxi0", "", fits, hwAvailPlenty); !granted {
		t.Fatalf("initial grant failed unexpectedly")
	}
	if granted := d.checkAndAddPending("c1", "cxi0", "", fits, hwAvailPlenty); !granted {
		t.Fatalf("re-evaluation of same claim on same device: got false, want granted")
	}

	if len(d.pending) != 1 {
		t.Errorf("pending len = %d after re-eval, want 1", len(d.pending))
	}
}

// buildDevices must not publish attributes that nothing consumes any more:
// active_namespaces (removed once the SHS14 duplicate-VNI constraint was
// lifted), and the avail_*/capacity_* resource attributes (superseded by
// consumable capacity).
func TestBuildDevices_DoesNotPublishRetiredAttributes(t *testing.T) {
	d := &Driver{
		nodeName: "test-node",
		devices:  cxi.MockDevices(3), // cxi0..cxi2
		pending:  make(map[pendingKey]pendingEntry),
	}

	devices := d.buildDevices()
	if len(devices) != 3 {
		t.Fatalf("buildDevices returned %d devices, want 3", len(devices))
	}
	for _, dev := range devices {
		for name := range dev.Attributes {
			n := string(name)
			if n == "active_namespaces" || strings.HasPrefix(n, "avail_") || strings.HasPrefix(n, "capacity_") {
				t.Errorf("device %s still publishes retired attribute %s", dev.Name, n)
			}
		}
	}
}
