package driver

import (
	"encoding/json"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

const testDriver = "cxi.slingshot.hpe.com"

func vniResult(device string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{Driver: testDriver, Pool: vniPoolName, Device: device, Request: "vni"}
}

func cxiResult(device string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{Driver: testDriver, Pool: "node-cxi", Device: device, Request: "cxi"}
}

func claimWith(results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
}

// minCXIConfig returns a runtime.RawExtension with minimum valid values for all
// required CXI resource config fields.
func minCXIConfig() runtime.RawExtension {
	m := map[string]int64{
		"txqs": 1, "tgqs": 1, "eqs": 1, "cts": 1,
		"ptes": 1, "acs": 1, "le_pools": 1, "tle_pools": 1,
	}
	raw, _ := json.Marshal(m)
	return runtime.RawExtension{Raw: raw}
}

// cxiOpaqueConfig returns a DeviceClaimConfiguration with opaque CXI resource
// params for testDriver. Applies to all requests (Requests is empty).
func cxiOpaqueConfig() resourceapi.DeviceClaimConfiguration {
	return resourceapi.DeviceClaimConfiguration{
		DeviceConfiguration: resourceapi.DeviceConfiguration{
			Opaque: &resourceapi.OpaqueDeviceConfiguration{
				Driver:     testDriver,
				Parameters: minCXIConfig(),
			},
		},
	}
}

func newTestDriver() *Driver {
	devices := cxi.MockDevices(1) // "cxi0"
	return &Driver{
		driverName:   testDriver,
		devices:      devices,
		store:        NewAllocationStore(),
		vniAllocator: &StaticVNIAllocator{VNI: 7},
	}
}

// M2: isVNIClaim must classify a claim as VNI-only only when it contains no
// real CXI device — otherwise a mixed claim is treated as logical-only and its
// CXI device is silently dropped.
func TestIsVNIClaim(t *testing.T) {
	tests := []struct {
		name  string
		claim *resourceapi.ResourceClaim
		want  bool
	}{
		{"pure VNI", claimWith(vniResult("vni-100")), true},
		{"pure CXI", claimWith(cxiResult("cxi0")), false},
		{"mixed VNI+CXI", claimWith(vniResult("vni-100"), cxiResult("cxi0")), false},
		{"no allocation", &resourceapi.ResourceClaim{}, false},
		{"empty results", claimWith(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVNIClaim(tt.claim, testDriver); got != tt.want {
				t.Errorf("isVNIClaim() = %v, want %v", got, tt.want)
			}
		})
	}
}

// M2: a mixed claim must still inject the CXI device — and must NOT treat the
// logical VNI result as a hardware device (no CDI ID, no resource reservation).
// The VNI must come from the VNI result in the same claim, not the fallback allocator.
func TestPrepareClaimMixedSkipsVNIResult(t *testing.T) {
	d := newTestDriver()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-1")},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: []resourceapi.DeviceClaimConfiguration{cxiOpaqueConfig()},
			},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						vniResult("vni-100"),
						cxiResult("cxi0"),
					},
				},
			},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{
				{Resource: "pods", UID: "pod-a"},
			},
		},
	}

	res := d.prepareClaim(claim, vniFromClaims([]*resourceapi.ResourceClaim{claim}, testDriver))

	if res.Err != nil {
		t.Fatalf("prepareClaim returned error: %v", res.Err)
	}
	if len(res.Devices) != 1 {
		t.Fatalf("got %d devices, want 1 (only cxi0; the VNI result must be skipped)", len(res.Devices))
	}
	if res.Devices[0].DeviceName != "cxi0" {
		t.Errorf("device = %q, want cxi0", res.Devices[0].DeviceName)
	}
	if got := d.store.Get("pod-a"); len(got) != 1 || got[0].VNI != 100 {
		t.Errorf("stored allocation = %+v, want one entry with VNI 100", got)
	}
}

// A pod requesting a CXI device must also request a VNI — prepareClaim must
// return an error if vniByPod has no entry for the pod.
func TestPrepareClaimRequiresVNIClaim(t *testing.T) {
	d := newTestDriver()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-1")},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: []resourceapi.DeviceClaimConfiguration{cxiOpaqueConfig()},
			},
		},
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

	res := d.prepareClaim(claim, nil) // no VNI claims → must fail
	if res.Err == nil {
		t.Fatal("expected error for pod without VNI claim, got nil")
	}
}

func TestVNIFromDeviceName(t *testing.T) {
	tests := []struct {
		in      string
		wantVNI int
		wantOK  bool
	}{
		{"vni-100", 100, true},
		{"vni-1", 1, true},
		{"cxi0", 0, false},
		{"vni-", 0, false},
		{"vni-abc", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			vni, ok := vniFromDeviceName(tt.in)
			if vni != tt.wantVNI || ok != tt.wantOK {
				t.Errorf("vniFromDeviceName(%q) = (%d, %v), want (%d, %v)", tt.in, vni, ok, tt.wantVNI, tt.wantOK)
			}
		})
	}
}
