package cxi

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestParseMockVFs(t *testing.T) {
	for _, tc := range []struct {
		in         string
		parents, n int
	}{{"2x4", 2, 4}, {" 1X1 ", 1, 1}, {"16x4", 16, 4}, {"1x64", 1, 64}} {
		p, n, err := ParseMockVFs(tc.in)
		if err != nil || p != tc.parents || n != tc.n {
			t.Errorf("ParseMockVFs(%q) = %d, %d, %v; want %d, %d", tc.in, p, n, err, tc.parents, tc.n)
		}
	}
	for _, in := range []string{"", "2", "x4", "2x", "axb", "0x4", "2x0", "-1x4", "17x1", "2x33", "2x4x1"} {
		if _, _, err := ParseMockVFs(in); err == nil {
			t.Errorf("ParseMockVFs(%q): want error", in)
		}
	}
}

func TestMockVFTopology_Layout(t *testing.T) {
	vfs, topo := MockVFTopology(2, 4)
	if len(vfs) != 8 || len(topo.Parents) != 2 {
		t.Fatalf("got %d VFs and %d parents, want 8 and 2", len(vfs), len(topo.Parents))
	}
	buses := map[string]bool{}
	for i, vf := range vfs {
		wantSvc := 4 + i/4
		if !vf.IsVF() || vf.Name != "cxi"+itoa(i+1) || vf.PFName != "cxi0" || vf.ParentSvcID != wantSvc ||
			vf.HostDevPath != "/dev/null" || vf.Capacity != (ResourceSet{}) {
			t.Errorf("VF %d = %+v, want VF cxi%d of cxi0 under svc %d with zero capacity", i, vf, i+1, wantSvc)
		}
		if buses[vf.PCIBusID] {
			t.Errorf("duplicate PCI address %s", vf.PCIBusID)
		}
		buses[vf.PCIBusID] = true
		if svcs := topo.VFServices[vf.Name]; len(svcs) != 1 || !svcs[0].IsSystem || svcs[0].Res != MockEthernetChild {
			t.Errorf("%s services = %+v, want one Ethernet system child", vf.Name, svcs)
		}
	}
	parents := Parents(vfs)
	if len(parents) != 2 {
		t.Fatalf("Parents() = %d parents, want 2", len(parents))
	}
	for k, p := range parents {
		if p.Name() != "cxi0-svc"+itoa(4+k) || len(p.VFs) != 4 || p.PFPCIBusID != "0000:81:00.0" || p.PFNetdev != "hsn0" {
			t.Errorf("parent %d = %s with %d VFs, PF %s/%s", k, p.Name(), len(p.VFs), p.PFPCIBusID, p.PFNetdev)
		}
	}
}

// vnisOf lists every VNI in a set (the mock sets are small).
func vnisOf(s VNISet) []uint16 {
	if lo, hi, ok := s.Range(); ok {
		var out []uint16
		for v := int(lo); v <= int(hi); v++ {
			out = append(out, uint16(v))
		}
		return out
	}
	return s.List()
}

func TestMockParentVNIs_DisjointRangeAndListForms(t *testing.T) {
	if lo, hi, ok := MockParentVNIs(0).Range(); !ok || lo != 100 || hi != 115 {
		t.Errorf("parent 0 = %s, want range 100-115", MockParentVNIs(0))
	}
	if got := MockParentVNIs(1).List(); !slices.Equal(got, []uint16{200, 202, 204}) {
		t.Errorf("parent 1 = %s, want list {200,202,204}", MockParentVNIs(1))
	}
	for a := range maxMockParents {
		for b := a + 1; b < maxMockParents; b++ {
			for _, v := range vnisOf(MockParentVNIs(a)) {
				if MockParentVNIs(b).Contains(v) {
					t.Fatalf("VNI %d in parents %d and %d", v, a, b)
				}
			}
		}
	}
	for _, v := range []uint16{99, 116, 150, 201, 203} {
		if MockParentVNIs(0).Contains(v) || MockParentVNIs(1).Contains(v) {
			t.Errorf("VNI %d should belong to no parent", v)
		}
	}
}

