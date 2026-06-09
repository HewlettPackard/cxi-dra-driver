package driver

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// buildDriverResources constructs the DriverResources payload for
// helper.PublishResources.
//
// Each physical CXI device is published with AllowMultipleAllocations=true —
// the scheduler may place multiple pods on the same device. Capacity tracking
// is not delegated to the scheduler; instead the driver performs a live
// fit-check in PrepareResourceClaims against the actual hardware state.
//
// Device attributes include both static topology info and current available
// resource counts. The available counts are refreshed periodically so that
// users can write CEL selectors against them (e.g. to prefer devices with
// more headroom). They are informational — the live fit-check is the
// authoritative gate.
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
		avail, err := cxi.ReadAvailableCapacity(dev.Name)
		if err != nil {
			// Log and use zeros — the fit-check will reject requests that
			// can't fit rather than silently overcommitting.
			d.logf("warning: reading available capacity for %s: %v", dev.Name, err)
			avail = cxi.ResourceSet{}
		}
		// Do NOT subtract overhead here. ReadAvailableCapacity already reflects
		// the actual hardware consumption of all running services, including the
		// pre-configured system services that --reserved-*-pools accounts for.
		// Subtracting overhead again would double-count those reservations.
		out = append(out, resourceapi.Device{
			Name: dev.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				// Topology — usable in CEL placement selectors.
				"type": {StringValue: ptr.To("device")},
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
				// Available — refreshed periodically; informational for CEL selectors.
				"avail_txqs": {IntValue: ptr.To(avail.TXQs)},
				"avail_tgqs": {IntValue: ptr.To(avail.TGQs)},
				"avail_eqs":  {IntValue: ptr.To(avail.EQs)},
				"avail_cts":  {IntValue: ptr.To(avail.CTs)},
				"avail_tles": {IntValue: ptr.To(avail.TLEs)},
				"avail_ptes": {IntValue: ptr.To(avail.PTEs)},
				"avail_les":  {IntValue: ptr.To(avail.LEs)},
				"avail_acs":  {IntValue: ptr.To(avail.ACs)},
			},
			AllowMultipleAllocations: ptr.To(true),
		})
	}
	return out
}
