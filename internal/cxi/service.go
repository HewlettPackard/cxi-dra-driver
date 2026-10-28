package cxi

import (
	"fmt"
	"syscall"
)

func childCreationError(device string, parent *Parent, vni int, operation string, rc int) error {
	if operation == "allocate" {
		switch rc {
		case -int(syscall.ENOSPC):
			return fmt.Errorf("VF %s parent %s: allocate failed: kernel rejected child because the shared parent budget is exhausted (ENOSPC, rc=%d)", device, parent.Name(), rc)
		case -int(syscall.EINVAL):
			return fmt.Errorf("VF %s parent %s: allocate failed: kernel rejected child parameters (EINVAL, rc=%d); check VNI %d against parent coverage and descriptor limits", device, parent.Name(), rc, vni)
		}
	}
	return fmt.Errorf("VF %s parent %s: %s failed (rc=%d)", device, parent.Name(), operation, rc)
}

// ServiceOptions identifies the parent of a VF child. A nil Parent preserves
// PF service creation; the parent must include all sibling VFs for accounting.
type ServiceOptions struct {
	Parent *Parent
}

func serviceParent(device string, resources ResourceSet, options []ServiceOptions) (*Parent, error) {
	if len(options) > 1 {
		return nil, fmt.Errorf("cxi: expected at most one service options value")
	}
	if len(options) == 0 || options[0].Parent == nil {
		return nil, nil
	}
	p := options[0].Parent
	if p.PFName == "" || p.SvcID <= 0 {
		return nil, fmt.Errorf("cxi: invalid VF parent %q service %d", p.PFName, p.SvcID)
	}
	found := false
	seen := make(map[string]bool)
	for _, vf := range p.VFs {
		if vf == nil || !vf.IsVF() || vf.PFName != p.PFName || vf.ParentSvcID != p.SvcID || seen[vf.Name] {
			return nil, fmt.Errorf("cxi: invalid sibling VF list for parent %s", p.Name())
		}
		seen[vf.Name] = true
		if vf.Name == device {
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("cxi: VF %s is not assigned to parent %s", device, p.Name())
	}
	if resources.LEs != 0 || resources.TLEs != 0 {
		return nil, fmt.Errorf("cxi: VF %s inherits parent LE/TLE pools; les and tles reservations must be zero", device)
	}
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"txqs", resources.TXQs}, {"tgqs", resources.TGQs}, {"eqs", resources.EQs},
		{"cts", resources.CTs}, {"ptes", resources.PTEs}, {"acs", resources.ACs},
	} {
		if field.value < 0 || field.value > 65535 {
			return nil, fmt.Errorf("cxi: VF %s resource %s=%d is outside [0, 65535]", device, field.name, field.value)
		}
	}
	return p, nil
}

func validateChild(p *Parent, desc ParentDescriptor, vni int) error {
	if !desc.ResourceLimits {
		return fmt.Errorf("parent %s: %w", p.Name(), ErrParentUnlimited)
	}
	if vni < 0 || vni > 65535 || !desc.VNIs.Contains(uint16(vni)) {
		return fmt.Errorf("cxi: VNI %d is not allowed by parent %s (VNIs %s)", vni, p.Name(), desc.VNIs)
	}
	if desc.LEMax <= 0 || desc.LEMax > 1<<32-1 {
		return fmt.Errorf("cxi: parent %s has invalid LE maximum %d", p.Name(), desc.LEMax)
	}
	return nil
}

// ServiceManager abstracts CXI service lifecycle operations. In production it
// wraps libcxi ioctls (CGo); in mock/test mode it is replaced by a no-op or
// file-writing implementation.
type ServiceManager interface {
	// CreateService creates a CXI service on deviceName scoped to the network
	// namespace identified by netnsInode, using the given VNI and resource
	// limits. It returns the allocated service ID (svc_id) on success.
	// Optional parent context selects VF child semantics; existing four-argument
	// calls retain PF behavior.
	CreateService(deviceName string, netnsInode uint64, vni int, resources ResourceSet, options ...ServiceOptions) (int, error)

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
