package driver

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// buildDriverResources constructs the DriverResources payload for
// helper.PublishResources. The helper's ResourceSlice controller takes care of
// creating/updating/deleting ResourceSlice objects on the API server.
func (d *Driver) buildDriverResources() resourceslice.DriverResources {
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			d.nodeName: {
				Slices: []resourceslice.Slice{
					{Devices: buildDevices(d.devices)},
				},
			},
		},
	}
}

func buildDevices(devices []*cxi.Device) []resourceapi.Device {
	out := make([]resourceapi.Device, 0, len(devices))
	for _, dev := range devices {
		out = append(out, resourceapi.Device{
			Name: dev.Name,
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				// Topology
				"numa_node": {IntValue: ptr.To(int64(dev.NUMANode))},
				"pcie_root": {StringValue: ptr.To(dev.PCIeRoot)},
				"hsn_iface": {StringValue: ptr.To(dev.HSNIface)},
				// Hardware resource capacity (Max values from the device).
				// These allow the scheduler to make topology- and capacity-aware
				// placement decisions. Res values reflect per-service reservations
				// and are tracked at runtime by the ResourceTracker, not exposed
				// here — ResourceSlice attributes are static at publish time.
				"capacity_txqs": {IntValue: ptr.To(dev.Capacity.TXQs.Max)},
				"capacity_tgqs": {IntValue: ptr.To(dev.Capacity.TGQs.Max)},
				"capacity_eqs":  {IntValue: ptr.To(dev.Capacity.EQs.Max)},
				"capacity_cts":  {IntValue: ptr.To(dev.Capacity.CTs.Max)},
				"capacity_tles": {IntValue: ptr.To(dev.Capacity.TLEs.Max)},
				"capacity_ptes": {IntValue: ptr.To(dev.Capacity.PTEs.Max)},
				"capacity_les":  {IntValue: ptr.To(dev.Capacity.LEs.Max)},
				"capacity_acs":  {IntValue: ptr.To(dev.Capacity.ACs.Max)},
			},
		})
	}
	return out
}
