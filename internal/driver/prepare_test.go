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
	return []resourceapi.DeviceClaimConfiguration{
		{
			Requests: []string{"cxi"},
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver: testDriver,
					Parameters: runtime.RawExtension{
						Raw: []byte(`{"txqs":2,"tgqs":1,"eqs":1,"cts":1,"ptes":1,"acs":1,"le_pools":0,"tle_pools":0}`),
					},
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

// TestPrepareClaimCDIDeviceIDs verifies that the env/nccl/ofi CDI IDs are
// appended to the per-device ID based on the CDIEnvConfig flags when CDI env
// integration is enabled.
func TestPrepareClaimCDIDeviceIDs(t *testing.T) {
	tests := []struct {
		name         string
		cdiEnvConfig cdi.CDIEnvConfig
		wantIDs      []string
	}{
		{
			name:         "base only",
			cdiEnvConfig: cdi.CDIEnvConfig{},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
			},
		},
		{
			name:         "with NCCL",
			cdiEnvConfig: cdi.CDIEnvConfig{NCCLPrefix: "/spack/nccl"},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/nccl=nccl",
			},
		},
		{
			name:         "with OFI",
			cdiEnvConfig: cdi.CDIEnvConfig{AWSofiNCCLPrefix: "/spack/aws-ofi-nccl"},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/aws-ofi-nccl=aws-ofi-nccl",
			},
		},
		{
			name: "with NCCL and OFI",
			cdiEnvConfig: cdi.CDIEnvConfig{
				NCCLPrefix:       "/spack/nccl",
				AWSofiNCCLPrefix: "/spack/aws-ofi-nccl",
			},
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
				cdiEnvConfig: tt.cdiEnvConfig,
				enableCDIEnv: true,
				kubeClient:   fake.NewSimpleClientset(annotatedNamespace()),
			}

			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "test-claim", Namespace: testNamespace, UID: "claim-uid-1"},
				Spec:       resourceapi.ResourceClaimSpec{Devices: resourceapi.DeviceClaim{Config: cxiConfig()}},
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
