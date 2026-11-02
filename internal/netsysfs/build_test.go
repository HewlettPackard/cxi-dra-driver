package netsysfs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuildReplicatesInterfacesAndHSN(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "net")

	ifaces := []Iface{
		{Name: "lo", Target: "../../devices/virtual/net/lo"},
		{Name: "eth0", Target: "../../devices/virtual/net/eth0"},
	}
	hsns := []HSN{
		{
			Iface:      "hsn0",
			DevicePath: "/sys/devices/pci0000:80/0000:80:00.0/0000:81:00.0",
			Speed:      "200000",
		},
	}

	if err := Build(dir, ifaces, hsns); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Real interfaces are replicated as symlinks with verbatim target text.
	for _, ic := range ifaces {
		got, err := os.Readlink(filepath.Join(dir, ic.Name))
		if err != nil {
			t.Fatalf("readlink %s: %v", ic.Name, err)
		}
		if got != ic.Target {
			t.Errorf("iface %s target = %q, want %q", ic.Name, got, ic.Target)
		}
	}

	// HSN entry: device symlink basename must equal the CXI BDF (what
	// libfabric's netdev_lookup compares), and operstate must be "up" to pass
	// the link gate that has no env bypass.
	devTarget, err := os.Readlink(filepath.Join(dir, "hsn0", "device"))
	if err != nil {
		t.Fatalf("readlink hsn0/device: %v", err)
	}
	if devTarget != hsns[0].DevicePath {
		t.Errorf("hsn0/device target = %q, want %q", devTarget, hsns[0].DevicePath)
	}
	if got := filepath.Base(devTarget); got != "0000:81:00.0" {
		t.Errorf("hsn0/device basename = %q, want CXI BDF 0000:81:00.0", got)
	}
	assertFile(t, filepath.Join(dir, "hsn0", "operstate"), "up\n")
	assertFile(t, filepath.Join(dir, "hsn0", "carrier"), "1\n")
	assertFile(t, filepath.Join(dir, "hsn0", "speed"), "200000\n")
}

func TestBuildHSNNameWinsOverInterface(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "net")
	// A real interface colliding with the HSN name must not be created as a
	// symlink; the populated HSN directory takes precedence.
	ifaces := []Iface{{Name: "hsn0", Target: "../../devices/virtual/net/hsn0"}}
	hsns := []HSN{{Iface: "hsn0", DevicePath: "/sys/devices/x/0000:81:00.0"}}

	if err := Build(dir, ifaces, hsns); err != nil {
		t.Fatalf("Build: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "hsn0"))
	if err != nil {
		t.Fatalf("lstat hsn0: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("hsn0 is a symlink; want a directory (HSN entry should win)")
	}
	if !info.IsDir() {
		t.Errorf("hsn0 is not a directory")
	}
}

func TestBuildOmitsSpeedWhenEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "net")
	hsns := []HSN{{Iface: "hsn0", DevicePath: "/sys/devices/x/0000:81:00.0"}}

	if err := Build(dir, nil, hsns); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hsn0", "speed")); !os.IsNotExist(err) {
		t.Errorf("speed file should be omitted when Speed is empty, stat err = %v", err)
	}
}

func TestBuildIsIdempotentAndClearsStaleEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "net")
	if err := Build(dir, []Iface{{Name: "eth9", Target: "../../devices/virtual/net/eth9"}}, nil); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	// Rebuild without eth9; the stale entry must be gone.
	if err := Build(dir, []Iface{{Name: "eth0", Target: "../../devices/virtual/net/eth0"}}, nil); err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "eth9")); !os.IsNotExist(err) {
		t.Errorf("stale eth9 not cleared on rebuild, err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "eth0")); err != nil {
		t.Errorf("eth0 missing after rebuild: %v", err)
	}
}

func TestHostHSNResolvesBDFAndSpeed(t *testing.T) {
	root := t.TempDir()
	netClass := filepath.Join(root, "class", "net")
	bdf := "0000:81:00.0"
	pciDir := filepath.Join(root, "devices", "pci0000:80", "0000:80:00.0", bdf)
	if err := os.MkdirAll(pciDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ifaceDir := filepath.Join(netClass, "hsn0")
	if err := os.MkdirAll(ifaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pciDir, filepath.Join(ifaceDir, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ifaceDir, "speed"), []byte("200000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hsn, err := hostHSN(netClass, "hsn0")
	if err != nil {
		t.Fatalf("hostHSN: %v", err)
	}
	if filepath.Base(hsn.DevicePath) != bdf {
		t.Errorf("DevicePath basename = %q, want %q", filepath.Base(hsn.DevicePath), bdf)
	}
	if hsn.Speed != "200000" {
		t.Errorf("Speed = %q, want 200000", hsn.Speed)
	}
}

func TestHostHSNForPodUsesPodNameAndHostMetadata(t *testing.T) {
	root := t.TempDir()
	netClass := filepath.Join(root, "class", "net")
	bdf := "0000:c5:00.1"
	pciDir := filepath.Join(root, "devices", "pci0000:c5", bdf)
	if err := os.MkdirAll(pciDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hostIface := "enp197s0f1"
	ifaceDir := filepath.Join(netClass, hostIface)
	if err := os.MkdirAll(ifaceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pciDir, filepath.Join(ifaceDir, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ifaceDir, "speed"), []byte("200000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		podIface   string
		wantIfName string
	}{
		{name: "VF pod name", podIface: "cxi4", wantIfName: "cxi4"},
		{name: "PF legacy name", wantIfName: hostIface},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hsn, err := hostHSNForPod(netClass, hostIface, tc.podIface)
			if err != nil {
				t.Fatalf("hostHSNForPod: %v", err)
			}
			if hsn.Iface != tc.wantIfName || hsn.Speed != "200000" || filepath.Base(hsn.DevicePath) != bdf {
				t.Fatalf("HSN = %+v, want name %q, speed 200000 and BDF %s", hsn, tc.wantIfName, bdf)
			}

			dir := filepath.Join(t.TempDir(), "net")
			if err := Build(dir, nil, []HSN{hsn}); err != nil {
				t.Fatalf("Build: %v", err)
			}
			devicePath, err := os.Readlink(filepath.Join(dir, tc.wantIfName, "device"))
			if err != nil {
				t.Fatalf("reading synthetic device link: %v", err)
			}
			if filepath.Base(devicePath) != bdf {
				t.Errorf("synthetic device BDF = %q, want VF BDF %q", filepath.Base(devicePath), bdf)
			}
			assertFile(t, filepath.Join(dir, tc.wantIfName, "speed"), "200000\n")
		})
	}
}

func TestHostHSNErrorsWhenDeviceLinkMissing(t *testing.T) {
	netClass := filepath.Join(t.TempDir(), "class", "net")
	if err := os.MkdirAll(filepath.Join(netClass, "hsn0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := hostHSN(netClass, "hsn0"); err == nil {
		t.Errorf("expected error when device symlink is absent")
	}
}

func TestReadSpeed(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		write   bool
		want    string
	}{
		{name: "positive", content: "200000\n", write: true, want: "200000"},
		{name: "negative", content: "-1\n", write: true, want: ""},
		{name: "zero", content: "0\n", write: true, want: ""},
		{name: "nonint", content: "fast\n", write: true, want: ""},
		{name: "missing", write: false, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if tc.write {
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := readSpeed(path); got != tc.want {
				t.Errorf("readSpeed(%q) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", filepath.Base(path), string(b), want)
	}
}
