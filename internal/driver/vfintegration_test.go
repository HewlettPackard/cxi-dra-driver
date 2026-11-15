package driver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/HewlettPackard/cxi-dra-driver/internal/cxi"
)

// setParentVNIs replaces the test parent's VNI set.
func setParentVNIs(t *testing.T, d *Driver, vnis cxi.VNISet) {
	t.Helper()
	p, ok := d.parents.get(vfTestParent)
	if !ok {
		t.Fatalf("parent %s not set up", vfTestParent)
	}
	p.vnis = vnis
}

func mustVNIRange(t *testing.T, lo, hi uint16) cxi.VNISet {
	t.Helper()
	s, err := cxi.NewVNIRange(lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// legacyConfig is opaque CXI config with the given LE/TLE pool counts.
func legacyConfig(lePools, tlePools int) []resourceapi.DeviceClaimConfiguration {
	raw := fmt.Sprintf(`{"txqs":2,"tgqs":1,"eqs":1,"cts":1,"ptes":1,"acs":1,"le_pools":%d,"tle_pools":%d}`, lePools, tlePools)
	return []resourceapi.DeviceClaimConfiguration{{
		Requests: []string{"cxi"},
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{Driver: testDriver, Parameters: runtime.RawExtension{Raw: []byte(raw)}},
		},
	}}
}

func wantErrContains(t *testing.T, err error, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", subs)
	}
	for _, s := range subs {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not contain %q", err, s)
		}
	}
}

func TestVFIntegration_RejectsConsumedLEsAndTLEs(t *testing.T) {
	for _, rs := range []cxi.ResourceSet{
		{TXQs: 2, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1, LEs: cxi.LEPoolSize},
		{TXQs: 2, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1, TLEs: cxi.TLEPoolSize},
	} {
		d := newVFTestDriver(t, 2)
		claim := vfClaim("c1", "p1")
		claim.Status.Allocation.Devices.Results[0].ConsumedCapacity = consumedOf(rs)
		res := d.prepareClaim(context.Background(), claim)
		wantErrContains(t, res.Err, "consumed capacity les/tles", "inherited from parent service cxi0/4 and must be 0")
		if d.store.Get("p1") != nil {
			t.Error("rejected claim left a store entry")
		}
	}
}

func TestVFIntegration_RejectsLegacyLEAndTLEPools(t *testing.T) {
	for _, pools := range [][2]int{{1, 0}, {0, 1}} {
		d := newVFTestDriver(t, 2)
		claim := vfClaim("c1", "p1")
		claim.Spec.Devices.Config = legacyConfig(pools[0], pools[1])
		res := d.prepareClaim(context.Background(), claim)
		wantErrContains(t, res.Err, "opaque config le_pools/tle_pools", "inherited from parent service cxi0/4 and must be 0")
	}
}

// Legacy config with zero pools next to consumed capacity is still accepted.
func TestVFIntegration_ZeroPoolsAccepted(t *testing.T) {
	d := newVFTestDriver(t, 2)
	claim := vfClaim("c1", "p1")
	claim.Status.Allocation.Devices.Results[0].ConsumedCapacity = consumedOf(cxi.ResourceSet{TXQs: 2, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1})
	if res := d.prepareClaim(context.Background(), claim); res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
}

// PF mode keeps accepting LE/TLE pools.
func TestVFIntegration_PFModeAcceptsLEPools(t *testing.T) {
	d := newTestDriver()
	claim := makeClaim("c1", testNamespace, "c1", "cxi0")
	claim.Spec.Devices.Config = legacyConfig(1, 1)
	rs, err := d.resolveClaim(claim, claim.Status.Allocation.Devices.Results[0])
	if err != nil {
		t.Fatalf("resolveClaim: %v", err)
	}
	if rs.LEs != cxi.LEPoolSize || rs.TLEs != cxi.TLEPoolSize {
		t.Errorf("resolved %s, want one LE and one TLE pool", rs)
	}
}

func TestVFIntegration_PrepareRejectsVNIOutsideParent(t *testing.T) {
	d := newVFTestDriver(t, 2)
	setParentVNIs(t, d, mustVNIRange(t, 100, 200))
	res := d.prepareClaim(context.Background(), vfClaim("c1", "p1"))
	wantErrContains(t, res.Err, "namespace VNI 42 is not in the VNI set {100-200} of parent service cxi0/4")
	if d.store.Get("p1") != nil || d.store.ClaimAllocs("c1") != nil {
		t.Error("rejected claim left store entries")
	}
}

func TestVFIntegration_PrepareAcceptsVNIInListSet(t *testing.T) {
	d := newVFTestDriver(t, 2)
	setParentVNIs(t, d, cxi.NewVNIList([]uint16{40, 42}))
	if res := d.prepareClaim(context.Background(), vfClaim("c1", "p1")); res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
}

func newVFBinder(t *testing.T, objs ...runtime.Object) (*ClaimBinder, *Driver, *fake.Clientset) {
	t.Helper()
	d := newVFTestDriver(t, 2, objs...)
	client := d.kubeClient.(*fake.Clientset)
	return NewClaimBinder(d, client, testDriver, vfTestPool), d, client
}

func TestVFIntegration_ClaimBinderVNINotInParentSet(t *testing.T) {
	claim := vfClaim("c1", "p1")
	b, d, client := newVFBinder(t, claim)
	setParentVNIs(t, d, mustVNIRange(t, 100, 200))
	b.evaluateClaim(context.Background(), claim)

	cond := conditionFor(t, client, claim, vfTestParent, "Unschedulable")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "VNINotInParentSet" {
		t.Fatalf("Unschedulable = %+v, want True/VNINotInParentSet", cond)
	}
	if !strings.Contains(cond.Message, "namespace VNI 42 is not in the VNI set {100-200}") {
		t.Errorf("message %q does not name the VNI and the set", cond.Message)
	}
	if c := conditionFor(t, client, claim, vfTestParent, "Ready"); c != nil {
		t.Errorf("Ready = %+v, want unset", c)
	}
	if d.hasPending("c1", vfTestParent, "c1-s0") {
		t.Error("pending grant recorded for a rejected claim")
	}
}

