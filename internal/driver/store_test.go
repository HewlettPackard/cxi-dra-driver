package driver

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// TestAllocationStoreSetCopiesInput is the regression test for H1: Set must
// snapshot its input slice, not alias it. prepareClaim reuses one podAllocs
// slice across every pod sharing a claim, mutating the VNI in place between
// Set calls. If Set stored by reference, all pods would end up with the last
// pod's VNI — silently scoping their CXI services to the wrong fabric network.
func TestAllocationStoreSetCopiesInput(t *testing.T) {
	s := NewAllocationStore()

	// Simulate prepareClaim's shared slice, reused across two pods on one claim.
	allocs := []PodAllocation{{DeviceName: "cxi0", VNI: 42}}
	s.Set(types.UID("pod-a"), allocs)

	allocs[0].VNI = 43 // next pod's VNI, written to the SAME backing array
	s.Set(types.UID("pod-b"), allocs)

	if got := s.Get(types.UID("pod-a"))[0].VNI; got != 42 {
		t.Errorf("pod-a VNI = %d, want 42 — Set must copy its input, not alias it", got)
	}
	if got := s.Get(types.UID("pod-b"))[0].VNI; got != 43 {
		t.Errorf("pod-b VNI = %d, want 43", got)
	}
}

// A pod that references more than one CXI claim has each claim's prepare call
// Set independently for the same pod UID. Set must MERGE by device, not
// replace: otherwise the second claim's prepare silently clobbers the first
// and NRI sees only one device. Re-Setting the same device overwrites it in
// place (idempotent re-prepare), and a new device is appended.
func TestAllocationStoreSetMergesByDevice(t *testing.T) {
	s := NewAllocationStore()
	pod := types.UID("pod-a")

	// Claim A prepares cxi0; claim B (same pod) prepares cxi1.
	s.Set(pod, []PodAllocation{{DeviceName: "cxi0", VNI: 42}})
	s.Set(pod, []PodAllocation{{DeviceName: "cxi1", VNI: 42}})

	got := s.Get(pod)
	if len(got) != 2 {
		t.Fatalf("after two single-device claims, Get = %+v (len %d), want both cxi0 and cxi1", got, len(got))
	}
	byDev := map[string]PodAllocation{}
	for _, a := range got {
		byDev[a.DeviceName] = a
	}
	if _, ok := byDev["cxi0"]; !ok {
		t.Errorf("cxi0 missing after merge; second Set clobbered the first")
	}
	if _, ok := byDev["cxi1"]; !ok {
		t.Errorf("cxi1 missing after merge")
	}

	// Re-prepare cxi0 with a new VNI: overwrite in place, do not duplicate.
	s.Set(pod, []PodAllocation{{DeviceName: "cxi0", VNI: 99}})
	got = s.Get(pod)
	if len(got) != 2 {
		t.Fatalf("after re-preparing cxi0, Get len = %d, want 2 (overwrite, not append)", len(got))
	}
	for _, a := range got {
		if a.DeviceName == "cxi0" && a.VNI != 99 {
			t.Errorf("cxi0 VNI = %d after re-prepare, want 99", a.VNI)
		}
	}
}
