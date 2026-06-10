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
// and BindingConditions=["Ready"]. The scheduler may allocate multiple claims
// to the same device, but must wait for the ClaimBinder to set Ready=True
// before binding each pod. This prevents pods from being bound before capacity
// is confirmed — eliminating the race condition that caused concurrent pods to
// collide at the libcxi layer.
//
// avail_* attributes reflect ReadAvailableCapacity minus any in-flight pending
// grants (resources granted Ready=True but whose services haven't been created
// yet). Publishing these pending-adjusted values steers the scheduler toward
// less-loaded devices for subsequent pod allocations.
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
		// Pending-adjusted available capacity. ReadAvailableCapacity already
		// reflects all running service consumption; we only subtract in-flight
		// pending grants on top of that.
		avail := d.effectiveAvail(dev.Name)

		out = append(out, resourceapi.Device{
			Name: dev.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				"type": {StringValue: ptr.To("device")},
				// Topology — usable in CEL placement selectors.
				"numa_node": {IntValue: ptr.To(int64(dev.NUMANode))},
				"pcie_root": {StringValue: ptr.To(dev.PCIeRoot)},
				"hsn_iface": {StringValue: ptr.To(dev.HSNIface)},
				// Hardware totals — static, from cxil_devinfo at discovery.
				"capacity_txqs": {IntValue: ptr.To(dev.Capacity.TXQs)},
				"capacity_tgqs": {IntValue: ptr.To(dev.Capacity.TGQs)},
				"capacity_eqs":  {IntValue: ptr.To(dev.Capacity.EQs)},
				"capacity_cts":  {IntValue: ptr.To(dev.Capacity.CTs)},
				"capacity_tles": {IntValue: ptr.To(dev.Capacity.TLEs)},
				"capacity_ptes": {IntValue: ptr.To(dev.Capacity.PTEs)},
				"capacity_les":  {IntValue: ptr.To(dev.Capacity.LEs)},
				"capacity_acs":  {IntValue: ptr.To(dev.Capacity.ACs)},
				// Effective available — pending-adjusted; updated on every
				// service create/delete and on the periodic refresh interval.
				"avail_txqs": {IntValue: ptr.To(avail.TXQs)},
				"avail_tgqs": {IntValue: ptr.To(avail.TGQs)},
				"avail_eqs":  {IntValue: ptr.To(avail.EQs)},
				"avail_cts":  {IntValue: ptr.To(avail.CTs)},
				"avail_tles": {IntValue: ptr.To(avail.TLEs)},
				"avail_ptes": {IntValue: ptr.To(avail.PTEs)},
				"avail_les":  {IntValue: ptr.To(avail.LEs)},
				"avail_acs":  {IntValue: ptr.To(avail.ACs)},
			},
		// BindingConditions: the scheduler will not bind a pod until the
		// ClaimBinder sets Ready=True, confirming capacity is available.
		// BindingFailureConditions: if the ClaimBinder sets Unschedulable=True
		// (device hardware is truly full), the scheduler deallocates the claim
		// and retries on a different device. Pods must use a merged CXI+VNI
		// claim template so that both allocations are deallocated atomically —
		// see ClaimBinder type comment for why separate claims cause Score errors.
		BindingConditions:        []string{"Ready"},
		BindingFailureConditions: []string{"Unschedulable"},
		AllowMultipleAllocations: ptr.To(true),
		})
	}
	return out
}
