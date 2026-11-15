package driver

import (
	"strconv"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/HewlettPackard/cxi-dra-driver/internal/cxi"
)

// The --mock-vfs topology goes through the same parent setup as discovered VFs
// and publishes one parent per mock parent service, in both VNI forms.
func TestMockVFTopology_PublishesParents(t *testing.T) {
	vfs, topo := cxi.MockVFTopology(2, 4)
	cxi.SetMockTopology(topo)
	t.Cleanup(cxi.ResetMockTopology)
	d := &Driver{driverName: testDriver, nodeName: "kind-worker", devices: vfs, vfOnly: true,
		logFn: func(string, ...any) {}}
	d.setupParents()

	devs := d.buildDevices()
	if len(devs) != 2 {
		t.Fatalf("published %d devices, want 2 parents", len(devs))
	}
	wantTXQs := cxi.MockParentBudget.TXQs - 4*cxi.MockEthernetChild.TXQs
	for _, tc := range []struct {
		name, vniMin, vniSet string
	}{{"cxi0-svc4", "100", ""}, {"cxi0-svc5", "", ",200,202,204,"}} {
		dev := deviceByName(t, devs, tc.name)
		if ft := dev.Attributes[attrFunctionType].StringValue; ft == nil || *ft != functionTypeParent {
			t.Errorf("%s function_type = %v, want parent", tc.name, ft)
		}
		if v := dev.Attributes[attrVNIMin].IntValue; (v != nil) != (tc.vniMin != "") || (v != nil && strconv.FormatInt(*v, 10) != tc.vniMin) {
			t.Errorf("%s vni_min = %v, want %q", tc.name, v, tc.vniMin)
		}
		if v := dev.Attributes[attrVNISet].StringValue; (v != nil) != (tc.vniSet != "") || (v != nil && *v != tc.vniSet) {
			t.Errorf("%s vni_set = %v, want %q", tc.name, v, tc.vniSet)
		}
		capOf := func(n resourceapi.QualifiedName) int64 { q := dev.Capacity[n].Value; return q.Value() }
		if got := capOf(capVFs); got != 4 {
			t.Errorf("%s vfs = %d, want 4", tc.name, got)
		}
		if got := capOf(capTXQs); got != wantTXQs {
			t.Errorf("%s txqs = %d, want %d (budget minus 4 Ethernet children)", tc.name, got, wantTXQs)
		}
		if capOf(capLEs) != 0 || capOf(capTLEs) != 0 {
			t.Errorf("%s publishes LEs/TLEs", tc.name)
		}
	}
}
