package driver

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

const (
	testDriver    = "cxi.slingshot.hpe.com"
	testNamespace = "default"
	testVNI       = 42
)

func cxiResult(device string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{Driver: testDriver, Pool: "node-cxi", Device: device, Request: "cxi"}
}

func cxiConfig() []resourceapi.DeviceClaimConfiguration {
	return cxiConfigWithProfiles(`[]`)
}

func cxiConfigWithProfiles(profilesJSON string) []resourceapi.DeviceClaimConfiguration {
	raw := `{"txqs":2,"tgqs":1,"eqs":1,"cts":1,"ptes":1,"acs":1,"le_pools":0,"tle_pools":0,"cdi_profiles":` + profilesJSON + `}`
	return []resourceapi.DeviceClaimConfiguration{
		{
			Requests: []string{"cxi"},
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver:     testDriver,
					Parameters: runtime.RawExtension{Raw: []byte(raw)},
				},
			},
		},
	}
}

// annotatedNamespace returns the default namespace annotated with testVNI.
func annotatedNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        testNamespace,
			Annotations: map[string]string{vniAnnotationKey: "42"},
		},
	}
}

func newTestDriver() *Driver {
	return &Driver{
		driverName: testDriver,
		devices:    cxi.MockDevices(1), // "cxi0"
		store:      NewAllocationStore(),
		kubeClient: fake.NewSimpleClientset(annotatedNamespace()),
	}
}

func TestNamespaceVNI_ReadsAnnotation(t *testing.T) {
	d := newTestDriver()
	vni, err := d.namespaceVNI(context.Background(), testNamespace)
	if err != nil {
		t.Fatalf("namespaceVNI: %v", err)
	}
	if vni != testVNI {
		t.Fatalf("vni = %d, want %d", vni, testVNI)
	}
}

func TestNamespaceVNI_MissingAnnotationErrors(t *testing.T) {
	d := &Driver{
		driverName: testDriver,
		devices:    cxi.MockDevices(1),
		store:      NewAllocationStore(),
		kubeClient: fake.NewSimpleClientset(&corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: testNamespace},
		}),
	}
	// Use a short deadline so the test exercises the give-up path without
	// waiting out the full production retry budget (10×1s).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := d.namespaceVNI(ctx, testNamespace); err == nil {
		t.Fatal("expected error for namespace without VNI annotation")
	}
}

// All pods sharing a claim in one namespace must receive the namespace's VNI,
// and the logical work must inject exactly the CXI device.
func TestPrepareClaimAssignsNamespaceVNI(t *testing.T) {
	d := newTestDriver()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-1"), Namespace: testNamespace},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfig()}},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0")},
				},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", UID: "pod-a"},
				{Resource: "pods", UID: "pod-b"},
			},
		},
	}

	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim returned error: %v", res.Err)
	}
	if len(res.Devices) != 1 || res.Devices[0].DeviceName != "cxi0" {
		t.Fatalf("devices = %+v, want one cxi0", res.Devices)
	}
	for _, pod := range []types.UID{"pod-a", "pod-b"} {
		got := d.store.Get(pod)
		if len(got) != 1 || got[0].VNI != testVNI {
			t.Errorf("pod %s stored allocation = %+v, want one entry with VNI %d", pod, got, testVNI)
		}
	}
}

// cdiTestClaim returns a claim for the given devices whose opaque config
// carries profilesJSON as its cdi_profiles literal.
func cdiTestClaim(profilesJSON string, devices ...string) *resourceapi.ResourceClaim {
	results := make([]resourceapi.DeviceRequestAllocationResult, 0, len(devices))
	for _, dev := range devices {
		results = append(results, cxiResult(dev))
	}
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: testNamespace, UID: "claim-uid-1"},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfigWithProfiles(profilesJSON)}},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", UID: "pod-a"},
			},
		},
	}
}

