//go:build !cxi

package cxi

import (
	"errors"
	"testing"
)

// Fixture numbers follow the SHS 15.0.0 measurements in the SR-IOV epic: the
// per-VF Ethernet system child holds 660 LEs, 8 PTEs and one each of
// TXQ/TGQ/EQ/AC; pod children inherit les=0/tles=0.
var (
	testEthChild = ServiceReservation{
		SvcID: 1, IsSystem: true, ResourceLimits: true,
		Res: ResourceSet{TXQs: 1, TGQs: 1, EQs: 1, PTEs: 8, LEs: 660, ACs: 1},
	}
	testParentRes = ResourceSet{
		TXQs: 64, TGQs: 32, EQs: 128, CTs: 100, TLEs: 0, PTEs: 200, LEs: 2048, ACs: 40,
	}
)

func podChild(svcID int, res ResourceSet) ServiceReservation {
	return ServiceReservation{SvcID: svcID, ResourceLimits: true, Res: res}
}

func installTopology(t *testing.T, top MockTopology) {
	t.Helper()
	SetMockTopology(top)
	t.Cleanup(ResetMockTopology)
}

func testParent(svcID int, limits ResourceSet) MockParent {
	return MockParent{
		PFName: "cxi0", SvcID: svcID,
		Descriptor: ParentDescriptor{
			ResourceLimits: true,
			Limits:         limits,
			VNIs:           NewVNIList([]uint16{40, 41, 50}),
		},
	}
}

func TestReadParentBudget(t *testing.T) {
	installTopology(t, MockTopology{Parents: []MockParent{testParent(3, testParentRes)}})

	budget, vnis, err := ReadParentBudget("cxi0", 3)
	if err != nil {
		t.Fatal(err)
	}
	if budget != testParentRes {
		t.Errorf("budget = %v, want %v", budget, testParentRes)
	}
	if got := vnis.String(); got != ",40,41,50," {
		t.Errorf("vnis = %q", got)
	}
}

func TestReadParentBudgetRange(t *testing.T) {
	p := testParent(3, testParentRes)
	p.Descriptor.VNIs = mustRange(t, 100, 199)
	installTopology(t, MockTopology{Parents: []MockParent{p}})

	_, vnis, err := ReadParentBudget("cxi0", 3)
	if err != nil {
		t.Fatal(err)
	}
	if lo, hi, ok := vnis.Range(); !ok || lo != 100 || hi != 199 {
		t.Errorf("Range() = %d, %d, %v", lo, hi, ok)
	}
}

func TestReadParentBudgetUnlimited(t *testing.T) {
	p := testParent(4, testParentRes)
	p.Descriptor.ResourceLimits = false
	installTopology(t, MockTopology{Parents: []MockParent{p}})

	_, _, err := ReadParentBudget("cxi0", 4)
	if !errors.Is(err, ErrParentUnlimited) {
		t.Fatalf("err = %v, want ErrParentUnlimited", err)
	}

	// The same rule applies to the remaining-budget read.
	if _, err := ReadParentRemaining("cxi0", 4, []string{"cxi4"}); !errors.Is(err, ErrParentUnlimited) {
		t.Fatalf("ReadParentRemaining err = %v, want ErrParentUnlimited", err)
	}
}

func TestReadParentBudgetMissing(t *testing.T) {
	installTopology(t, MockTopology{Parents: []MockParent{testParent(3, testParentRes)}})

	for _, tc := range []struct {
		pf  string
		svc int
	}{{"cxi0", 9}, {"cxi1", 3}} {
		_, _, err := ReadParentBudget(tc.pf, tc.svc)
		if err == nil {
			t.Errorf("ReadParentBudget(%s, %d): expected error", tc.pf, tc.svc)
		}
		if errors.Is(err, ErrParentUnlimited) {
			t.Errorf("ReadParentBudget(%s, %d): missing parent must not look unlimited", tc.pf, tc.svc)
		}
	}
}

