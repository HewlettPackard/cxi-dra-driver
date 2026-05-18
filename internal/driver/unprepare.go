package driver

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// UnprepareResourceClaims is called when pods using CXI claims finish or are
// removed. We release any VNI allocations and resource reservations held for
// these claims.
func (d *Driver) UnprepareResourceClaims(
	_ context.Context,
	claims []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		// Release resource reservations and store entries for every pod that
		// held this claim.
		for _, podUID := range d.store.PodUIDsForClaim(claim.UID) {
			d.tracker.Release(podUID)
			d.vniAllocator.Release(podUID)
			d.store.Delete(podUID)
		}
		d.store.DeleteClaim(claim.UID)
		result[claim.UID] = nil
	}
	return result, nil
}
