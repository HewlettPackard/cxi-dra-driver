package driver

import (
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/types"
)

// VNIAllocator is the interface for obtaining a VNI for a pod.
//
// Each pod must receive a unique VNI: cxil_svc_set_vni_range returns -EEXIST
// if the requested VNI range is already owned by another service on the same
// device.
//
// To plug in a new allocator, implement this interface and pass it to
// driver.New() in cmd/cxi-dra-driver/main.go.
type VNIAllocator interface {
	// Allocate returns a VNI for the given pod UID. Implementations must be
	// idempotent: repeated calls for the same podUID must return the same VNI.
	// Called from PrepareResourceClaims, before any containers start.
	Allocate(podUID types.UID) (int, error)

	// Release returns the VNI allocated to podUID back to the pool.
	// Called from UnprepareResourceClaims when the pod is torn down.
	Release(podUID types.UID)
}

// PoolVNIAllocator allocates unique VNIs from a configurable node-local range.
// VNIs are allocated in sequential order from min to max; released VNIs are
// returned to the pool and may be reused. Allocate is idempotent: calling it
// multiple times for the same podUID (e.g. when a pod has several claims)
// returns the same VNI.
//
// The allocator is node-local and in-memory. On driver restart, existing
// pod→VNI assignments are lost; service reconciliation on restart is tracked
// as a future work item.
type PoolVNIAllocator struct {
	mu    sync.Mutex
	min   int
	max   int
	inUse map[types.UID]int // podUID → allocated VNI
	byVNI map[int]types.UID // VNI → owning podUID (reverse index for O(1) vacancy check)
}

// NewPoolVNIAllocator creates a VNI allocator over the inclusive range [min, max].
// min must be ≥ 1 and max must be ≥ min.
func NewPoolVNIAllocator(min, max int) *PoolVNIAllocator {
	if min < 1 {
		panic(fmt.Sprintf("PoolVNIAllocator: min VNI must be >= 1, got %d", min))
	}
	if max < min {
		panic(fmt.Sprintf("PoolVNIAllocator: max VNI (%d) must be >= min (%d)", max, min))
	}
	return &PoolVNIAllocator{
		min:   min,
		max:   max,
		inUse: make(map[types.UID]int),
		byVNI: make(map[int]types.UID),
	}
}

// Allocate returns a unique VNI for podUID, searching sequentially from min to
// max. Idempotent: if podUID already has an allocated VNI the same value is
// returned without consuming a new slot.
func (a *PoolVNIAllocator) Allocate(podUID types.UID) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Idempotent: return the already-allocated VNI if present.
	if vni, ok := a.inUse[podUID]; ok {
		return vni, nil
	}

	for vni := a.min; vni <= a.max; vni++ {
		if _, taken := a.byVNI[vni]; !taken {
			a.inUse[podUID] = vni
			a.byVNI[vni] = podUID
			return vni, nil
		}
	}
	return 0, fmt.Errorf("VNI pool exhausted: range [%d, %d] fully allocated (%d pods)",
		a.min, a.max, len(a.inUse))
}

// Release returns the VNI allocated to podUID back to the pool.
// No-op if podUID has no allocation.
func (a *PoolVNIAllocator) Release(podUID types.UID) {
	a.mu.Lock()
	defer a.mu.Unlock()

	vni, ok := a.inUse[podUID]
	if !ok {
		return
	}
	delete(a.inUse, podUID)
	delete(a.byVNI, vni)
}