func TestVFIntegration_ClaimBinderCoveredVNIGetsReady(t *testing.T) {
	claim := vfClaim("c1", "p1")
	b, _, client := newVFBinder(t, claim)
	b.evaluateClaim(context.Background(), claim)
	if cond := conditionFor(t, client, claim, vfTestParent, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True", cond)
	}
}

// An unannotated namespace is left to Prepare, which waits for the annotation.
func TestVFIntegration_ClaimBinderSkipsUnannotatedNamespace(t *testing.T) {
	claim := vfClaim("c1", "p1")
	claim.Namespace = "fresh"
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "fresh"}}
	b, d, client := newVFBinder(t, claim, ns)
	setParentVNIs(t, d, mustVNIRange(t, 100, 200))
	b.evaluateClaim(context.Background(), claim)
	if cond := conditionFor(t, client, claim, vfTestParent, "Unschedulable"); cond != nil {
		t.Fatalf("Unschedulable = %+v, want unset before the namespace is annotated", cond)
	}
	if cond := conditionFor(t, client, claim, vfTestParent, "Ready"); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready = %+v, want True", cond)
	}
}

// setupLogs reruns setupParents with the given parent budget and VNI range and
// returns the log lines.
func setupLogs(t *testing.T, budgetLEs int64, vnis cxi.VNISet, rangeStart, rangeEnd int) string {
	t.Helper()
	d := newVFTestDriver(t, 4)
	budget := vfTestBudget
	budget.LEs = budgetLEs
	vfSvcs := make(map[string][]cxi.ServiceReservation)
	for _, vf := range d.devices {
		vfSvcs[vf.Name] = []cxi.ServiceReservation{{IsSystem: true, ResourceLimits: true, Res: vfTestEth}}
	}
	cxi.SetMockTopology(cxi.MockTopology{
		Parents:    []cxi.MockParent{{PFName: "cxi0", SvcID: 4, Descriptor: cxi.ParentDescriptor{ResourceLimits: true, Limits: budget, VNIs: vnis}}},
		VFServices: vfSvcs,
	})
	var logs []string
	d.logFn = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	d.vniRangeStart, d.vniRangeEnd = rangeStart, rangeEnd
	d.setupParents()
	return strings.Join(logs, "\n")
}

func TestVFIntegration_StartupLogsParent(t *testing.T) {
	logs := setupLogs(t, 8192, vfTestVNIs, 40, 50)
	for _, want := range []string{
		"parent cxi0-svc4: VFs [cxi1 cxi2 cxi3 cxi4]",
		"budget (" + cxi.ResourceSet{TXQs: 100, TGQs: 100, EQs: 100, CTs: 100, PTEs: 100, ACs: 100, LEs: 8192, TLEs: 10}.String() + ")",
		"Ethernet-child reservation (" + vfTestEth.String() + ") per VF",
		"published ceiling (" + cxi.ResourceSet{TXQs: 96, TGQs: 96, EQs: 92, CTs: 96, PTEs: 96, ACs: 96}.String() + ")",
		"VNI set {32-63}",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("startup log missing %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "warning") {
		t.Errorf("unexpected warning for a covering parent with enough LEs:\n%s", logs)
	}
}

func TestVFIntegration_StartupWarnsVNIRangeNotCovered(t *testing.T) {
	logs := setupLogs(t, 8192, vfTestVNIs, 40, 100)
	if !strings.Contains(logs, "warning: parent cxi0-svc4 VNI set {32-63} does not cover --vni-range-start..--vni-range-end 40-100") {
		t.Errorf("no coverage warning:\n%s", logs)
	}
}

// Without a cluster VNI range there is nothing to compare against.
func TestVFIntegration_StartupNoRangeNoCoverageWarning(t *testing.T) {
	if logs := setupLogs(t, 8192, vfTestVNIs, 0, 0); strings.Contains(logs, "does not cover") {
		t.Errorf("coverage warning without a range:\n%s", logs)
	}
}

func TestVFIntegration_StartupWarnsLowLEs(t *testing.T) {
	// 4 Ethernet children of 16 LEs leave 1024 - 64 = 960, below one pool.
	logs := setupLogs(t, cxi.LEPoolSize, vfTestVNIs, 0, 0)
	if !strings.Contains(logs, fmt.Sprintf("warning: parent cxi0-svc4 has 960 LEs left after its VFs' Ethernet children, less than one pool (%d)", cxi.LEPoolSize)) {
		t.Errorf("no low-LE warning:\n%s", logs)
	}
	if logs := setupLogs(t, cxi.LEPoolSize+64, vfTestVNIs, 0, 0); strings.Contains(logs, "LEs left") {
		t.Errorf("low-LE warning with exactly one pool left:\n%s", logs)
	}
}

// PF mode publishes no parents and logs none of the VF startup lines.
func TestVFIntegration_PFModeNoParents(t *testing.T) {
	d := newTestDriver()
	var logs []string
	d.logFn = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	d.vniRangeStart, d.vniRangeEnd = 1, 2
	d.setupParents()
	if len(d.parents.list()) != 0 || len(logs) != 0 {
		t.Errorf("PF mode: parents %d, logs %q", len(d.parents.list()), logs)
	}
}
