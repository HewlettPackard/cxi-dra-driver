package cxi

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// addVF adds a VF cxi<idx> at bdf under PF function pf as virtfn<n>, with its
// svc_id file written as raw (skipped when raw is "-").
func addVF(f *fakeSysfs, pfName, pf string, n int, cxiName, bdf, raw string) {
	vf := f.pciDev(bdf)
	f.sriovLink(pf, vf, n)
	f.link("cxi_user", cxiName, vf)
	if raw != "-" {
		f.parentSvc(pfName, n, raw)
	}
}

func vfOnly(t *testing.T, f *fakeSysfs) ([]*Device, []string) {
	t.Helper()
	x, _, logs := f.discoverer()
	devs, err := x.discover(DiscoverOptions{VFOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return devs, *logs
}

func TestDiscoverParentSvcID(t *testing.T) {
	f, pf0 := pfOnlyNode(t)
	addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "7\n")  // assigned
	addVF(f, "cxi0", pf0, 1, "cxi5", "0000:c5:00.2", "0\n")  // unassigned
	addVF(f, "cxi0", pf0, 2, "cxi6", "0000:c5:00.3", "abc")  // malformed
	addVF(f, "cxi0", pf0, 3, "cxi7", "0000:c5:00.4", "-")    // svc_id file missing
	addVF(f, "cxi0", pf0, 4, "cxi8", "0000:c5:00.5", "-3\n") // negative

	devs, logs := vfOnly(t, f)
	if len(devs) != 1 || devs[0].Name != "cxi4" || devs[0].ParentSvcID != 7 {
		t.Fatalf("devices = %v, want only cxi4 with parent 7", deviceNames(devs))
	}
	for _, name := range []string{"cxi5", "cxi6", "cxi7", "cxi8"} {
		if !logged(logs, "ERROR: skipping VF "+name+": PF cxi0 vf") || !logged(logs, "parent service not assigned") {
			t.Errorf("no unassigned error for %s in %q", name, logs)
		}
	}
	if !logged(logs, "malformed svc_id \"abc\"") {
		t.Errorf("logs %q: want the malformed value quoted", logs)
	}
}

func TestDiscoverMissingVirtfnLink(t *testing.T) {
	f, pf0 := pfOnlyNode(t)
	vf := f.pciDev("0000:c5:00.1")
	if err := os.Symlink(pf0, filepath.Join(vf, "physfn")); err != nil {
		t.Fatal(err)
	}
	f.link("cxi_user", "cxi4", vf)
	f.parentSvc("cxi0", 0, "4\n")

	devs, logs := vfOnly(t, f)
	if len(devs) != 0 {
		t.Fatalf("devices = %v, want none", deviceNames(devs))
	}
	if !logged(logs, "ERROR: skipping VF cxi4: PF cxi0: no virtfn link resolves to 0000:c5:00.1") {
		t.Errorf("logs %q: want the missing virtfn error", logs)
	}
}

// groupsOf summarizes parents as "<name>=<vf names>".
func groupsOf(ps []*Parent) []string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s=%v", p.Name(), deviceNames(p.VFs)))
	}
	return out
}

