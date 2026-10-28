//go:build !cxi

package cxi

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestMockChildRejectsInvalidParentDescriptor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func()
		want   string
	}{
		{"missing parent", func() { mockTopology.Parents = nil }, "no parent"},
		{"unlimited parent", func() { mockTopology.Parents[0].Descriptor.ResourceLimits = false }, "no resource limits"},
		{"zero LE maximum", func() { mockTopology.Parents[0].Descriptor.LEMax = 0 }, "invalid LE maximum"},
		{"negative LE maximum", func() { mockTopology.Parents[0].Descriptor.LEMax = -1 }, "invalid LE maximum"},
		{"overflow LE maximum", func() { mockTopology.Parents[0].Descriptor.LEMax = 1 << 32 }, "invalid LE maximum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mgr := childFixture(t)
			mockTopologyMu.Lock()
			tc.change()
			mockTopologyMu.Unlock()
			before := MockVFServices("cxi4")
			_, err := mgr.CreateService("cxi4", 123, 33, ResourceSet{TXQs: 1}, ServiceOptions{Parent: p})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if tc.name == "unlimited parent" && !errors.Is(err, ErrParentUnlimited) {
				t.Fatalf("error does not wrap ErrParentUnlimited: %v", err)
			}
			after := MockVFServices("cxi4")
			if len(after) != len(before) || after[0] != before[0] {
				t.Fatalf("rejection changed reservations: before=%v after=%v", before, after)
			}
		})
	}
}

func childFixture(t *testing.T) (*Parent, MockServiceManager) {
	t.Helper()
	p := &Parent{PFName: "cxi0", SvcID: 4}
	for _, name := range []string{"cxi4", "cxi5"} {
		p.VFs = append(p.VFs, &Device{Name: name, FunctionType: FunctionVF, PFName: p.PFName, ParentSvcID: p.SvcID})
	}
	SetMockTopology(MockTopology{
		Parents: []MockParent{{
			PFName: p.PFName, SvcID: p.SvcID,
			Descriptor: ParentDescriptor{
				ResourceLimits: true, Limits: ResourceSet{TXQs: 10, LEs: 4096},
				LEMax: 16384, VNIs: NewVNIList([]uint16{33, 40}),
			},
		}},
		VFServices: map[string][]ServiceReservation{
			"cxi4": {{SvcID: 2, IsSystem: true, ResourceLimits: true, Res: ResourceSet{TXQs: 1, LEs: 660}}},
			"cxi5": {{SvcID: 2, IsSystem: true, ResourceLimits: true, Res: ResourceSet{TXQs: 1, LEs: 660}}},
		},
	})
	t.Cleanup(ResetMockTopology)
	return p, MockServiceManager{}
}

func createChild(t *testing.T, mgr MockServiceManager, p *Parent, vf string, txqs int64) int {
	t.Helper()
	id, err := mgr.CreateService(vf, 123, 33, ResourceSet{TXQs: txqs}, ServiceOptions{Parent: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mgr.DeleteService(vf, id); err != nil {
			t.Error(err)
		}
	})
	return id
}

func TestMockChildSharedBudgetAndCleanup(t *testing.T) {
	p, mgr := childFixture(t)
	id := createChild(t, mgr, p, "cxi4", 5)
	raw, err := os.ReadFile(MockServicePath("cxi4", id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "name: LEs\n    max: 16384\n    res: 0") {
		t.Fatalf("child must inherit parent's LE maximum, not its reserved count:\n%s", raw)
	}
	if !strings.Contains(string(raw), "name: TLEs\n    max: 0\n    res: 0") {
		t.Fatalf("child must not reserve TLEs:\n%s", raw)
	}
	if _, err := mgr.CreateService("cxi5", 124, 33, ResourceSet{TXQs: 4}, ServiceOptions{Parent: p}); err == nil || !strings.Contains(err.Error(), "ENOSPC") {
		t.Fatalf("sibling overcommit error = %v", err)
	}
	// The same VNI on a sibling is valid when the shared budget permits it.
	createChild(t, mgr, p, "cxi5", 3)
	remaining, err := ReadParentRemaining(p.PFName, p.SvcID, []string{"cxi4", "cxi5"})
	if err != nil || remaining.TXQs != 0 || remaining.LEs != 2776 {
		t.Fatalf("remaining = %v, %v", remaining, err)
	}
	if err := mgr.DeleteService("cxi4", id); err != nil {
		t.Fatal(err)
	}
	remaining, err = ReadParentRemaining(p.PFName, p.SvcID, []string{"cxi4", "cxi5"})
	if err != nil || remaining.TXQs != 5 {
		t.Fatalf("remaining after delete = %v, %v", remaining, err)
	}
}

func TestMockChildRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		vni  int
		res  ResourceSet
		want string
	}{
		{"VNI outside list", 34, ResourceSet{}, "not allowed"},
		{"VNI overflow", 65569, ResourceSet{}, "not allowed"},
		{"LE reservation", 33, ResourceSet{LEs: 1}, "must be zero"},
		{"TLE reservation", 33, ResourceSet{TLEs: 1}, "must be zero"},
		{"budget", 33, ResourceSet{TXQs: 9}, "ENOSPC"},
		{"negative request", 33, ResourceSet{TXQs: -1}, "outside"},
		{"overflow request", 33, ResourceSet{TXQs: 65536}, "outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, mgr := childFixture(t)
			if _, err := mgr.CreateService("cxi4", 123, tc.vni, tc.res, ServiceOptions{Parent: p}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}

			if got := MockVFServices("cxi4"); len(got) != 1 {
				t.Fatalf("failed creation changed service list: %v", got)
			}
		})
	}
}

