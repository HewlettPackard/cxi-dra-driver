//go:build !cxi

package cxi

// readDeviceCapacity returns the static fallback capacity in non-cxi builds
// (CI, mock mode). Production builds use the cxi tag which reads real values
// from hardware via libcxi — see capacity_cgo.go.
func readDeviceCapacity(_ string) (ResourceSet, error) {
	return DeviceCapacity(), nil
}

// readReservedForSvcs returns the zero ResourceSet in non-cxi builds — there
// are no live CXI services in mock mode, so the blessed system services
// (--system-svc-ids) reserve nothing. Production builds sum the
// cxil_get_svc_list entries whose svc_id matches — see service_cgo.go.
func readReservedForSvcs(_ string, _ []int) (ResourceSet, error) {
	return ResourceSet{}, nil
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

// ReadActiveServices returns an empty slice in non-cxi builds — there are no
// live CXI services in mock mode, so the orphan-service reaper has nothing to
// scan and short-circuits. Production builds enumerate cxil_get_svc_list via
// libcxi — see capacity_cgo.go.
func ReadActiveServices(_ string) ([]ServiceInfo, error) {
	return nil, nil
}

// ReadServiceNetns returns 0 in non-cxi builds — there are no live CXI
// services in mock mode, so the reaper never reaches this path. Production
// builds read cxil_svc_get_netns via libcxi — see capacity_cgo.go.
func ReadServiceNetns(_ string, _ int) (uint64, error) {
	return 0, nil
}
