package driver

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// testParentCeiling is a ceiling with non-zero LEs/TLEs on purpose: parents
// force them to 0 in readParent, and buildDevices must publish what it is given.
var testParentCeiling = cxi.ResourceSet{TXQs: 100, TGQs: 90, EQs: 80, CTs: 70, PTEs: 60, ACs: 50}

// testParent builds a VF-mode parent with nVFs VFs on PF pf.
func testParent(pf string, svc, nVFs int, vnis cxi.VNISet, pci string, numa int, netdev string) *parentInfo {
	vfs := make([]*cxi.Device, nVFs)
	for i := range vfs {
		vfs[i] = &cxi.Device{Name: fmt.Sprintf("cxi%d%d%d", len(pf), svc, i), FunctionType: cxi.FunctionVF, PFName: pf, ParentSvcID: svc}
	}
	return &parentInfo{
		Parent:  &cxi.Parent{PFName: pf, SvcID: svc, VFs: vfs, PFPCIBusID: pci, PFNUMANode: numa, PFNetdev: netdev},
		ceiling: testParentCeiling,
		vnis:    vnis,
	}
}

func vfDriverWith(parents ...*parentInfo) *Driver {
	d := &Driver{nodeName: "test-node", vfOnly: true}
	d.parents.set(parents)
	return d
}

