package driver

import (
	"context"
	"fmt"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// ClaimBinder watches ResourceClaims cluster-wide and manages DRA binding
// conditions for claims that allocate CXI devices on this node.
//
// When the scheduler allocates a claim to a CXI device on this node it writes
// the allocation into the claim's status. The ClaimBinder sees this event and
// decides whether the pod can proceed to binding:
//
//   - Ready=True: the requested resources fit within currently available
//     hardware capacity minus any in-flight pending grants, AND no other
//     claim from the same namespace is already on this device. The grant is
//     recorded in the driver's pending map and the (namespace, device)
//     occupancy counter is incremented so that subsequent evaluations on the
//     same device see the reduced capacity and don't over-commit, and so
//     that other claims in the same namespace are steered to other devices.
//     TriggerRepublish is called so the ResourceSlice immediately reflects
//     the reduced avail_* — steering the scheduler toward other devices for
//     subsequent pods.
//
//   - Ready=False: pending grants (not yet backed by hardware services) are
//     blocking the request, but the raw hardware capacity would fit it.
//     The claim is added to falseClaims for periodic re-evaluation. When a
//     CXI service is deleted the NRI plugin calls TriggerRetry so waiting
//     pods unblock quickly.
//
//   - Unschedulable=True (DeviceAtCapacity): the raw hardware capacity
//     (ReadAvailableCapacity, already reflecting all running services)
//     cannot fit the request even before accounting for pending grants. The
//     device is truly full. Setting Unschedulable causes the DRA scheduler
//     to deallocate the claim and retry on a different device; the updated
//     ResourceSlice (avail_* near zero for the full device) steers it to a
//     device with real capacity.
//
//   - Unschedulable=True (NamespaceDeviceOccupied): another claim from the
//     same namespace is already pending or active on this device. SHS14
//     enforces VNI uniqueness per device, and every claim in a namespace
//     shares the namespace's VNI, so co-locating two claims from the same
//     namespace on the same device would collide at cxil_svc_set_vni_range
//     with -EEXIST. Setting Unschedulable forces the scheduler to retry on
//     a different device. (The ResourceSlice cannot express this constraint
//     directly because it has no notion of per-namespace state.)
//
// Pods carry exactly one ResourceClaim: the CXI device. The VNI is assigned
// per namespace (see NamespaceVNIConfig) and read at prepare time, so it is
// not part of any ResourceClaim. When Unschedulable=True is set on a full or
// already-occupied CXI device the scheduler deallocates that single claim
// and retries on another device — there is no second (VNI) claim to leave
// dangling, so the DRA Score plugin cannot hit the "number of allocations N
// is smaller than number of claims" mismatch that splitting CXI and VNI
// across two claims would cause.
type ClaimBinder struct {
	driver     *Driver
	client     kubernetes.Interface
	driverName string
	nodeName   string

	mu          sync.Mutex
	falseClaims map[types.UID]*resourceapi.ResourceClaim

	retrigger chan struct{} // signals immediate re-evaluation of falseClaims
}

// NewClaimBinder creates a ClaimBinder for the given driver and node.
func NewClaimBinder(d *Driver, client kubernetes.Interface, driverName, nodeName string) *ClaimBinder {
	return &ClaimBinder{
		driver:      d,
		client:      client,
		driverName:  driverName,
		nodeName:    nodeName,
		falseClaims: make(map[types.UID]*resourceapi.ResourceClaim),
		retrigger:   make(chan struct{}, 1),
	}
}

// TriggerRetry signals the binder to immediately re-evaluate claims in the
// Ready=False state. Call this after hardware capacity has been freed (e.g.
// after a CXI service is deleted) so waiting pods unblock quickly.
func (b *ClaimBinder) TriggerRetry() {
	select {
	case b.retrigger <- struct{}{}:
	default:
	}
}

// Run watches all ResourceClaims and manages binding conditions until ctx is
// cancelled. The watch restarts automatically on disconnect.
func (b *ClaimBinder) Run(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := b.runWatch(ctx, ticker); err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				b.logf("watch error: %v; restarting in 5s", err)
				time.Sleep(5 * time.Second)
			}
		} else {
			return nil
		}
	}
}

