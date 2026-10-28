package driver

import "testing"

func TestServiceOptionsFor(t *testing.T) {
	d := newVFTestDriver(t, 2)
	options, err := d.ServiceOptionsFor(PodAllocation{DeviceName: "cxi1", ParentName: vfTestParent})
	if err != nil || options.Parent == nil || options.Parent.Name() != vfTestParent || len(options.Parent.VFs) != 2 {
		t.Fatalf("VF service options = %+v, %v", options, err)
	}
	options, err = d.ServiceOptionsFor(PodAllocation{DeviceName: "cxi0"})
	if err != nil || options.Parent != nil {
		t.Fatalf("PF service options = %+v, %v", options, err)
	}
	if _, err := d.ServiceOptionsFor(PodAllocation{DeviceName: "cxi1", ParentName: "missing"}); err == nil {
		t.Fatal("expected an error for an unknown parent")
	}
}
