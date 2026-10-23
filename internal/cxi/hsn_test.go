package cxi

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds a temp sysfs with PCI functions under devices/, plus
// class/cxi_user and class/net entries whose "device" links point at them.
type fakeSysfs struct {
	t    *testing.T
	root string
}

func newFakeSysfs(t *testing.T) *fakeSysfs {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"devices", "class/cxi_user", "class/net"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &fakeSysfs{t: t, root: root}
}

func (f *fakeSysfs) pciFunc(bdf string) string {
	f.t.Helper()
	p := filepath.Join(f.root, "devices", "pci0000:c0", bdf)
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fakeSysfs) link(class, name, pci string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, "class", class, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Symlink(pci, filepath.Join(dir, "device")); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *fakeSysfs) netPath() string { return filepath.Join(f.root, "class", "net") }

func TestPairDevice(t *testing.T) {
	t.Run("PF paired with hsn netdev", func(t *testing.T) {
		fs := newFakeSysfs(t)
		pf := fs.pciFunc("0000:c5:00.0")
		dev := &Device{Name: "cxi0", SysfsPath: fs.link("cxi_user", "cxi0", pf)}
		fs.link("net", "hsn3", pf)

		if err := pairDevice(dev, fs.netPath()); err != nil {
			t.Fatal(err)
		}
		if dev.HSNIface != "hsn3" {
			t.Errorf("HSNIface = %q, want hsn3", dev.HSNIface)
		}
	})

	t.Run("VF paired with non-hsn netdev", func(t *testing.T) {
		fs := newFakeSysfs(t)
		vf := fs.pciFunc("0000:c5:00.1")
		dev := &Device{Name: "cxi4", SysfsPath: fs.link("cxi_user", "cxi4", vf)}
		fs.link("net", "enp197s0f1", vf)

		if err := pairDevice(dev, fs.netPath()); err != nil {
			t.Fatal(err)
		}
		if dev.HSNIface != "enp197s0f1" {
			t.Errorf("HSNIface = %q, want enp197s0f1", dev.HSNIface)
		}
	})

	t.Run("sibling functions are not cross-paired", func(t *testing.T) {
		// .1 and .10 share a string prefix; only the exact function must match.
		fs := newFakeSysfs(t)
		pf := fs.pciFunc("0000:c5:00.0")
		vf1 := fs.pciFunc("0000:c5:00.1")
		vf10 := fs.pciFunc("0000:c5:00.10")
		fs.link("net", "hsn3", pf)
		fs.link("net", "enp197s0f1", vf1)
		fs.link("net", "enp197s0f10", vf10)

		dev1 := &Device{Name: "cxi4", SysfsPath: fs.link("cxi_user", "cxi4", vf1)}
		dev10 := &Device{Name: "cxi13", SysfsPath: fs.link("cxi_user", "cxi13", vf10)}
		for _, d := range []*Device{dev1, dev10} {
			if err := pairDevice(d, fs.netPath()); err != nil {
				t.Fatal(err)
			}
		}
		if dev1.HSNIface != "enp197s0f1" {
			t.Errorf("cxi4 HSNIface = %q, want enp197s0f1", dev1.HSNIface)
		}
		if dev10.HSNIface != "enp197s0f10" {
			t.Errorf("cxi13 HSNIface = %q, want enp197s0f10", dev10.HSNIface)
		}
	})

	t.Run("netdev only on a different function is not paired", func(t *testing.T) {
		fs := newFakeSysfs(t)
		pf := fs.pciFunc("0000:c5:00.0")
		vf := fs.pciFunc("0000:c5:00.1")
		fs.link("net", "hsn3", pf)
		// Virtual netdev without a device link must be skipped silently.
		if err := os.MkdirAll(filepath.Join(fs.netPath(), "lo"), 0o755); err != nil {
			t.Fatal(err)
		}

		dev := &Device{Name: "cxi4", SysfsPath: fs.link("cxi_user", "cxi4", vf)}
		if err := pairDevice(dev, fs.netPath()); err == nil {
			t.Fatal("expected error, got nil")
		}
		if dev.HSNIface != "" {
			t.Errorf("HSNIface = %q, want empty", dev.HSNIface)
		}
	})
}

func TestSysfsNetPath(t *testing.T) {
	t.Setenv(sysNetPathEnv, "")
	if got := sysfsNetPath(); got != "/sys/class/net" {
		t.Errorf("default = %q, want /sys/class/net", got)
	}
	t.Setenv(sysNetPathEnv, "/custom/net")
	if got := sysfsNetPath(); got != "/custom/net" {
		t.Errorf("override = %q, want /custom/net", got)
	}
}
