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
