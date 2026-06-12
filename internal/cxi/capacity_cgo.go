//go:build cxi

package cxi

// readDeviceCapacity returns real hardware resource maxima for deviceName by
// calling into libcxi. Returns an error if the device cannot be opened or
// queried — this is a hard failure in production; Discover() will abort.
func readDeviceCapacity(deviceName string) (ResourceSet, error) {
	return ReadDeviceCapacity(deviceName)
}

// ReadAvailableCapacity polls the live service list on deviceName, sums the
// reserved values across all services, and returns hardware_total minus that
// sum. LEs and TLEs in the reserved sum are rounded up to pool boundaries
// before subtracting, reflecting the true pool consumption.
func ReadAvailableCapacity(deviceName string) (ResourceSet, error) {
	return readAvailableCapacity(deviceName)
}

// ReadActiveServiceVNIs returns the VNI assigned to each live CXI service on
// deviceName. Used by the driver's polling reconciler to reverse-map
// (device, vni) → namespace via the namespace annotation cache so that the
// in-memory nsDeviceOccupancy state can be reconciled with actual hardware
// state — recovering from any event-path drift (lost watch events, driver
// restart, scheduler de-allocations that bypassed both NRI and the
// ClaimBinder watch).
//
// SHS14 stores one VNI per service (vni_min == vni_max under the DRA driver's
// create path), so each entry in the returned slice corresponds to one
// service. Services without a VNI restriction (system services, services
// created outside DRA) are silently skipped.
func ReadActiveServiceVNIs(deviceName string) ([]uint16, error) {
	return readActiveServiceVNIs(deviceName)
}