func TestParentsGrouping(t *testing.T) {
	t.Run("two VFs share one parent", func(t *testing.T) {
		f, pf0 := pfOnlyNode(t)
		addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "4")
		addVF(f, "cxi0", pf0, 1, "cxi5", "0000:c5:00.2", "4")
		devs, _ := vfOnly(t, f)
		if got, want := groupsOf(Parents(devs)), []string{"cxi0-svc4=[cxi4 cxi5]"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("parents = %v, want %v", got, want)
		}
	})

	t.Run("two parents on one PF", func(t *testing.T) {
		f, pf0 := pfOnlyNode(t)
		addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "5")
		addVF(f, "cxi0", pf0, 1, "cxi5", "0000:c5:00.2", "4")
		devs, _ := vfOnly(t, f)
		if got, want := groupsOf(Parents(devs)), []string{"cxi0-svc4=[cxi5]", "cxi0-svc5=[cxi4]"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("parents = %v, want %v (ordered by svc_id)", got, want)
		}
	})

	t.Run("same svc_id on two PFs is two parents", func(t *testing.T) {
		f := newFakeSysfs(t)
		pf0, pf1 := f.pciDev("0000:c5:00.0"), f.pciDev("0000:cd:00.0")
		f.link("cxi_user", "cxi0", pf0)
		f.link("cxi_user", "cxi1", pf1)
		addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "4")
		addVF(f, "cxi1", pf1, 0, "cxi5", "0000:cd:00.1", "4")
		devs, _ := vfOnly(t, f)
		if got, want := groupsOf(Parents(devs)), []string{"cxi0-svc4=[cxi4]", "cxi1-svc4=[cxi5]"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("parents = %v, want %v", got, want)
		}
	})

	t.Run("stable order by PF index then svc_id, VFs by index", func(t *testing.T) {
		mk := func(pf string, svc, index int, name string) *Device {
			return &Device{Name: name, Index: index, FunctionType: FunctionVF, PFName: pf, ParentSvcID: svc}
		}
		in := []*Device{
			mk("cxi10", 2, 31, "cxi31"), mk("cxi2", 9, 20, "cxi20"), mk("cxi2", 3, 21, "cxi21"),
			mk("cxi2", 9, 22, "cxi22"), mk("cxi10", 2, 30, "cxi30"),
			{Name: "cxi0", FunctionType: FunctionPF},
			{Name: "cxi99", FunctionType: FunctionVF, PFName: "cxi2"},
		}
		want := []string{"cxi2-svc3=[cxi21]", "cxi2-svc9=[cxi20 cxi22]", "cxi10-svc2=[cxi30 cxi31]"}
		for range 3 {
			if got := groupsOf(Parents(in)); !reflect.DeepEqual(got, want) {
				t.Fatalf("parents = %v, want %v", got, want)
			}
		}
	})
}

func TestDiscoverWarnsOnPFWithoutVFs(t *testing.T) {
	f, pf0 := pfOnlyNode(t)
	addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "4")
	devs, logs := vfOnly(t, f)
	if len(devs) != 1 {
		t.Fatalf("devices = %v, want [cxi4]", deviceNames(devs))
	}
	if !logged(logs, "WARNING: --vf-only: PF cxi1 has no SR-IOV VFs") || logged(logs, "PF cxi0 has no SR-IOV VFs") {
		t.Errorf("logs %q: want a warning for cxi1 only", logs)
	}
}

func TestDiscoverPFTopologyOnParents(t *testing.T) {
	f := newFakeSysfs(t)
	pf0, pf1 := f.pciDev("0000:c5:00.0"), f.pciDev("0000:cd:00.0")
	// cxi1 has a different NUMA node and no netdev.
	if err := os.WriteFile(filepath.Join(pf1, "numa_node"), []byte("-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.link("cxi_user", "cxi0", pf0)
	f.link("cxi_user", "cxi1", pf1)
	f.link("net", "hsn3", pf0)
	addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "4")
	addVF(f, "cxi0", pf0, 1, "cxi5", "0000:c5:00.2", "5")
	addVF(f, "cxi1", pf1, 0, "cxi6", "0000:cd:00.1", "4")

	devs, logs := vfOnly(t, f)
	type topo struct {
		pci   string
		numa  int
		netdv string
	}
	got := map[string]topo{}
	for _, p := range Parents(devs) {
		got[p.Name()] = topo{p.PFPCIBusID, p.PFNUMANode, p.PFNetdev}
	}
	want := map[string]topo{
		"cxi0-svc4": {"0000:c5:00.0", 3, "hsn3"},
		"cxi0-svc5": {"0000:c5:00.0", 3, "hsn3"},
		"cxi1-svc4": {"0000:cd:00.0", -1, ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parent PF topology = %v, want %v", got, want)
	}
	if !logged(logs, "WARNING: PF cxi1 has no netdev") || logged(logs, "WARNING: PF cxi0 has no netdev") {
		t.Errorf("logs %q: want a netdev warning for cxi1 only", logs)
	}
}
