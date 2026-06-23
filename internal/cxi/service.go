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

// ServiceInfo describes one live CXI service as read from the kernel service
// list (cxil_get_svc_list). Used by the driver's orphan-service reaper to
// decide whether a service is a leak. The netns inode bound to the service is
// NOT carried here — it is not part of the service descriptor and is read
// separately and lazily via ReadServiceNetns only for reap candidates.
type ServiceInfo struct {
	SvcID    int    // kernel-allocated service id
	VNI      uint16 // restricted VNI (vnis[0]); 0 when the service has no VNI restriction
	IsSystem bool   // kernel is_system_svc bit (e.g. the Ethernet service)
}