func mustRange(t *testing.T, lo, hi uint16) cxi.VNISet {
	t.Helper()
	s, err := cxi.NewVNIRange(lo, hi)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func deviceByName(t *testing.T, devs []resourceapi.Device, name string) resourceapi.Device {
	t.Helper()
	for _, d := range devs {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no device %q in %v", name, devs)
	return resourceapi.Device{}
}

func attrKeys(d resourceapi.Device) []string {
	var keys []string
	for k := range d.Attributes {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	return keys
}

func strAttr(t *testing.T, d resourceapi.Device, name string) string {
	t.Helper()
	a, ok := d.Attributes[resourceapi.QualifiedName(name)]
	if !ok || a.StringValue == nil {
		t.Fatalf("device %s: no string attribute %q in %v", d.Name, name, attrKeys(d))
	}
	return *a.StringValue
}

func intAttr(t *testing.T, d resourceapi.Device, name string) int64 {
	t.Helper()
	a, ok := d.Attributes[resourceapi.QualifiedName(name)]
	if !ok || a.IntValue == nil {
		t.Fatalf("device %s: no int attribute %q in %v", d.Name, name, attrKeys(d))
	}
	return *a.IntValue
}

func capValue(t *testing.T, d resourceapi.Device, name resourceapi.QualifiedName) int64 {
	t.Helper()
	c, ok := d.Capacity[name]
	if !ok {
		t.Fatalf("device %s: no capacity %q", d.Name, name)
	}
	v, ok := c.Value.AsInt64()
	if !ok {
		t.Fatalf("device %s: capacity %q = %s is not an integer", d.Name, name, c.Value.String())
	}
	return v
}

func TestBuildDevices_ParentAttributesAndCapacity(t *testing.T) {
	d := vfDriverWith(testParent("cxi0", 4, 3, mustRange(t, 32, 63), "0000:c5:00.0", 3, "hsn3"))
	devs := d.buildDevices()
	if len(devs) != 1 {
		t.Fatalf("len(devs) = %d, want 1", len(devs))
	}
	dev := devs[0]
	if dev.Name != "cxi0-svc4" {
		t.Fatalf("name = %q", dev.Name)
	}

	wantKeys := []string{
		"function_type", "parent_svc_id", "pf_name", "pf_netdev", "type", "vni_max", "vni_min",
		"resource.kubernetes.io/numaNode", "resource.kubernetes.io/pciBusID",
	}
	sort.Strings(wantKeys)
	if got := attrKeys(dev); !slices.Equal(got, wantKeys) {
		t.Fatalf("attributes = %v, want %v", got, wantKeys)
	}
	for name, want := range map[string]string{
		"type": "device", "function_type": "parent", "pf_name": "cxi0", "pf_netdev": "hsn3",
		"resource.kubernetes.io/pciBusID": "0000:c5:00.0",
	} {
		if got := strAttr(t, dev, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := intAttr(t, dev, "parent_svc_id"); got != 4 {
		t.Errorf("parent_svc_id = %d, want 4", got)
	}
	if got := intAttr(t, dev, "resource.kubernetes.io/numaNode"); got != 3 {
		t.Errorf("numaNode = %d, want 3", got)
	}
	if _, ok := dev.Attributes["hsn_iface"]; ok {
		t.Error("a parent must not publish hsn_iface")
	}

	if dev.AllowMultipleAllocations == nil || !*dev.AllowMultipleAllocations {
		t.Error("a parent must allow multiple allocations")
	}
	if !slices.Equal(dev.BindingConditions, []string{"Ready"}) || !slices.Equal(dev.BindingFailureConditions, []string{"Unschedulable"}) {
		t.Errorf("binding conditions = %v / %v", dev.BindingConditions, dev.BindingFailureConditions)
	}

	vfs := dev.Capacity[capVFs]
	if got := capValue(t, dev, capVFs); got != 3 {
		t.Errorf("vfs capacity = %d, want 3", got)
	}
	p := vfs.RequestPolicy
	if p == nil || p.Default == nil || p.Default.Value() != 1 ||
		p.ValidRange == nil || p.ValidRange.Min == nil || p.ValidRange.Min.Value() != 1 ||
		p.ValidRange.Max == nil || p.ValidRange.Max.Value() != 1 || p.ValidRange.Step != nil {
		t.Errorf("vfs request policy = %+v, want default 1, validRange {min 1, max 1}", p)
	}

	if got := capValue(t, dev, capTXQs); got != testParentCeiling.TXQs {
		t.Errorf("txqs = %d, want the ceiling %d", got, testParentCeiling.TXQs)
	}
	// LEs/TLEs are 0 on a parent: the scheduler then rejects any non-zero request.
	for _, name := range []resourceapi.QualifiedName{capLEs, capTLEs} {
		if got := capValue(t, dev, name); got != 0 {
			t.Errorf("%s = %d, want 0 on a parent", name, got)
		}
		if r := dev.Capacity[name].RequestPolicy; r == nil || r.Default == nil || r.Default.Value() != 0 {
			t.Errorf("%s request policy = %+v, want default 0", name, r)
		}
	}
}

func TestBuildDevices_PFAttributesUnchangedExceptFunctionType(t *testing.T) {
	d := &Driver{nodeName: "test-node", devices: cxi.MockDevices(1)}
	dev := d.buildDevices()[0]
	mock := cxi.MockDevices(1)[0]

	wantKeys := []string{
		"function_type", "hsn_iface", "type",
		"resource.kubernetes.io/numaNode", "resource.kubernetes.io/pciBusID",
	}
	sort.Strings(wantKeys)
	if got := attrKeys(dev); !slices.Equal(got, wantKeys) {
		t.Fatalf("attributes = %v, want %v", got, wantKeys)
	}
	if got := strAttr(t, dev, "function_type"); got != "pf" {
		t.Errorf("function_type = %q, want pf", got)
	}
	if got := strAttr(t, dev, "hsn_iface"); got != mock.HSNIface {
		t.Errorf("hsn_iface = %q, want %q", got, mock.HSNIface)
	}
	if _, ok := dev.Capacity[capVFs]; ok {
		t.Error("a PF must not publish a vfs capacity")
	}
	if got := capValue(t, dev, capLEs); got != cxi.DeviceCapacity().LEs {
		t.Errorf("PF les = %d, want the device total %d", got, cxi.DeviceCapacity().LEs)
	}
}

func TestBuildDevices_ParentVNIAttributes(t *testing.T) {
	list := func(v ...uint16) cxi.VNISet { return cxi.NewVNIList(v) }
	tests := []struct {
		name    string
		vnis    cxi.VNISet
		wantMin int64
		wantMax int64
		wantSet string // "" means vni_min/vni_max
	}{
		{name: "range", vnis: mustRange(t, 32, 63), wantMin: 32, wantMax: 63},
		{name: "single VNI range", vnis: mustRange(t, 70, 70), wantMin: 70, wantMax: 70},
		{name: "contiguous list is a range", vnis: list(42, 40, 41), wantMin: 40, wantMax: 42},
		{name: "non-contiguous list", vnis: list(40, 42, 140), wantSet: ",40,42,140,"},
		{name: "unreadable set never matches", vnis: cxi.VNISet{}, wantSet: noVNIs},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev := vfDriverWith(testParent("cxi0", 4, 1, tc.vnis, "0000:c5:00.0", 0, "hsn0")).buildDevices()[0]
			_, hasMin := dev.Attributes["vni_min"]
			_, hasMax := dev.Attributes["vni_max"]
			_, hasSet := dev.Attributes["vni_set"]
			if tc.wantSet != "" {
				if hasMin || hasMax || !hasSet {
					t.Fatalf("attributes %v: want vni_set only", attrKeys(dev))
				}
				if got := strAttr(t, dev, "vni_set"); got != tc.wantSet {
					t.Errorf("vni_set = %q, want %q", got, tc.wantSet)
				}
				return
			}
			if hasSet || !hasMin || !hasMax {
				t.Fatalf("attributes %v: want vni_min/vni_max only", attrKeys(dev))
			}
			if got := intAttr(t, dev, "vni_min"); got != tc.wantMin {
				t.Errorf("vni_min = %d, want %d", got, tc.wantMin)
			}
			if got := intAttr(t, dev, "vni_max"); got != tc.wantMax {
				t.Errorf("vni_max = %d, want %d", got, tc.wantMax)
			}
		})
	}
}

// The admission selector tests ",<vni>," with contains(): ",40," must never
// match inside ",140,".
func TestBuildDevices_VNISetSubstringMatch(t *testing.T) {
	dev := vfDriverWith(testParent("cxi0", 4, 1, cxi.NewVNIList([]uint16{40, 42, 140}), "", -1, "")).buildDevices()[0]
	set := strAttr(t, dev, "vni_set")
	for vni, want := range map[int]bool{40: true, 42: true, 140: true, 14: false, 4: false, 41: false, 1: false} {
		if got := strings.Contains(set, fmt.Sprintf(",%d,", vni)); got != want {
			t.Errorf("contains(%d) = %t, want %t in %q", vni, got, want, set)
		}
	}
}

func TestBuildDevices_ParentOmitsUnknownTopology(t *testing.T) {
	dev := vfDriverWith(testParent("cxi0", 4, 1, mustRange(t, 32, 63), "", -1, "")).buildDevices()[0]
	for _, name := range []string{"pf_netdev", "resource.kubernetes.io/numaNode", "resource.kubernetes.io/pciBusID"} {
		if _, ok := dev.Attributes[resourceapi.QualifiedName(name)]; ok {
			t.Errorf("attribute %s published although unknown", name)
		}
	}
	if got := strAttr(t, dev, "pf_name"); got != "cxi0" {
		t.Errorf("pf_name = %q", got)
	}
}

func TestBuildDevices_MultipleParentsOnOnePF(t *testing.T) {
	a := testParent("cxi0", 4, 2, mustRange(t, 32, 63), "0000:c5:00.0", 3, "hsn3")
	b := testParent("cxi0", 5, 4, mustRange(t, 64, 95), "0000:c5:00.0", 3, "hsn3")
	devs := vfDriverWith(a, b).buildDevices()
	if len(devs) != 2 {
		t.Fatalf("len(devs) = %d, want 2", len(devs))
	}
	da, db := deviceByName(t, devs, "cxi0-svc4"), deviceByName(t, devs, "cxi0-svc5")
	if intAttr(t, da, "parent_svc_id") != 4 || intAttr(t, db, "parent_svc_id") != 5 {
		t.Error("parent_svc_id does not tell the parents apart")
	}
	if intAttr(t, da, "vni_min") != 32 || intAttr(t, db, "vni_min") != 64 {
		t.Error("each parent must carry its own VNI set")
	}
	if capValue(t, da, capVFs) != 2 || capValue(t, db, capVFs) != 4 {
		t.Error("each parent must carry its own VF count")
	}
	for _, dev := range devs {
		if strAttr(t, dev, "pf_name") != "cxi0" || strAttr(t, dev, "pf_netdev") != "hsn3" {
			t.Errorf("%s: parents of one PF must agree on pf_name/pf_netdev", dev.Name)
		}
	}
}

func TestBuildDevices_ParentsOnSeveralPFsCarryTheirOwnTopology(t *testing.T) {
	devs := vfDriverWith(
		testParent("cxi0", 4, 1, mustRange(t, 32, 63), "0000:c5:00.0", 0, "hsn0"),
		testParent("cxi1", 4, 1, mustRange(t, 32, 63), "0000:cd:00.0", 1, "hsn1"),
		testParent("cxi2", 4, 1, mustRange(t, 32, 63), "0000:d5:00.0", 2, "hsn2"),
	).buildDevices()
	want := map[string]struct {
		pci    string
		numa   int64
		netdev string
	}{
		"cxi0-svc4": {"0000:c5:00.0", 0, "hsn0"},
		"cxi1-svc4": {"0000:cd:00.0", 1, "hsn1"},
		"cxi2-svc4": {"0000:d5:00.0", 2, "hsn2"},
	}
	if len(devs) != len(want) {
		t.Fatalf("len(devs) = %d, want %d", len(devs), len(want))
	}
	for name, w := range want {
		dev := deviceByName(t, devs, name)
		if got := strAttr(t, dev, "resource.kubernetes.io/pciBusID"); got != w.pci {
			t.Errorf("%s pciBusID = %q, want %q", name, got, w.pci)
		}
		if got := intAttr(t, dev, "resource.kubernetes.io/numaNode"); got != w.numa {
			t.Errorf("%s numaNode = %d, want %d", name, got, w.numa)
		}
		if got := strAttr(t, dev, "pf_netdev"); got != w.netdev {
			t.Errorf("%s pf_netdev = %q, want %q", name, got, w.netdev)
		}
	}
}

// The API rejects a ResourceSlice whose device carries more than 32 attributes
// and capacities combined, which would fail the whole publication.
func TestBuildDevices_AttributesAndCapacitiesWithinLimit(t *testing.T) {
	limit := resourceapi.ResourceSliceMaxAttributesAndCapacitiesPerDevice
	parent := vfDriverWith(
		testParent("cxi0", 4, 4, mustRange(t, 32, 63), "0000:c5:00.0", 3, "hsn3"),
		testParent("cxi0", 5, 4, cxi.NewVNIList([]uint16{1, 3, 5}), "0000:c5:00.0", 3, "hsn3"),
		testParent("cxi1", 4, 4, cxi.VNISet{}, "0000:cd:00.0", 1, "hsn1"),
	)
	pf := &Driver{nodeName: "test-node", devices: cxi.MockDevices(4)}
	for mode, d := range map[string]*Driver{"VF": parent, "PF": pf} {
		for _, dev := range d.buildDevices() {
			if n := len(dev.Attributes) + len(dev.Capacity); n > limit {
				t.Errorf("%s mode device %s has %d attributes+capacities, limit %d", mode, dev.Name, n, limit)
			}
		}
	}
}

func TestWarnParent_UnreadableVNISet(t *testing.T) {
	d := vfDriverWith()
	var logs []string
	d.logFn = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	d.warnParent(testParent("cxi0", 4, 1, cxi.VNISet{}, "", -1, ""))
	if len(logs) != 1 || !strings.Contains(logs[0], "warning: parent cxi0-svc4 has no readable VNI set") {
		t.Errorf("logs = %q, want one unreadable-VNI-set warning", logs)
	}
}
