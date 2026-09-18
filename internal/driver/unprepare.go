package driver

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

// UnprepareResourceClaims is called when pods using CXI claims finish or are
// removed. We release store entries held for these claims.
// Note: CXI service destruction happens in the NRI StopPodSandbox hook, not here.
func (d *Driver) UnprepareResourceClaims(
	_ context.Context,
	claims []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	result := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		for _, podUID := range d.store.PodUIDsForClaim(claim.UID) {
			// Best-effort: the pod is going away regardless, and a leaked
			// hint file is cleaned up by the next write for that podUID
			// (podUIDs are not reused) or by the reaper's host, so a
			// removal failure must not block claim release.
			if err := d.deleteCNIHint(podUID); err != nil {
				d.logf("warning: removing CNI hint file for pod %s: %v", podUID, err)
			}
			d.store.Delete(podUID)
		}
		d.store.DeleteClaim(claim.UID)
		result[claim.UID] = nil
	}
	return result, nil
}
