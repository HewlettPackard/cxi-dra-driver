package driver

import (
	"slices"
	"sync"

	"k8s.io/apimachinery/pkg/types"

	"github.com/HewlettPackard/cxi-dra-driver/internal/cxi"
)

// PodAllocation records which CXI devices were allocated to a pod.
// ClaimUID/ClaimNamespace/ClaimName attribute each device back to its
// source claim so the NRI plugin can publish the assigned svc_id to
// the right RC.Status.Devices[] entry after CreateService.
type PodAllocation struct {
	DeviceName     string          // programmable device: the PF ("cxi0") or, in VF mode, the VF chosen at Prepare ("cxi5")
	DevPath        string          // e.g. "/dev/cxi0" — host path of the character device
	HSNIface       string          // host link the CNI attaches to, e.g. "hsn2" or a VF netdev
	PodIfName      string          // pod-side CNI/sysfs name; empty preserves PF-mode HSNIface naming
	VNI            int             // Virtual Network Identifier for the Slingshot fabric service
	Resources      cxi.ResourceSet // negotiated per-service resource limits
	ClaimUID       types.UID
	ClaimNamespace string
	ClaimName      string

	// ParentName is the published parent ("cxi0-svc4") the VF was selected
	// under; empty in PF mode, where the allocated device is DeviceName.
	ParentName string
	// ShareID is the allocation result's ShareID ("" when unset). Results of
	// one claim on the same parent differ only by it.
	ShareID string
}

// AllocatedDevice is the device name the scheduler allocated: the parent in VF
// mode, the PF otherwise.
func (a PodAllocation) AllocatedDevice() string {
	if a.ParentName != "" {
		return a.ParentName
	}
	return a.DeviceName
}

// AllocationStore is a concurrency-safe map of pod UID → allocated CXI devices.
// It is written by PrepareResourceClaims (DRA side) and read by the NRI plugin
// in RunPodSandbox, where the network namespace is first available.
type AllocationStore struct {
	mu          sync.Mutex
	byPodUID    map[types.UID][]PodAllocation
	claimToPods map[types.UID][]types.UID     // claim UID → pod UIDs that reserved it
	podToClaim  map[types.UID]types.UID       // pod UID → CXI claim UID (reverse map)
	claimAllocs map[types.UID][]PodAllocation // claim UID → allocation recorded when it was prepared here
}

func NewAllocationStore() *AllocationStore {
	return &AllocationStore{
		byPodUID:    make(map[types.UID][]PodAllocation),
		claimToPods: make(map[types.UID][]types.UID),
		podToClaim:  make(map[types.UID]types.UID),
		claimAllocs: make(map[types.UID][]PodAllocation),
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
// accumulate pod UIDs for the same claim; recording the same pod again is a
// no-op. Also populates the reverse map so the NRI plugin can look up the
// claim UID from a pod UID after service creation.
func (s *AllocationStore) SetClaimPod(claimUID, podUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.claimToPods[claimUID], podUID) {
		s.claimToPods[claimUID] = append(s.claimToPods[claimUID], podUID)
	}
	s.podToClaim[podUID] = claimUID
}

// HasClaimPod reports whether podUID was ever recorded for claimUID. It stays
// true after Delete(podUID), so a stopped pod still listed in the claim's
// ReservedFor is not recorded again.
func (s *AllocationStore) HasClaimPod(claimUID, podUID types.UID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.claimToPods[claimUID], podUID)
}

// ForgetClaimPod undoes SetClaimPod for podUID, so HasClaimPod reports false
// again and the pod is picked up by the next recordSharedClaimPods pass. Used
// when the pod's CNI hint could not be written.
func (s *AllocationStore) ForgetClaimPod(claimUID, podUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pods := slices.DeleteFunc(slices.Clone(s.claimToPods[claimUID]), func(u types.UID) bool { return u == podUID }); len(pods) > 0 {
		s.claimToPods[claimUID] = pods
	} else {
		delete(s.claimToPods, claimUID)
	}
	if s.podToClaim[podUID] == claimUID {
		delete(s.podToClaim, podUID)
	}
}

// SetClaimAllocs records the allocation prepared for claimUID on this node,
// for pods that reserve the claim after it was prepared.
func (s *AllocationStore) SetClaimAllocs(claimUID types.UID, allocs []PodAllocation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimAllocs[claimUID] = slices.Clone(allocs)
}

// ClaimAllocs returns a copy of the allocation prepared for claimUID, or nil
// if the claim was not prepared on this node.
func (s *AllocationStore) ClaimAllocs(claimUID types.UID) []PodAllocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.claimAllocs[claimUID])
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

// DeleteClaim removes the claim → pod mapping and the claim's prepared
// allocation (called on teardown).
func (s *AllocationStore) DeleteClaim(claimUID types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.claimToPods, claimUID)
	delete(s.claimAllocs, claimUID)
}

// DeviceHolders maps every programmable device named in a stored allocation to
// who holds it ("pod <uid>", "claim <uid>"). A prepared claim counts until
// DeleteClaim, so a VF stays held between Prepare and the pod's first record.
func (s *AllocationStore) DeviceHolders() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string)
	for uid, allocs := range s.byPodUID {
		for _, a := range allocs {
			out[a.DeviceName] = append(out[a.DeviceName], "pod "+string(uid))
		}
	}
	for uid, allocs := range s.claimAllocs {
		for _, a := range allocs {
			out[a.DeviceName] = append(out[a.DeviceName], "claim "+string(uid))
		}
	}
	for _, h := range out {
		slices.Sort(h)
	}
	return out
}
