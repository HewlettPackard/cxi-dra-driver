package driver

import (
	"fmt"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// Consumable capacity names published on every CXI device. Claims request
// them under spec.devices.requests[].exactly.capacity.requests, and the
// scheduler records what each allocation consumed in
// status.allocation.devices.results[].consumedCapacity. LEs and TLEs are
// absolute counts, not pool counts.
const (
	capTXQs resourceapi.QualifiedName = "txqs"
	capTGQs resourceapi.QualifiedName = "tgqs"
	capEQs  resourceapi.QualifiedName = "eqs"
	capCTs  resourceapi.QualifiedName = "cts"
	capPTEs resourceapi.QualifiedName = "ptes"
	capACs  resourceapi.QualifiedName = "acs"
	capLEs  resourceapi.QualifiedName = "les"
	capTLEs resourceapi.QualifiedName = "tles"

	// capVFs is published on VF-mode parents only. Each allocation takes
	// exactly one, so a parent with N VFs is shared by at most N pods and a
	// VF is never given to two pods at once. It is a count of VFs, not a CXI
	// resource, so it is absent from cxi.ResourceSet.
	capVFs resourceapi.QualifiedName = "vfs"
)

// defaultRequest is what an allocation consumes for any capacity the claim
// does not request (requestPolicy.default). The queue/counter defaults match
// the cxi-small template; LEs and TLEs default to 0, i.e. the shared default
// pool, following Slingshot's guidance to reserve pools only when needed.
var defaultRequest = cxi.ResourceSet{
	TXQs: 4,
	TGQs: 2,
	EQs:  8,
	CTs:  4,
	PTEs: 4,
	ACs:  2,
	LEs:  0,
	TLEs: 0,
}

// buildCapacity returns the consumable capacities for a device whose usable
// ceiling (hardware total minus the blessed system services) is ceiling.
//
// The scheduler keeps the sum of consumedCapacity across all allocations of
// the device within each value, which replaces the avail_* CEL selectors and
// the ClaimBinder's pending-grant arithmetic as the primary capacity check.
// LEs and TLEs carry a step of one pool, so the scheduler rounds a request up
// to whole pools exactly as the hardware does.
func buildCapacity(ceiling cxi.ResourceSet) map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
	c := ceiling.ClampToZero()
	return map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
		capTXQs: deviceCapacity(c.TXQs, defaultRequest.TXQs, 0),
		capTGQs: deviceCapacity(c.TGQs, defaultRequest.TGQs, 0),
		capEQs:  deviceCapacity(c.EQs, defaultRequest.EQs, 0),
		capCTs:  deviceCapacity(c.CTs, defaultRequest.CTs, 0),
		capPTEs: deviceCapacity(c.PTEs, defaultRequest.PTEs, 0),
		capACs:  deviceCapacity(c.ACs, defaultRequest.ACs, 0),
		capLEs:  deviceCapacity(c.LEs, defaultRequest.LEs, cxi.LEPoolSize),
		capTLEs: deviceCapacity(c.TLEs, defaultRequest.TLEs, cxi.TLEPoolSize),
	}
}

// deviceCapacity builds one capacity entry with a request policy of
// [0, value] in increments of step (no step when step is 0). The API server
// rejects a policy whose default exceeds the value or whose min+step exceeds
// it, which would fail the whole ResourceSlice, so on a device too depleted
// for that the default drops to 0 and the step to none.
func deviceCapacity(value, def, step int64) resourceapi.DeviceCapacity {
	if def > value {
		def = 0
	}
	r := &resourceapi.CapacityRequestPolicyRange{Min: qty(0)}
	if step > 0 && step <= value {
		r.Step = qty(step)
	}
	return resourceapi.DeviceCapacity{
		Value: *qty(value),
		RequestPolicy: &resourceapi.CapacityRequestPolicy{
			Default:    qty(def),
			ValidRange: r,
		},
	}
}

// vfsCapacity is the parent's vfs capacity: nVFs in total, one per allocation
// (default 1, valid range [1, 1]) so a claim cannot take more or fewer.
func vfsCapacity(nVFs int) resourceapi.DeviceCapacity {
	return resourceapi.DeviceCapacity{
		Value: *qty(int64(nVFs)),
		RequestPolicy: &resourceapi.CapacityRequestPolicy{
			Default:    qty(1),
			ValidRange: &resourceapi.CapacityRequestPolicyRange{Min: qty(1), Max: qty(1)},
		},
	}
}

func qty(v int64) *resource.Quantity {
	return resource.NewQuantity(v, resource.DecimalSI)
}

// resourceSetFromConsumed converts an allocation's consumedCapacity into the
// ResourceSet the NRI plugin programs into the CXI service. Every capacity is
// required: the scheduler records all of a device's capacities, including
// zeros, so a missing one means the device was published by an incompatible
// driver version. Keys may be bare or prefixed with the driver name.
func resourceSetFromConsumed(
	consumed map[resourceapi.QualifiedName]resource.Quantity,
	driverName string,
) (cxi.ResourceSet, error) {
	byName := make(map[resourceapi.QualifiedName]int64, len(consumed))
	for k, q := range consumed {
		name := resourceapi.QualifiedName(strings.TrimPrefix(string(k), driverName+"/"))
		v, ok := q.AsInt64()
		if !ok || v < 0 {
			return cxi.ResourceSet{}, fmt.Errorf("consumed capacity %s=%s is not a non-negative integer", k, q.String())
		}
		byName[name] = v
	}
	var missing []string
	get := func(name resourceapi.QualifiedName) int64 {
		v, ok := byName[name]
		if !ok {
			missing = append(missing, string(name))
		}
		return v
	}
	rs := cxi.ResourceSet{
		TXQs: get(capTXQs),
		TGQs: get(capTGQs),
		EQs:  get(capEQs),
		CTs:  get(capCTs),
		PTEs: get(capPTEs),
		ACs:  get(capACs),
		LEs:  get(capLEs),
		TLEs: get(capTLEs),
	}
	if len(missing) > 0 {
		return cxi.ResourceSet{}, fmt.Errorf("consumed capacity missing %v", missing)
	}
	return rs, nil
}