// TestPrepareClaimCDIDeviceIDs verifies every device gets its own cxi= ID
// plus the shared env ID, whether or not host libs are configured, and that
// the deprecated cdi_profiles field accepts only "env".
func TestPrepareClaimCDIDeviceIDs(t *testing.T) {
	wantIDs := []string{testDriver + "/cxi=cxi0", testDriver + "/env=env"}
	for _, env := range []struct {
		name string
		cfg  cdi.CDIEnvConfig
	}{
		{"env configured", cdi.CDIEnvConfig{Libs: []string{"/usr/lib64/libcxi.so.1"}}},
		{"env disabled", cdi.CDIEnvConfig{}},
	} {
		for _, profiles := range []string{`null`, `[]`, `["env"]`} {
			t.Run(env.name+"/"+profiles, func(t *testing.T) {
				d := &Driver{
					driverName:   testDriver,
					devices:      cxi.MockDevices(1), // "cxi0"
					store:        NewAllocationStore(),
					cdiEnvConfig: env.cfg,
					kubeClient:   fake.NewSimpleClientset(annotatedNamespace()),
				}
				result := d.prepareClaim(context.Background(), cdiTestClaim(profiles, "cxi0"))
				if result.Err != nil {
					t.Fatalf("prepareClaim error: %v", result.Err)
				}
				if len(result.Devices) != 1 {
					t.Fatalf("got %d devices, want 1", len(result.Devices))
				}
				got := result.Devices[0].CDIDeviceIDs
				if len(got) != len(wantIDs) {
					t.Fatalf("CDIDeviceIDs = %v, want %v", got, wantIDs)
				}
				for i, id := range got {
					if id != wantIDs[i] {
						t.Errorf("CDIDeviceIDs[%d] = %q, want %q", i, id, wantIDs[i])
					}
				}
			})
		}
	}
}

// A multi-device claim (e.g. count: 2) must reference the shared env spec
// exactly once across all devices. It is a driver-global singleton; emitting
// it per device would make containerd apply its bind-mounts twice and reject
// the container. Per-device cxi= IDs are still emitted for every device.
func TestPrepareClaimCDIDeviceIDsMultiDevice(t *testing.T) {
	d := &Driver{
		driverName: testDriver,
		devices:    cxi.MockDevices(2), // "cxi0", "cxi1"
		store:      NewAllocationStore(),
		kubeClient: fake.NewSimpleClientset(annotatedNamespace()),
	}

	result := d.prepareClaim(context.Background(), cdiTestClaim(`[]`, "cxi0", "cxi1"))
	if result.Err != nil {
		t.Fatalf("prepareClaim error: %v", result.Err)
	}
	if len(result.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(result.Devices))
	}

	counts := map[string]int{}
	for _, dev := range result.Devices {
		for _, id := range dev.CDIDeviceIDs {
			counts[id]++
		}
	}
	for _, dev := range result.Devices {
		if want := cdi.DeviceID(testDriver, dev.DeviceName); counts[want] != 1 {
			t.Errorf("device %s: own CDI ID %q appears %d times; got %v", dev.DeviceName, want, counts[want], dev.CDIDeviceIDs)
		}
	}
	if id := cdi.EnvDeviceID(testDriver); counts[id] != 1 {
		t.Errorf("env CDI ID %q appears %d times across devices, want exactly 1 (counts=%v)", id, counts[id], counts)
	}
}

// Claims still asking for the nccl/aws-ofi-nccl profiles, now provided by the
// k8s-modules, are rejected with a pointer to it.
func TestPrepareClaimRejectsRemovedProfiles(t *testing.T) {
	for _, profiles := range []string{`["nccl"]`, `["env","aws-ofi-nccl"]`, `["bogus"]`} {
		t.Run(profiles, func(t *testing.T) {
			d := &Driver{
				driverName: testDriver,
				devices:    cxi.MockDevices(1),
				store:      NewAllocationStore(),
				kubeClient: fake.NewSimpleClientset(annotatedNamespace()),
			}
			res := d.prepareClaim(context.Background(), cdiTestClaim(profiles, "cxi0"))
			if res.Err == nil {
				t.Fatal("expected error from prepareClaim, got nil")
			}
			if !strings.Contains(res.Err.Error(), "modules.brics.ac.uk/load") {
				t.Errorf("error %q does not point at k8s-modules", res.Err)
			}
		})
	}
}

