package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
//  1. Resolve the requested ResourceSet from the allocation's
//     consumedCapacity (or, for claims allocated before the device published
//     capacity, from the claim's opaque config)
//  2. Check the request against the usable ceiling (hardware - blessed
//     system-service reserved) as a fast-fail for requests that can never
//     succeed on this hardware
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

	var results []resourceapi.DeviceRequestAllocationResult
	var requests []cxi.ResourceSet
	for _, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != d.driverName {
			continue
		}

		requested, err := d.resolveClaim(claim, alloc)
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
		results = append(results, alloc)
		requests = append(requests, requested)
	}

	// The VNI is namespace-scoped: a namespace is one cluster allocation, so
	// every pod in it shares the VNI and can form RDMA connections. The claim
	// and the pods that reserved it share claim.Namespace. Read before taking
	// the parent locks: it may wait for the allocator to annotate the namespace.
	vni, err := d.namespaceVNI(ctx, claim.Namespace)
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	// Fallback for claims allocated before the namespace was annotated; the
	// admission VNI selector normally keeps the scheduler off such a parent.
	for _, alloc := range results {
		if p, ok := d.parents.get(alloc.Device); ok {
			if err := p.checkVNI(vni); err != nil {
				return kubeletplugin.PrepareResult{
					Err: fmt.Errorf("claim %s/%s device %s: %w", claim.Namespace, claim.Name, alloc.Device, err),
				}
			}
		}
	}

	unlock := d.lockParents(results)
	progs, err := d.programmableFor(claim.UID, results)
	if err != nil {
		unlock()
		return kubeletplugin.PrepareResult{Err: fmt.Errorf("claim %s/%s: %w", claim.Namespace, claim.Name, err)}
	}

	var kubeletDevices []kubeletplugin.Device
	var podAllocs []PodAllocation

	// The shared env CDI spec is a driver-global singleton. For a multi-device
	// claim (count > 1) each device's resolveCDIDeviceIDs returns the same env
	// ID, but containerd must reference the spec only once — applying its
	// bind-mounts twice makes container creation fail. seenCDIIDs dedupes
	// across devices; the per-device cxi= ID is unique so it always passes
	// through.
	seenCDIIDs := make(map[string]bool)

	for i, alloc := range results {
		// The pod gets the programmable device (the VF in VF mode); kubelet
		// still identifies the allocation by the published device.
		cdiIDs := d.resolveCDIDeviceIDs(progs[i])

		// Drop any CDI ID already referenced by an earlier device in this
		// claim. The shared env spec is emitted once; per-device cxi= IDs
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

		pa := d.podAllocationBase(progs[i])
		pa.Resources = requests[i]
		pa.VNI = vni
		pa.ClaimUID = claim.UID
		pa.ClaimNamespace = claim.Namespace
		pa.ClaimName = claim.Name
		if progs[i] != alloc.Device {
			pa.ParentName = alloc.Device
		}
		pa.ShareID = shareIDOf(alloc)
		podAllocs = append(podAllocs, pa)
	}

	d.store.SetClaimAllocs(claim.UID, podAllocs)
	err = d.recordClaimPods(claim, podAllocs, claim.Status.ReservedFor)
	unlock()
	if err != nil {
		return kubeletplugin.PrepareResult{Err: err}
	}
	if d.vfOnly {
		d.logf("claim %s/%s: %s", claim.Namespace, claim.Name, describeAllocs(podAllocs))
	}
	d.annotateClaimPods(ctx, claim.Namespace, claim.Status.ReservedFor)

	return kubeletplugin.PrepareResult{Devices: kubeletDevices}
}

// describeAllocs renders "parent→VF" pairs for the Prepare log line.
func describeAllocs(allocs []PodAllocation) string {
	parts := make([]string, len(allocs))
	for i, a := range allocs {
		parts[i] = a.AllocatedDevice() + "→" + a.DeviceName
	}
	return strings.Join(parts, ", ")
}

