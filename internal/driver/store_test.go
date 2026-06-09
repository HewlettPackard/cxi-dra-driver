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
