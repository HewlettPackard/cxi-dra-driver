package driver

import (
	"context"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

func consumedOf(rs cxi.ResourceSet) map[resourceapi.QualifiedName]resource.Quantity {
	q := func(v int64) resource.Quantity { return *resource.NewQuantity(v, resource.DecimalSI) }
	return map[resourceapi.QualifiedName]resource.Quantity{
		capTXQs: q(rs.TXQs), capTGQs: q(rs.TGQs), capEQs: q(rs.EQs), capCTs: q(rs.CTs),
		capPTEs: q(rs.PTEs), capACs: q(rs.ACs), capLEs: q(rs.LEs), capTLEs: q(rs.TLEs),
	}
}

func TestBuildDevices_PublishesConsumableCapacity(t *testing.T) {
	d := &Driver{
		nodeName: "test-node",
		devices:  cxi.MockDevices(1),
		pending:  make(map[pendingKey]pendingEntry),
	}
	d.devices[0].Reserved = cxi.ResourceSet{TXQs: 5, LEs: 2048, TLEs: 512}
	ceiling := d.usableCeiling("cxi0")

	dev := d.buildDevices()[0]
	if dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations {
		t.Fatal("device must allow multiple allocations for consumable capacity")
	}
	if n := len(dev.Attributes) + len(dev.Capacity); n > resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice {
		t.Fatalf("attributes+capacities = %d, over the API limit of %d", n, resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice)
	}

	want := map[resourceapi.QualifiedName]struct{ value, def, step int64 }{
		capTXQs: {ceiling.TXQs, defaultRequest.TXQs, 0},
		capTGQs: {ceiling.TGQs, defaultRequest.TGQs, 0},
		capEQs:  {ceiling.EQs, defaultRequest.EQs, 0},
		capCTs:  {ceiling.CTs, defaultRequest.CTs, 0},
		capPTEs: {ceiling.PTEs, defaultRequest.PTEs, 0},
		capACs:  {ceiling.ACs, defaultRequest.ACs, 0},
		capLEs:  {ceiling.LEs, defaultRequest.LEs, cxi.LEPoolSize},
		capTLEs: {ceiling.TLEs, defaultRequest.TLEs, cxi.TLEPoolSize},
	}
	if len(dev.Capacity) != len(want) {
		t.Fatalf("published %d capacities, want %d", len(dev.Capacity), len(want))
	}
	for name, w := range want {
		c, ok := dev.Capacity[name]
		if !ok {
			t.Errorf("missing capacity %s", name)
			continue
		}
		if c.Value.Value() != w.value {
			t.Errorf("%s value = %s, want %d (usable ceiling)", name, c.Value.String(), w.value)
		}
		p := c.RequestPolicy
		if p == nil || p.Default == nil || p.ValidRange == nil || p.ValidRange.Min == nil {
			t.Errorf("%s request policy incomplete: %+v", name, p)
			continue
		}
		if p.Default.Value() != w.def {
			t.Errorf("%s default = %s, want %d", name, p.Default.String(), w.def)
		}
		if p.ValidRange.Min.Value() != 0 {
			t.Errorf("%s min = %s, want 0", name, p.ValidRange.Min.String())
		}
		gotStep := int64(0)
		if p.ValidRange.Step != nil {
			gotStep = p.ValidRange.Step.Value()
		}
		if gotStep != w.step {
			t.Errorf("%s step = %d, want %d", name, gotStep, w.step)
		}
	}
}

// A depleted device must still publish a policy the API server accepts:
// default <= value and min+step <= value.
func TestDeviceCapacity_DegradesOnDepletedDevice(t *testing.T) {
	c := deviceCapacity(100, 512, 512)
	if c.RequestPolicy.ValidRange.Step != nil {
		t.Error("step larger than the value must be dropped")
	}
	if c.RequestPolicy.Default.Value() != 0 {
		t.Errorf("default = %s, want 0 when it exceeds the value", c.RequestPolicy.Default.String())
	}

	c = deviceCapacity(0, 4, 0)
	if c.RequestPolicy.Default.Value() != 0 {
		t.Errorf("default = %s, want 0 on a zero-capacity device", c.RequestPolicy.Default.String())
	}
}

func TestResourceSetFromConsumed(t *testing.T) {
	want := cxi.ResourceSet{TXQs: 4, TGQs: 2, EQs: 8, CTs: 4, PTEs: 4, ACs: 2, LEs: 1024, TLEs: 512}

	got, err := resourceSetFromConsumed(consumedOf(want), testDriver)
	if err != nil || got != want {
		t.Fatalf("bare keys: got %v, %v; want %v", got, err, want)
	}

	prefixed := map[resourceapi.QualifiedName]resource.Quantity{}
	for k, v := range consumedOf(want) {
		prefixed[resourceapi.QualifiedName(testDriver+"/"+string(k))] = v
	}
	got, err = resourceSetFromConsumed(prefixed, testDriver)
	if err != nil || got != want {
		t.Fatalf("driver-prefixed keys: got %v, %v; want %v", got, err, want)
	}

	missing := consumedOf(want)
	delete(missing, capTLEs)
	if _, err := resourceSetFromConsumed(missing, testDriver); err == nil || !strings.Contains(err.Error(), "tles") {
		t.Errorf("missing tles: err = %v, want one naming tles", err)
	}
}

// consumedClaim is cdiTestClaim with consumedCapacity recorded on every
// allocation result, as the scheduler does for a shareable device.
func consumedClaim(consumed cxi.ResourceSet) *resourceapi.ResourceClaim {
	claim := cdiTestClaim(`[]`, "cxi0")
	for i := range claim.Status.Allocation.Devices.Results {
		claim.Status.Allocation.Devices.Results[i].ConsumedCapacity = consumedOf(consumed)
	}
	return claim
}

func TestPrepareClaim_UsesConsumedCapacity(t *testing.T) {
	d := newTestDriver()
	consumed := cxi.ResourceSet{TXQs: 16, TGQs: 8, EQs: 32, CTs: 32, PTEs: 16, ACs: 8, LEs: 2048, TLEs: 512}
	claim := consumedClaim(consumed)
	claim.Spec.Devices.Config = nil // capacity requests only, no opaque config

	res := d.prepareClaim(context.Background(), claim)
	if res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
	got := d.store.Get("pod-a")
	if len(got) != 1 || got[0].Resources != consumed {
		t.Fatalf("stored resources = %+v, want %v", got, consumed)
	}
}

// Opaque config that fits within what the scheduler consumed is accepted; the
// consumed amount (pool-rounded) is what the service gets.
func TestPrepareClaim_ConsumedCoversLegacyConfig(t *testing.T) {
	d := newTestDriver()
	// cxiConfig asks for txqs 2, tgqs/eqs/cts/ptes/acs 1, no pools.
	consumed := cxi.ResourceSet{TXQs: 2, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1}
	res := d.prepareClaim(context.Background(), consumedClaim(consumed))
	if res.Err != nil {
		t.Fatalf("prepareClaim: %v", res.Err)
	}
	if got := d.store.Get("pod-a"); got[0].Resources != consumed {
		t.Fatalf("stored resources = %v, want consumed %v", got[0].Resources, consumed)
	}
}

// Opaque config asking for more than was consumed means the claim was never
// converted to capacity requests; preparing it would under-provision the
// service, so it is rejected.
func TestPrepareClaim_RejectsLegacyConfigExceedingConsumed(t *testing.T) {
	d := newTestDriver()
	consumed := cxi.ResourceSet{TXQs: 1, TGQs: 1, EQs: 1, CTs: 1, PTEs: 1, ACs: 1}
	res := d.prepareClaim(context.Background(), consumedClaim(consumed))
	if res.Err == nil || !strings.Contains(res.Err.Error(), "capacity.requests") {
		t.Fatalf("err = %v, want rejection pointing at capacity.requests", res.Err)
	}
}

func TestPrepareClaim_NoConsumedNoConfigErrors(t *testing.T) {
	d := newTestDriver()
	claim := cdiTestClaim(`[]`, "cxi0")
	claim.Spec.Devices.Config = nil
	if res := d.prepareClaim(context.Background(), claim); res.Err == nil {
		t.Fatal("expected an error for a claim with neither consumed capacity nor opaque config")
	}
}

// Opaque config carrying only cdi_profiles is fine alongside capacity
// requests, but a removed profile is still rejected.
func TestPrepareClaim_ConsumedChecksCDIProfiles(t *testing.T) {
	consumed := cxi.ResourceSet{TXQs: 4, TGQs: 2, EQs: 8, CTs: 4, PTEs: 4, ACs: 2}
	for _, tc := range []struct {
		params  string
		wantErr bool
	}{
		{`{"cdi_profiles":["env"]}`, false},
		{`{"cdi_profiles":["nccl"]}`, true},
	} {
		t.Run(tc.params, func(t *testing.T) {
			d := newTestDriver()
			claim := consumedClaim(consumed)
			claim.Spec.Devices.Config[0].Opaque.Parameters.Raw = []byte(tc.params)
			res := d.prepareClaim(context.Background(), claim)
			if (res.Err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", res.Err, tc.wantErr)
			}
		})
	}
}
