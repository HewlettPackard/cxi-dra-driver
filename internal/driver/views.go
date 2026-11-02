package driver

import (
	"fmt"
	"sync"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// parentInfo is one VF-mode parent as the driver uses it: the VFs grouped
// under it by discovery, its budget and VNI set, and the ceiling published for it.
type parentInfo struct {
	*cxi.Parent
	budget   cxi.ResourceSet
	ethPerVF cxi.ResourceSet // Ethernet-child reservation held for every VF
	ceiling  cxi.ResourceSet
	vnis     cxi.VNISet
}

// checkVNI reports why a namespace VNI cannot be used on this parent, or nil.
func (p *parentInfo) checkVNI(vni int) error {
	if p.vnis.IsEmpty() {
		return fmt.Errorf("the VNI set of parent service %s/%d could not be read", p.PFName, p.SvcID)
	}
	if vni < 0 || vni > 0xffff || !p.vnis.Contains(uint16(vni)) {
		return fmt.Errorf("namespace VNI %d is not in the VNI set {%s} of parent service %s/%d", vni, p.vnis, p.PFName, p.SvcID)
	}
	return nil
}

func (p *parentInfo) vfNames() []string {
	names := make([]string, len(p.VFs))
	for i, vf := range p.VFs {
		names[i] = vf.Name
	}
	return names
}

// parentSet holds the VF-mode parents, built once in Run before the plugin
// serves, and read afterwards by Prepare, the ClaimBinder and the NRI plugin.
type parentSet struct {
	mu     sync.RWMutex
	byName map[string]*parentInfo
	order  []*parentInfo
}

func (s *parentSet) get(name string) (*parentInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byName[name]
	return p, ok
}

func (s *parentSet) list() []*parentInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.order
}

