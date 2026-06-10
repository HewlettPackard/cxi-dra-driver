package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
//     as a fast-fail for requests that can never succeed on this hardware
//  3. Record the allocation in the store for the NRI plugin
//
// The live capacity fit-check is NOT performed here. It is owned by two
// complementary mechanisms:
//   - ClaimBinder (primary): sets Ready=True only when capacity is confirmed,
//     preventing the scheduler from binding the pod until it is safe to do so.
//   - NRI RunPodSandbox (secondary): re-checks under a per-device mutex as a
//     safety net for the narrow window between Ready=True and service creation.
func (d *Driver) PrepareResourceClaims(
	_ context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	// First pass: extract VNIs from VNI pool claims so CXI claims can use them.
	vniByPod := vniFromClaims(claims, d.driverName)

	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		if isVNIClaim(claim, d.driverName) {
			// VNI devices are logical — no CDI device to inject into the container.
			result[claim.UID] = kubeletplugin.PrepareResult{}
			continue
		}
		result[claim.UID] = d.prepareClaim(claim, vniByPod)
	}
	return result, nil
}

// vniFromClaims scans all claims for VNI pool devices and returns a map of
// pod UID → VNI for pods that have a VNI allocated from the cluster-wide pool.
func vniFromClaims(claims []*resourceapi.ResourceClaim, driverName string) map[types.UID]int {
	vniByPod := make(map[types.UID]int)
	for _, claim := range claims {
		if claim.Status.Allocation == nil {
			continue
		}
		for _, alloc := range claim.Status.Allocation.Devices.Results {
			if alloc.Driver != driverName || alloc.Pool != vniPoolName {
				continue
			}
			vni, ok := vniFromDeviceName(alloc.Device)
			if !ok {
				continue
			}
			for _, ref := range claim.Status.ReservedFor {
				if ref.Resource == "pods" {
					vniByPod[types.UID(ref.UID)] = vni
				}
			}
		}
	}
	return vniByPod
}

// isVNIClaim reports whether the claim contains only VNI pool devices (for this
// driver). A claim that also carries a real CXI device is not VNI-only and must
// go through prepareClaim so the CXI device is injected.
func isVNIClaim(claim *resourceapi.ResourceClaim, driverName string) bool {
	if claim.Status.Allocation == nil {
		return false
	}
	sawVNI := false
	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != driverName {
			continue
		}
		if alloc.Pool != vniPoolName {
			return false // a real CXI device → not a VNI-only claim
		}
		sawVNI = true
	}
	return sawVNI
}

// vniFromDeviceName parses the VNI integer from a device name of the form "vni-<n>".
func vniFromDeviceName(name string) (int, bool) {
	s, ok := strings.CutPrefix(name, "vni-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (d *Driver) prepareClaim(claim *resourceapi.ResourceClaim, vniByPod map[types.UID]int) kubeletplugin.PrepareResult {
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
		if alloc.Pool == vniPoolName {
			// VNI devices are logical — no CDI device and no hardware reservation.
			// The VNI itself is extracted by vniFromClaims.
			continue
		}

		// Resolve the requested resources from the claim's opaque config.
		// Claims without config are rejected — all fields are required.
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

		cdiIDs := []string{
			cdi.DeviceID(d.driverName, alloc.Device),
		}
		if d.enableCDIEnv {
			cdiIDs = append(cdiIDs, cdi.EnvDeviceID(d.driverName))
			if d.cdiEnvConfig.NCCLPrefix != "" {
				cdiIDs = append(cdiIDs, cdi.NCCLDeviceID(d.driverName))
			}
			if d.cdiEnvConfig.AWSofiNCCLPrefix != "" {
				cdiIDs = append(cdiIDs, cdi.AWSofiNCCLDeviceID(d.driverName))
			}
		}

		kubeletDevices = append(kubeletDevices, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: cdiIDs,
		})

		pa := d.podAllocationBase(alloc.Device)
		pa.Resources = requested
		podAllocs = append(podAllocs, pa)
	}

	var preparedPods []types.UID
	rollback := func() {
		for _, podUID := range preparedPods {
			d.store.Delete(podUID)
		}
		d.store.DeleteClaim(claim.UID)
	}
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		podUID := types.UID(ref.UID)

		// VNI must come from the pod's allocated VNI ResourceClaim.
		// Pods without a cxi-vni claim are rejected — all workloads must
		// request a VNI from the cluster-wide pool.
		vni, ok := vniByPod[podUID]
		if !ok {
			rollback()
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("pod %s has no VNI allocated — add a cxi-vni ResourceClaim to the workload",
					podUID),
			}
		}
		for i := range podAllocs {
			podAllocs[i].VNI = vni
		}
		d.store.Set(podUID, podAllocs)
		d.store.SetClaimPod(claim.UID, podUID)
		preparedPods = append(preparedPods, podUID)
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
	// Look for opaque config entries in the claim spec targeting this driver
	// and request. We read from spec (where the user wrote it) rather than
	// status.allocation.devices.config, which is a scheduler-copied mirror
	// and may not be present if the claim was allocated by an older scheduler.
	for _, cfg := range claim.Spec.Devices.Config {
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
	// Reject negative values — they would pass Fits checks (e.g. -1 <= available)
	// and then overflow to very large unsigned values in the CGo service creation path.
	if *cfg.TXQs < 0 || *cfg.TGQs < 0 || *cfg.EQs < 0 || *cfg.CTs < 0 ||
		*cfg.PTEs < 0 || *cfg.ACs < 0 || *cfg.LEPools < 0 || *cfg.TLEPools < 0 {
		return cxi.ResourceSet{}, fmt.Errorf("CXI resource config values must be non-negative")
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