func podRef(uid string) resourceapi.ResourceClaimConsumerReference {
	return resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: uid, UID: types.UID(uid)}
}

// sharedClaim is a claim allocated cxi0 on nodePool and reserved by pods.
func sharedClaim(pods ...string) *resourceapi.ResourceClaim {
	c := makeClaim("claim-1", testNamespace, "claim-1", "cxi0")
	for _, p := range pods {
		c.Status.ReservedFor = append(c.Status.ReservedFor, podRef(p))
	}
	return c
}

// Kubelet prepares a claim once per node: a pod that reserves it afterwards
// must still get the allocation and a CNI hint, or its sandbox never starts.
func TestRecordSharedClaimPods_LaterPodGetsHint(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()
	if res := d.prepareClaim(context.Background(), sharedClaim("pod-a")); res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}

	d.recordSharedClaimPods(context.Background(), sharedClaim("pod-a", "pod-b"))

	got := readHintFile(t, d.cniHintsDir, "pod-b")
	if len(got) != 1 || got[0].Device != "cxi0" || got[0].VNI != testVNI {
		t.Fatalf("pod-b hint = %+v, want one cxi0 entry with VNI %d", got, testVNI)
	}
	if a := d.store.Get("pod-b"); len(a) != 1 || a[0].DeviceName != "cxi0" || a[0].ClaimUID != "claim-1" {
		t.Fatalf("pod-b allocation = %+v, want cxi0 from claim-1", a)
	}
	if c := d.store.ClaimForPod("pod-b"); c != "claim-1" {
		t.Errorf("ClaimForPod(pod-b) = %q, want claim-1", c)
	}
	if pods := d.store.PodUIDsForClaim("claim-1"); len(pods) != 2 {
		t.Errorf("PodUIDsForClaim = %v, want [pod-a pod-b]", pods)
	}

	if _, err := d.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{
		{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "claim-1"}, UID: "claim-1"},
	}); err != nil {
		t.Fatalf("UnprepareResourceClaims: %v", err)
	}
	for _, p := range []types.UID{"pod-a", "pod-b"} {
		if _, err := os.Stat(d.cniHintPath(p)); !os.IsNotExist(err) {
			t.Errorf("hint for %s still present after unprepare: err=%v", p, err)
		}
	}
	if a := d.store.ClaimAllocs("claim-1"); a != nil {
		t.Errorf("ClaimAllocs after unprepare = %+v, want nil", a)
	}
}

// Claims allocated on other nodes, or not yet prepared here, are ignored.
func TestRecordSharedClaimPods_UnpreparedClaimIgnored(t *testing.T) {
	d := newTestDriver()
	d.cniHintsDir = t.TempDir()

	d.recordSharedClaimPods(context.Background(), sharedClaim("pod-a"))

	if a := d.store.Get("pod-a"); a != nil {
		t.Fatalf("pod-a allocation = %+v, want none for an unprepared claim", a)
	}
	if _, err := os.Stat(d.cniHintPath("pod-a")); !os.IsNotExist(err) {
		t.Fatalf("hint written for an unprepared claim: err=%v", err)
	}
}

// NRI's StopPodSandbox drops a pod from the store while the claim may still
// list it in ReservedFor; it must not be recorded again.
func TestRecordSharedClaimPods_StoppedPodNotRecordedAgain(t *testing.T) {
	d := newTestDriver()
	if res := d.prepareClaim(context.Background(), sharedClaim("pod-a")); res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
	d.store.Delete("pod-a")

	d.recordSharedClaimPods(context.Background(), sharedClaim("pod-a"))

	if a := d.store.Get("pod-a"); a != nil {
		t.Fatalf("stopped pod-a recorded again: %+v", a)
	}
}