func (s *parentSet) set(parents []*parentInfo) {
	byName := make(map[string]*parentInfo, len(parents))
	for _, p := range parents {
		byName[p.Name()] = p
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName, s.order = byName, parents
}

// parentOf returns the parent a VF belongs to.
func (s *parentSet) parentOf(vf string) (*parentInfo, bool) {
	for _, p := range s.list() {
		for _, d := range p.VFs {
			if d.Name == vf {
				return p, true
			}
		}
	}
	return nil, false
}

// publishedDevice is one entry of the ResourceSlice: a PF in PF mode, a parent
// in VF mode.
type publishedDevice struct {
	name     string
	numaNode int
	pciBusID string
	hsnIface string
	// parent is set for a VF-mode parent and nil for a PF.
	parent *parentInfo
}

// publishedDevices is what the ResourceSlice, the ClaimBinder and the pending
// map work on. In PF mode every PF is both published and programmable.
func (d *Driver) publishedDevices() []publishedDevice {
	if !d.vfOnly {
		out := make([]publishedDevice, 0, len(d.devices))
		for _, dev := range d.devices {
			out = append(out, publishedDevice{name: dev.Name, numaNode: dev.NUMANode, pciBusID: dev.PCIBusID, hsnIface: dev.HSNIface})
		}
		return out
	}
	parents := d.parents.list()
	out := make([]publishedDevice, 0, len(parents))
	for _, p := range parents {
		// A parent has no single netdev; its topology is its PF's.
		out = append(out, publishedDevice{name: p.Name(), numaNode: p.PFNUMANode, pciBusID: p.PFPCIBusID, parent: p})
	}
	return out
}

// programmableDevices are the devices services are created on: CDI specs, the
// allocation handed to the NRI plugin, and the reaper. PFs, or VFs with --vf-only.
func (d *Driver) programmableDevices() []*cxi.Device {
	return d.devices
}

// setupParents groups the discovered VFs by parent, reads each parent's budget
// and VNI set, and logs what it publishes. A no-op in PF mode.
func (d *Driver) setupParents() {
	if !d.vfOnly {
		return
	}
	var parents []*parentInfo
	for _, p := range cxi.Parents(d.programmableDevices()) {
		info := &parentInfo{Parent: p}
		if err := readParent(info); err != nil {
			d.logf("ERROR: parent %s: %v; publishing it with zero capacity", p.Name(), err)
		}
		d.logf("parent %s: VFs %v, budget (%s), Ethernet-child reservation (%s) per VF, published ceiling (%s), VNI set {%s}",
			p.Name(), info.vfNames(), info.budget, info.ethPerVF, info.ceiling, info.vnis)
		d.warnParent(info)
		parents = append(parents, info)
	}
	d.parents.set(parents)
}

// warnParent logs the startup warnings for a parent: a VNI set that does not
// cover the cluster VNI range, and fewer remaining LEs than one pool.
func (d *Driver) warnParent(p *parentInfo) {
	if p.vnis.IsEmpty() {
		d.logf("warning: parent %s has no readable VNI set; publishing vni_set=%q so the admission selector never matches it and no namespace is scheduled onto it",
			p.Name(), noVNIs)
		return
	}
	if d.vniRangeStart != 0 || d.vniRangeEnd != 0 {
		if !ValidVNI(d.vniRangeStart) || !ValidVNI(d.vniRangeEnd) ||
			!p.vnis.Covers(uint16(d.vniRangeStart), uint16(d.vniRangeEnd)) {
			d.logf("warning: parent %s VNI set {%s} does not cover --vni-range-start..--vni-range-end %d-%d; claims from namespaces outside it fail on this parent",
				p.Name(), p.vnis, d.vniRangeStart, d.vniRangeEnd)
		}
	}
	remaining, err := cxi.ReadParentRemaining(p.PFName, p.SvcID, p.vfNames())
	if err != nil {
		d.logf("warning: parent %s: reading remaining budget: %v", p.Name(), err)
		return
	}
	if remaining.LEs < cxi.LEPoolSize {
		d.logf("warning: parent %s has %d LEs left after its VFs' Ethernet children, less than one pool (%d); pod services on its VFs may fail to create",
			p.Name(), remaining.LEs, cxi.LEPoolSize)
	}
}

// readParent fills in the parent's budget, VNI set, Ethernet-child reservation
// and ceiling: the budget minus one Ethernet-child reservation per VF
// (measured on any VF whose netdev is up), with LEs and TLEs forced to 0
// because children inherit them from the parent.
func readParent(p *parentInfo) error {
	budget, vnis, err := cxi.ReadParentBudget(p.PFName, p.SvcID)
	if err != nil {
		return err
	}
	p.budget, p.vnis = budget, vnis
	for _, vf := range p.VFs {
		r, err := cxi.ReadVFSystemReservation(vf.Name)
		if err != nil {
			return fmt.Errorf("reading the Ethernet child on %s: %w", vf.Name, err)
		}
		if r != (cxi.ResourceSet{}) {
			p.ethPerVF = r
			break
		}
	}
	var reserved cxi.ResourceSet
	for range p.VFs {
		reserved = reserved.Add(p.ethPerVF)
	}
	c := budget.Sub(reserved).ClampToZero()
	c.LEs, c.TLEs = 0, 0
	p.ceiling = c
	return nil
}

// liveAvailable is the capacity left right now on a published device: the
// parent's remaining budget in VF mode, the PF's free resources otherwise.
func (d *Driver) liveAvailable(name string) (cxi.ResourceSet, error) {
	if p, ok := d.parents.get(name); ok {
		return cxi.ReadParentRemaining(p.PFName, p.SvcID, p.vfNames())
	}
	return cxi.ReadAvailableCapacity(name)
}

// AvailableFor is the live capacity the NRI plugin checks an allocation against
// before creating its service.
func (d *Driver) AvailableFor(alloc PodAllocation) (cxi.ResourceSet, error) {
	if alloc.ParentName != "" {
		return d.liveAvailable(alloc.ParentName)
	}

	return cxi.ReadAvailableCapacity(alloc.DeviceName)
}

// ServiceOptionsFor supplies the discovered parent and sibling VFs for child
// creation. PF allocations retain the default service behavior.
func (d *Driver) ServiceOptionsFor(alloc PodAllocation) (cxi.ServiceOptions, error) {
	if alloc.ParentName == "" {
		return cxi.ServiceOptions{}, nil
	}
	parent, ok := d.parents.get(alloc.ParentName)
	if !ok {
		return cxi.ServiceOptions{}, fmt.Errorf("no discovered parent %s for VF %s", alloc.ParentName, alloc.DeviceName)
	}
	return cxi.ServiceOptions{Parent: parent.Parent}, nil
}
