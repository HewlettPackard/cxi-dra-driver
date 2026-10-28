package cxi

import (
	"errors"
	"fmt"
)

// ErrParentUnlimited is returned (wrapped) when a parent service descriptor has
// resource_limits unset. Such a parent makes the kernel enforce no per-parent
// budget, so two of them on one PF would each publish the whole device; the
// VF-mode budget model does not support it.
var ErrParentUnlimited = errors.New("cxi: parent service has no resource limits")

// ParentDescriptor is the part of a parent service's cxi_svc_desc the driver
// needs, read from the PF.
type ParentDescriptor struct {
	// ResourceLimits mirrors the descriptor's resource_limits bit. When false
	// the Limits values are meaningless.
	ResourceLimits bool
	// Limits holds limits.*.res — the budget shared by the parent's VFs.
	Limits ResourceSet
	// LEMax is limits.les.max, not the reserved LE budget in Limits.LEs.
	LEMax int64
	// VNIs is the set of VNIs the parent allows.
	VNIs VNISet
}

// ServiceReservation is one entry of a VF's service list (cxil_get_svc_list on
// the VF device).
type ServiceReservation struct {
	SvcID    int  // service id, local to the VF
	IsSystem bool // is_system_svc bit (e.g. the Ethernet child the kernel creates)
	// ResourceLimits mirrors the resource_limits bit. A service without it
	// reserves nothing against the parent, matching the PF-mode sums.
	ResourceLimits bool
	Res            ResourceSet // limits.*.res
}

// ReadParentBudget reads the parent service svcID on the PF device pfName
// (e.g. "cxi0") and returns its resource budget and VNI set. It returns an
// error wrapping ErrParentUnlimited when the parent has no resource limits.
// Values are raw: LEs and TLEs are not pool-rounded.
func ReadParentBudget(pfName string, svcID int) (ResourceSet, VNISet, error) {
	desc, err := readParentDescriptor(pfName, svcID)
	if err != nil {
		return ResourceSet{}, VNISet{}, fmt.Errorf("reading parent svc %d on %s: %w", svcID, pfName, err)
	}
	if !desc.ResourceLimits {
		return ResourceSet{}, VNISet{}, fmt.Errorf("parent svc %d on %s: %w", svcID, pfName, ErrParentUnlimited)
	}
	return desc.Limits, desc.VNIs, nil
}

// ReadParentRemaining returns the budget left on the parent service svcID of
// PF pfName: the parent's res minus the res of every service listed on every
// VF in vfNames, system children included. The parent itself is never listed on
// a VF, so it is not part of the sum. The kernel does not expose the remaining
// budget (it answers ENOSPC past it), hence this arithmetic.
//
// Values are raw, with no LE/TLE pool rounding: pod children inherit les=0 and
// tles=0, so only the Ethernet system child consumes LEs. The result can be
// negative if the kernel's accounting and the lists ever disagree; callers
// clamp where a non-negative value is required.
func ReadParentRemaining(pfName string, svcID int, vfNames []string) (ResourceSet, error) {
	budget, _, err := ReadParentBudget(pfName, svcID)
	if err != nil {
		return ResourceSet{}, err
	}
	var used ResourceSet
	for _, vf := range vfNames {
		svcs, err := readVFServices(vf)
		if err != nil {
			return ResourceSet{}, fmt.Errorf("listing services on %s: %w", vf, err)
		}
		used = used.Add(sumReservations(svcs, false))
	}
	return budget.Sub(used), nil
}

// ReadVFSystemReservation returns the summed res of every system-bit service on
// the VF device vfName. On SHS 15.0.0 that is the single Ethernet child the
// kernel creates while the VF netdev is up; any further system service (for
// example from a retry handler running on the VF) is included by the same rule.
// The value is raw, with no LE/TLE pool rounding.
func ReadVFSystemReservation(vfName string) (ResourceSet, error) {
	svcs, err := readVFServices(vfName)
	if err != nil {
		return ResourceSet{}, fmt.Errorf("listing services on %s: %w", vfName, err)
	}
	return sumReservations(svcs, true), nil
}

// sumReservations adds up the res of services that carry resource limits,
// optionally only the system ones.
func sumReservations(svcs []ServiceReservation, systemOnly bool) ResourceSet {
	var sum ResourceSet
	for _, s := range svcs {
		if !s.ResourceLimits || (systemOnly && !s.IsSystem) {
			continue
		}
		sum = sum.Add(s.Res)
	}
	return sum
}
