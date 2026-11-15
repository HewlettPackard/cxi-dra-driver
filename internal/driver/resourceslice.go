package driver

import (
	"maps"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/HewlettPackard/cxi-dra-driver/internal/cxi"
)

// buildDriverResources constructs the DriverResources payload for
// helper.PublishResources.
//
// Each published device — a PF, or a VF-mode parent — is published with
// AllowMultipleAllocations=true and one consumable capacity per CXI resource
// (see buildCapacity). The
// scheduler shares a device between claims and keeps the sum of their
// consumedCapacity within the device's usable ceiling, so capacity accounting
// for Kubernetes-allocated claims happens atomically in the scheduler.
//
// A parent additionally publishes a vfs capacity (one VF per allocation, so
// VFs stay exclusive) and its VNI set and PF topology as attributes; the
// admission policy's VNI selector matches on the former.
//
// BindingConditions=["Ready"] remain as a safety net: the ClaimBinder checks
// each allocation against live hardware before the pod is bound, catching
// consumption the scheduler cannot see (services created outside Kubernetes,
// claims allocated before the device published capacity).
//
// Nothing in the slice changes at runtime, so it is published once at startup.
func (d *Driver) buildDriverResources() resourceslice.DriverResources {
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			d.nodeName: {
				Slices: []resourceslice.Slice{
					{Devices: d.buildDevices()},
				},
			},
		},
	}
}

// Attribute names and values the admission selector and users' CEL select on.
// They are published unqualified; the API qualifies them with the driver domain.
const (
	attrFunctionType = "function_type"
	attrPFName       = "pf_name"
	attrPFNetdev     = "pf_netdev"
	attrParentSvcID  = "parent_svc_id"
	attrVNIMin       = "vni_min"
	attrVNIMax       = "vni_max"
	attrVNISet       = "vni_set"

	functionTypePF     = "pf"
	functionTypeParent = "parent"

	// noVNIs is the vni_set of a parent whose VNI set could not be read. It
	// holds no ",<vni>," so the admission selector excludes the parent for
	// every namespace; omitting the attributes would instead match like a PF.
	noVNIs = ","
)

// buildDevices constructs the Device list for the ResourceSlice from the
// published view: PFs, or the VF-mode parents.
func (d *Driver) buildDevices() []resourceapi.Device {
	published := d.publishedDevices()
	out := make([]resourceapi.Device, 0, len(published))
	for _, dev := range published {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {StringValue: ptr.To("device")},
		}
		capacity := buildCapacity(d.usableCeiling(dev.name))
		if p := dev.parent; p != nil {
			// A parent spans several VF netdevs, so it publishes no hsn_iface,
			// which promises "the netdev your pod gets".
			attrs[attrFunctionType] = resourceapi.DeviceAttribute{StringValue: ptr.To(functionTypeParent)}
			attrs[attrPFName] = resourceapi.DeviceAttribute{StringValue: ptr.To(p.PFName)}
			attrs[attrParentSvcID] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(p.SvcID))}
			if p.PFNetdev != "" {
				attrs[attrPFNetdev] = resourceapi.DeviceAttribute{StringValue: ptr.To(p.PFNetdev)}
			}
			maps.Copy(attrs, vniAttributes(p.vnis))
			capacity[capVFs] = vfsCapacity(len(p.VFs))
		} else {
			// Topology — usable in CEL placement selectors.
			attrs[attrFunctionType] = resourceapi.DeviceAttribute{StringValue: ptr.To(functionTypePF)}
			attrs["hsn_iface"] = resourceapi.DeviceAttribute{StringValue: ptr.To(dev.hsnIface)}
		}

		// Standard cross-driver attributes (Kubernetes v1.37+ DRA standard
		// device attributes). numaNode is omitted rather than published as -1
		// when the topology is unknown, since a present-but-meaningless value
		// would still participate in scheduler matchAttribute comparisons
		// against other devices/drivers.
		if dev.numaNode >= 0 {
			attrs["resource.kubernetes.io/numaNode"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(dev.numaNode))}
		}
		if dev.pciBusID != "" {
			attrs["resource.kubernetes.io/pciBusID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(dev.pciBusID)}
		}

		out = append(out, resourceapi.Device{
			Name:       dev.name,
			Attributes: attrs,
			// BindingConditions: the scheduler will not bind a pod until the
			// ClaimBinder sets Ready=True, confirming capacity is available.
			// BindingFailureConditions: if the ClaimBinder sets Unschedulable=True
			// (device hardware is truly full), the scheduler deallocates this single
			// CXI claim and retries on a different device. VNIs are namespace-scoped
			// and not part of the claim, so there is nothing else to deallocate.
			BindingConditions:        []string{"Ready"},
			BindingFailureConditions: []string{"Unschedulable"},
			AllowMultipleAllocations: ptr.To(true),
			Capacity:                 capacity,
		})
	}
	return out
}

// vniAttributes publishes a parent's VNI set in exactly one form, so the
// admission selector's two branches never both apply:
//
//   - vni_min/vni_max (ints) for a range, which cxi.NewVNIList also yields for
//     a contiguous list;
//   - vni_set, the delimiter-wrapped list ",40,42,", for a non-contiguous one.
//     It is a string tested with contains(",<vni>,") because the DRA CEL
//     environment does not guarantee the split string extension;
//   - vni_set=noVNIs for an empty (unreadable) set.
func vniAttributes(s cxi.VNISet) map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	if lo, hi, ok := s.Range(); ok {
		return map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			attrVNIMin: {IntValue: ptr.To(int64(lo))},
			attrVNIMax: {IntValue: ptr.To(int64(hi))},
		}
	}
	set := s.String()
	if s.IsEmpty() {
		set = noVNIs
	}
	return map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		attrVNISet: {StringValue: ptr.To(set)},
	}
}
