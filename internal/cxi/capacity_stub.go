//go:build !cxi

package cxi

// readDeviceCapacity returns the static fallback capacity in non-cxi builds
// (CI, mock mode). Production builds use the cxi tag which reads real values
// from hardware via libcxi — see capacity_cgo.go.
func readDeviceCapacity(_ string) (ResourceSet, error) {
	return DeviceCapacity(), nil
}

// ReadAvailableCapacity returns the full device capacity as available in
// non-cxi builds — there are no live services to subtract. Production builds
// poll cxil_get_svc_list via libcxi — see capacity_cgo.go.
func ReadAvailableCapacity(_ string) (ResourceSet, error) {
	return DeviceCapacity(), nil
}
