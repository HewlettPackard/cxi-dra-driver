package driver

import "k8s.io/apimachinery/pkg/types"

// VNIAllocator is the interface for obtaining a VNI for a pod.
//
// The current implementation (StaticVNIAllocator) returns a hardcoded value.
// The next phase will replace this with a real allocator that coordinates with
// a cluster-wide VNI pool ResourceSlice, ensuring each workload gets a unique
// VNI scoped to the Slingshot fabric.
//
// To plug in a new allocator, implement this interface and pass it to
// driver.New() in cmd/cxi-dra-driver/main.go.
type VNIAllocator interface {
	// Allocate returns a VNI for the given pod UID.
	// It is called from PrepareResourceClaims, before any containers start.
	Allocate(podUID types.UID) (int, error)

	// Release is called when the pod's last container has stopped, allowing
	// the allocator to return the VNI to the pool.
	Release(podUID types.UID)
}

// StaticVNIAllocator always returns the same VNI.
// This is a placeholder until the cluster-wide VNI pool is implemented.
type StaticVNIAllocator struct {
	VNI int
}

func (s *StaticVNIAllocator) Allocate(_ types.UID) (int, error) {
	return s.VNI, nil
}

func (s *StaticVNIAllocator) Release(_ types.UID) {
	// Nothing to release — static allocation.
}
