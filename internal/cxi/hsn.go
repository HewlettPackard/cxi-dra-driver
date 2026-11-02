package cxi

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// sysNetPathEnv overrides the net class directory scanned for pairing.
	sysNetPathEnv = "SYS_NET_PATH"

	defaultSysfsNetPath = "/sys/class/net"
)

// sysfsNetPath returns $SYS_NET_PATH, or /sys/class/net if unset or empty.
func sysfsNetPath() string {
	if v := os.Getenv(sysNetPathEnv); v != "" {
		return v
	}
	return defaultSysfsNetPath
}

// PairDevice finds the network interface backed by the same PCI function as
// dev and sets dev.HSNIface.
//
// A CXI device and its netdev are two class devices on one PCI function, so
// the resolved "device" symlink of both points at the same sysfs path. The
// match is exact on that path, independent of the netdev name: PFs are named
// hsnN but VFs carry predictable names such as enp197s0f1, and a prefix match
// would confuse function .1 with .10.
func PairDevice(dev *Device) error {
	return pairDevice(dev, sysfsNetPath())
}

// pairDevice is PairDevice with the net class directory injectable for tests.
func pairDevice(dev *Device, netPath string) error {
	cxiPCIPath, err := filepath.EvalSymlinks(filepath.Join(dev.SysfsPath, "device"))
	if err != nil {
		return fmt.Errorf("resolving CXI sysfs link for %s: %w", dev.Name, err)
	}

	iface, err := netdevOnPCIPath(netPath, cxiPCIPath)
	if err != nil {
		return fmt.Errorf("%s: %w", dev.Name, err)
	}
	dev.HSNIface = iface
	return nil
}

// netdevOnPCIPath returns the netdev in netPath whose resolved device link is
// the PCI function at pciPath (already symlink-resolved).
func netdevOnPCIPath(netPath, pciPath string) (string, error) {
	entries, err := os.ReadDir(netPath)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", netPath, err)
	}

	for _, e := range entries {
		iface := e.Name()
		ifacePCIPath, err := filepath.EvalSymlinks(filepath.Join(netPath, iface, "device"))
		if err != nil {
			// Virtual netdevs (lo, bridges, veth) have no device link.
			continue
		}
		if ifacePCIPath == pciPath {
			return iface, nil
		}
	}

	return "", fmt.Errorf("no netdev found on PCI function %s", pciPath)
}