// recordClaimPods records allocs for every pod in refs and writes its CNI hint.
func (d *Driver) recordClaimPods(claim *resourceapi.ResourceClaim, allocs []PodAllocation,
	refs []resourceapi.ResourceClaimConsumerReference) error {
	for _, ref := range refs {
		if ref.Resource != "pods" {
			continue
		}
		podUID := types.UID(ref.UID)
		d.store.Set(podUID, allocs)
		d.store.SetClaimPod(claim.UID, podUID)

		// The hint file is the CNI plugin's read-side contract (no access to
		// this in-process store) — a write failure must fail the claim so
		// kubelet retries, rather than silently handing the pod to CRI
		// without net-hints.
		if err := d.writeCNIHint(podUID); err != nil {
			return fmt.Errorf("claim %s/%s: writing CNI hint file for pod %s: %w",
				claim.Namespace, claim.Name, podUID, err)
		}
	}
	return nil
}

// annotateClaimPods best-effort mirrors each pod's hints into its annotation,
// an observability side channel only; never load-bearing.
func (d *Driver) annotateClaimPods(ctx context.Context, namespace string, refs []resourceapi.ResourceClaimConsumerReference) {
	for _, ref := range refs {
		if ref.Resource == "pods" {
			d.annotatePodCNIHints(ctx, namespace, ref.Name, cniHintsFor(d.store.Get(types.UID(ref.UID))))
		}
	}
}

