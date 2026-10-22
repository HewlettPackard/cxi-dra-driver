package cni

import (
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// inetSuffix is appended to the effective name of the in-pod interface
// when the bare name is rejected by the kernel (e.g. it collides
// with an interface the pod's netns already has).
const inetSuffix = "_inet"

// Linux interface names have IFNAMSIZ-1 usable bytes.
const maxIfaceNameLen = 15

// ifaceNameCandidates returns the in-pod interface names tried, in order,
// starting with the effective name, then a suffixed fallback.
// ADD's naming outcome is not persisted, so DEL/CHECK probe the same two
// names to find the interface again.
func ifaceNameCandidates(ifName string) []string {
	return []string{ifName, ifName + inetSuffix}
}

// linkManager is the kernel-facing half of the plugin: everything that
// touches netlink lives behind it so CmdAdd/CmdDel/CmdCheck's orchestration
// (multi-device loop, rollback, idempotency) is unit-testable with a fake on
// any platform, while the real implementation (network_linux.go) is
// exercised only where a netns can actually be created.
type linkManager interface {
	// CreateIPVLAN creates an L2 IPVLAN sub-interface on the host interface
	// hsnIface, moves it into the netns at nsPath, renames it to the first
	// accepted candidate from ifaceNameCandidates(ifName), brings it up, and
	// returns the name used. Cleans up after itself on failure.
	CreateIPVLAN(hsnIface, ifName, containerID, nsPath string) (string, error)

	// Find probes the effective ifName and its fallback in the netns at
	// nsPath, returning "" if neither the interface nor the netns exists.
	Find(nsPath, ifName string) (string, error)

	// Delete removes the named interface from the netns at nsPath.
	// Idempotent: a missing interface or a missing netns is not an error.
	Delete(nsPath, ifName string) error

	// ConfigureAddress applies the IPAM result's addresses and routes to the
	// named interface in the netns at nsPath.
	ConfigureAddress(nsPath, ifName string, result *types100.Result) error

	// Check verifies the named interface exists in the netns at nsPath, is
	// an IPVLAN link, and is up.
	Check(nsPath, ifName string) error
}