func TestReadParentRemaining(t *testing.T) {
	// The svc_id 3 parent on cxi0 owns two VFs. svc_id 3 on cxi1 is a different
	// parent with the same id; its VF must not leak into the sum.
	otherPF := testParent(3, ResourceSet{TXQs: 999})
	otherPF.PFName = "cxi1"

	podRes := ResourceSet{TXQs: 4, TGQs: 2, EQs: 8, CTs: 10, PTEs: 16, ACs: 2}
	installTopology(t, MockTopology{
		Parents: []MockParent{testParent(3, testParentRes), otherPF},
		VFServices: map[string][]ServiceReservation{
			"cxi4": {testEthChild, podChild(2, podRes)},
			"cxi5": {testEthChild},
			"cxi6": {testEthChild, podChild(2, podRes)}, // belongs to the cxi1 parent
		},
	})

	got, err := ReadParentRemaining("cxi0", 3, []string{"cxi4", "cxi5"})
	if err != nil {
		t.Fatal(err)
	}
	want := testParentRes.Sub(testEthChild.Res).Sub(testEthChild.Res).Sub(podRes)
	if got != want {
		t.Errorf("remaining = %v, want %v", got, want)
	}
	// Both Ethernet children charge LEs; the pod child (les=0) does not.
	if got.LEs != testParentRes.LEs-2*660 {
		t.Errorf("LEs = %d, want %d", got.LEs, testParentRes.LEs-2*660)
	}
	if got.TLEs != testParentRes.TLEs {
		t.Errorf("TLEs = %d, want untouched %d", got.TLEs, testParentRes.TLEs)
	}
}

func TestReadParentRemainingNoVFs(t *testing.T) {
	installTopology(t, MockTopology{Parents: []MockParent{testParent(3, testParentRes)}})

	got, err := ReadParentRemaining("cxi0", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != testParentRes {
		t.Errorf("remaining = %v, want full budget %v", got, testParentRes)
	}
}

func TestReadParentRemainingSkipsUnlimitedChildren(t *testing.T) {
	// A service without resource_limits reserves nothing, as in the PF sums.
	unlimited := ServiceReservation{SvcID: 7, ResourceLimits: false, Res: ResourceSet{TXQs: 50}}
	installTopology(t, MockTopology{
		Parents:    []MockParent{testParent(3, testParentRes)},
		VFServices: map[string][]ServiceReservation{"cxi4": {unlimited}},
	})

	got, err := ReadParentRemaining("cxi0", 3, []string{"cxi4"})
	if err != nil {
		t.Fatal(err)
	}
	if got != testParentRes {
		t.Errorf("remaining = %v, want %v", got, testParentRes)
	}
}

func TestReadParentRemainingOverCommitIsNegative(t *testing.T) {
	installTopology(t, MockTopology{
		Parents:    []MockParent{testParent(3, ResourceSet{TXQs: 1, LEs: 100})},
		VFServices: map[string][]ServiceReservation{"cxi4": {testEthChild}},
	})

	got, err := ReadParentRemaining("cxi0", 3, []string{"cxi4"})
	if err != nil {
		t.Fatal(err)
	}
	if got.LEs != 100-660 {
		t.Errorf("LEs = %d, want %d (callers clamp)", got.LEs, 100-660)
	}
}

func TestReadVFSystemReservation(t *testing.T) {
	podRes := ResourceSet{TXQs: 4, PTEs: 16}
	rhChild := ServiceReservation{
		SvcID: 3, IsSystem: true, ResourceLimits: true,
		Res: ResourceSet{TXQs: 2, EQs: 3},
	}
	installTopology(t, MockTopology{VFServices: map[string][]ServiceReservation{
		"cxi4": {testEthChild, podChild(2, podRes)},
		"cxi5": {testEthChild, rhChild, podChild(2, podRes)},
		"cxi6": {podChild(2, podRes)},
	}})

	tests := []struct {
		vf   string
		want ResourceSet
	}{
		{"cxi4", testEthChild.Res},
		{"cxi5", testEthChild.Res.Add(rhChild.Res)}, // every system service counts
		{"cxi6", ResourceSet{}},                     // netdev down: no system child yet
		{"cxi7", ResourceSet{}},                     // unknown VF: empty list
	}
	for _, tt := range tests {
		t.Run(tt.vf, func(t *testing.T) {
			got, err := ReadVFSystemReservation(tt.vf)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("reservation = %v, want %v", got, tt.want)
			}
		})
	}
}
