//go:build !cxi

package cxi

import (
	"reflect"
	"testing"
)

func TestDiscoverDropsParentsWithoutLimits(t *testing.T) {
	f, pf0 := pfOnlyNode(t)
	addVF(f, "cxi0", pf0, 0, "cxi4", "0000:c5:00.1", "4")
	addVF(f, "cxi0", pf0, 1, "cxi5", "0000:c5:00.2", "4")
	addVF(f, "cxi0", pf0, 2, "cxi6", "0000:c5:00.3", "5")
	addVF(f, "cxi0", pf0, 3, "cxi7", "0000:c5:00.4", "6")

	x, _, logs := f.discoverer()
	SetMockTopology(MockTopology{Parents: []MockParent{
		{
			PFName: "cxi0", SvcID: 4,
			Descriptor: ParentDescriptor{ResourceLimits: false},
		},
		{
			PFName: "cxi0", SvcID: 5,
			Descriptor: ParentDescriptor{ResourceLimits: true},
		},
	}})
	t.Cleanup(ResetMockTopology)
	x.parentHasLimits = parentHasResourceLimits
	devs, err := x.discover(DiscoverOptions{VFOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := deviceNames(devs); !reflect.DeepEqual(got, []string{"cxi6"}) {
		t.Fatalf("devices = %v, want [cxi6]", got)
	}
	for _, want := range []string{
		"ERROR: skipping VF cxi4: parent service 4 on PF cxi0: it has no resource limits",
		"ERROR: skipping VF cxi5: parent service 4 on PF cxi0: it has no resource limits",
		"ERROR: skipping VF cxi7: parent service 6 on PF cxi0: cannot read its resource limits: reading parent svc 6 on cxi0: cxi-mock: no parent svc 6 on cxi0",
	} {
		if !logged(*logs, want) {
			t.Errorf("logs %q: missing %q", *logs, want)
		}
	}
	for _, skipped := range []string{"cxi4", "cxi5", "cxi7"} {
		if logged(*logs, "device "+skipped+" has no paired netdev") {
			t.Errorf("logged a netdev warning for skipped VF %s: %q", skipped, *logs)
		}
	}
}