func TestMockVFTopology_ParentReads(t *testing.T) {
	vfs, topo := MockVFTopology(2, 4)
	SetMockTopology(topo)
	t.Cleanup(ResetMockTopology)

	budget, vnis, err := ReadParentBudget("cxi0", 5)
	if err != nil || budget != MockParentBudget || vnis.String() != ",200,202,204," {
		t.Fatalf("ReadParentBudget = %s, %s, %v", budget, vnis, err)
	}
	if r, err := ReadVFSystemReservation("cxi8"); err != nil || r != MockEthernetChild {
		t.Errorf("ReadVFSystemReservation(cxi8) = %s, %v", r, err)
	}
	names := []string{vfs[0].Name, vfs[1].Name, vfs[2].Name, vfs[3].Name}
	want := MockParentBudget
	for range names {
		want = want.Sub(MockEthernetChild)
	}
	if got, err := ReadParentRemaining("cxi0", 4, names); err != nil || got != want {
		t.Errorf("ReadParentRemaining = %s, %v; want %s", got, err, want)
	}
}

// Children on sibling VFs share their parent's budget, and a child must use a
// VNI in its parent's set.
func TestMockVFTopology_ServiceManagerSharesParent(t *testing.T) {
	vfs, topo := MockVFTopology(2, 4)
	SetMockTopology(topo)
	t.Cleanup(ResetMockTopology)
	parents := Parents(vfs)
	a, b := ServiceOptions{Parent: parents[0]}, ServiceOptions{Parent: parents[1]}
	m := MockServiceManager{}
	half := ResourceSet{TXQs: (MockParentBudget.TXQs - 4*MockEthernetChild.TXQs) / 2}

	s1, err := m.CreateService("cxi1", 1, 100, half, a)
	if err != nil {
		t.Fatalf("first child: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteService("cxi1", s1) })
	s2, err := m.CreateService("cxi2", 1, 115, half, a)
	if err != nil {
		t.Fatalf("second child on a sibling VF: %v", err)
	}
	if _, err := m.CreateService("cxi3", 1, 100, ResourceSet{TXQs: 1}, a); err == nil || !strings.Contains(err.Error(), "ENOSPC") {
		t.Fatalf("third child past the shared budget: err = %v, want ENOSPC", err)
	}
	if err := m.DeleteService("cxi2", s2); err != nil {
		t.Fatal(err)
	}
	s3, err := m.CreateService("cxi3", 1, 100, ResourceSet{TXQs: 1}, a)
	if err != nil {
		t.Fatalf("child after a sibling was deleted: %v", err)
	}
	t.Cleanup(func() { _ = m.DeleteService("cxi3", s3) })

	for _, tc := range []struct {
		vf   string
		vni  int
		opts ServiceOptions
		ok   bool
	}{
		{"cxi5", 202, b, true}, {"cxi5", 201, b, false}, {"cxi5", 100, b, false}, {"cxi4", 200, a, false}, {"cxi5", 100, a, false},
	} {
		id, err := m.CreateService(tc.vf, 1, tc.vni, ResourceSet{TXQs: 1}, tc.opts)
		if (err == nil) != tc.ok {
			t.Errorf("%s VNI %d under %s: err = %v, want ok=%v", tc.vf, tc.vni, tc.opts.Parent.Name(), err, tc.ok)
		}
		if err == nil {
			_ = m.DeleteService(tc.vf, id)
		}
	}
}

// --mock-devices keeps its PF-only layout.
func TestMockDevices_PFsUnchanged(t *testing.T) {
	devs := MockDevices(2)
	for i, d := range devs {
		if d.IsVF() || d.Name != "cxi"+itoa(i) || d.Capacity != DeviceCapacity() || d.ParentSvcID != 0 {
			t.Errorf("device %d = %+v, want PF cxi%d with full capacity", i, d, i)
		}
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
