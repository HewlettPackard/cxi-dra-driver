package driver

import (
	"context"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// namespaceWithVNI is a tiny helper for building corev1.Namespace objects
// with the VNI annotation set, matching what the vniallocator would write
// in production.
func namespaceWithVNI(name string, vni int) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{vniAnnotationKey: strconv.Itoa(vni)},
		},
	}
}

// reconcileApplyHarness returns a Driver with both occupancy maps initialised
// and a notify channel so tests can assert republish triggering. No
// kubeClient or devices are needed because applyReconcile is pure: it takes
// the hardware set as a parameter.
func reconcileApplyHarness() *Driver {
	return newDriverForTestWithNotify()
}

// applyReconcile on an empty driver with an empty hwSet must be a no-op:
// nothing in nsDeviceOccupancy to remove, nothing in hwSet to add, no
// republish needed.
func TestApplyReconcile_NoOpWhenBothEmpty(t *testing.T) {
	d := reconcileApplyHarness()
	drainNotify(d)

	d.applyReconcile(map[occupancyKey]struct{}{})

	if len(d.nsDeviceOccupancy) != 0 {
		t.Errorf("nsDeviceOccupancy = %+v, want empty", d.nsDeviceOccupancy)
	}
	if notifyFired(d) {
		t.Errorf("applyReconcile triggered republish with no state change")
	}
}

// An entry in nsDeviceOccupancy whose claim UID is in pending (real claim
// mid-flight between Ready=True grant and NRI CreateService) must be kept
// even if the hardware set is empty. Releasing it would let another claim
// grab the slot and collide with the pending one at CreateService.
func TestApplyReconcile_KeepsPendingEntryNotYetInHardware(t *testing.T) {
	d := reconcileApplyHarness()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	drainNotify(d)

	// hwSet is empty: NRI hasn't created the service yet.
	d.applyReconcile(map[occupancyKey]struct{}{})

	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Errorf("ns1/cxi0 = %q, want %q (pending claim must not be cleared)", got, "c1")
	}
	if !hasPend(d, "c1", "cxi0") {
		t.Errorf("pending[c1] removed by reconcile; reconciler must not touch pending")
	}
	if notifyFired(d) {
		t.Errorf("reconcile triggered republish when keeping a pending entry; no state changed")
	}
}

// An entry in nsDeviceOccupancy whose claim UID is NOT in pending and whose
// (ns, dev) IS in the hardware set is the steady-state case: real claim,
// NRI created the service, RemovePending fired. The reconciler must keep
// the entry.
func TestApplyReconcile_KeepsRealClaimBackedByHardware(t *testing.T) {
	d := reconcileApplyHarness()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.RemovePending("c1") // NRI created the service.
	drainNotify(d)

	hwSet := map[occupancyKey]struct{}{
		{namespace: "ns1", deviceName: "cxi0"}: {},
	}
	d.applyReconcile(hwSet)

	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Errorf("ns1/cxi0 = %q, want %q (real claim with hw backing must not be overwritten by synthetic)", got, "c1")
	}
	if got := occSlot(t, d, "c1"); got.namespace != "ns1" || got.deviceName != "cxi0" {
		t.Errorf("claimToOccupancy[c1] = %+v, want {ns1, cxi0}", got)
	}
	if notifyFired(d) {
		t.Errorf("reconcile triggered republish in steady state; no state changed")
	}
}

// The leak case the reconciler exists to fix: an entry whose claim UID is
// NOT in pending AND whose (ns, dev) is NOT in hardware. The binder's
// RemoveOccupancyForClaim should have fired but didn't (lost watch event,
// pod force-deleted before admission, etc.). The reconciler must release
// the slot, prune empty namespace buckets, drop the reverse-index entry,
// and trigger a republish so the freed slot reaches the scheduler.
func TestApplyReconcile_ReleasesLeakedEntry(t *testing.T) {
	d := reconcileApplyHarness()
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.RemovePending("c1") // simulate NRI created then we lost track somehow
	drainNotify(d)

	// hwSet is empty: hardware service was destroyed but we missed the event.
	d.applyReconcile(map[occupancyKey]struct{}{})

	if _, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; ok {
		t.Errorf("ns1/cxi0 slot still present; leak not released")
	}
	if _, ok := d.nsDeviceOccupancy["ns1"]; ok {
		t.Errorf("ns1 namespace bucket should be pruned when empty")
	}
	if _, ok := d.claimToOccupancy["c1"]; ok {
		t.Errorf("claimToOccupancy[c1] still present; reverse index not cleaned")
	}
	if !notifyFired(d) {
		t.Errorf("leak release did not trigger republish; scheduler won't see freed slot until next periodic refresh")
	}
}

