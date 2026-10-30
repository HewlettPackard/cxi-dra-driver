package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

const (
	vfTestPool   = "node-cxi"
	vfTestParent = "cxi0-svc4"
)

var (
	vfTestBudget = cxi.ResourceSet{TXQs: 100, TGQs: 100, EQs: 100, CTs: 100, PTEs: 100, ACs: 100, LEs: 100, TLEs: 10}
	// vfTestEth is the Ethernet system child the kernel creates on every VF.
	vfTestEth = cxi.ResourceSet{TXQs: 1, TGQs: 1, EQs: 2, CTs: 1, PTEs: 1, ACs: 1, LEs: 16}
	// vfTestVNIs is the parent's VNI set; it covers testVNI.
	vfTestVNIs, _ = cxi.NewVNIRange(32, 63)
)

// newVFTestDriver returns a --vf-only driver with one parent, cxi0-svc4, whose
// VFs are cxi1..cxiN, each carrying an Ethernet system child.
func newVFTestDriver(t *testing.T, nVFs int, objs ...runtime.Object) *Driver {
	t.Helper()
	vfs := make([]*cxi.Device, nVFs)
	vfSvcs := make(map[string][]cxi.ServiceReservation, nVFs)
	for i := range vfs {
		name := fmt.Sprintf("cxi%d", i+1)
		vfs[i] = &cxi.Device{
			Name: name, Index: i + 1,
			FunctionType: cxi.FunctionVF, PFName: "cxi0", ParentSvcID: 4,
		}
		vfSvcs[name] = []cxi.ServiceReservation{{IsSystem: true, ResourceLimits: true, Res: vfTestEth}}
	}
	cxi.SetMockTopology(cxi.MockTopology{
		Parents: []cxi.MockParent{{
			PFName: "cxi0", SvcID: 4,
			Descriptor: cxi.ParentDescriptor{ResourceLimits: true, Limits: vfTestBudget, VNIs: vfTestVNIs},
		}},
		VFServices: vfSvcs,
	})
	t.Cleanup(cxi.ResetMockTopology)

	d := &Driver{
		driverName:    testDriver,
		nodeName:      vfTestPool,
		devices:       vfs,
		vfOnly:        true,
		store:         NewAllocationStore(),
		pending:       make(map[pendingKey]pendingEntry),
		recoveredBusy: make(map[string]string),
		kubeClient:    fake.NewSimpleClientset(append([]runtime.Object{annotatedNamespace()}, objs...)...),
	}
	d.setupParents()
	return d
}

// vfClaim is a claim for pod with one result on the test parent per share ID.
func vfClaim(uid, pod string, shareIDs ...string) *resourceapi.ResourceClaim {
	if len(shareIDs) == 0 {
		shareIDs = []string{uid + "-s0"}
	}
	results := make([]resourceapi.DeviceRequestAllocationResult, len(shareIDs))
	for i, s := range shareIDs {
		share := types.UID(s)
		results[i] = resourceapi.DeviceRequestAllocationResult{
			Driver: testDriver, Pool: vfTestPool, Device: vfTestParent, Request: "cxi", ShareID: &share,
		}
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid), Namespace: testNamespace, Name: uid},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfig()}},
		Status: resourceapi.ResourceClaimStatus{
			Allocation:  &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results}},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod, UID: types.UID(pod)}},
		},
	}
}

// prepareVFs prepares claim and returns the VFs its pod was given, in result order.
func prepareVFs(t *testing.T, d *Driver, claim *resourceapi.ResourceClaim) []string {
	t.Helper()
	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim(%s): %v", claim.UID, res.Err)
	}
	return deviceNames(d.store.Get(types.UID(claim.Status.ReservedFor[0].UID)))
}

func TestVFSelect_LowestFreeVFInOrder(t *testing.T) {
	d := newVFTestDriver(t, 4)
	for i, want := range []string{"cxi1", "cxi2", "cxi3"} {
		got := prepareVFs(t, d, vfClaim(fmt.Sprintf("c%d", i), fmt.Sprintf("p%d", i)))
		if !slices.Equal(got, []string{want}) {
			t.Fatalf("claim %d got %v, want [%s]", i, got, want)
		}
	}
}

