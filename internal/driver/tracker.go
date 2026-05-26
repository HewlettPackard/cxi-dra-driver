package driver

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// ResourceTracker maintains a per-device view of reserved CXI resources across
// all active services on this node. It is the driver's runtime accounting layer,
// sitting between PrepareResourceClaims (admission) and the NRI plugin (service
// creation).
//
// At startup it is seeded with device capacity from hardware discovery. It does
// not query the live CXI service list at startup yet — that will be added when
// CGo/libcxi is available, allowing it to reconcile with services that survived
// a driver restart.
//
// Future "user asks for resources" model: when claims carry explicit resource
// requests, Admit() will check against Available() before accepting the claim,
// and Reserve()/Release() will track the negotiated quantities.
type ResourceTracker struct {
	mu       sync.Mutex
	capacity map[string]cxi.ResourceSet // device name → total hardware capacity
	reserved map[string]cxi.ResourceSet // device name → sum of reserved resources across active services
	podUsage map[types.UID]podUsage     // pod UID → which devices it reserved and how much
}

type deviceUsage struct {
	deviceName string
	resources  cxi.ResourceSet
}

// podUsage is an alias kept for clarity; a pod may have multiple device reservations.
type podUsage = []deviceUsage

// NewResourceTracker creates a ResourceTracker seeded with the capacity of each
// discovered device. Reserved counts start at zero; they will be reconciled with
// the live CXI service list once CGo/libcxi is available.
func NewResourceTracker(devices []*cxi.Device) *ResourceTracker {
	t := &ResourceTracker{
		capacity: make(map[string]cxi.ResourceSet, len(devices)),
		reserved: make(map[string]cxi.ResourceSet, len(devices)),
		podUsage: make(map[types.UID]podUsage),
	}
	for _, dev := range devices {
		t.capacity[dev.Name] = dev.Capacity
		t.reserved[dev.Name] = cxi.ResourceSet{} // zero reserved at startup
	}
	return t
}

// Available returns the ResourceSet for deviceName with Res fields reflecting
// how many resources are currently free (Max - reserved).
func (t *ResourceTracker) Available(deviceName string) (cxi.ResourceSet, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cap, ok := t.capacity[deviceName]
	if !ok {
		return cxi.ResourceSet{}, fmt.Errorf("unknown device %s", deviceName)
	}
	res := t.reserved[deviceName]

	// Return capacity with Res fields showing what is currently free.
	return cxi.ResourceSet{
		TXQs: cxi.ResourceLimits{Max: cap.TXQs.Max, Res: cap.TXQs.Max - res.TXQs.Res},
		TGQs: cxi.ResourceLimits{Max: cap.TGQs.Max, Res: cap.TGQs.Max - res.TGQs.Res},
		EQs:  cxi.ResourceLimits{Max: cap.EQs.Max, Res: cap.EQs.Max - res.EQs.Res},
		CTs:  cxi.ResourceLimits{Max: cap.CTs.Max, Res: cap.CTs.Max - res.CTs.Res},
		TLEs: cxi.ResourceLimits{Max: cap.TLEs.Max, Res: cap.TLEs.Max - res.TLEs.Res},
		PTEs: cxi.ResourceLimits{Max: cap.PTEs.Max, Res: cap.PTEs.Max - res.PTEs.Res},
		LEs:  cxi.ResourceLimits{Max: cap.LEs.Max, Res: cap.LEs.Max - res.LEs.Res},
		ACs:  cxi.ResourceLimits{Max: cap.ACs.Max, Res: cap.ACs.Max - res.ACs.Res},
	}, nil
}

// Admit checks whether requested resources fit within what is currently
// available on deviceName. Returns an error if the device is unknown or
// resources are exhausted.
func (t *ResourceTracker) Admit(deviceName string, requested cxi.ResourceSet) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	cap, ok := t.capacity[deviceName]
	if !ok {
		return fmt.Errorf("unknown device %s", deviceName)
	}
	res := t.reserved[deviceName]
	// Fits checks: res.Res + requested.Res <= cap.Max for each resource type.
	// We pass cap as the receiver (which carries the real Max values) and build
	// a synthetic "already used" set so Fits sees the correct headroom.
	used := cxi.ResourceSet{
		TXQs: cxi.ResourceLimits{Max: cap.TXQs.Max, Res: res.TXQs.Res},
		TGQs: cxi.ResourceLimits{Max: cap.TGQs.Max, Res: res.TGQs.Res},
		EQs:  cxi.ResourceLimits{Max: cap.EQs.Max, Res: res.EQs.Res},
		CTs:  cxi.ResourceLimits{Max: cap.CTs.Max, Res: res.CTs.Res},
		TLEs: cxi.ResourceLimits{Max: cap.TLEs.Max, Res: res.TLEs.Res},
		PTEs: cxi.ResourceLimits{Max: cap.PTEs.Max, Res: res.PTEs.Res},
		LEs:  cxi.ResourceLimits{Max: cap.LEs.Max, Res: res.LEs.Res},
		ACs:  cxi.ResourceLimits{Max: cap.ACs.Max, Res: res.ACs.Res},
	}
	if !used.Fits(requested) {
		return fmt.Errorf("insufficient resources on %s: requested %+v, capacity %+v, already reserved %+v",
			deviceName, requested, cap, res)
	}
	return nil
}

// Reserve records that podUID is consuming requested resources on deviceName.
// Multiple calls for the same podUID (one per device) are accumulated.
// Should be called after a successful Admit, within PrepareResourceClaims.
func (t *ResourceTracker) Reserve(podUID types.UID, deviceName string, requested cxi.ResourceSet) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reserved[deviceName] = t.reserved[deviceName].Add(requested)
	t.podUsage[podUID] = append(t.podUsage[podUID], deviceUsage{deviceName: deviceName, resources: requested})
}

// Release removes all resource reservations for podUID, returning them to the
// available pool. Called from UnprepareResourceClaims.
func (t *ResourceTracker) Release(podUID types.UID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	usages, ok := t.podUsage[podUID]
	if !ok {
		return
	}
	for _, u := range usages {
		t.reserved[u.deviceName] = t.reserved[u.deviceName].Sub(u.resources)
	}
	delete(t.podUsage, podUID)
}