func (b *ClaimBinder) runWatch(ctx context.Context, ticker *time.Ticker) error {
	list, err := b.client.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing ResourceClaims: %w", err)
	}
	for i := range list.Items {
		b.evaluateClaim(ctx, &list.Items[i])
	}

	watcher, err := b.client.ResourceV1().ResourceClaims("").Watch(ctx, metav1.ListOptions{
		ResourceVersion: list.ResourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watching ResourceClaims: %w", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				if claim, ok := event.Object.(*resourceapi.ResourceClaim); ok {
					b.evaluateClaim(ctx, claim)
				}
			case watch.Deleted:
				if claim, ok := event.Object.(*resourceapi.ResourceClaim); ok {
					b.onClaimDeleted(claim)
				}
			}
		case <-b.retrigger:
			b.reEvaluateFalse(ctx)
		case <-ticker.C:
			b.reEvaluateFalse(ctx)
		}
	}
}

// evaluateClaim processes one ResourceClaim event.
func (b *ClaimBinder) evaluateClaim(ctx context.Context, claim *resourceapi.ResourceClaim) {
	if claim.Status.Allocation == nil {
		// Not yet allocated (or de-allocated) — clean up any stale state.
		b.driver.RemovePending(claim.UID)
		b.mu.Lock()
		delete(b.falseClaims, claim.UID)
		b.mu.Unlock()
		return
	}

	result, requested, err := b.findOurDevice(claim)
	if err != nil {
		b.logf("warning: resolving device for claim %s/%s: %v", claim.Namespace, claim.Name, err)
		return
	}
	if result == nil {
		return // not our claim / not our node
	}

	// Already granted — pending entry exists, service not yet created.
	if b.driver.hasPending(claim.UID) {
		return
	}

	// Already granted and service was created (or being torn down).
	if cond := b.getCondition(claim, result, "Ready"); cond != nil && cond.Status == metav1.ConditionTrue {
		return
	}

	// Read hardware capacity outside the lock to avoid blocking the mutex
	// during I/O. Do NOT subtract overhead — ReadAvailableCapacity already
	// reflects all running service consumption including system services.
	hwAvail, readErr := cxi.ReadAvailableCapacity(result.Device)
	if readErr != nil {
		// Skip this evaluation cycle; using zero capacity would incorrectly
		// trigger Unschedulable for a transient read failure.
		b.logf("warning: reading capacity for %s: %v; skipping evaluation", result.Device, readErr)
		return
	}

	// Four-way decision based on namespace occupancy and hardware capacity:
	//
	//  1. (namespace, device) is already occupied by another claim from this
	//     namespace                                  → Unschedulable
	//                                                 (NamespaceDeviceOccupied)
	//  2. hwAvail − pendingSum  fits requested       → Ready=True  (grant)
	//  3. hwAvail alone         fits requested       → Ready=False (pending
	//                                                 blocking, retry)
	//  4. hwAvail               can't fit alone      → Unschedulable
	//                                                 (DeviceAtCapacity)
	granted, occupied := b.driver.checkAndAddPending(
		claim.UID, claim.Namespace, result.Device, requested, hwAvail,
	)
	switch {
	case granted:
		// Case 2: grant. Record in pending so subsequent claims on the same
		// device see reduced capacity. Republish so the scheduler sees updated
		// avail_* and steers future pods to less-loaded devices.
		b.logf("Ready=True: claim=%s/%s device=%s requested=(%s)",
			claim.Namespace, claim.Name, result.Device, requested)
		b.setCondition(ctx, claim, result, "Ready", metav1.ConditionTrue,
			"DeviceCapacityAvailable",
			fmt.Sprintf("requested resources (%s) fit within effective available capacity", requested))
		b.mu.Lock()
		delete(b.falseClaims, claim.UID)
		b.mu.Unlock()
		// Update ResourceSlice so the scheduler sees reduced avail_* for
		// this device when allocating the next pod.
		b.driver.TriggerRepublish()

	case occupied:
		// Case 1: another claim from the same namespace already holds this
		// device. SHS14 would reject the second cxil_svc_set_vni_range with
		// -EEXIST. Tell the scheduler to put this claim on a different
		// device. ResourceSlice has no per-namespace state, so we cannot
		// steer scheduling via avail_* — Unschedulable is the only signal.
		b.logf("Unschedulable: claim=%s/%s device=%s — namespace %s already holds this device",
			claim.Namespace, claim.Name, result.Device, claim.Namespace)
		b.setCondition(ctx, claim, result, "Unschedulable", metav1.ConditionTrue,
			"NamespaceDeviceOccupied",
			fmt.Sprintf("namespace %s already has another claim on device %s; the scheduler will reallocate to a different device (SHS14 enforces VNI uniqueness per device)",
				claim.Namespace, result.Device))
		b.mu.Lock()
		delete(b.falseClaims, claim.UID)
		b.mu.Unlock()

	case requested.Fits(hwAvail):
		// Case 3: hardware would fit but in-flight pending grants are consuming
		// the remaining capacity. Add to falseClaims for re-evaluation when
		// TriggerRetry fires (after a pending service is actually created and
		// RemovePending is called).
		b.logf("Ready=False: claim=%s/%s device=%s requested=(%s)",
			claim.Namespace, claim.Name, result.Device, requested)
		b.setCondition(ctx, claim, result, "Ready", metav1.ConditionFalse,
			"InsufficientCapacity",
			fmt.Sprintf("requested resources (%s) do not fit after accounting for in-flight grants; waiting", requested))
		b.mu.Lock()
		b.falseClaims[claim.UID] = claim
		b.mu.Unlock()

	default:
		// Case 4: hardware is truly at capacity — the request cannot fit even
		// without counting any pending grants. Signal Unschedulable so the DRA
		// scheduler deallocates this (single, CXI-only) claim and retries on a
		// different device. The VNI is namespace-scoped and not part of the
		// claim, so there is no partial-allocation state on retry.
		b.logf("Unschedulable: claim=%s/%s device=%s requested=(%s) — device at capacity",
			claim.Namespace, claim.Name, result.Device, requested)
		b.setCondition(ctx, claim, result, "Unschedulable", metav1.ConditionTrue,
			"DeviceAtCapacity",
			fmt.Sprintf("device %s hardware capacity is exhausted; the scheduler will reallocate to a less-loaded device", result.Device))
		b.mu.Lock()
		delete(b.falseClaims, claim.UID)
		b.mu.Unlock()
		// Republish so the updated avail_* (near zero for the full device)
		// steers the retry allocation to a device with real capacity.
		b.driver.TriggerRepublish()
	}
}

