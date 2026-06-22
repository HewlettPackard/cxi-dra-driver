package cxi

// ServiceManager abstracts CXI service lifecycle operations. In production it
// wraps libcxi ioctls (CGo); in mock/test mode it is replaced by a no-op or
// file-writing implementation.
type ServiceManager interface {
	// CreateService creates a CXI service on deviceName scoped to the network
	// namespace identified by netnsInode, using the given VNI and resource
	// limits. It returns the allocated service ID (svc_id) on success.
	CreateService(deviceName string, netnsInode uint64, vni int, resources ResourceSet) (int, error)

	// DeleteService destroys the CXI service on deviceName identified by the
	// kernel-allocated svcID returned from CreateService.
	DeleteService(deviceName string, svcID int) error
}