// The pod gets the VF's CDI device and a store entry naming the VF, while
// kubelet still sees the parent it allocated.
func TestVFSelect_AllocationRecordsParentAndVF(t *testing.T) {
	d := newVFTestDriver(t, 2)
	claim := vfClaim("c1", "p1", "share-a")
	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
	if len(res.Devices) != 1 || res.Devices[0].DeviceName != vfTestParent {
		t.Fatalf("kubelet devices = %+v, want one %s", res.Devices, vfTestParent)
	}
	if !slices.Contains(res.Devices[0].CDIDeviceIDs, cdi.DeviceID(testDriver, "cxi1")) {
		t.Errorf("CDI IDs %v do not inject cxi1", res.Devices[0].CDIDeviceIDs)
	}
	got := d.store.Get("p1")
	if len(got) != 1 {
		t.Fatalf("store = %+v, want one allocation", got)
	}
	a := got[0]
	if a.DeviceName != "cxi1" || a.ParentName != vfTestParent || a.ShareID != "share-a" || a.AllocatedDevice() != vfTestParent {
		t.Errorf("allocation = %+v, want VF cxi1 under %s with ShareID share-a", a, vfTestParent)
	}
}

func TestVFSelect_SkipsBusyVFs(t *testing.T) {
	d := newVFTestDriver(t, 4)
	d.store.Set("other-pod", []PodAllocation{{DeviceName: "cxi1"}})
	d.recoveredBusy["cxi2"] = "child svc 9 without a hint file"
	if got := prepareVFs(t, d, vfClaim("c1", "p1")); !slices.Equal(got, []string{"cxi3"}) {
		t.Fatalf("got %v, want [cxi3]", got)
	}
}

func TestVFSelect_CountTwoGetsDistinctVFs(t *testing.T) {
	d := newVFTestDriver(t, 4)
	claim := vfClaim("c1", "p1", "s0", "s1")
	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
	if got := deviceNames(d.store.Get("p1")); !slices.Equal(got, []string{"cxi1", "cxi2"}) {
		t.Fatalf("got %v, want [cxi1 cxi2]", got)
	}
	if len(res.Devices) != 2 {
		t.Fatalf("kubelet devices = %+v, want 2", res.Devices)
	}
	for i, vf := range []string{"cxi1", "cxi2"} {
		if !slices.Contains(res.Devices[i].CDIDeviceIDs, cdi.DeviceID(testDriver, vf)) {
			t.Errorf("result %d CDI IDs %v do not inject %s", i, res.Devices[i].CDIDeviceIDs, vf)
		}
	}
}

func TestVFSelect_NoFreeVFErrors(t *testing.T) {
	d := newVFTestDriver(t, 2)
	prepareVFs(t, d, vfClaim("c1", "p1"))
	prepareVFs(t, d, vfClaim("c2", "p2"))
	res := d.prepareClaim(context.Background(), vfClaim("c3", "p3"))
	if res.Err == nil {
		t.Fatal("third claim on a 2-VF parent: want error")
	}
	for _, want := range []string{"parent cxi0-svc4 has no free VF", "cxi1 (claim c1, pod p1)", "cxi2 (claim c2, pod p2)"} {
		if !strings.Contains(res.Err.Error(), want) {
			t.Errorf("error %q does not contain %q", res.Err, want)
		}
	}
	if d.store.Get("p3") != nil || d.store.ClaimAllocs("c3") != nil {
		t.Error("failed prepare left store entries behind")
	}
}

// A claim needing more VFs than the parent has fails without taking any.
func TestVFSelect_CountAboveVFsErrorsAtomically(t *testing.T) {
	d := newVFTestDriver(t, 2)
	if res := d.prepareClaim(context.Background(), vfClaim("c1", "p1", "s0", "s1", "s2")); res.Err == nil {
		t.Fatal("count 3 on a 2-VF parent: want error")
	}
	if got := prepareVFs(t, d, vfClaim("c2", "p2")); !slices.Equal(got, []string{"cxi1"}) {
		t.Fatalf("after failed claim got %v, want [cxi1]", got)
	}
}

// kubelet may call Prepare again for a claim it already prepared; the claim
// must keep its VFs rather than be moved or take new ones.
func TestVFSelect_RePrepareReusesVF(t *testing.T) {
	d := newVFTestDriver(t, 4)
	prepareVFs(t, d, vfClaim("c1", "p1", "s0", "s1"))
	prepareVFs(t, d, vfClaim("c2", "p2"))
	if got := prepareVFs(t, d, vfClaim("c1", "p1", "s0", "s1")); !slices.Equal(got, []string{"cxi1", "cxi2"}) {
		t.Fatalf("re-prepare got %v, want [cxi1 cxi2]", got)
	}
	if got := prepareVFs(t, d, vfClaim("c3", "p3")); !slices.Equal(got, []string{"cxi4"}) {
		t.Fatalf("after re-prepare a new claim got %v, want [cxi4]", got)
	}
}

