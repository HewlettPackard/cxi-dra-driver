package driver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cni"
)

func readHintFile(t *testing.T, dir string, podUID types.UID) []CNIHint {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, string(podUID)+".json"))
	if err != nil {
		t.Fatalf("reading hint file: %v", err)
	}
	var hints []CNIHint
	if err := json.Unmarshal(data, &hints); err != nil {
		t.Fatalf("unmarshaling hint file: %v", err)
	}
	return hints
}

func TestWriteCNIHint_SingleDevice(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()
	pod := types.UID("pod-a")
	d.store.Set(pod, []PodAllocation{{DeviceName: "cxi0", HSNIface: "hsn0", VNI: 137}})

	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint: %v", err)
	}

	got := readHintFile(t, d.cniHintsDir, pod)
	want := []CNIHint{{Device: "cxi0", HSNIface: "hsn0", VNI: 137}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("hint file = %+v, want %+v", got, want)
	}
	data, err := os.ReadFile(d.cniHintPath(pod))
	if err != nil {
		t.Fatalf("reading hint JSON: %v", err)
	}
	const wantJSON = `[
  {
    "device": "cxi0",
    "hsn_iface": "hsn0",
    "vni": 137
  }
]`
	if string(data) != wantJSON {
		t.Errorf("PF hint JSON = %s, want unchanged unversioned schema:\n%s", data, wantJSON)
	}
}

func TestWriteCNIHint_VFNameRoundTrip(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()
	pod := types.UID("pod-vf")
	d.store.Set(pod, []PodAllocation{{
		DeviceName: "cxi4",
		HSNIface:   "enp197s0f1",
		PodIfName:  "cxi4",
		VNI:        137,
	}})

	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint: %v", err)
	}

	got, err := cni.ReadHint(d.cniHintsDir, string(pod), 0, 0)
	if err != nil {
		t.Fatalf("CNI ReadHint: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("CNI parsed %d hints, want one", len(got))
	}
	if got[0].SchemaVersion != 1 ||
		got[0].Device != "cxi4" ||
		got[0].HSNIface != "enp197s0f1" ||
		got[0].IfName != "cxi4" ||
		got[0].VNI != 137 {
		t.Errorf("CNI parsed hint = %+v, want v1 cxi4 on host parent enp197s0f1, VNI 137", got[0])
	}

	if _, err := d.kubeClient.CoreV1().Pods(testNamespace).Create(
		context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vf-pod", Namespace: testNamespace}},
		metav1.CreateOptions{},
	); err != nil {
		t.Fatalf("creating pod for annotation test: %v", err)
	}
	d.annotatePodCNIHints(context.Background(), testNamespace, "vf-pod", cniHintsFor(d.store.Get(pod)))
	annotated, err := d.kubeClient.CoreV1().Pods(testNamespace).Get(context.Background(), "vf-pod", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading annotated pod: %v", err)
	}
	var annotationHints []CNIHint
	if err := json.Unmarshal([]byte(annotated.Annotations[cniHintAnnotationKey]), &annotationHints); err != nil {
		t.Fatalf("parsing pod CNI hint annotation: %v", err)
	}
	if len(annotationHints) != 1 || annotationHints[0] != (CNIHint{
		SchemaVersion: 1,
		Device:        "cxi4",
		HSNIface:      "enp197s0f1",
		IfName:        "cxi4",
		VNI:           137,
	}) {
		t.Errorf("pod CNI hint annotation = %+v, want the same VF hint as the file", annotationHints)
	}
}

func TestWriteCNIHint_MultiDevice(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()
	pod := types.UID("pod-a")
	d.store.Set(pod, []PodAllocation{
		{DeviceName: "cxi0", HSNIface: "hsn0", VNI: 137},
		{DeviceName: "cxi1", HSNIface: "hsn1", VNI: 137},
	})

	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint: %v", err)
	}

	got := readHintFile(t, d.cniHintsDir, pod)
	if len(got) != 2 {
		t.Fatalf("hint file = %+v, want 2 entries", got)
	}
	byDev := map[string]CNIHint{}
	for _, h := range got {
		byDev[h.Device] = h
	}
	if h, ok := byDev["cxi0"]; !ok || h.HSNIface != "hsn0" || h.VNI != 137 {
		t.Errorf("cxi0 entry = %+v, ok=%v", h, ok)
	}
	if h, ok := byDev["cxi1"]; !ok || h.HSNIface != "hsn1" || h.VNI != 137 {
		t.Errorf("cxi1 entry = %+v, ok=%v", h, ok)
	}
}

func TestWriteCNIHint_DisabledIsNoop(t *testing.T) {
	d := newTestDriver()
	// cniHintsDir left as "" (disabled).
	pod := types.UID("pod-a")
	d.store.Set(pod, []PodAllocation{{DeviceName: "cxi0", HSNIface: "hsn0", VNI: 137}})

	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint: %v", err)
	}
}

func TestWriteCNIHint_OverwritesExisting(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()
	pod := types.UID("pod-a")

	d.store.Set(pod, []PodAllocation{{DeviceName: "cxi0", HSNIface: "hsn0", VNI: 137}})
	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint (1st): %v", err)
	}

	d.store.Set(pod, []PodAllocation{{DeviceName: "cxi0", HSNIface: "hsn0", VNI: 200}})
	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint (2nd): %v", err)
	}

	got := readHintFile(t, d.cniHintsDir, pod)
	if len(got) != 1 || got[0].VNI != 200 {
		t.Fatalf("hint file = %+v, want single entry with VNI 200", got)
	}
}

func TestDeleteCNIHint_RemovesFile(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()
	pod := types.UID("pod-a")
	d.store.Set(pod, []PodAllocation{{DeviceName: "cxi0", HSNIface: "hsn0", VNI: 137}})

	if err := d.writeCNIHint(pod); err != nil {
		t.Fatalf("writeCNIHint: %v", err)
	}
	path := d.cniHintPath(pod)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("hint file not created: %v", err)
	}

	if err := d.deleteCNIHint(pod); err != nil {
		t.Fatalf("deleteCNIHint: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hint file still present after delete: err=%v", err)
	}
}

func TestDeleteCNIHint_MissingFileNoError(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()

	if err := d.deleteCNIHint(types.UID("never-written")); err != nil {
		t.Fatalf("deleteCNIHint on missing file: %v", err)
	}
}

func TestDeleteCNIHint_DisabledIsNoop(t *testing.T) {
	d := newTestDriver()
	// cniHintsDir left as "" (disabled).
	if err := d.deleteCNIHint(types.UID("pod-a")); err != nil {
		t.Fatalf("deleteCNIHint: %v", err)
	}
}

// End-to-end: PrepareResourceClaims writes the hint file, UnprepareResourceClaims removes it.
func TestPrepareUnprepare_CNIHintLifecycle(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-1"), Namespace: testNamespace, Name: "claim-1"},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfig()}},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0")},
				},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", Name: "pod-a", UID: "pod-a"},
			},
		},
	}

	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}

	got := readHintFile(t, d.cniHintsDir, "pod-a")
	if len(got) != 1 || got[0].Device != "cxi0" || got[0].VNI != testVNI {
		t.Fatalf("hint file = %+v, want one cxi0 entry with VNI %d", got, testVNI)
	}

	if _, err := d.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{
		{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "claim-1"}, UID: claim.UID},
	}); err != nil {
		t.Fatalf("UnprepareResourceClaims: %v", err)
	}

	if _, err := os.Stat(d.cniHintPath("pod-a")); !os.IsNotExist(err) {
		t.Fatalf("hint file still present after unprepare: err=%v", err)
	}
}
