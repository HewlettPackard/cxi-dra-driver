package driver

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// PrepareResourceClaims is called by the kubelet plugin helper when a pod
// using CXI claims is scheduled to this node. The helper has already fetched
// the ResourceClaim objects and verified their allocation; we just need to
// return the CDI device IDs for each allocated device and record the
// allocation in the store for the NRI plugin to consume in RunPodSandbox.
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

	var devices []kubeletplugin.Device
	var podAllocs []PodAllocation
	var deviceNames []string
	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != d.driverName {
			continue
		}
		devices = append(devices, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: []string{cdi.DeviceID(d.driverName, alloc.Device)},
		})
		podAllocs = append(podAllocs, d.podAllocation(alloc.Device))
		deviceNames = append(deviceNames, alloc.Device)
	}

	// Allocate a VNI for this pod and stamp it onto every device allocation.
	// The NRI plugin reads alloc.VNI in CreateContainer to scope the CXI
	// service to the correct Slingshot fabric virtual network.
	//
	// TODO: replace StaticVNIAllocator with a real cluster-wide VNI pool
	// allocator once the VNI ResourceSlice phase is implemented.
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		podUID := types.UID(ref.UID)

		// Admit and reserve resources for each allocated device.
		// Until claims carry explicit resource requests we use DefaultResourceSet
		// as a conservative "one service worth" reservation.
		requested := cxi.DefaultResourceSet()
		for _, devName := range deviceNames {
			if err := d.tracker.Admit(devName, requested); err != nil {
				return kubeletplugin.PrepareResult{
					Err: fmt.Errorf("admitting resources for pod %s on device %s: %w", podUID, devName, err),
				}
			}
		}
		for _, devName := range deviceNames {
			d.tracker.Reserve(podUID, devName, requested)
		}

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

	return kubeletplugin.PrepareResult{Devices: devices}
}

// podAllocation builds a PodAllocation for a device name by looking it up in
// the discovered device list for its HSN interface pairing.
func (d *Driver) podAllocation(deviceName string) PodAllocation {
	for _, dev := range d.devices {
		if dev.Name == deviceName {
			return PodAllocation{DeviceName: dev.Name, DevPath: dev.DevPath, HSNIface: dev.HSNIface}
		}
	}
	return PodAllocation{DeviceName: deviceName}
}