func TestVFSelect_UnprepareFreesVF(t *testing.T) {
	d := newVFTestDriver(t, 2)
	d.cniHintsDir = t.TempDir()
	prepareVFs(t, d, vfClaim("c1", "p1"))
	prepareVFs(t, d, vfClaim("c2", "p2"))
	if _, err := d.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: "c1"}}); err != nil {
		t.Fatalf("Unprepare: %v", err)
	}
	if got := prepareVFs(t, d, vfClaim("c3", "p3")); !slices.Equal(got, []string{"cxi1"}) {
		t.Fatalf("after unprepare got %v, want [cxi1]", got)
	}
}

// Concurrent Prepares on one parent must hand out each VF at most once: with
// 4 VFs and 8 claims, exactly 4 succeed on 4 distinct VFs.
func TestVFSelect_ConcurrentPrepareDistinctVFs(t *testing.T) {
	d := newVFTestDriver(t, 4)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = d.prepareClaim(context.Background(), vfClaim(fmt.Sprintf("c%d", i), fmt.Sprintf("p%d", i))).Err
		}()
	}
	wg.Wait()

	seen := make(map[string]string)
	failed := 0
	for i, err := range errs {
		if err != nil {
			if !strings.Contains(err.Error(), "has no free VF") {
				t.Errorf("claim %d: unexpected error %v", i, err)
			}
			failed++
			continue
		}
		got := d.store.Get(types.UID(fmt.Sprintf("p%d", i)))
		if len(got) != 1 {
			t.Fatalf("claim %d store = %+v", i, got)
		}
		if prev, dup := seen[got[0].DeviceName]; dup {
			t.Errorf("%s given to both %s and claim %d", got[0].DeviceName, prev, i)
		}
		seen[got[0].DeviceName] = fmt.Sprintf("claim %d", i)
	}
	if len(seen) != 4 || failed != 4 {
		t.Fatalf("succeeded on %d VFs with %d failures, want 4 and 4", len(seen), failed)
	}
}

// The parent is published with its budget minus one Ethernet reservation per
// VF, LEs and TLEs zero; the PF's VFs are not published themselves.
func TestVFSelect_PublishesParentCeiling(t *testing.T) {
	d := newVFTestDriver(t, 4)
	pubs := d.publishedDevices()
	if len(pubs) != 1 || pubs[0].name != vfTestParent {
		t.Fatalf("published = %+v, want one %s", pubs, vfTestParent)
	}
	want := cxi.ResourceSet{TXQs: 96, TGQs: 96, EQs: 92, CTs: 96, PTEs: 96, ACs: 96}
	if got := d.usableCeiling(vfTestParent); got != want {
		t.Errorf("ceiling = %s, want %s", got, want)
	}
}

func TestVFSelect_AvailableForUsesParentRemaining(t *testing.T) {
	d := newVFTestDriver(t, 4)
	got, err := d.AvailableFor(PodAllocation{DeviceName: "cxi2", ParentName: vfTestParent})
	if err != nil {
		t.Fatalf("AvailableFor: %v", err)
	}
	want := vfTestBudget.Sub(cxi.ResourceSet{TXQs: 4, TGQs: 4, EQs: 8, CTs: 4, PTEs: 4, ACs: 4, LEs: 64})
	if got != want {
		t.Errorf("available = %s, want %s", got, want)
	}
}

// Two results of one claim on the same parent are distinct pending grants
// that both count against the parent.
func TestVFSelect_PendingPerShareID(t *testing.T) {
	d := newVFTestDriver(t, 2)
	req := cxi.ResourceSet{TXQs: 60}
	avail := cxi.ResourceSet{TXQs: 100}
	if !d.checkAndAddPending("c1", vfTestParent, "s0", req, avail) {
		t.Fatal("first share: want granted")
	}
	if d.checkAndAddPending("c1", vfTestParent, "s1", req, avail) {
		t.Fatal("second share exceeds the parent with the first pending: want denied")
	}
	if !d.hasPending("c1", vfTestParent, "s0") || d.hasPending("c1", vfTestParent, "s1") {
		t.Fatal("pending entries are not tracked per ShareID")
	}
	d.reconcileClaimDevices("c1", map[pendingKey]bool{{deviceName: vfTestParent, shareID: "s1"}: true})
	if d.hasPending("c1", vfTestParent, "s0") {
		t.Error("reconcile kept a share the claim no longer holds")
	}
}

