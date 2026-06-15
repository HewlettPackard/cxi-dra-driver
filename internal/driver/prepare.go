package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	ctx context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	result := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		result[claim.UID] = d.prepareClaim(ctx, claim)
	}
	return result, nil
}

func (d *Driver) prepareClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	if claim.Status.Allocation == nil {
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("ResourceClaim %s/%s has no allocation", claim.Namespace, claim.Name),
		}
	}

	var kubeletDevices []kubeletplugin.Device
	var podAllocs []PodAllocation

	// Shared CDI profile specs (env/nccl/aws-ofi-nccl) are driver-global
	// singletons. For a multi-device claim (count > 1) each device's
	// resolveCDIDeviceIDs returns the same profile IDs, but containerd must
	// reference each profile spec only once — applying its bind-mounts twice
	// makes container creation fail. seenCDIIDs dedupes across devices; the
	// per-device cxi= ID is unique so it always passes through.
	seenCDIIDs := make(map[string]bool)

	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != d.driverName {
			continue
		}

		requested, profiles, err := d.resolveClaim(claim, alloc)
		if err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: resolving config: %w",
					claim.Namespace, claim.Name, alloc.Device, err),
			}
		}

		ceiling := d.usableCeiling(alloc.Device)
		if !requested.Fits(ceiling) {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: requested resources (%s) exceed usable ceiling (%s) — request will never fit on this hardware",
					claim.Namespace, claim.Name, alloc.Device, requested, ceiling),
			}
		}

		cdiIDs, err := d.resolveCDIDeviceIDs(alloc.Device, profiles)
		if err != nil {
			return kubeletplugin.PrepareResult{
				Err: fmt.Errorf("claim %s/%s device %s: %w",
					claim.Namespace, claim.Name, alloc.Device, err),
			}
		}

		// Drop any CDI ID already referenced by an earlier device in this
		// claim. Shared profile specs are emitted once; per-device cxi= IDs
		// are unique and always survive.
		deduped := cdiIDs[:0]
		for _, id := range cdiIDs {
			if seenCDIIDs[id] {
				continue
			}
			seenCDIIDs[id] = true
			deduped = append(deduped, id)
		}
		cdiIDs = deduped

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

	// The VNI is namespace-scoped: a namespace is one cluster allocation, so
	// every pod in it shares the VNI and can form RDMA connections. The claim
	// and the pods that reserved it share claim.Namespace.
	vni, err := d.namespaceVNI(ctx, claim.Namespace)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	for i := range podAllocs {
		podAllocs[i].VNI = vni
	}

	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" {
			continue
		}
		podUID := types.UID(ref.UID)
		d.store.Set(podUID, podAllocs)
		d.store.SetClaimPod(claim.UID, podUID)
	}

	return kubeletplugin.PrepareResult{Devices: kubeletDevices}
}

// namespaceVNI returns the VNI assigned to the namespace by the allocator,
// read from the vniAnnotationKey annotation. It retries briefly to tolerate
// the race where a pod is prepared before the allocator has annotated a
// freshly created namespace.
func (d *Driver) namespaceVNI(ctx context.Context, namespace string) (int, error) {
	const (
		attempts = 10
		delay    = time.Second
	)
	var lastErr error
	for i := 0; i < attempts; i++ {
		ns, err := d.kubeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if err != nil {
			lastErr = fmt.Errorf("getting namespace %s: %w", namespace, err)
		} else if v, ok := ns.Annotations[vniAnnotationKey]; ok {
			vni, perr := strconv.Atoi(v)
			if perr != nil {
				return 0, fmt.Errorf("namespace %s has invalid VNI annotation %q: %w", namespace, v, perr)
			}
			if !ValidVNI(vni) {
				return 0, fmt.Errorf("namespace %s VNI %d out of range [%d,%d]", namespace, vni, MinVNI, MaxVNI)
			}
			return vni, nil
		} else {
			lastErr = fmt.Errorf("namespace %s has no %s annotation yet", namespace, vniAnnotationKey)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(delay):
		}
	}
	return 0, fmt.Errorf("VNI not available for namespace %s after %d attempts: %w", namespace, attempts, lastErr)
}

// Valid values for the cdi_profiles field in a claim's opaque config.
const (
	CDIProfileEnv        = "env"
	CDIProfileNCCL       = "nccl"
	CDIProfileAWSofiNCCL = "aws-ofi-nccl"
)

