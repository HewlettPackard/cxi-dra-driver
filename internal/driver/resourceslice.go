package driver

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"
)

// buildDriverResources constructs the DriverResources payload for
// helper.PublishResources.
//
// Each physical CXI device is published with AllowMultipleAllocations=true
// and one consumable capacity per CXI resource (see buildCapacity). The
// scheduler shares a device between claims and keeps the sum of their
// consumedCapacity within the device's usable ceiling, so capacity accounting
// for Kubernetes-allocated claims happens atomically in the scheduler.
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

// buildDevices constructs the Device list for the ResourceSlice.
func (d *Driver) buildDevices() []resourceapi.Device {
	out := make([]resourceapi.Device, 0, len(d.devices))
	for _, dev := range d.devices {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {StringValue: ptr.To("device")},
			// Topology — usable in CEL placement selectors.
			"hsn_iface": {StringValue: ptr.To(dev.HSNIface)},
		}

		// Standard cross-driver attributes (Kubernetes v1.37+ DRA standard
		// device attributes). numaNode is omitted rather than published as -1
		// when the topology is unknown, since a present-but-meaningless value
		// would still participate in scheduler matchAttribute comparisons
		// against other devices/drivers.
		if dev.NUMANode >= 0 {
			attrs["resource.kubernetes.io/numaNode"] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(dev.NUMANode))}
		}
		if dev.PCIBusID != "" {
			attrs["resource.kubernetes.io/pciBusID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(dev.PCIBusID)}
		}

		out = append(out, resourceapi.Device{
			Name:       dev.Name,
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
			Capacity:                 buildCapacity(d.usableCeiling(dev.Name)),
		})
	}
	return out
}
