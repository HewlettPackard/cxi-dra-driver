package driver

import (
	"context"
	"encoding/json"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// PrepareResourceClaims is called by the kubelet plugin helper when a pod
// using CXI claims is scheduled to this node. For each allocated device we:
//  1. Resolve the requested ResourceSet from the claim's opaque config
//     (all fields required — claims without config are rejected)
//  2. Check the request against the usable ceiling (hardware - admin overhead)
//     to give a clear "will never fit" rejection before touching hardware
//  3. Poll live available capacity on the device and check the request fits
//  4. Record the allocation in the store for the NRI plugin
func (d *Driver) PrepareResourceClaims(
	_ context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		result[claim.UID] = d.prepareClaim(claim)
	}
	return result, nil
}

func (d *Driver) prepareClaim(claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("ResourceClaim %s/%s has no allocation", claim.Namespace, claim.Name),
		}
	}

	var kubeletDevices []kubeletplugin.Device
	var podAllocs []PodAllocation

	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != d.driverName {
			continue
		}

		// Resolve the requested resources: claim config takes priority,
		// driver defaults apply when no config is present.
		requested, err := d.resolveResources(claim, alloc)
		if err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: resolving resources: %w",
					claim.Namespace, claim.Name, alloc.Device, err),
			}
		}

		// Usable ceiling = hardware total - admin overhead. If the request
		// exceeds this it can never succeed on any device of this type.
		ceiling := d.usableCeiling(alloc.Device)
		if !requested.Fits(ceiling) {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: requested resources (%s) exceed usable ceiling (%s) — request will never fit on this hardware",
					claim.Namespace, claim.Name, alloc.Device, requested, ceiling),
			}
		}

		// Live fit-check: poll current available capacity on the device.
		avail, err := cxi.ReadAvailableCapacity(alloc.Device)
		if err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: reading available capacity: %w",
					claim.Namespace, claim.Name, alloc.Device, err),
			}
		}
		if !requested.Fits(avail) {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: requested resources (%s) do not fit in currently available (%s)",
					claim.Namespace, claim.Name, alloc.Device, requested, avail),
			}
		}

		kubeletDevices = append(kubeletDevices, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: []string{cdi.DeviceID(d.driverName, alloc.Device)},
		})

		pa := d.podAllocationBase(alloc.Device)
		pa.Resources = requested
		podAllocs = append(podAllocs, pa)
	}

	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		podUID := types.UID(ref.UID)

		vni, err := d.vniAllocator.Allocate(podUID)
		if err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("allocating VNI for pod %s: %w", podUID, err),
			}
		}
		for i := range podAllocs {
			podAllocs[i].VNI = vni
		}
		d.store.Set(podUID, podAllocs)
		d.store.SetClaimPod(claim.UID, podUID)
	}

	return kubeletplugin.PrepareResult{Devices: kubeletDevices}
}

// resolveResources returns the ResourceSet to use for a single device
// allocation. All fields must be present in the claim's opaque config —
// there are no driver-level defaults. Claims without opaque config are
// rejected with a clear error directing the user to the example templates.
func (d *Driver) resolveResources(
	claim *resourceapi.ResourceClaim,
	alloc resourceapi.DeviceRequestAllocationResult,
) (cxi.ResourceSet, error) {
	// Look for opaque config entries targeting this driver and request.
	if claim.Status.Allocation != nil {
		for _, cfg := range claim.Status.Allocation.Devices.Config {
			if cfg.Opaque == nil || cfg.Opaque.Driver != d.driverName {
				continue
			}
			// Config applies to this request if Requests is empty (applies to
			// all) or explicitly names this request.
			if len(cfg.Requests) > 0 {
				found := false
				for _, r := range cfg.Requests {
					if r == alloc.Request {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			return parseResourceConfig(cfg.Opaque.Parameters.Raw)
		}
	}
	return cxi.ResourceSet{}, fmt.Errorf(
		"claim %s/%s has no CXI resource config for request %q — "+
			"all resource fields are required; see deploy/kubernetes/test/resourceclaim.yaml for examples",
		claim.Namespace, claim.Name, alloc.Request,
	)
}

// CXIResourceConfig is the JSON structure the user places in the claim's
// opaque config parameters to request specific CXI resources.
// All fields are required — there are no driver-level defaults.
type CXIResourceConfig struct {
	TXQs     *int64 `json:"txqs"`
	TGQs     *int64 `json:"tgqs"`
	EQs      *int64 `json:"eqs"`
	CTs      *int64 `json:"cts"`
	PTEs     *int64 `json:"ptes"`
	ACs      *int64 `json:"acs"`
	LEPools  *int64 `json:"le_pools"`  // expressed as pool count; 1 pool = 1024 LEs
	TLEPools *int64 `json:"tle_pools"` // expressed as pool count; 1 pool = 512 TLEs
}

// parseResourceConfig unmarshals opaque claim parameters into a ResourceSet.
// Returns an error if any field is missing — all fields are required.
func parseResourceConfig(raw []byte) (cxi.ResourceSet, error) {
	var cfg CXIResourceConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cxi.ResourceSet{}, fmt.Errorf("parsing CXI resource config: %w", err)
	}
	missing := []string{}
	if cfg.TXQs == nil {
		missing = append(missing, "txqs")
	}
	if cfg.TGQs == nil {
		missing = append(missing, "tgqs")
	}
	if cfg.EQs == nil {
		missing = append(missing, "eqs")
	}
	if cfg.CTs == nil {
		missing = append(missing, "cts")
	}
	if cfg.PTEs == nil {
		missing = append(missing, "ptes")
	}
	if cfg.ACs == nil {
		missing = append(missing, "acs")
	}
	if cfg.LEPools == nil {
		missing = append(missing, "le_pools")
	}
	if cfg.TLEPools == nil {
		missing = append(missing, "tle_pools")
	}
	if len(missing) > 0 {
		return cxi.ResourceSet{}, fmt.Errorf("CXI resource config missing required fields: %v", missing)
	}
	return cxi.ResourceSet{
		TXQs: *cfg.TXQs,
		TGQs: *cfg.TGQs,
		EQs:  *cfg.EQs,
		CTs:  *cfg.CTs,
		PTEs: *cfg.PTEs,
		ACs:  *cfg.ACs,
		LEs:  cxi.PoolsToLEs(*cfg.LEPools),
		TLEs: cxi.PoolsToTLEs(*cfg.TLEPools),
	}, nil
}

// usableCeiling returns the maximum resources a service can ever request on
// a device: hardware total minus the admin-configured overhead.
func (d *Driver) usableCeiling(deviceName string) cxi.ResourceSet {
	total := d.deviceCapacity(deviceName)
	return total.Sub(d.overhead)
}

// podAllocationBase builds a PodAllocation skeleton for a device name.
func (d *Driver) podAllocationBase(deviceName string) PodAllocation {
	for _, dev := range d.devices {
		if dev.Name == deviceName {
			return PodAllocation{DeviceName: dev.Name, DevPath: dev.DevPath, HSNIface: dev.HSNIface}
		}
	}
	return PodAllocation{DeviceName: deviceName}
}

// deviceCapacity returns the hardware total for a device by name.
func (d *Driver) deviceCapacity(deviceName string) cxi.ResourceSet {
	for _, dev := range d.devices {
		if dev.Name == deviceName {
			return dev.Capacity
		}
	}
	return cxi.DeviceCapacity()
}
