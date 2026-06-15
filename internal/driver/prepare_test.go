package driver

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

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

// fullyConfiguredEnv populates every CDI profile so the test exercises
// profile selection, not node-side availability.
func fullyConfiguredEnv() cdi.CDIEnvConfig {
	return cdi.CDIEnvConfig{
		EnvMounts:    []cdi.MountSpec{{HostPath: "/usr/lib64/libcxi.so.1", ContainerPath: "/usr/lib64/libcxi.so.1"}},
		NCCLPrefix:   "/spack/nccl",
		AWSofiMounts: []cdi.MountSpec{{HostPath: "/spack/aws-ofi-nccl/lib/libnccl-net.so", ContainerPath: "/usr/lib/libnccl-net.so"}},
	}
}

// TestPrepareClaimCDIDeviceIDs verifies that the env/nccl/ofi CDI IDs are
// appended to the per-device ID based on the cdi_profiles field in the
// claim's opaque config. The node has all three CDI profiles configured;
// only the claim's opt-in list decides what is injected.
func TestPrepareClaimCDIDeviceIDs(t *testing.T) {
	tests := []struct {
		name     string
		profiles string // JSON literal for cdi_profiles
		wantIDs  []string
	}{
		{
			name:     "no profiles",
			profiles: `[]`,
			wantIDs:  []string{testDriver + "/cxi=cxi0"},
		},
		{
			name:     "env only",
			profiles: `["env"]`,
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
			},
		},
		{
			name:     "env+nccl",
			profiles: `["env","nccl"]`,
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/nccl=nccl",
			},
		},
		{
			name:     "env+aws-ofi-nccl",
			profiles: `["env","aws-ofi-nccl"]`,
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/aws-ofi-nccl=aws-ofi-nccl",
			},
		},
		{
			name:     "all three",
			profiles: `["env","nccl","aws-ofi-nccl"]`,
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/nccl=nccl",
				testDriver + "/aws-ofi-nccl=aws-ofi-nccl",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &Driver{
				driverName:   testDriver,
				devices:      cxi.MockDevices(1), // "cxi0"
				store:        NewAllocationStore(),
				cdiEnvConfig: fullyConfiguredEnv(),
				enableCDIEnv: true,
				kubeClient:   fake.NewSimpleClientset(annotatedNamespace()),
			}

			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: testNamespace, UID: "claim-uid-1"},
				Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfigWithProfiles(tt.profiles)}},
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0")},
						},
					},
					ReservedFor: []resourceapi.ResourceClaimConsumerReference{
						{Resource: "pods", UID: "pod-a"},
					},
				},
			}

			result := d.prepareClaim(context.Background(), claim)
			if result.Err != nil {
				t.Fatalf("prepareClaim error: %v", result.Err)
			}
			if len(result.Devices) != 1 {
				t.Fatalf("got %d devices, want 1", len(result.Devices))
			}
			got := result.Devices[0].CDIDeviceIDs
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("CDIDeviceIDs = %v, want %v", got, tt.wantIDs)
			}
			for i, id := range got {
				if id != tt.wantIDs[i] {
					t.Errorf("CDIDeviceIDs[%d] = %q, want %q", i, id, tt.wantIDs[i])
				}
			}
		})
	}
}

// A multi-device claim (e.g. count: 2) must reference each shared CDI
// profile spec (env/nccl/aws-ofi-nccl) exactly once across all devices.
// Those specs are driver-global singletons; emitting them per device would
// make containerd apply their bind-mounts twice and reject the container.
// Per-device cxi= IDs are still emitted for every device.
func TestPrepareClaimCDIDeviceIDsMultiDevice(t *testing.T) {
	d := &Driver{
		driverName:   testDriver,
		devices:      cxi.MockDevices(2), // "cxi0", "cxi1"
		store:        NewAllocationStore(),
		cdiEnvConfig: fullyConfiguredEnv(),
		enableCDIEnv: true,
		kubeClient:   fake.NewSimpleClientset(annotatedNamespace()),
	}

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: testNamespace, UID: "claim-uid-1"},
		Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfigWithProfiles(`["env","nccl"]`)}},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0"), cxiResult("cxi1")},
				},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", UID: "pod-a"},
			},
		},
	}

	result := d.prepareClaim(context.Background(), claim)
	if result.Err != nil {
		t.Fatalf("prepareClaim error: %v", result.Err)
	}
	if len(result.Devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(result.Devices))
	}

	// Every device must carry its own per-device cxi= ID.
	var cxiIDs []string
	counts := map[string]int{}
	for _, dev := range result.Devices {
		for _, id := range dev.CDIDeviceIDs {
			counts[id]++
		}
	}
	for _, dev := range result.Devices {
		want := cdi.DeviceID(testDriver, dev.DeviceName)
		found := false
		for _, id := range dev.CDIDeviceIDs {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Errorf("device %s missing its own CDI ID %q; got %v", dev.DeviceName, want, dev.CDIDeviceIDs)
		}
		cxiIDs = append(cxiIDs, want)
	}

	// Shared profile specs must appear exactly once across the whole claim.
	for _, id := range []string{cdi.EnvDeviceID(testDriver), cdi.NCCLDeviceID(testDriver)} {
		if counts[id] != 1 {
			t.Errorf("shared CDI ID %q appears %d times across devices, want exactly 1 (counts=%v)", id, counts[id], counts)
		}
	}
}

// Reject claims that ask for a profile the node hasn't configured, or
// that ask for any profile when CDI env injection is disabled cluster-side.
func TestPrepareClaimCDIProfileErrors(t *testing.T) {
	cases := []struct {
		name         string
		enableCDIEnv bool
		env          cdi.CDIEnvConfig
		profiles     string
	}{
		{
			name:         "unknown profile",
			enableCDIEnv: true,
			env:          fullyConfiguredEnv(),
			profiles:     `["bogus"]`,
		},
		{
			name:         "nccl requested but not configured on node",
			enableCDIEnv: true,
			env: cdi.CDIEnvConfig{
				EnvMounts: []cdi.MountSpec{{HostPath: "/usr/lib64/libcxi.so.1", ContainerPath: "/usr/lib64/libcxi.so.1"}},
			},
			profiles: `["nccl"]`,
		},
		{
			name:         "env requested when cdi env disabled",
			enableCDIEnv: false,
			env:          fullyConfiguredEnv(),
			profiles:     `["env"]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Driver{
				driverName:   testDriver,
				devices:      cxi.MockDevices(1),
				store:        NewAllocationStore(),
				cdiEnvConfig: tc.env,
				enableCDIEnv: tc.enableCDIEnv,
				kubeClient:   fake.NewSimpleClientset(annotatedNamespace()),
			}
			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: testNamespace, UID: "u"},
				Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfigWithProfiles(tc.profiles)}},
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0")},
						},
					},
				},
			}
			if res := d.prepareClaim(context.Background(), claim); res.Err == nil {
				t.Error("expected error from prepareClaim, got nil")
			}
		})
	}
}
