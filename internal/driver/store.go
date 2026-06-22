package driver

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// PodAllocation records which CXI devices were allocated to a pod.
// ClaimUID/ClaimNamespace/ClaimName attribute each device back to its
// source claim so the NRI plugin can publish the assigned svc_id to
// the right RC.Status.Devices[] entry after CreateService.
type PodAllocation struct {
	DeviceName     string          // e.g. "cxi0"
	DevPath        string          // e.g. "/dev/cxi0" — host path of the character device
	HSNIface       string          // e.g. "hsn2"
	VNI            int             // Virtual Network Identifier for the Slingshot fabric service
	Resources      cxi.ResourceSet // negotiated per-service resource limits
	ClaimUID       types.UID
	ClaimNamespace string
	ClaimName      string
}

// AllocationStore is a concurrency-safe map of pod UID → allocated CXI devices.
// It is written by PrepareResourceClaims (DRA side) and read by the NRI plugin
// in RunPodSandbox, where the network namespace is first available.
type AllocationStore struct {
	mu          sync.Mutex
	byPodUID    map[types.UID][]PodAllocation
	claimToPods map[types.UID][]types.UID // claim UID → pod UIDs that reserved it
	podToClaim  map[types.UID]types.UID   // pod UID → CXI claim UID (reverse map)
}

func NewAllocationStore() *AllocationStore {
	return &AllocationStore{
		byPodUID:    make(map[types.UID][]PodAllocation),
		claimToPods: make(map[types.UID][]types.UID),
		podToClaim:  make(map[types.UID]types.UID),
	}
}

// Set merges allocs into the pod's stored allocation, keyed by device name:
// a device already recorded for the pod is overwritten, new devices are
// appended. This makes a pod that references more than one CXI claim
// accumulate every claim's device(s) instead of the last writer silently
// clobbering the earlier ones (each claim's prepare calls Set independently
// for the same pod UID). Re-preparing the same claim is idempotent because the
// device names match and overwrite in place.
//
// The input slice is not retained — entries are copied into the stored slice,
// so later mutation of the caller's slice cannot alter the stored allocation
// (prepareClaim reuses one slice across every pod sharing a claim).
func (s *AllocationStore) Set(podUID types.UID, allocs []PodAllocation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing := s.byPodUID[podUID]
	merged := make([]PodAllocation, len(existing))
	copy(merged, existing)
	for _, a := range allocs {
		idx := -1
		for i := range merged {
			if merged[i].DeviceName == a.DeviceName {
				idx = i
				break
			}
		}
		if idx >= 0 {
			merged[idx] = a
		} else {
			merged = append(merged, a)
		}
	}
	s.byPodUID[podUID] = merged
}

// SetClaimPods records that claimUID was reserved by podUID. Multiple calls
// accumulate pod UIDs for the same claim. Also populates the reverse map so
// the NRI plugin can look up the claim UID from a pod UID after service creation.
func (s *AllocationStore) SetClaimPod(claimUID, podUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimToPods[claimUID] = append(s.claimToPods[claimUID], podUID)
	s.podToClaim[podUID] = claimUID
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
	delete(s.podToClaim, podUID)
}

// ClaimForPod returns the CXI claim UID for a pod, or "" if none is recorded.
// Used by the NRI plugin to call RemovePending after a service is created.
func (s *AllocationStore) ClaimForPod(podUID types.UID) types.UID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.podToClaim[podUID]
}

// DeleteClaim removes the claim → pod mapping (called on teardown).
func (s *AllocationStore) DeleteClaim(claimUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimToPods, claimUID)
}