// Driver-restart recovery: nsDeviceOccupancy is empty (fresh process) but
// hardware has live services. The reconciler must add synthetic entries
// with "hw:" prefix UIDs so the namespace is correctly marked as occupying
// the device in active_namespaces, preventing the scheduler from placing
// a colliding claim on that device.
func TestApplyReconcile_AddsHardwareOnlyEntryWithSyntheticUID(t *testing.T) {
	d := reconcileApplyHarness()
	drainNotify(d)

	hwSet := map[occupancyKey]struct{}{
		{namespace: "ns1", deviceName: "cxi0"}: {},
	}
	d.applyReconcile(hwSet)

	got := d.nsDeviceOccupancy["ns1"]["cxi0"]
	want := types.UID("hw:cxi0/ns1")
	if got != want {
		t.Errorf("ns1/cxi0 = %q, want %q (synthetic UID for hardware-only entry)", got, want)
	}
	// Reverse index must be populated so future RemoveOccupancy(ns1, cxi0)
	// (e.g. when NRI eventually fires StopPodSandbox for this service)
	// finds and removes the entry.
	if rev := occSlot(t, d, want); rev.namespace != "ns1" || rev.deviceName != "cxi0" {
		t.Errorf("claimToOccupancy[%q] = %+v, want {ns1, cxi0}", want, rev)
	}
	if !notifyFired(d) {
		t.Errorf("synthetic-entry add did not trigger republish; active_namespaces will be stale until next refresh")
	}
}

// A synthetic entry from a prior reconcile must be released when the
// hardware service it tracked disappears (e.g. operator destroyed it via
// cxi_service utility). This is the self-cleaning property that prevents
// synthetic entries from accumulating across reconciles.
func TestApplyReconcile_ReleasesSyntheticEntryWhenHardwareGone(t *testing.T) {
	d := reconcileApplyHarness()

	// First reconcile: hardware has the service, synthetic entry created.
	d.applyReconcile(map[occupancyKey]struct{}{
		{namespace: "ns1", deviceName: "cxi0"}: {},
	})
	if _, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; !ok {
		t.Fatalf("setup: synthetic entry was not created")
	}
	drainNotify(d)

	// Second reconcile: hardware no longer has the service.
	d.applyReconcile(map[occupancyKey]struct{}{})

	if _, ok := d.nsDeviceOccupancy["ns1"]["cxi0"]; ok {
		t.Errorf("synthetic entry still present after hardware disappeared; reconciler is not self-cleaning")
	}
	if _, ok := d.claimToOccupancy["hw:cxi0/ns1"]; ok {
		t.Errorf("synthetic reverse-index entry still present after release")
	}
	if !notifyFired(d) {
		t.Errorf("synthetic release did not trigger republish")
	}
}

// Mixed state: one real claim mid-flight (in pending) on cxi0; one real
// claim with hardware backing on cxi1; one leaked entry on cxi2; one
// hardware-only service on cxi3 for a different namespace. The reconciler
// must keep the first two, release the leak, and add the synthetic entry,
// in a single pass.
func TestApplyReconcile_MixedState(t *testing.T) {
	d := reconcileApplyHarness()

	// cxi0: pending real claim.
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	// cxi1: real claim with hardware backing.
	d.checkAndAddPending("c2", "ns2", "cxi1", fits, hwAvailPlenty)
	d.RemovePending("c2")
	// cxi2: leaked real claim (no longer in pending, no hardware).
	d.checkAndAddPending("c3", "ns3", "cxi2", fits, hwAvailPlenty)
	d.RemovePending("c3")
	drainNotify(d)

	hwSet := map[occupancyKey]struct{}{
		{namespace: "ns2", deviceName: "cxi1"}: {}, // backs c2
		{namespace: "ns4", deviceName: "cxi3"}: {}, // hardware-only
	}
	d.applyReconcile(hwSet)

	// cxi0: c1 still in pending → kept.
	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Errorf("ns1/cxi0 = %q, want %q (pending must be kept)", got, "c1")
	}
	// cxi1: c2 in hardware → kept (no overwrite by synthetic).
	if got := d.nsDeviceOccupancy["ns2"]["cxi1"]; got != "c2" {
		t.Errorf("ns2/cxi1 = %q, want %q (real claim with hw backing kept)", got, "c2")
	}
	// cxi2: c3 leaked → released.
	if _, ok := d.nsDeviceOccupancy["ns3"]; ok {
		t.Errorf("ns3 bucket still present; c3 leak not released")
	}
	// cxi3: hardware-only → synthetic entry added.
	want := types.UID("hw:cxi3/ns4")
	if got := d.nsDeviceOccupancy["ns4"]["cxi3"]; got != want {
		t.Errorf("ns4/cxi3 = %q, want %q (hardware-only synthetic)", got, want)
	}
	if !notifyFired(d) {
		t.Errorf("mixed-state reconcile did not trigger republish; one of release/add should have")
	}
}

