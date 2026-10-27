package cxi

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The tests below extend hsn_test.go's fakeSysfs with what Discover reads:
// numa_node/uevent on each PCI function and the physfn/virtfnN links between
// a VF and its PF.

func (f *fakeSysfs) pciDev(bdf string) string {
	f.t.Helper()
	p := f.pciFunc(bdf)
	for name, data := range map[string]string{"numa_node": "3\n", "uevent": "PCI_SLOT_NAME=" + bdf + "\n"} {
		if err := os.WriteFile(filepath.Join(p, name), []byte(data), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return p
}

// sriovLink links VF function vf to PF function pf as virtfn<n>, both ways.
func (f *fakeSysfs) sriovLink(pf, vf string, n int) {
	f.t.Helper()
	if err := os.Symlink(pf, filepath.Join(vf, "physfn")); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(vf, filepath.Join(pf, fmt.Sprintf("virtfn%d", n))); err != nil {
		f.t.Fatal(err)
	}
}

// parentSvc writes the PF's vf/<n>/svc_id file; raw is written verbatim.
func (f *fakeSysfs) parentSvc(pf string, n int, raw string) {
	f.t.Helper()
	dir := filepath.Join(f.root, "class", "cxi", pf, "vf", fmt.Sprint(n))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "svc_id"), []byte(raw), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// discoverer returns a discoverer over the fake tree that records every
// libcxi-backed read (capacity by name, reserved as "reserved:<name>") and
// every log line.
func (f *fakeSysfs) discoverer() (*discoverer, *[]string, *[]string) {
	var opened, logs []string
	x := &discoverer{
		classPath:   filepath.Join(f.root, "class", "cxi_user"),
		netPath:     f.netPath(),
		pfClassPath: filepath.Join(f.root, "class", "cxi"),
		readCapacity: func(name string) (ResourceSet, error) {
			opened = append(opened, name)
			return DeviceCapacity(), nil
		},
		readReserved: func(name string, _ []int) (ResourceSet, error) {
			opened = append(opened, "reserved:"+name)
			return ResourceSet{}, nil
		},
		logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	}
	return x, &opened, &logs
}

// pfOnlyNode is two PFs, cxi0 (hsn3) and cxi1 (hsn1), with no VFs.
func pfOnlyNode(t *testing.T) (*fakeSysfs, string) {
	f := newFakeSysfs(t)
	pf0, pf1 := f.pciDev("0000:c5:00.0"), f.pciDev("0000:cd:00.0")
	f.link("cxi_user", "cxi0", pf0)
	f.link("cxi_user", "cxi1", pf1)
	f.link("net", "hsn3", pf0)
	f.link("net", "hsn1", pf1)
	return f, pf0
}

// sriovNode adds VFs cxi4, cxi5 and cxi10 under cxi0. cxi10 has no netdev, so
// it covers the unpaired warning and numeric (not lexical) index order.
func sriovNode(t *testing.T) *fakeSysfs {
	f, pf0 := pfOnlyNode(t)
	for i, v := range []struct{ cxi, bdf, netdev string }{
		{"cxi4", "0000:c5:00.1", "enp197s0f1"},
		{"cxi5", "0000:c5:00.2", "enp197s0f2"},
		{"cxi10", "0000:c5:00.3", ""},
	} {
		vf := f.pciDev(v.bdf)
		f.sriovLink(pf0, vf, i)
		f.parentSvc("cxi0", i, "4\n")
		f.link("cxi_user", v.cxi, vf)
		if v.netdev != "" {
			f.link("net", v.netdev, vf)
		}
	}
	return f
}

func deviceNames(devs []*Device) []string {
	var out []string
	for _, d := range devs {
		out = append(out, d.Name)
	}
	return out
}

func logged(logs []string, sub string) bool {
	for _, l := range logs {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestDiscoverPFOnlyNode(t *testing.T) {
	t.Run("default publishes the PFs", func(t *testing.T) {
		f, _ := pfOnlyNode(t)
		x, _, _ := f.discoverer()
		devs, err := x.discover(DiscoverOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := deviceNames(devs); !reflect.DeepEqual(got, []string{"cxi0", "cxi1"}) {
			t.Fatalf("devices = %v, want [cxi0 cxi1]", got)
		}
		d := devs[0]
		if d.IsVF() || d.FunctionType != FunctionPF || d.VFIndex != -1 || d.PFName != "" {
			t.Errorf("cxi0 = %+v, want a PF with VFIndex -1 and no PFName", d)
		}
		if d.HSNIface != "hsn3" || d.PCIBusID != "0000:c5:00.0" || d.NUMANode != 3 {
			t.Errorf("cxi0 hsn=%q pci=%q numa=%d, want hsn3 0000:c5:00.0 3", d.HSNIface, d.PCIBusID, d.NUMANode)
		}
		if d.Capacity != DeviceCapacity() {
			t.Errorf("cxi0 capacity = %+v, want the libcxi read %+v", d.Capacity, DeviceCapacity())
		}
	})

	t.Run("vf-only finds no VF", func(t *testing.T) {
		f, _ := pfOnlyNode(t)
		x, opened, logs := f.discoverer()
		devs, err := x.discover(DiscoverOptions{VFOnly: true})
		if err != nil || len(devs) != 0 {
			t.Fatalf("got %v, %v; want an empty list and no error", deviceNames(devs), err)
		}
		if len(*opened) != 0 {
			t.Errorf("libcxi reads %v; a PF must never be opened in --vf-only mode", *opened)
		}
		if !logged(*logs, "found no usable SR-IOV VF") {
			t.Errorf("logs %q: want the no-VF error", *logs)
		}
	})
}

func TestDiscoverSRIOVNodeDefaultSkipsVFs(t *testing.T) {
	x, opened, logs := sriovNode(t).discoverer()
	devs, err := x.discover(DiscoverOptions{SystemSvcIDs: []int{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if got := deviceNames(devs); !reflect.DeepEqual(got, []string{"cxi0", "cxi1"}) {
		t.Fatalf("devices = %v, want only the PFs", got)
	}
	for _, vf := range []string{"cxi4", "cxi5", "cxi10"} {
		if !logged(*logs, "skipping "+vf+": SR-IOV VF") {
			t.Errorf("no skip log for %s in %q", vf, *logs)
		}
	}
	if want := []string{"cxi0", "reserved:cxi0", "cxi1", "reserved:cxi1"}; !reflect.DeepEqual(*opened, want) {
		t.Errorf("libcxi reads = %v, want %v (no VF opened)", *opened, want)
	}
}

func TestDiscoverSRIOVNodeVFOnly(t *testing.T) {
	x, opened, logs := sriovNode(t).discoverer()
	devs, err := x.discover(DiscoverOptions{SystemSvcIDs: []int{1, 2}, VFOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := deviceNames(devs); !reflect.DeepEqual(got, []string{"cxi4", "cxi5", "cxi10"}) {
		t.Fatalf("devices = %v, want [cxi4 cxi5 cxi10] in index order", got)
	}
	for i, want := range []struct{ pci, hsn string }{
		{"0000:c5:00.1", "enp197s0f1"}, {"0000:c5:00.2", "enp197s0f2"}, {"0000:c5:00.3", ""},
	} {
		d := devs[i]
		if !d.IsVF() || d.PFName != "cxi0" || d.VFIndex != i {
			t.Errorf("%s: type=%q pf=%q vf=%d, want vf cxi0 %d", d.Name, d.FunctionType, d.PFName, d.VFIndex, i)
		}
		if d.PCIBusID != want.pci || d.HSNIface != want.hsn {
			t.Errorf("%s: pci=%q hsn=%q, want %q %q", d.Name, d.PCIBusID, d.HSNIface, want.pci, want.hsn)
		}
		if d.Capacity != (ResourceSet{}) || d.Reserved != (ResourceSet{}) || d.ParentSvcID != 4 || d.PodIfName != "" {
			t.Errorf("%s: capacity=%+v reserved=%+v parent=%d podif=%q, want zero capacity/reserved/podif and parent 4",
				d.Name, d.Capacity, d.Reserved, d.ParentSvcID, d.PodIfName)
		}
	}
	if len(*opened) != 0 {
		t.Errorf("libcxi reads = %v; --vf-only discovery must not open any device", *opened)
	}
	for _, pf := range []string{"cxi0", "cxi1"} {
		if !logged(*logs, "skipping "+pf+": physical function in --vf-only mode") {
			t.Errorf("no skip log for PF %s in %q", pf, *logs)
		}
	}
	if !logged(*logs, "device cxi10 has no paired netdev") {
		t.Errorf("logs %q: want the unpaired warning for cxi10", *logs)
	}
}

func TestDiscoverVFWithoutPhysfnTarget(t *testing.T) {
	build := func(t *testing.T) *fakeSysfs {
		f, pf0 := pfOnlyNode(t)
		good := f.pciDev("0000:c5:00.2")
		f.sriovLink(pf0, good, 1)
		f.parentSvc("cxi0", 1, "4\n")
		f.link("cxi_user", "cxi5", good)
		// cxi4's physfn dangles: still a VF, but its PF cannot be resolved.
		bad := f.pciDev("0000:c5:00.1")
		if err := os.Symlink(filepath.Join(f.root, "devices", "missing"), filepath.Join(bad, "physfn")); err != nil {
			t.Fatal(err)
		}
		f.link("cxi_user", "cxi4", bad)
		return f
	}

	t.Run("vf-only skips it", func(t *testing.T) {
		x, opened, logs := build(t).discoverer()
		devs, err := x.discover(DiscoverOptions{VFOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := deviceNames(devs); !reflect.DeepEqual(got, []string{"cxi5"}) {
			t.Fatalf("devices = %v, want [cxi5]", got)
		}
		if !logged(*logs, "skipping VF cxi4: device/physfn does not resolve") {
			t.Errorf("logs %q: want the unresolved-physfn error for cxi4", *logs)
		}
		if len(*opened) != 0 {
			t.Errorf("libcxi reads = %v, want none", *opened)
		}
	})

	t.Run("default still treats it as a VF", func(t *testing.T) {
		x, _, logs := build(t).discoverer()
		devs, err := x.discover(DiscoverOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := deviceNames(devs); !reflect.DeepEqual(got, []string{"cxi0", "cxi1"}) {
			t.Fatalf("devices = %v, want [cxi0 cxi1]", got)
		}
		if !logged(*logs, "skipping cxi4: SR-IOV VF") {
			t.Errorf("logs %q: want cxi4 skipped as a VF", *logs)
		}
	})
}

func TestDiscoverNoCXIClassDir(t *testing.T) {
	for _, vfOnly := range []bool{false, true} {
		x, _, logs := newFakeSysfs(t).discoverer()
		x.classPath = filepath.Join(t.TempDir(), "absent")
		devs, err := x.discover(DiscoverOptions{VFOnly: vfOnly})
		if err != nil || devs != nil {
			t.Fatalf("vfOnly=%v: got %v, %v; want nil, nil", vfOnly, devs, err)
		}
		if got := logged(*logs, "--vf-only"); got != vfOnly {
			t.Errorf("vfOnly=%v: --vf-only error logged = %v", vfOnly, got)
		}
	}
}