func (b *ClaimBinder) onClaimDeleted(claim *resourceapi.ResourceClaim) {
	b.driver.RemovePending(claim.UID)
	b.mu.Lock()
	delete(b.falseClaims, claim.UID)
	b.mu.Unlock()
}

// reEvaluateFalse fetches fresh copies of all Ready=False claims and
// re-evaluates them. Fetching fresh copies avoids resource-version conflicts
// when calling UpdateStatus.
func (b *ClaimBinder) reEvaluateFalse(ctx context.Context) {
	b.mu.Lock()
	toEval := make([]*resourceapi.ResourceClaim, 0, len(b.falseClaims))
	for _, c := range b.falseClaims {
		toEval = append(toEval, c)
	}
	b.mu.Unlock()

	for _, stale := range toEval {
		latest, err := b.client.ResourceV1().ResourceClaims(stale.Namespace).Get(
			ctx, stale.Name, metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				b.logf("warning: getting claim %s/%s: %v", stale.Namespace, stale.Name, err)
			}
			b.mu.Lock()
			delete(b.falseClaims, stale.UID)
			b.mu.Unlock()
			continue
		}
		b.evaluateClaim(ctx, latest)
	}
}

// findOurDevice returns the allocation result for a CXI device on this node
// plus the parsed ResourceSet. Returns (nil, {}, nil) if the claim is not
// ours or has no CXI device allocated on this node.
func (b *ClaimBinder) findOurDevice(claim *resourceapi.ResourceClaim) (*resourceapi.DeviceRequestAllocationResult, cxi.ResourceSet, error) {
	if claim.Status.Allocation == nil {
		return nil, cxi.ResourceSet{}, nil
	}
	for i, alloc := range claim.Status.Allocation.Devices.Results {
		if alloc.Driver != b.driverName {
			continue
		}
		if alloc.Pool != b.nodeName {
			continue
		}
		requested, err := b.driver.resolveResources(claim, alloc)
		if err != nil {
			return nil, cxi.ResourceSet{}, fmt.Errorf("resolving resources: %w", err)
		}
		return &claim.Status.Allocation.Devices.Results[i], requested, nil
	}
	return nil, cxi.ResourceSet{}, nil
}

// getCondition returns the named condition from the claim's AllocatedDeviceStatus
// for the given allocation result, or nil if not found.
func (b *ClaimBinder) getCondition(
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	condType string,
) *metav1.Condition {
	for _, dev := range claim.Status.Devices {
		if dev.Driver != result.Driver || dev.Pool != result.Pool || dev.Device != result.Device {
			continue
		}
		for i, c := range dev.Conditions {
			if c.Type == condType {
				return &dev.Conditions[i]
			}
		}
	}
	return nil
}

