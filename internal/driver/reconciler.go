package driver

import (
	"context"
	"fmt"
	"math"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// reconcileOccupancyFromHardware reconciles the in-memory nsDeviceOccupancy
// state with the actual set of live CXI services on the node. The event path
// (ClaimBinder watch + NRI StopPodSandbox + checkAndAddPending defensive
// release) is authoritative under normal operation; this poll-driven
// reconciliation is a safety net for the cases where the event path cannot
// fire or was missed:
//
//   - Driver restart: pending and nsDeviceOccupancy are empty on startup,
//     but hardware services from the previous process are still alive.
//   - Lost watch events: a transient API-server disconnect can drop a
//     watch.Deleted that would have driven onClaimDeleted.
//   - Operator intervention: cxi_service utility creating or destroying
//     services out-of-band.
//   - Pods force-deleted before RunPodSandbox: NRI never fires
//     StopPodSandbox, leaving an occupancy slot that the binder may also
//     have missed (e.g. claim already removed by GC by the time the
//     watch.Deleted reached us).
//
// Skipped entirely in mock mode (mockDevices != nil): there are no real
// services, so cxi.ReadActiveServiceVNIs returns an empty slice and the only
// effect would be to delete every in-memory slot, which is wrong.
//
// Errors in I/O (namespace list, per-device VNI read) are logged and the
// affected step is skipped; partial reconciliation is preferred over no
// reconciliation. The next ticker pass will retry.
func (d *Driver) reconcileOccupancyFromHardware(ctx context.Context) {
	if d.mockDevices != nil {
		return
	}
	vniToNamespace, err := d.listNamespaceVNIs(ctx)
	if err != nil {
		d.logf("warning: reconciler listing namespaces: %v", err)
		return
	}
	hwSet := d.readHardwareOccupancy(vniToNamespace)
	d.applyReconcile(hwSet)
}

// listNamespaceVNIs returns the VNI → namespace map from the cluster's
// namespace annotations. Namespaces without the vniAnnotationKey or with an
// invalid annotation value (non-numeric, out of uint16 range, zero) are
// silently skipped — they cannot back any DRA-managed CXI service.
//
// Uses a live LIST per reconcile because namespaces are low-cardinality and
// changes are rare; matches the vniallocator's assignedVNIs pattern and
// avoids the operational complexity of an informer that the rest of the
// driver does not maintain.
func (d *Driver) listNamespaceVNIs(ctx context.Context) (map[uint16]string, error) {
	nsList, err := d.kubeClient.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing namespaces: %w", err)
	}
	out := make(map[uint16]string, len(nsList.Items))
	for i := range nsList.Items {
		ns := &nsList.Items[i]
		vniStr := ns.Annotations[vniAnnotationKey]
		if vniStr == "" {
			continue
		}
		vni, err := strconv.Atoi(vniStr)
		if err != nil || vni <= 0 || vni > math.MaxUint16 {
			continue
		}
		out[uint16(vni)] = ns.Name
	}
	return out, nil
}

// readHardwareOccupancy queries each device's live service VNIs and returns
// the set of (namespace, device) pairs that those VNIs map back to via
// vniToNamespace. A VNI without a matching namespace annotation is silently
// dropped — it may be a system service, a service from a deleted namespace,
// or a stale leftover; in all cases we cannot attribute it to any namespace
// and so it must not block the reconciler from making decisions about slots
// it CAN attribute.
//
// Per-device errors are logged and that device is skipped, preserving
// reconciliation for the other devices on the node.
func (d *Driver) readHardwareOccupancy(vniToNamespace map[uint16]string) map[occupancyKey]struct{} {
	hwSet := make(map[occupancyKey]struct{})
	for _, dev := range d.devices {
		vnis, err := cxi.ReadActiveServiceVNIs(dev.Name)
		if err != nil {
			d.logf("warning: reconciler reading service VNIs for %s: %v", dev.Name, err)
			continue
		}
		for _, vni := range vnis {
			ns, ok := vniToNamespace[vni]
			if !ok {
				continue
			}
			hwSet[occupancyKey{namespace: ns, deviceName: dev.Name}] = struct{}{}
		}
	}
	return hwSet
}

