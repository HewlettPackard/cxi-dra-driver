package cxi

import (
	"fmt"
	"strconv"
	"strings"
)

// MockDevices returns n synthetic CXI devices for use in local testing
// environments (e.g. kind) where no real hardware is present.
//
// DevPath is set to /dev/null so CDI can bind-mount a valid file into the
// container. SysfsPath points to /tmp/mock-cxi/cxiN which the driver creates
// at startup. All other attributes are plausible but fabricated.
func MockDevices(n int) []*Device {
	devices := make([]*Device, n)
	for i := range n {
		devices[i] = &Device{
			Name:        fmt.Sprintf("cxi%d", i),
			Index:       i,
			DevPath:     fmt.Sprintf("/dev/cxi%d", i),
			HostDevPath: "/dev/null",
			SysfsPath:   fmt.Sprintf("/tmp/mock-cxi/cxi%d", i),
			NUMANode:    i % 2,
			PCIBusID:    fmt.Sprintf("0000:%02x:00.0", 0x81+i),
			HSNIface:    fmt.Sprintf("hsn%d", i*2),
			Capacity:    DeviceCapacity(),
		}
	}
	return devices
}

// Mock VF topology (--mock-vfs): one mock PF, cxi0, with parent services whose
// budgets and Ethernet children match what the hardware tests measured.
const (
	mockPFName       = "cxi0"
	mockFirstParent  = 4 // svc ids 1-3 are the PF's default and system services
	mockVFSystemSvc  = 2 // per-VF id of the Ethernet child
	mockParentLEMax  = 16384
	maxMockParents   = 16
	maxMockVFsPerPF  = 64 // sriov_totalvfs on Cassini
	mockVNIBlockSize = 100
)

var (
	// MockParentBudget is each mock parent's reservation: the parent template
	// used on the SR-IOV test nodes (LEs sized for the VF Ethernet children).
	MockParentBudget = ResourceSet{ACs: 64, CTs: 128, EQs: 128, LEs: 4096, PTEs: 64, TGQs: 32, TXQs: 64, TLEs: 512}
	// MockEthernetChild is the system child the kernel creates on every VF whose
	// netdev is up, as measured on SHS 15.0.0.
	MockEthernetChild = ResourceSet{ACs: 1, EQs: 1, LEs: 660, PTEs: 8, TGQs: 1, TXQs: 1}
)

// ParseMockVFs parses the --mock-vfs value "<parents>x<vfs-per-parent>", e.g. "2x4".
func ParseMockVFs(s string) (parents, vfsPerParent int, err error) {
	ps, vs, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), "x")
	if ok {
		parents, err = strconv.Atoi(ps)
		if err == nil {
			vfsPerParent, err = strconv.Atoi(vs)
		}
	}
	if !ok || err != nil {
		return 0, 0, fmt.Errorf("--mock-vfs %q: want <parents>x<vfs-per-parent>, e.g. 2x4", s)
	}
	if parents < 1 || parents > maxMockParents || vfsPerParent < 1 || parents*vfsPerParent > maxMockVFsPerPF {
		return 0, 0, fmt.Errorf("--mock-vfs %q: need 1-%d parents and 1-%d VFs in total", s, maxMockParents, maxMockVFsPerPF)
	}
	return parents, vfsPerParent, nil
}

// MockParentVNIs is the VNI set of mock parent k (0-based). Sets are disjoint:
// even parents get the range [100(k+1), 100(k+1)+15], odd parents the
// non-contiguous list {100(k+1), 100(k+1)+2, 100(k+1)+4}, so both published
// forms reach the admission selector. VNIs between the sets belong to no parent.
func MockParentVNIs(k int) VNISet {
	base := uint16(mockVNIBlockSize * (k + 1))
	if k%2 == 1 {
		return NewVNIList([]uint16{base, base + 2, base + 4})
	}
	s, _ := NewVNIRange(base, base+15)
	return s
}

// MockVFTopology returns the VF devices of a mock PF cxi0 with parents parent
// services of vfsPerParent VFs each, and the topology the parent reads and the
// mock ServiceManager answer from (install it with SetMockTopology). Parent k
// is svc 4+k on cxi0 and gets VFs cxi(1+k*vfsPerParent)...; every VF carries
// its Ethernet system child. Like real discovery in --vf-only mode, the PF
// itself is not returned.
func MockVFTopology(parents, vfsPerParent int) ([]*Device, MockTopology) {
	topo := MockTopology{VFServices: make(map[string][]ServiceReservation)}
	var vfs []*Device
	for k := range parents {
		svc := mockFirstParent + k
		topo.Parents = append(topo.Parents, MockParent{
			PFName: mockPFName,
			SvcID:  svc,
			Descriptor: ParentDescriptor{
				ResourceLimits: true,
				Limits:         MockParentBudget,
				LEMax:          mockParentLEMax,
				VNIs:           MockParentVNIs(k),
			},
		})
		for range vfsPerParent {
			n := len(vfs) + 1 // VF n is cxi<n> and PCI function n of the PF
			name := fmt.Sprintf("cxi%d", n)
			vfs = append(vfs, &Device{
				Name:         name,
				Index:        n,
				DevPath:      fmt.Sprintf("/dev/cxi%d", n),
				HostDevPath:  "/dev/null",
				SysfsPath:    fmt.Sprintf("/tmp/mock-cxi/cxi%d", n),
				NUMANode:     0,
				PCIBusID:     fmt.Sprintf("0000:81:%02x.%d", n/8, n%8),
				HSNIface:     fmt.Sprintf("enp129s%df%d", n/8, n%8),
				FunctionType: FunctionVF,
				PFName:       mockPFName,
				PFPCIBusID:   "0000:81:00.0",
				PFNUMANode:   0,
				PFNetdev:     "hsn0",
				VFIndex:      n - 1,
				ParentSvcID:  svc,
			})
			topo.VFServices[name] = []ServiceReservation{{
				SvcID: mockVFSystemSvc, IsSystem: true, ResourceLimits: true, Res: MockEthernetChild,
			}}
		}
	}
	return vfs, topo
}