// recordSharedClaimPods records pods that reserved claim after it was prepared
// on this node. Kubelet prepares a claim once per node, so a later pod sharing
// it never reaches PrepareResourceClaims and would have no allocation or CNI
// hint. A no-op for claims not prepared here.
//
// It reports false when a pod could not be recorded (its hint file write
// failed). Those pods are forgotten so a later call retries them; the caller
// must schedule that retry, because the claim watch alone may not fire again.
func (d *Driver) recordSharedClaimPods(ctx context.Context, claim *resourceapi.ResourceClaim) bool {
	allocs := d.store.ClaimAllocs(claim.UID)
	if allocs == nil {
		return true
	}
	var added []resourceapi.ResourceClaimConsumerReference
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource == "pods" && !d.store.HasClaimPod(claim.UID, types.UID(ref.UID)) {
			added = append(added, ref)
		}
	}
	if len(added) == 0 {
		return true
	}
	if err := d.recordClaimPods(claim, allocs, added); err != nil {
		d.logf("warning: %v; will retry", err)
		// recordClaimPods marks a pod recorded before writing its hint and
		// stops at the first failure. Forget the whole batch: pods it never
		// reached are retried, and any recorded before the failure are simply
		// re-recorded, which is idempotent.
		for _, ref := range added {
			d.store.ForgetClaimPod(claim.UID, types.UID(ref.UID))
		}
		return false
	}
	for _, ref := range added {
		d.logf("claim %s/%s: recorded pod %s (%s), which reserved the claim after it was prepared",
			claim.Namespace, claim.Name, ref.Name, ref.UID)
	}
	d.annotateClaimPods(ctx, claim.Namespace, added)
	return true
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
		} else if vni, ok, perr := vniFromAnnotations(namespace, ns.Annotations); perr != nil {
			return 0, perr
		} else if ok {
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

// vniFromAnnotations parses the namespace VNI annotation; ok is false when the
// namespace has not been annotated yet.
func vniFromAnnotations(namespace string, annotations map[string]string) (vni int, ok bool, err error) {
	v, ok := annotations[vniAnnotationKey]
	if !ok {
		return 0, false, nil
	}
	vni, err = strconv.Atoi(v)
	if err != nil {
		return 0, false, fmt.Errorf("namespace %s has invalid VNI annotation %q: %w", namespace, v, err)
	}
	if !ValidVNI(vni) {
		return 0, false, fmt.Errorf("namespace %s VNI %d out of range [%d,%d]", namespace, vni, MinVNI, MaxVNI)
	}
	return vni, true, nil
}

// resolveClaim returns the ResourceSet for a single device allocation.
//
// The allocation's consumedCapacity is authoritative: it is what the
// scheduler charged against the device, already rounded up to whole LE/TLE
// pools. A claim may still carry legacy opaque resource config; if that asks
// for more than was consumed (the admission policy that converts it to
// capacity requests is disabled, say) the claim is rejected rather than
// silently given less than it asked for.
//
// An allocation without consumedCapacity was made against a device that
// published no capacity (before this driver version), so the opaque config is
// the only record of the request.
func (d *Driver) resolveClaim(
	claim *resourceapi.ResourceClaim,
	alloc resourceapi.DeviceRequestAllocationResult,
) (cxi.ResourceSet, error) {
	legacy, hasLegacy, legacyErr := d.legacyResourceConfig(claim, alloc.Request)
	parent, isParent := d.parents.get(alloc.Device)
	if isParent && hasLegacy && legacyErr == nil && (legacy.LEs != 0 || legacy.TLEs != 0) {
		return cxi.ResourceSet{}, inheritedPoolsError(parent, "opaque config le_pools/tle_pools", legacy)
	}

	if len(alloc.ConsumedCapacity) == 0 {
		if legacyErr != nil {
			return cxi.ResourceSet{}, legacyErr
		}
		if !hasLegacy {
			return cxi.ResourceSet{}, fmt.Errorf(
				"claim %s/%s request %q has neither consumed capacity nor CXI opaque resource config",
				claim.Namespace, claim.Name, alloc.Request)
		}
		return legacy, nil
	}

	consumed, err := resourceSetFromConsumed(alloc.ConsumedCapacity, d.driverName)
	if err != nil {
		return cxi.ResourceSet{}, err
	}
	// The scheduler already blocks this (the parent publishes zero LEs/TLEs).
	if isParent && (consumed.LEs != 0 || consumed.TLEs != 0) {
		return cxi.ResourceSet{}, inheritedPoolsError(parent, "consumed capacity les/tles", consumed)
	}
	// Opaque config without resource fields is fine when capacity was
	// requested; any other problem with it (a removed cdi_profiles entry, bad
	// JSON) still rejects the claim.
	if legacyErr != nil && !errors.Is(legacyErr, errMissingResourceFields) {
		return cxi.ResourceSet{}, legacyErr
	}
	if hasLegacy && legacyErr == nil && !legacy.Fits(consumed) {
		return cxi.ResourceSet{}, fmt.Errorf(
			"opaque config requests (%s) exceed the scheduler-allocated capacity (%s); "+
				"move the request to spec.devices.requests[].exactly.capacity.requests",
			legacy, consumed)
	}
	return consumed, nil
}

// inheritedPoolsError rejects LEs/TLEs on a VF-mode parent: child services on
// its VFs take their LE/TLE pools from the parent service.
func inheritedPoolsError(p *parentInfo, source string, rs cxi.ResourceSet) error {
	return fmt.Errorf("device %s is a parent of SR-IOV VFs: %s request %d LEs / %d TLEs, but LE and TLE pools are inherited from parent service %s/%d and must be 0",
		p.Name(), source, rs.LEs, rs.TLEs, p.PFName, p.SvcID)
}

// legacyResourceConfig parses the claim's opaque CXI config for request, if
// any. found is false when no opaque config for this driver applies to the
// request. Read from spec (where the user wrote it) rather than
// status.allocation.devices.config, which is a scheduler-copied mirror and may
// not be present if the claim was allocated by an older scheduler.
func (d *Driver) legacyResourceConfig(
	claim *resourceapi.ResourceClaim,
	request string,
) (rs cxi.ResourceSet, found bool, err error) {
	for _, cfg := range claim.Spec.Devices.Config {
		if cfg.Opaque == nil || cfg.Opaque.Driver != d.driverName {
			continue
		}
		// Config applies to this request if Requests is empty (applies to
		// all) or explicitly names this request.
		if len(cfg.Requests) > 0 && !slices.Contains(cfg.Requests, request) {
			continue
		}
		rs, err := parseResourceConfig(cfg.Opaque.Parameters.Raw)
		return rs, true, err
	}
	return cxi.ResourceSet{}, false, nil
}

// resolveCDIDeviceIDs returns the CDI device IDs attached for one device:
// its own cxi=<dev> spec and the shared env spec (kdreg2/gdrdrv/xpmem nodes,
// and the host libs and env vars when --enable-cdi-env is set).
// Like the NVIDIA DRA driver, there is nothing to choose per claim; NCCL and
// aws-ofi-nccl are injected by k8s-modules (modules.brics.ac.uk/load).
func (d *Driver) resolveCDIDeviceIDs(deviceName string) []string {
	return []string{cdi.DeviceID(d.driverName, deviceName), cdi.EnvDeviceID(d.driverName)}
}

// CXIResourceConfig is the legacy JSON structure for resource requests in the
// claim's opaque config parameters, superseded by capacity requests. All
// resource fields are required. CDIProfiles is deprecated: the env spec is always
// injected, so only omitted, [] or ["env"] are accepted; any other profile is
// rejected because the driver no longer provides it.
type CXIResourceConfig struct {
	TXQs        *int64   `json:"txqs"`
	TGQs        *int64   `json:"tgqs"`
	EQs         *int64   `json:"eqs"`
	CTs         *int64   `json:"cts"`
	PTEs        *int64   `json:"ptes"`
	ACs         *int64   `json:"acs"`
	LEPools     *int64   `json:"le_pools"`     // expressed as pool count; 1 pool = 1024 LEs
	TLEPools    *int64   `json:"tle_pools"`    // expressed as pool count; 1 pool = 512  TLEs
	CDIProfiles []string `json:"cdi_profiles"` // deprecated; only "env" accepted (and ignored)
}

// errMissingResourceFields reports opaque config that lacks resource fields.
var errMissingResourceFields = errors.New("CXI resource config missing required fields")

// parseResourceConfig unmarshals opaque claim parameters into a ResourceSet.
// Returns an error if any resource field is missing or cdi_profiles names a
// profile other than "env".
func parseResourceConfig(raw []byte) (cxi.ResourceSet, error) {
	var cfg CXIResourceConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cxi.ResourceSet{}, fmt.Errorf("parsing CXI resource config: %w", err)
	}
	for _, p := range cfg.CDIProfiles {
		if p != cdi.EnvName {
			return cxi.ResourceSet{}, fmt.Errorf(
				"cdi_profiles %q is no longer provided by the CXI DRA driver; load it with k8s-modules via the pod annotation modules.brics.ac.uk/load (the env spec is always injected, so cdi_profiles can be removed)", p)
		}
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
		return cxi.ResourceSet{}, fmt.Errorf("%w: %v", errMissingResourceFields, missing)
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
// a published device: for a VF-mode parent its published ceiling, otherwise
// hardware total minus the resources reserved by the blessed system services
// (--system-svc-ids), summed once at discovery time.
func (d *Driver) usableCeiling(deviceName string) cxi.ResourceSet {
	if p, ok := d.parents.get(deviceName); ok {
		return p.ceiling
	}
	total := d.deviceCapacity(deviceName)
	return total.Sub(d.deviceReserved(deviceName))
}

// podAllocationBase builds a PodAllocation skeleton for a programmable device.
func (d *Driver) podAllocationBase(deviceName string) PodAllocation {
	for _, dev := range d.programmableDevices() {
		if dev.Name == deviceName {
			return PodAllocation{
				DeviceName: dev.Name,
				DevPath:    dev.DevPath,
				HSNIface:   dev.HSNIface,
				PodIfName:  dev.PodIfName,
			}
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

// deviceReserved returns the resources held by the blessed system services
// (--system-svc-ids) on a device by name, summed at discovery time. Zero in
// mock/non-cxi builds, where no live services exist.
func (d *Driver) deviceReserved(deviceName string) cxi.ResourceSet {
	for _, dev := range d.devices {
		if dev.Name == deviceName {
			return dev.Reserved
		}
	}
	return cxi.ResourceSet{}
}
