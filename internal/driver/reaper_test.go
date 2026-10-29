package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

const (
	reapLiveNetns uint64 = 4026531840
	reapDeadNetns uint64 = 4026532999
)

// fakeReapSvcMgr records the services the reaper deletes.
type fakeReapSvcMgr struct{ deleted []reapKey }

func (f *fakeReapSvcMgr) CreateService(string, uint64, int, cxi.ResourceSet, ...cxi.ServiceOptions) (int, error) {
	return 0, fmt.Errorf("unexpected CreateService")
}

func (f *fakeReapSvcMgr) DeleteService(dev string, id int) error {
	f.deleted = append(f.deleted, reapKey{dev, id})
	return nil
}

// reapEnv drives reapOrphanServices against fake per-device service lists.
type reapEnv struct {
	d      *Driver
	mgr    *fakeReapSvcMgr
	svcs   map[string][]cxi.ServiceInfo
	netns  map[reapKey]uint64
	nri    map[string]map[int]bool
	logs   []string
	hintsD string
}

func newReapEnv(t *testing.T, d *Driver) *reapEnv {
	t.Helper()
	e := &reapEnv{
		d:      d,
		mgr:    &fakeReapSvcMgr{},
		svcs:   make(map[string][]cxi.ServiceInfo),
		netns:  make(map[reapKey]uint64),
		nri:    make(map[string]map[int]bool),
		hintsD: t.TempDir(),
	}
	d.reapSvcMgr = e.mgr
	d.reaperMisses = make(map[reapKey]int)
	d.cniHintsDir = e.hintsD
	d.liveSvcIDs = func() map[string]map[int]bool { return e.nri }
	d.logFn = func(f string, a ...any) { e.logs = append(e.logs, fmt.Sprintf(f, a...)) }

	oldRead, oldNetns, oldLive := readActiveServices, readServiceNetns, listLiveNetns
	readActiveServices = func(dev string) ([]cxi.ServiceInfo, error) { return e.svcs[dev], nil }
	readServiceNetns = func(dev string, id int) (uint64, error) { return e.netns[reapKey{dev, id}], nil }
	listLiveNetns = func() (map[uint64]bool, error) { return map[uint64]bool{reapLiveNetns: true}, nil }
	t.Cleanup(func() { readActiveServices, readServiceNetns, listLiveNetns = oldRead, oldNetns, oldLive })
	return e
}

func (e *reapEnv) addSvc(dev string, id int, system bool, netns uint64) {
	e.svcs[dev] = append(e.svcs[dev], cxi.ServiceInfo{SvcID: id, IsSystem: system})
	e.netns[reapKey{dev, id}] = netns
}

// writeHint records a hint file for a pod holding dev.
func (e *reapEnv) writeHint(t *testing.T, pod, dev string) {
	t.Helper()
	body := fmt.Sprintf(`[{"device":%q,"hsn_iface":"enp1s0f1","vni":42}]`, dev)
	if err := os.WriteFile(filepath.Join(e.hintsD, pod+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *reapEnv) cycles(n int) {
	for i := 0; i < n; i++ {
		e.d.reapOrphanServices()
	}
}

func (e *reapEnv) warnings() int {
	n := 0
	for _, l := range e.logs {
		if strings.Contains(l, "no live pod or hint file accounts for") {
			n++
		}
	}
	return n
}

func (e *reapEnv) deleted(dev string, id int) bool {
	for _, k := range e.mgr.deleted {
		if k == (reapKey{dev, id}) {
			return true
		}
	}
	return false
}

func TestReaper_VF_EthernetChildNeverReaped(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 2))
	e.addSvc("cxi1", 1, true, reapDeadNetns)
	e.cycles(reapGraceCycles + 2)
	if len(e.mgr.deleted) != 0 {
		t.Fatalf("deleted %v, want none: the Ethernet child carries the system bit", e.mgr.deleted)
	}
}

func TestReaper_VF_OrphanPodChildReapedAfterGraceCycles(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 2))
	e.addSvc("cxi1", 1, true, reapDeadNetns)
	e.addSvc("cxi1", 2, false, reapDeadNetns)
	e.cycles(reapGraceCycles - 1)
	if len(e.mgr.deleted) != 0 {
		t.Fatalf("deleted %v before %d cycles", e.mgr.deleted, reapGraceCycles)
	}
	e.cycles(1)
	if len(e.mgr.deleted) != 1 || !e.deleted("cxi1", 2) {
		t.Fatalf("deleted %v, want only cxi1/2", e.mgr.deleted)
	}
}

func TestReaper_VF_LiveNetnsAndNRIChildrenKept(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 2))
	e.addSvc("cxi1", 2, false, reapLiveNetns)
	e.addSvc("cxi2", 2, false, reapDeadNetns)
	e.nri["cxi2"] = map[int]bool{2: true}
	e.cycles(reapGraceCycles + 2)
	if len(e.mgr.deleted) != 0 {
		t.Fatalf("deleted %v, want none", e.mgr.deleted)
	}
}

