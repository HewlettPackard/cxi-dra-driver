package driver

import (
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
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: cxiConfig(),
			},
		},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
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

func newTestDriver() *Driver {
	devices := cxi.MockDevices(1) // "cxi0"
	return &Driver{
		driverName: testDriver,
		devices:    devices,
		store:      NewAllocationStore(),
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
func TestPrepareClaimMixedSkipsVNIResult(t *testing.T) {
	d := newTestDriver()
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-1")},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: cxiConfig(),
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
	// The pod's VNI must come from the VNI result in the same claim, not the fallback.
	if got := d.store.Get("pod-a"); len(got) != 1 || got[0].VNI != 100 {
		t.Errorf("stored allocation = %+v, want one entry with VNI 100", got)
	}
}

// L1: when multiple pods share a claim and a later pod has no VNI claim,
// earlier store entries must be rolled back.
func TestPrepareClaimRollsBackOnPartialFailure(t *testing.T) {
	devices := cxi.MockDevices(1)
	d := &Driver{
		driverName: testDriver,
		devices:    devices,
		store:      NewAllocationStore(),
	}

	// One CXI device shared by two pods: pod-a has a VNI, pod-b does not.
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-1")},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: cxiConfig(),
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
				{Resource: "pods", UID: "pod-b"},
			},
		},
	}
	// Only pod-a has a VNI allocated from the K8s pool.
	vniByPod := map[types.UID]int{"pod-a": 42}
	res := d.prepareClaim(claim, vniByPod)
	if res.Err == nil {
		t.Fatal("want error (pod-b has no VNI claim), got nil")
	}
	if got := d.store.Get("pod-a"); got != nil {
		t.Errorf("pod-a store entry = %+v, want nil after rollback", got)
	}
	if got := d.store.PodUIDsForClaim("claim-1"); len(got) != 0 {
		t.Errorf("claim-1 pod mappings = %+v, want empty after rollback", got)
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