// resolveClaim parses the claim's opaque config and returns the ResourceSet
// and the list of requested CDI profiles for a single device allocation.
// All fields are required; claims without matching opaque config are
// rejected with an error directing the user to the example templates.
func (d *Driver) resolveClaim(
	claim *resourceapi.ResourceClaim,
	alloc resourceapi.DeviceRequestAllocationResult,
) (cxi.ResourceSet, []string, error) {
	// Read from spec (where the user wrote it) rather than
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
	return cxi.ResourceSet{}, nil, fmt.Errorf(
		"claim %s/%s has no CXI resource config for request %q — "+
			"all resource fields are required; see deploy/kubernetes/test/resourceclaim.yaml for examples",
		claim.Namespace, claim.Name, alloc.Request,
	)
}

// resolveCDIDeviceIDs translates the requested CDI profile names into the
// CDI device IDs that must be attached to the pod. It rejects unknown
// profile names, and profiles whose backing spec is not configured on
// this node (e.g. "nccl" requested when ncclPrefix is empty).
func (d *Driver) resolveCDIDeviceIDs(deviceName string, profiles []string) ([]string, error) {
	ids := []string{cdi.DeviceID(d.driverName, deviceName)}
	if len(profiles) == 0 {
		return ids, nil
	}
	if !d.enableCDIEnv {
		return nil, fmt.Errorf("cdi_profiles requested but --enable-cdi-env is not set on the driver")
	}
	for _, p := range profiles {
		switch p {
		case CDIProfileEnv:
			if len(d.cdiEnvConfig.EnvMounts) == 0 {
				return nil, fmt.Errorf("cdi_profile %q requested but envMounts is empty in the driver's env config", p)
			}
			ids = append(ids, cdi.EnvDeviceID(d.driverName))
		case CDIProfileNCCL:
			if d.cdiEnvConfig.NCCLPrefix == "" {
				return nil, fmt.Errorf("cdi_profile %q requested but ncclPrefix is empty in the driver's env config", p)
			}
			ids = append(ids, cdi.NCCLDeviceID(d.driverName))
		case CDIProfileAWSofiNCCL:
			if len(d.cdiEnvConfig.AWSofiMounts) == 0 {
				return nil, fmt.Errorf("cdi_profile %q requested but awsOfiMounts is empty in the driver's env config", p)
			}
			ids = append(ids, cdi.AWSofiNCCLDeviceID(d.driverName))
		default:
			return nil, fmt.Errorf("unknown cdi_profile %q (valid: %q, %q, %q)", p,
				CDIProfileEnv, CDIProfileNCCL, CDIProfileAWSofiNCCL)
		}
	}
	return ids, nil
}

// CXIResourceConfig is the JSON structure the user places in the claim's
// opaque config parameters. All fields are required — there are no
// driver-level defaults. CDIProfiles may be an empty list to indicate
// "no CDI injection beyond the per-device node," but the field itself
// must be present.
type CXIResourceConfig struct {
	TXQs        *int64    `json:"txqs"`
	TGQs        *int64    `json:"tgqs"`
	EQs         *int64    `json:"eqs"`
	CTs         *int64    `json:"cts"`
	PTEs        *int64    `json:"ptes"`
	ACs         *int64    `json:"acs"`
	LEPools     *int64    `json:"le_pools"`     // expressed as pool count; 1 pool = 1024 LEs
	TLEPools    *int64    `json:"tle_pools"`    // expressed as pool count; 1 pool = 512  TLEs
	CDIProfiles *[]string `json:"cdi_profiles"` // env / nccl / aws-ofi-nccl; [] means none
}

// parseResourceConfig unmarshals opaque claim parameters into a ResourceSet
// and a CDI-profile list. Returns an error if any field is missing.
func parseResourceConfig(raw []byte) (cxi.ResourceSet, []string, error) {
	var cfg CXIResourceConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cxi.ResourceSet{}, nil, fmt.Errorf("parsing CXI resource config: %w", err)
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
	if cfg.CDIProfiles == nil {
		missing = append(missing, "cdi_profiles")
	}
	if len(missing) > 0 {
		return cxi.ResourceSet{}, nil, fmt.Errorf("CXI resource config missing required fields: %v", missing)
	}
	// Reject negative values — they would pass Fits checks (e.g. -1 <= available)
	// and then overflow to very large unsigned values in the CGo service creation path.
	if *cfg.TXQs < 0 || *cfg.TGQs < 0 || *cfg.EQs < 0 || *cfg.CTs < 0 ||
		*cfg.PTEs < 0 || *cfg.ACs < 0 || *cfg.LEPools < 0 || *cfg.TLEPools < 0 {
		return cxi.ResourceSet{}, nil, fmt.Errorf("CXI resource config values must be non-negative")
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
	}, *cfg.CDIProfiles, nil
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
