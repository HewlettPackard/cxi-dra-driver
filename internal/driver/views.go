package driver

import (
	"fmt"
	"sync"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// parentInfo is one VF-mode parent as the driver uses it: the VFs grouped
// under it by discovery and the ceiling published for it.
type parentInfo struct {
	*cxi.Parent
	ceiling cxi.ResourceSet
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
		// A parent's VFs share the PF's NUMA node; it has no single netdev or PCI function.
		out = append(out, publishedDevice{name: p.Name(), numaNode: p.VFs[0].NUMANode})
	}
	return out
}

// programmableDevices are the devices services are created on: CDI specs, the
// allocation handed to the NRI plugin, and the reaper. PFs, or VFs with --vf-only.
func (d *Driver) programmableDevices() []*cxi.Device {
	return d.devices
}

// setupParents groups the discovered VFs by parent and reads each parent's
// ceiling. A no-op in PF mode.
func (d *Driver) setupParents() {
	if !d.vfOnly {
		return
	}
	var parents []*parentInfo
	for _, p := range cxi.Parents(d.programmableDevices()) {
		info := &parentInfo{Parent: p}
		ceiling, err := parentCeiling(p)
		if err != nil {
			d.logf("ERROR: parent %s: %v; publishing it with zero capacity", p.Name(), err)
		} else {
			info.ceiling = ceiling
		}
		d.logf("parent %s: VFs %v, published ceiling (%s)", p.Name(), info.vfNames(), info.ceiling)
		parents = append(parents, info)
	}
	d.parents.set(parents)
}

// parentCeiling is the parent's budget minus one Ethernet-child reservation
// per VF (measured on any VF whose netdev is up), with LEs and TLEs forced to
// 0 because children inherit them from the parent.
func parentCeiling(p *cxi.Parent) (cxi.ResourceSet, error) {
	budget, _, err := cxi.ReadParentBudget(p.PFName, p.SvcID)
	if err != nil {
		return cxi.ResourceSet{}, err
	}
	var perVF cxi.ResourceSet
	for _, vf := range p.VFs {
		r, err := cxi.ReadVFSystemReservation(vf.Name)
		if err != nil {
			return cxi.ResourceSet{}, fmt.Errorf("reading the Ethernet child on %s: %w", vf.Name, err)
		}
		if r != (cxi.ResourceSet{}) {
			perVF = r
			break
		}
	}
	var reserved cxi.ResourceSet
	for range p.VFs {
		reserved = reserved.Add(perVF)
	}
	c := budget.Sub(reserved).ClampToZero()
	c.LEs, c.TLEs = 0, 0
	return c, nil
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
