package driver

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// PodAllocation records which CXI devices were allocated to a pod.
type PodAllocation struct {
	DeviceName string          // e.g. "cxi0"
	DevPath    string          // e.g. "/dev/cxi0" — host path of the character device
	HSNIface   string          // e.g. "hsn2"
	VNI        int             // Virtual Network Identifier for the Slingshot fabric service
	Resources  cxi.ResourceSet // negotiated per-service resource limits
}

// AllocationStore is a concurrency-safe map of pod UID → allocated CXI devices.
// It is written by PrepareResourceClaims (DRA side) and read by the NRI plugin
// in RunPodSandbox, where the network namespace is first available.
type AllocationStore struct {
	mu          sync.Mutex
	byPodUID    map[types.UID][]PodAllocation
	claimToPods map[types.UID][]types.UID // claim UID → pod UIDs that reserved it
}

func NewAllocationStore() *AllocationStore {
	return &AllocationStore{
		byPodUID:    make(map[types.UID][]PodAllocation),
		claimToPods: make(map[types.UID][]types.UID),
	}
}

// Set stores the allocations for a pod, replacing any previous entry. The input
// slice is copied so later mutation of the caller's slice cannot alter the stored
// allocation — prepareClaim reuses one slice across every pod sharing a claim.
func (s *AllocationStore) Set(podUID types.UID, allocs []PodAllocation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := make([]PodAllocation, len(allocs))
	copy(stored, allocs)
	s.byPodUID[podUID] = stored
}

// SetClaimPods records that claimUID was reserved by podUID. Multiple calls
// accumulate pod UIDs for the same claim.
func (s *AllocationStore) SetClaimPod(claimUID, podUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimToPods[claimUID] = append(s.claimToPods[claimUID], podUID)
}

// PodUIDsForClaim returns all pod UIDs that reserved the given claim.
func (s *AllocationStore) PodUIDsForClaim(claimUID types.UID) []types.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimToPods[claimUID]
}

// Get returns the allocations for a pod, or nil if none are recorded.
func (s *AllocationStore) Get(podUID types.UID) []PodAllocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byPodUID[podUID]
}

// Delete removes the allocation record for a pod (called on teardown).
func (s *AllocationStore) Delete(podUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byPodUID, podUID)
}

// DeleteClaim removes the claim → pod mapping (called on teardown).
func (s *AllocationStore) DeleteClaim(claimUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimToPods, claimUID)
}