func writeHint(t *testing.T, dir, pod string, hints ...CNIHint) {
	t.Helper()
	data, err := json.Marshal(hints)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pod+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// After a restart, a pod whose hint file a live claim still reserves keeps its
// VF, and Unprepare of that claim releases the VF and the hint file.
func TestRecoverVFState_RestoresHintedPods(t *testing.T) {
	claim := vfClaim("c1", "p1")
	d := newVFTestDriver(t, 4, claim)
	d.cniHintsDir = t.TempDir()
	writeHint(t, d.cniHintsDir, "p1", CNIHint{Device: "cxi2", VNI: testVNI})
	writeHint(t, d.cniHintsDir, "stale", CNIHint{Device: "cxi3", VNI: testVNI})

	d.recoverVFState(context.Background())

	got := d.store.Get("p1")
	if len(got) != 1 || got[0].DeviceName != "cxi2" || got[0].ParentName != vfTestParent || got[0].VNI != testVNI {
		t.Fatalf("restored = %+v, want cxi2 under %s with VNI %d", got, vfTestParent, testVNI)
	}
	if !slices.Equal(d.store.PodUIDsForClaim("c1"), []types.UID{"p1"}) {
		t.Errorf("claim c1 pods = %v, want [p1]", d.store.PodUIDsForClaim("c1"))
	}
	// cxi2 is held; the stale hint leaves cxi3 free.
	for i, want := range []string{"cxi1", "cxi3"} {
		if got := prepareVFs(t, d, vfClaim(fmt.Sprintf("n%d", i), fmt.Sprintf("np%d", i))); !slices.Equal(got, []string{want}) {
			t.Fatalf("new claim %d got %v, want [%s]", i, got, want)
		}
	}
	// Re-prepare of the recovered claim keeps its VF.
	if got := prepareVFs(t, d, claim); !slices.Equal(got, []string{"cxi2"}) {
		t.Fatalf("re-prepare of recovered claim got %v, want [cxi2]", got)
	}

	if _, err := d.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: "c1"}}); err != nil {
		t.Fatalf("Unprepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.cniHintsDir, "p1.json")); !os.IsNotExist(err) {
		t.Errorf("hint file of p1 not removed by Unprepare after recovery: %v", err)
	}
	if got := prepareVFs(t, d, vfClaim("n2", "np2")); !slices.Equal(got, []string{"cxi2"}) {
		t.Fatalf("after unprepare got %v, want [cxi2]", got)
	}
}

// If the claims cannot be listed, every hinted VF stays busy.
func TestRecoverVFState_ListFailureKeepsHintedVFsBusy(t *testing.T) {
	d := newVFTestDriver(t, 4)
	d.kubeClient.(*fake.Clientset).PrependReactor("list", "resourceclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	d.cniHintsDir = t.TempDir()
	writeHint(t, d.cniHintsDir, "p1", CNIHint{Device: "cxi1", VNI: testVNI})

	d.recoverVFState(context.Background())

	if got := prepareVFs(t, d, vfClaim("c1", "p2")); !slices.Equal(got, []string{"cxi2"}) {
		t.Fatalf("got %v, want [cxi2]", got)
	}
}

// A VF with a pod child service but no hint file is busy until the reaper
// sees the child gone.
func TestRecoverVFState_ChildWithoutHintBusyUntilReaped(t *testing.T) {
	d := newVFTestDriver(t, 4)
	orig := readActiveServices
	t.Cleanup(func() { readActiveServices = orig })
	readActiveServices = func(dev string) ([]cxi.ServiceInfo, error) {
		svcs := []cxi.ServiceInfo{{SvcID: 2, IsSystem: true}}
		if dev == "cxi1" {
			svcs = append(svcs, cxi.ServiceInfo{SvcID: 9, VNI: testVNI})
		}
		return svcs, nil
	}

	d.recoverVFState(context.Background())

	if got := prepareVFs(t, d, vfClaim("c1", "p1")); !slices.Equal(got, []string{"cxi2"}) {
		t.Fatalf("got %v, want [cxi2]", got)
	}
	d.releaseRecovered("cxi1", []cxi.ServiceInfo{{SvcID: 9, VNI: testVNI}})
	if got := prepareVFs(t, d, vfClaim("c2", "p2")); !slices.Equal(got, []string{"cxi3"}) {
		t.Fatalf("child still present: got %v, want [cxi3]", got)
	}
	d.releaseRecovered("cxi1", []cxi.ServiceInfo{{SvcID: 2, IsSystem: true}})
	if got := prepareVFs(t, d, vfClaim("c3", "p3")); !slices.Equal(got, []string{"cxi1"}) {
		t.Fatalf("after child removed: got %v, want [cxi1]", got)
	}
}

// PF mode is untouched: the allocated device is the programmable device.
func TestVFSelect_PFModeUsesAllocatedDevice(t *testing.T) {
	d := newTestDriver()
	got, err := d.programmableFor("c1", []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0")})
	if err != nil || !slices.Equal(got, []string{"cxi0"}) {
		t.Fatalf("programmableFor = %v, %v; want [cxi0]", got, err)
	}
	if pubs := d.publishedDevices(); len(pubs) != 1 || pubs[0].name != "cxi0" {
		t.Fatalf("published = %+v, want [cxi0]", pubs)
	}
}
