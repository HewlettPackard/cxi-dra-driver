//go:build !linux

package netsysfs

import "fmt"

// PodInterfaces is only implemented on Linux; the driver runs the netns switch
// against real pod sandboxes in production. This stub lets the package build on
// developer machines (e.g. macOS) for the cross-platform Build/HostHSN logic.
func PodInterfaces(netnsPath string) ([]Iface, error) {
	return nil, fmt.Errorf("netsysfs: pod interface enumeration is only supported on linux")
}