// applyReconcile is the pure state-mutation phase of the reconciler. Given
// the hardware-observed set of (namespace, device) pairs, it brings
// nsDeviceOccupancy into agreement using these rules:
//
//   - Each existing entry (ns, dev) → claimUID is KEPT if the claim is still
//     in pending (event path is mid-flight; hardware service not yet created)
//     OR if (ns, dev) is in hwSet (hardware backs the slot).
//   - All other existing entries are RELEASED via removeOccupancyLocked: they
//     are leaks left over from a watch event the binder missed, a force-
//     deleted pod that bypassed NRI, or a stale entry from before this
//     driver process started.
//   - Each hwSet entry not already in nsDeviceOccupancy is ADDED with a
//     synthetic UID of the form "hw:<dev>/<ns>". The "hw:" prefix can never
//     collide with a real Kubernetes UUID (UUIDs are 8-4-4-4-12 hex with
//     hyphens), so the reverse-index entry is safe and the ClaimBinder will
//     never try to release it via RemoveOccupancyForClaim. Synthetic entries
//     self-clean on the next reconcile cycle in which their hardware service
//     is no longer observed.
//
// Triggers a single TriggerRepublish on any net change so the updated
// active_namespaces attribute reaches the scheduler immediately.
//
// Holds pendingMu for the entire mutation phase so it serializes with
// checkAndAddPending, RemoveOccupancy, and RemoveOccupancyForClaim. I/O
// (namespace list, hardware reads) is intentionally done before this call to
// keep the lock window small.
func (d *Driver) applyReconcile(hwSet map[occupancyKey]struct{}) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()
	changed := false

	// Phase 1: collect leaked entries (mutating during range over the map
	// would be safe in Go but reads less clearly).
	var toRemove []occupancyKey
	for ns, devs := range d.nsDeviceOccupancy {
		for devName, claimUID := range devs {
			if _, inPending := d.pending[claimUID]; inPending {
				continue
			}
			if _, inHw := hwSet[occupancyKey{namespace: ns, deviceName: devName}]; inHw {
				continue
			}
			toRemove = append(toRemove, occupancyKey{namespace: ns, deviceName: devName})
		}
	}
	for _, kv := range toRemove {
		if d.removeOccupancyLocked(kv.namespace, kv.deviceName) {
			changed = true
			d.logf("reconciler: released leaked occupancy slot ns=%s dev=%s", kv.namespace, kv.deviceName)
		}
	}

	// Phase 2: add hardware-observed entries not yet in memory.
	for key := range hwSet {
		if _, ok := d.nsDeviceOccupancy[key.namespace][key.deviceName]; ok {
			continue
		}
		if d.nsDeviceOccupancy[key.namespace] == nil {
			d.nsDeviceOccupancy[key.namespace] = make(map[string]types.UID)
		}
		syntheticUID := syntheticOccupancyUID(key.deviceName, key.namespace)
		d.nsDeviceOccupancy[key.namespace][key.deviceName] = syntheticUID
		d.claimToOccupancy[syntheticUID] = occupancyKey{namespace: key.namespace, deviceName: key.deviceName}
		changed = true
		d.logf("reconciler: discovered hardware occupancy ns=%s dev=%s (synthetic uid)", key.namespace, key.deviceName)
	}

	if changed {
		d.TriggerRepublish()
	}
}

// syntheticOccupancyUID builds the placeholder claim UID used when the
// reconciler discovers a hardware service that has no matching in-memory
// claim entry (driver restart, missed events). The "hw:" prefix guarantees
// no collision with real Kubernetes UUIDs, which are always 8-4-4-4-12 hex
// with hyphens. Two synthetic entries on different (device, namespace) pairs
// produce different UIDs, so claimToOccupancy can hold them all in parallel.
func syntheticOccupancyUID(deviceName, namespace string) types.UID {
	return types.UID(fmt.Sprintf("hw:%s/%s", deviceName, namespace))
}