// In mock mode the reconciler is skipped: there are no real services, so
// cxi.ReadActiveServiceVNIs returns an empty slice; if the reconciler ran
// it would delete every in-memory slot, which is wrong because in mock
// mode the slots represent the desired state, not the hardware state.
//
// Exercises the top-level reconcileOccupancyFromHardware so the mockDevices
// gate is verified end-to-end.
func TestReconcileOccupancyFromHardware_SkipsInMockMode(t *testing.T) {
	d := reconcileApplyHarness()
	d.mockDevices = cxi.MockDevices(1) // any non-nil value triggers the skip
	d.checkAndAddPending("c1", "ns1", "cxi0", fits, hwAvailPlenty)
	d.RemovePending("c1") // pre-condition: would be released if reconcile ran
	drainNotify(d)

	d.reconcileOccupancyFromHardware(context.Background())

	if got := d.nsDeviceOccupancy["ns1"]["cxi0"]; got != "c1" {
		t.Errorf("ns1/cxi0 = %q, want %q (mock mode must skip reconcile)", got, "c1")
	}
	if notifyFired(d) {
		t.Errorf("mock-mode reconcile triggered republish; should have been a no-op")
	}
}

// listNamespaceVNIs returns only namespaces with a valid integer VNI
// annotation in the uint16 range. Namespaces with no annotation, with
// an unparseable annotation, with a non-positive value, or with a value
// above uint16 max are silently dropped — they cannot back a real CXI
// service, so they cannot contribute to hardware reverse-mapping.
func TestListNamespaceVNIs_FiltersInvalid(t *testing.T) {
	objs := []runtime.Object{
		namespaceWithVNI("good-1", 100),
		namespaceWithVNI("good-2", 65535),
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "no-annotation",
				Annotations: map[string]string{},
			},
		},
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "unparseable",
				Annotations: map[string]string{vniAnnotationKey: "not-a-number"},
			},
		},
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "zero",
				Annotations: map[string]string{vniAnnotationKey: "0"},
			},
		},
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "negative",
				Annotations: map[string]string{vniAnnotationKey: "-5"},
			},
		},
		&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "too-big",
				Annotations: map[string]string{vniAnnotationKey: "65536"},
			},
		},
	}
	d := reconcileApplyHarness()
	d.kubeClient = fake.NewSimpleClientset(objs...)

	got, err := d.listNamespaceVNIs(context.Background())
	if err != nil {
		t.Fatalf("listNamespaceVNIs returned error: %v", err)
	}

	want := map[uint16]string{
		100:   "good-1",
		65535: "good-2",
	}
	if len(got) != len(want) {
		t.Errorf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for vni, ns := range want {
		if got[vni] != ns {
			t.Errorf("vni %d → %q, want %q", vni, got[vni], ns)
		}
	}
}

// readHardwareOccupancy maps each device's live service VNIs to (ns, dev)
// pairs via the namespace map. VNIs without a matching namespace are
// silently dropped (system services, deleted namespaces, services for
// other clusters sharing a node).
//
// Since cxi.ReadActiveServiceVNIs in non-cxi builds always returns nil,
// this test exercises only the empty case end-to-end. The mapping logic is
// covered indirectly by TestApplyReconcile_* which passes the hwSet
// directly. A full integration test would require the cxi build tag and a
// real device.
func TestReadHardwareOccupancy_EmptyInNonCXIBuild(t *testing.T) {
	d := reconcileApplyHarness()
	d.devices = cxi.MockDevices(2)

	hwSet := d.readHardwareOccupancy(map[uint16]string{100: "ns1"})
	if len(hwSet) != 0 {
		t.Errorf("non-cxi build readHardwareOccupancy returned %+v, want empty", hwSet)
	}
}
