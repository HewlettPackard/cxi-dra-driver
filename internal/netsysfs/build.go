// Package netsysfs builds a per-pod synthetic /sys/class/net directory that is
// bind-mounted over the pod's own /sys/class/net so that STOCK (unpatched)
// libfabric can bootstrap the CXI device from inside the pod.
//
// # Why this exists
//
// RDMA itself works from inside a pod with no in-pod HSN interface at all: the
// CXI character device (/dev/cxiN) is kernel-bypass and carries the data path.
// libfabric's CXI provider, however, reads link metadata for the paired HSN
// netdev from /sys/class/net/<hsn> during initialization:
//
//   - cxip_query_if_list() globs /sys/class/net/* and, for each entry, reads the
//     <iface>/device symlink, takes its basename, and matches the PCI BDF against
//     the CXI device (netdev_lookup). This is how it pairs a netdev to a NIC.
//   - netdev_link() reads <iface>/operstate (and <iface>/carrier). cxip_get_if()
//     hard-fails with -FI_ENODEV when the resulting link is down — there is NO
//     environment override for the link gate, so operstate MUST report "up".
//   - netdev_speed() reads <iface>/speed (informational; a miss only warns).
//   - netdev_ama_check() reads <iface>/addr_assign_type and <iface>/address and,
//     unless CXIP_SKIP_AMA_CHECK=1 is set, drops any device whose netdev does not
//     present a recognized MAC. This tree does NOT synthesize those two files: the
//     AMA gate must be bypassed out of band by setting CXIP_SKIP_AMA_CHECK=1,
//     supplied via the CDI env config (see internal/cdi/env.go) rather than
//     hardcoded into the NRI mount path.
//
// A pod's /sys/class/net is network-namespace scoped and read-only, so we cannot
// simply add an hsnN entry to it. Instead we hand-build a replacement directory
// on a host path and bind-mount it over /sys/class/net. To avoid masking the
// pod's own interfaces (eth0, lo, ...) the replacement replicates each real
// interface as an identical symlink; because only /sys/class/net is overmounted
// (not /sys/devices), those relative symlinks still resolve in the pod.
package netsysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultNetClassDir is the host sysfs net-class directory the driver reads to
// mirror an HSN interface's metadata into a pod.
const DefaultNetClassDir = "/sys/class/net"

// Iface is one of the pod's real network interfaces, replicated verbatim into
// the synthetic tree so the pod keeps seeing its own NICs under /sys/class/net.
// Target is the symlink text exactly as read from the pod's sysfs, e.g.
// "../../devices/virtual/net/eth0".
type Iface struct {
	Name   string
	Target string
}

// HSN describes one synthetic HSN interface to expose to libfabric.
//
// DevicePath is an absolute path whose basename is the CXI device's PCI BDF
// (e.g. "/sys/devices/pci0000:80/.../0000:81:00.0"); libfabric only readlink()s
// it and compares the basename, so the target need not even exist in the pod —
// though in practice it does, since the PCI device tree is not netns scoped.
// Speed is the link speed in Mb/s as decimal text, or "" to omit the file.
type HSN struct {
	Iface      string
	DevicePath string
	Speed      string
}

// Build writes a synthetic /sys/class/net tree into dir: one symlink per real
// pod interface followed by one populated directory per HSN device. dir is
// recreated from scratch (any existing contents are removed). When a real
// interface name collides with an HSN name the HSN entry wins.
func Build(dir string, ifaces []Iface, hsns []HSN) error {
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("clearing %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	hsnNames := make(map[string]bool, len(hsns))
	for _, h := range hsns {
		hsnNames[h.Iface] = true
	}

	for _, ic := range ifaces {
		if ic.Name == "" || ic.Target == "" || hsnNames[ic.Name] {
			continue
		}
		if err := os.Symlink(ic.Target, filepath.Join(dir, ic.Name)); err != nil {
			return fmt.Errorf("replicating interface %s: %w", ic.Name, err)
		}
	}

	for _, h := range hsns {
		if err := buildHSN(dir, h); err != nil {
			return err
		}
	}
	return nil
}

// buildHSN writes a single synthetic hsnN/ directory with the files libfabric
// reads. operstate is forced "up" (with carrier "1") to satisfy the link gate
// at cxip_if.c:142, which has no environment bypass.
func buildHSN(dir string, h HSN) error {
	if h.Iface == "" {
		return fmt.Errorf("HSN entry has empty interface name")
	}
	d := filepath.Join(dir, h.Iface)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", d, err)
	}
	if h.DevicePath != "" {
		if err := os.Symlink(h.DevicePath, filepath.Join(d, "device")); err != nil {
			return fmt.Errorf("writing %s/device: %w", h.Iface, err)
		}
	}
	files := map[string]string{
		"operstate": "up\n",
		"carrier":   "1\n",
	}
	if h.Speed != "" {
		files["speed"] = h.Speed + "\n"
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o444); err != nil {
			return fmt.Errorf("writing %s/%s: %w", h.Iface, name, err)
		}
	}
	return nil
}

// HostHSN reads the host's view of an HSN interface (under DefaultNetClassDir)
// to mirror its PCI binding and link speed into a pod.
func HostHSN(iface string) (HSN, error) {
	return hostHSN(DefaultNetClassDir, iface)
}

// hostHSN is the testable core of HostHSN, parameterised on the net-class dir.
func hostHSN(netClassDir, iface string) (HSN, error) {
	base := filepath.Join(netClassDir, iface)
	devicePath, err := filepath.EvalSymlinks(filepath.Join(base, "device"))
	if err != nil {
		return HSN{}, fmt.Errorf("resolving %s device link: %w", iface, err)
	}
	return HSN{
		Iface:      iface,
		DevicePath: devicePath,
		Speed:      readSpeed(filepath.Join(base, "speed")),
	}, nil
}

// readSpeed returns the interface speed as decimal text only when sysfs reports
// a positive integer. A down link can report "-1" or be unreadable; in that
// case we return "" and omit the speed file (libfabric only warns on a miss),
// rather than feeding libfabric's "%u" parser a negative value.
func readSpeed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(b))
	if n, err := strconv.Atoi(s); err != nil || n <= 0 {
		return ""
	}
	return s
}