// setCondition writes a condition to the claim's AllocatedDeviceStatus via
// UpdateStatus, retrying on resource-version conflict up to maxRetries times.
//
// result is used only to identify which device (Driver/Pool/Device) to attach
// the condition to. On each attempt we re-fetch the latest claim and re-find
// the current allocation result for that device — this avoids a validation
// error that occurs when AllowMultipleAllocations=true causes the scheduler
// to generate a ShareID on the allocation: if the claim was re-allocated
// between evaluateClaim and the UpdateStatus call, the stale ShareID from the
// original result would not match the new allocation and the API server would
// reject the write with "must be an allocated device in the claim".
//
// status.devices requires "update"/"patch" on the RBAC resource
// "resourceclaims/driver" (checked via SubjectAccessReview by the admission
// plugin) in addition to "update" on "resourceclaims/status" for the HTTP
// routing layer.
func (b *ClaimBinder) setCondition(
	ctx context.Context,
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	condType string,
	condStatus metav1.ConditionStatus,
	reason, message string,
) {
	cond := metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		latest, err := b.client.ResourceV1().ResourceClaims(claim.Namespace).Get(
			ctx, claim.Name, metav1.GetOptions{})
		if err != nil {
			if !k8serrors.IsNotFound(err) {
				b.logf("warning: getting claim %s/%s (attempt %d): %v",
					claim.Namespace, claim.Name, attempt+1, err)
			}
			return
		}

		// Re-find the current allocation result for this device in the
		// latest claim. The ShareID may differ from result if the scheduler
		// re-allocated the claim between evaluateClaim and this write.
		// If the device is no longer allocated, the condition is moot.
		latestResult := findAllocationResult(latest, result.Driver, result.Pool, result.Device)
		if latestResult == nil {
			return
		}

		upsertDeviceCondition(latest, latestResult, cond)
		_, err = b.client.ResourceV1().ResourceClaims(latest.Namespace).UpdateStatus(
			ctx, latest, metav1.UpdateOptions{})
		if err == nil {
			return
		}
		if k8serrors.IsConflict(err) {
			continue
		}
		b.logf("warning: updating %s condition for claim %s/%s: %v",
			condType, claim.Namespace, claim.Name, err)
		return
	}
	b.logf("warning: giving up updating %s condition for %s/%s after %d attempts",
		condType, claim.Namespace, claim.Name, maxRetries)
}

// findAllocationResult returns the first allocation result in the claim that
// matches the given driver, pool, and device name, or nil if not found.
func findAllocationResult(
	claim *resourceapi.ResourceClaim,
	driver, pool, device string,
) *resourceapi.DeviceRequestAllocationResult {
	if claim.Status.Allocation == nil {
		return nil
	}
	for i, r := range claim.Status.Allocation.Devices.Results {
		if r.Driver == driver && r.Pool == pool && r.Device == device {
			return &claim.Status.Allocation.Devices.Results[i]
		}
	}
	return nil
}

// upsertDeviceCondition sets or updates a condition in the claim's
// AllocatedDeviceStatus, creating the entry if it does not exist.
// The (Driver, Pool, Device, ShareID) tuple in AllocatedDeviceStatus must
// exactly match the corresponding DeviceRequestAllocationResult entry; the
// API server validates this when the driver subresource is patched.
func upsertDeviceCondition(
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	cond metav1.Condition,
) {
	// Normalise ShareID for comparison: nil and "" both map to "".
	resultShareID := ""
	if result.ShareID != nil {
		resultShareID = string(*result.ShareID)
	}

	for i, dev := range claim.Status.Devices {
		if dev.Driver != result.Driver || dev.Pool != result.Pool || dev.Device != result.Device {
			continue
		}
		devShareID := ""
		if dev.ShareID != nil {
			devShareID = *dev.ShareID
		}
		if devShareID != resultShareID {
			continue
		}
		for j, c := range dev.Conditions {
			if c.Type == cond.Type {
				if c.Status != cond.Status {
					claim.Status.Devices[i].Conditions[j] = cond
				}
				return
			}
		}
		claim.Status.Devices[i].Conditions = append(claim.Status.Devices[i].Conditions, cond)
		return
	}

	// No existing entry — create one. Copy ShareID so the tuple matches the
	// allocation result and passes the driver-subresource admission check.
	entry := resourceapi.AllocatedDeviceStatus{
		Driver:     result.Driver,
		Pool:       result.Pool,
		Device:     result.Device,
		Conditions: []metav1.Condition{cond},
	}
	if result.ShareID != nil {
		s := string(*result.ShareID)
		entry.ShareID = &s
	}
	claim.Status.Devices = append(claim.Status.Devices, entry)
}

func (b *ClaimBinder) logf(format string, args ...any) {
	fmt.Printf("cxi-claimbinder: "+format+"\n", args...)
}
