package cxi

import (
	"strings"
	"syscall"
	"testing"
)

func TestChildCreationError(t *testing.T) {
	parent := &Parent{PFName: "cxi0", SvcID: 4}
	for _, operation := range []string{"open device", "allocate", "disable", "set netns", "enable"} {
		for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EINVAL, syscall.EPERM} {
			t.Run(operation+"/"+errno.Error(), func(t *testing.T) {
				message := childCreationError("cxi4", parent, 33, operation, -int(errno)).Error()
				for _, want := range []string{"cxi4", parent.Name(), operation + " failed", "rc=-"} {
					if !strings.Contains(message, want) {
						t.Fatalf("error %q lacks %q", message, want)
					}
				}
				if got, want := strings.Contains(message, "shared parent budget"), operation == "allocate" && errno == syscall.ENOSPC; got != want {
					t.Fatalf("incorrect budget diagnosis: %s", message)
				}
				if got, want := strings.Contains(message, "check VNI 33"), operation == "allocate" && errno == syscall.EINVAL; got != want {
					t.Fatalf("incorrect VNI diagnosis: %s", message)
				}
			})
		}
	}
}

func TestServiceParentValidation(t *testing.T) {
	fixture := func() *Parent {
		return &Parent{PFName: "cxi0", SvcID: 4, VFs: []*Device{
			{Name: "cxi4", FunctionType: FunctionVF, PFName: "cxi0", ParentSvcID: 4},
		}}
	}
	for _, tc := range []struct {
		name   string
		change func(*Parent)
	}{
		{"missing PF", func(p *Parent) { p.PFName = "" }},
		{"invalid ID", func(p *Parent) { p.SvcID = 0 }},
		{"missing target", func(p *Parent) { p.VFs = nil }},
		{"nil sibling", func(p *Parent) { p.VFs = append(p.VFs, nil) }},
		{"duplicate sibling", func(p *Parent) { p.VFs = append(p.VFs, p.VFs[0]) }},
		{"wrong PF", func(p *Parent) { p.VFs[0].PFName = "cxi1" }},
		{"wrong parent", func(p *Parent) { p.VFs[0].ParentSvcID = 5 }},
		{"not a VF", func(p *Parent) { p.VFs[0].FunctionType = FunctionPF }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture()
			tc.change(p)
			if _, err := serviceParent("cxi4", ResourceSet{}, []ServiceOptions{{Parent: p}}); err == nil {
				t.Fatal("invalid parent context accepted")
			}
		})
	}
	if _, err := serviceParent("cxi4", ResourceSet{}, []ServiceOptions{{Parent: fixture()}, {}}); err == nil {
		t.Fatal("multiple options accepted")
	}
	for _, options := range [][]ServiceOptions{nil, {{}}} {
		if parent, err := serviceParent("cxi0", ResourceSet{}, options); err != nil || parent != nil {
			t.Fatalf("PF options: parent=%v err=%v", parent, err)
		}
	}
	p := fixture()
	if got, err := serviceParent("cxi4", ResourceSet{}, []ServiceOptions{{Parent: p}}); err != nil || got != p {
		t.Fatalf("valid VF context: parent=%v err=%v", got, err)
	}
}