func TestMockChildRangeAndParentIsolation(t *testing.T) {
	p, mgr := childFixture(t)
	mockTopologyMu.Lock()
	vnis, err := NewVNIRange(32, 63)
	if err != nil {
		mockTopologyMu.Unlock()
		t.Fatal(err)
	}
	mockTopology.Parents[0].Descriptor.VNIs = vnis
	mockTopology.Parents = append(mockTopology.Parents, MockParent{
		PFName: "cxi1", SvcID: 4, Descriptor: mockTopology.Parents[0].Descriptor,
	})
	mockTopologyMu.Unlock()
	createChild(t, mgr, p, "cxi4", 8)
	other := &Parent{PFName: "cxi1", SvcID: 4, VFs: []*Device{
		{Name: "cxi6", FunctionType: FunctionVF, PFName: "cxi1", ParentSvcID: 4},
	}}
	createChild(t, mgr, other, "cxi6", 8)
	if _, err := mgr.CreateService("cxi5", 123, 64, ResourceSet{}, ServiceOptions{Parent: p}); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("out-of-range VNI error = %v", err)
	}
}

func TestMockChildConcurrentSharedBudget(t *testing.T) {
	p, mgr := childFixture(t)
	var wg sync.WaitGroup
	ids := make(chan struct {
		vf string
		id int
	}, 2)
	for _, vf := range []string{"cxi4", "cxi5"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := mgr.CreateService(vf, 123, 33, ResourceSet{TXQs: 5}, ServiceOptions{Parent: p})
			if err == nil {
				ids <- struct {
					vf string
					id int
				}{vf, id}
			} else if !strings.Contains(err.Error(), "ENOSPC") {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	close(ids)
	count := 0
	for created := range ids {
		count++
		if err := mgr.DeleteService(created.vf, created.id); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("successful creates = %d, want 1", count)
	}
}

func TestMockChildAllTrackedResources(t *testing.T) {
	p, mgr := childFixture(t)
	mockTopologyMu.Lock()
	mockTopology.Parents[0].Descriptor.Limits = ResourceSet{
		TXQs: 10, TGQs: 10, EQs: 10, CTs: 10, PTEs: 10, ACs: 10, LEs: 4096,
	}
	mockTopologyMu.Unlock()
	for _, request := range []ResourceSet{
		{TXQs: 9}, {TGQs: 11}, {EQs: 11}, {CTs: 11}, {PTEs: 11}, {ACs: 11},
	} {
		if _, err := mgr.CreateService("cxi4", 123, 33, request, ServiceOptions{Parent: p}); err == nil || !strings.Contains(err.Error(), "ENOSPC") {
			t.Fatalf("request %s: expected over-budget rejection, got %v", request, err)
		}
	}
	request := ResourceSet{TXQs: 8, TGQs: 10, EQs: 10, CTs: 10, PTEs: 10, ACs: 10}
	id, err := mgr.CreateService("cxi4", 123, 33, request, ServiceOptions{Parent: p})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mgr.DeleteService("cxi4", id); err != nil {
			t.Error(err)
		}
	})
	for _, service := range MockVFServices("cxi4") {
		if service.SvcID == id && service.Res != request {
			t.Fatalf("reservation = %s, want %s", service.Res, request)
		}
	}
}

func TestMockPFCreateWithoutOptions(t *testing.T) {
	mgr := MockServiceManager{}
	id, err := mgr.CreateService("cxi0", 123, 33, ResourceSet{LEs: 1024, TLEs: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mgr.DeleteService("cxi0", id); err != nil {
			t.Error(err)
		}
	})
	raw, err := os.ReadFile(MockServicePath("cxi0", id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "name: LEs\n    max: 1024\n    res: 1024") {
		t.Fatalf("PF mock output changed:\n%s", raw)
	}
}