// A sibling VF's child is judged on its own: only the dead one is reaped.
func TestReaper_VF_SiblingChildUntouched(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 2))
	e.addSvc("cxi1", 2, false, reapDeadNetns)
	e.addSvc("cxi2", 2, false, reapLiveNetns)
	e.cycles(reapGraceCycles)
	if len(e.mgr.deleted) != 1 || !e.deleted("cxi1", 2) {
		t.Fatalf("deleted %v, want only cxi1/2", e.mgr.deleted)
	}
}

// --system-svc-ids are PF ids; on a VF the same number is a pod child.
func TestReaper_VF_IgnoresSystemSvcIDs(t *testing.T) {
	d := newVFTestDriver(t, 1)
	d.systemSvcIDs = []int{1, 2, 3}
	e := newReapEnv(t, d)
	e.addSvc("cxi1", 2, false, reapDeadNetns)
	e.cycles(reapGraceCycles)
	if !e.deleted("cxi1", 2) {
		t.Fatalf("deleted %v, want cxi1/2: blessed ids must not apply in VF mode", e.mgr.deleted)
	}
}

func TestReaper_VF_WarnsAboutUnattributableChild(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 3))
	e.addSvc("cxi1", 1, true, reapLiveNetns)
	e.addSvc("cxi1", 2, false, reapLiveNetns) // no hint, no NRI: foreign
	e.addSvc("cxi2", 2, false, reapLiveNetns) // hinted: a pod's
	e.addSvc("cxi3", 2, false, reapLiveNetns) // NRI-owned: a pod's
	e.writeHint(t, "pod-a", "cxi2")
	e.nri["cxi3"] = map[int]bool{2: true}

	e.cycles(1)
	if e.warnings() != 1 {
		t.Fatalf("warnings = %d, want 1; logs: %v", e.warnings(), e.logs)
	}
	line := e.logs[len(e.logs)-1]
	if !strings.Contains(line, "cxi1/svc 2") || strings.Contains(line, "cxi2/") || strings.Contains(line, "cxi3/") || strings.Contains(line, "/svc 1") {
		t.Errorf("warning names the wrong services: %s", line)
	}
	if len(e.mgr.deleted) != 0 {
		t.Errorf("deleted %v, want none: a live-netns child is only reported", e.mgr.deleted)
	}
}

func TestReaper_VF_WarningRepeatsEveryReapWarnCycles(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 1))
	e.addSvc("cxi1", 2, false, reapLiveNetns)
	e.cycles(reapWarnEveryCycles)
	if e.warnings() != 1 {
		t.Fatalf("warnings after %d cycles = %d, want 1", reapWarnEveryCycles, e.warnings())
	}
	e.cycles(1)
	if e.warnings() != 2 {
		t.Fatalf("warnings after %d cycles = %d, want 2", reapWarnEveryCycles+1, e.warnings())
	}
}

func TestReaper_VF_WarningReArmsAfterChildGoes(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 1))
	e.addSvc("cxi1", 2, false, reapLiveNetns)
	e.cycles(1)
	e.svcs["cxi1"] = nil
	e.cycles(1)
	e.addSvc("cxi1", 2, false, reapLiveNetns)
	e.cycles(1)
	if e.warnings() != 2 {
		t.Fatalf("warnings = %d, want 2", e.warnings())
	}
}

// A dead-netns child is reaped by the unchanged rules, not reported meanwhile.
func TestReaper_VF_DeadNetnsChildNotWarnedAbout(t *testing.T) {
	e := newReapEnv(t, newVFTestDriver(t, 1))
	e.addSvc("cxi1", 2, false, reapDeadNetns)
	e.cycles(reapGraceCycles)
	if e.warnings() != 0 {
		t.Fatalf("warnings = %d, want 0; logs: %v", e.warnings(), e.logs)
	}
}

func TestReaper_PFMode_Unchanged(t *testing.T) {
	d := newTestDriver()
	d.systemSvcIDs = []int{1}
	e := newReapEnv(t, d)
	e.addSvc("cxi0", 1, false, reapDeadNetns) // blessed
	e.addSvc("cxi0", 2, true, reapDeadNetns)  // kernel-system
	e.addSvc("cxi0", 3, false, reapDeadNetns) // orphan
	e.addSvc("cxi0", 4, false, reapLiveNetns) // live, unattributed but PF mode never warns
	e.cycles(reapGraceCycles)
	if len(e.mgr.deleted) != 1 || !e.deleted("cxi0", 3) {
		t.Fatalf("deleted %v, want only cxi0/3", e.mgr.deleted)
	}
	if e.warnings() != 0 {
		t.Errorf("PF mode warned: %v", e.logs)
	}
}
