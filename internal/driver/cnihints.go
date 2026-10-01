package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CNIHint is one claimed device's entry in a pod's CNI hint file. This is the
// read-side contract for the Slingshot CNI plugin: a
// CNI plugin invoked by containerd/CRI for RunPodSandbox has no access to
// this driver's in-process AllocationStore, so the allocation is handed off
// via a host file instead. See docs/cni/cnidra-cni-hint-contract.md for the full
// contract, including why ordering against CNI ADD is guaranteed.
type CNIHint struct {
	Device   string `json:"device"`
	HSNIface string `json:"hsn_iface"`
	VNI      int    `json:"vni"`
}

// cniHintPath returns the host path of the hint file for a pod.
func (d *Driver) cniHintPath(podUID types.UID) string {
	return filepath.Join(d.cniHintsDir, string(podUID)+".json")
}

// cniHintsFor builds the hint-file entries for a pod's current store
// allocation, one CNIHint per claimed device.
func cniHintsFor(allocs []PodAllocation) []CNIHint {
	hints := make([]CNIHint, 0, len(allocs))
	for _, a := range allocs {
		hints = append(hints, CNIHint{Device: a.DeviceName, HSNIface: a.HSNIface, VNI: a.VNI})
	}
	return hints
}

// writeCNIHint writes the hint file for podUID from its current store
// allocation (one CNIHint per claimed device). A no-op when --cni-hints-dir
// is empty. The write is atomic (temp file + rename) so a concurrent reader
// never observes a partially written file.
func (d *Driver) writeCNIHint(podUID types.UID) error {
	if d.cniHintsDir == "" {
		return nil
	}
	hints := cniHintsFor(d.store.Get(podUID))
	data, err := json.MarshalIndent(hints, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling CNI hint for pod %s: %w", podUID, err)
	}

	if err := os.MkdirAll(d.cniHintsDir, 0o755); err != nil {
		return fmt.Errorf("creating CNI hints dir %s: %w", d.cniHintsDir, err)
	}
	final := d.cniHintPath(podUID)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing CNI hint file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("renaming CNI hint file %s: %w", final, err)
	}
	return nil
}

// deleteCNIHint removes the hint file for podUID. A no-op when
// --cni-hints-dir is empty or the file does not exist.
func (d *Driver) deleteCNIHint(podUID types.UID) error {
	if d.cniHintsDir == "" {
		return nil
	}
	if err := os.Remove(d.cniHintPath(podUID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing CNI hint file for pod %s: %w", podUID, err)
	}
	return nil
}

// cniHintAnnotationKey is the pod annotation the driver best-effort writes
// with the same content as the hint file, purely for `kubectl describe pod`
// visibility. The CNI plugin MUST NOT read this annotation — it reads the
// hint file written by writeCNIHint. See docs/cni/cnidra-cni-hint-contract.md.
const cniHintAnnotationKey = "cxi.slingshot.hpe.com/net-hints"

// annotatePodCNIHints best-effort patches the pod's annotations with the same
// hints written to its hint file, as an observability-only side channel.
// Failures are logged and otherwise ignored — this is never load-bearing.
func (d *Driver) annotatePodCNIHints(ctx context.Context, namespace, podName string, hints []CNIHint) {
	if podName == "" {
		return
	}
	data, err := json.Marshal(hints)
	if err != nil {
		d.logf("warning: marshaling CNI hint annotation for pod %s/%s: %v", namespace, podName, err)
		return
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]string{cniHintAnnotationKey: string(data)},
		},
	})
	if err != nil {
		d.logf("warning: marshaling CNI hint annotation patch for pod %s/%s: %v", namespace, podName, err)
		return
	}
	if _, err := d.kubeClient.CoreV1().Pods(namespace).Patch(
		ctx, podName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		d.logf("warning: annotating pod %s/%s with CNI hints: %v", namespace, podName, err)
	}
}
