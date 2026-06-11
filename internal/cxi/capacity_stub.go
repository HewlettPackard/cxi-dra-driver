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

// ReadActiveServiceVNIs returns an empty slice in non-cxi builds — there are
// no live CXI services in mock mode, and the reconciler skips the reconcile
// pass entirely in that case. Production builds enumerate
// cxil_get_svc_list + cxil_svc_get_vni_range via libcxi (capacity_cgo.go).
func ReadActiveServiceVNIs(_ string) ([]uint16, error) {
	return nil, nil
}
