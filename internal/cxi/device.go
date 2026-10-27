package cxi

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const sysfsClassPath = "/sys/class/cxi_user"

// FunctionType distinguishes an SR-IOV physical function from a virtual one.
type FunctionType string

const (
	// FunctionPF is a physical function: a whole Cassini NIC.
	FunctionPF FunctionType = "pf"
	// FunctionVF is an SR-IOV virtual function (has device/physfn in sysfs).
	FunctionVF FunctionType = "vf"
)

// Device represents a discovered CXI PCIe device on this node.
type Device struct {
	Name        string // "cxi0"
	Index       int    // 0
	DevPath     string // "/dev/cxi0" — container-visible device path
	HostDevPath string // host-side device path when it differs from DevPath (e.g. mock uses /dev/null)
	SysfsPath   string // host-side sysfs path; container sees it at /sys/class/cxi_user/<name>
	NUMANode    int    // NUMA node affinity (-1 if unknown)
	PCIBusID    string // PCI bus address in extended BDF form, e.g. "0000:81:00.0"
	HSNIface    string // paired HSN interface, e.g. "hsn2" (set by PairDevice)

	// Capacity holds the total hardware resource limits for this device,
	// read from cxil_devinfo via libcxi at discovery time (production builds).
	// In non-cxi builds DeviceCapacity() is used. Discovery hard-fails if
	// capacity cannot be read in production. Zero for a VF until its parent
	// service's budget is read (SSHOTECO-120/123): a VF's own devinfo reports
	// whole-NIC limits that its parent cannot honour, so no request may fit
	// until VF parent-budget accounting is wired into the driver.
	Capacity ResourceSet

	// Reserved holds the resources consumed by the blessed system services
	// (--system-svc-ids) on this device, summed at discovery time. Subtracted
	// from Capacity to form the usable ceiling used by the prepare-time
	// fast-fail. Zero in non-cxi/mock builds, and always zero for a VF
	// (--system-svc-ids ids are PF service ids).
	Reserved ResourceSet

	// FunctionType is FunctionPF or FunctionVF. The zero value is treated as a
	// PF so devices built without it (mock devices) keep PF behavior.
	FunctionType FunctionType

	// PFName is the cxi name of the PF a VF belongs to ("" for a PF).
	PFName string

	// VFIndex is the VF's index under its PF, from the PF's virtfnN link.
	// Meaningful only for a VF.
	VFIndex int

	// ParentSvcID is the svc_id of the parent service assigned to a VF.
	// Read from the PF's sysfs vf/<N>/svc_id; always non-zero for a discovered
	// VF (unassigned VFs are dropped) and 0 for a PF.
	ParentSvcID int

	// PodIfName is the pod-side interface name the CNI gives this device.
	// Placeholder for SSHOTECO-125; empty until then.
	PodIfName string
}

// IsVF reports whether the device is an SR-IOV virtual function.
func (d *Device) IsVF() bool {
	return d.FunctionType == FunctionVF
}

// DiscoverOptions selects what Discover publishes.
type DiscoverOptions struct {
	// SystemSvcIDs are the blessed system service svc_ids whose reserved
	// resources are summed into each PF's Reserved field. Ignored for VFs.
	SystemSvcIDs []int

	// VFOnly selects VF mode (--vf-only): only SR-IOV VFs are discovered, from
	// sysfs alone, and PFs are skipped before any libcxi read or netdev
	// pairing, so a PF is never opened. In the default mode VFs are skipped.
	VFOnly bool
}

// Discover walks /sys/class/cxi_user and returns one Device per cxi* entry of
// the selected function type (PFs by default, VFs with opts.VFOnly), ordered
// by device index. PFs and VFs are never mixed. Every skipped device is logged
// with the reason. In VF mode, finding no usable VF is logged as an error and
// returns an empty list rather than an error, so the driver publishes an empty
// slice instead of crash-looping.
func Discover(opts DiscoverOptions) ([]*Device, error) {
	return newDiscoverer().discover(opts)
}

// discoverer holds Discover's sysfs directories and hardware-read seams so the
// sysfs walk is unit-testable against a fake tree. Production uses
// /sys/class/cxi_user, the net class dir from $SYS_NET_PATH, and the
// libcxi-backed reads.
type discoverer struct {
	classPath    string
	netPath      string
	readCapacity func(name string) (ResourceSet, error)
	readReserved func(name string, svcIDs []int) (ResourceSet, error)
	// pfClassPath holds the PFs' vf/<N>/svc_id files.
	pfClassPath string
	// parentHasLimits reports whether the kernel enforces resource limits on
	// a parent service. Production discovery wires this to ReadParentBudget;
	// tests can inject the result to isolate sysfs discovery behavior.
	parentHasLimits func(pfName string, svcID int) (bool, error)
	logf            func(format string, args ...any)
}

func newDiscoverer() *discoverer {
	return &discoverer{
		classPath:       sysfsClassPath,
		netPath:         sysfsNetPath(),
		readCapacity:    readDeviceCapacity,
		readReserved:    readReservedForSvcs,
		pfClassPath:     sysfsPFClassPath,
		parentHasLimits: parentHasResourceLimits,
		logf: func(format string, args ...any) {
			log.Printf("cxi-dra-driver: "+format, args...)
		},
	}
}

func parentHasResourceLimits(pfName string, svcID int) (bool, error) {
	_, _, err := ReadParentBudget(pfName, svcID)
	if errors.Is(err, ErrParentUnlimited) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// candidate is one cxi* sysfs entry, classified from sysfs alone.
type candidate struct {
	name    string
	index   int
	sysfs   string
	pciPath string // resolved <cxi>/device
	isVF    bool
	pfPCI   string // resolved <cxi>/device/physfn; "" if it does not resolve
}

func (x *discoverer) discover(opts DiscoverOptions) ([]*Device, error) {
	entries, err := os.ReadDir(x.classPath)
	if err != nil {
		if os.IsNotExist(err) {
			if opts.VFOnly {
				x.logf("ERROR: --vf-only: %s does not exist; no SR-IOV VF to publish", x.classPath)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", x.classPath, err)
	}

	var cands []candidate
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cxi") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimPrefix(name, "cxi"))
		if err != nil {
			continue
		}
		cands = append(cands, classify(name, idx, filepath.Join(x.classPath, name)))
	}
	// ReadDir sorts by name, which puts cxi10 before cxi2.
	sort.Slice(cands, func(i, j int) bool { return cands[i].index < cands[j].index })

	// PF PCI path -> PF cxi name, for resolving a VF's physfn from sysfs alone.
	pfByPCI := make(map[string]string)
	for _, c := range cands {
		if !c.isVF && c.pciPath != "" {
			pfByPCI[c.pciPath] = c.name
		}
	}

	var devices []*Device
	for _, c := range cands {
		var dev *Device
		switch {
		case !opts.VFOnly && c.isVF:
			x.logf("skipping %s: SR-IOV VF (device/physfn present); run with --vf-only to publish VFs", c.name)
			continue
		case opts.VFOnly && !c.isVF:
			x.logf("skipping %s: physical function in --vf-only mode", c.name)
			continue
		case opts.VFOnly:
			dev, err = x.buildVF(c, pfByPCI)
			if err != nil {
				x.logf("ERROR: skipping VF %s: %v", c.name, err)
				continue
			}
		default:
			dev, err = x.buildPF(c, opts.SystemSvcIDs)
			if err != nil {
				return nil, err
			}
		}

		devices = append(devices, dev)
	}

	if opts.VFOnly {
		devices = x.dropUnlimitedParents(devices)
		x.warnPFsWithoutVFs(cands)
	}
	for _, dev := range devices {
		if err := pairDevice(dev, x.netPath); err != nil {
			// Best-effort, but the CNI rejects a hint with an empty hsn_iface.
			x.logf("WARNING: device %s has no paired netdev (hsn_iface will be empty, CNI attach will fail): %v", dev.Name, err)
		}
	}
	if opts.VFOnly && len(devices) == 0 {
		x.logf("ERROR: --vf-only found no usable SR-IOV VF; publishing an empty ResourceSlice")
	}
	return devices, nil
}

// classify resolves an entry's PCI function and whether it is a VF. Only VFs
// have device/physfn; its presence counts even if the link is dangling.
func classify(name string, idx int, sysfs string) candidate {
	c := candidate{name: name, index: idx, sysfs: sysfs}
	if p, err := filepath.EvalSymlinks(filepath.Join(sysfs, "device")); err == nil {
		c.pciPath = p
	}
	physfn := filepath.Join(sysfs, "device", "physfn")
	if _, err := os.Lstat(physfn); err == nil {
		c.isVF = true
		if p, err := filepath.EvalSymlinks(physfn); err == nil {
			c.pfPCI = p
		}
	}
	return c
}

func baseDevice(c candidate) *Device {
	return &Device{
		Name:      c.name,
		Index:     c.index,
		DevPath:   filepath.Join("/dev", c.name),
		SysfsPath: c.sysfs,
		NUMANode:  readIntFile(filepath.Join(c.sysfs, "device", "numa_node"), -1),
		PCIBusID:  readPCISlot(filepath.Join(c.sysfs, "device", "uevent")),
	}
}

func (x *discoverer) buildPF(c candidate, systemSvcIDs []int) (*Device, error) {
	capacity, err := x.readCapacity(c.name)
	if err != nil {
		return nil, fmt.Errorf("reading capacity for device %s: %w", c.name, err)
	}
	reserved, err := x.readReserved(c.name, systemSvcIDs)
	if err != nil {
		return nil, fmt.Errorf("reading reserved capacity for device %s: %w", c.name, err)
	}
	dev := baseDevice(c)
	dev.FunctionType = FunctionPF
	dev.VFIndex = -1
	dev.Capacity = capacity
	dev.Reserved = reserved
	return dev, nil
}

// buildVF resolves a VF's PF and VF index from sysfs alone; nothing is opened
// through libcxi. Capacity stays zero until the parent budget is read.
func (x *discoverer) buildVF(c candidate, pfByPCI map[string]string) (*Device, error) {
	if c.pfPCI == "" {
		return nil, fmt.Errorf("device/physfn does not resolve")
	}
	pfName, ok := pfByPCI[c.pfPCI]
	if !ok {
		return nil, fmt.Errorf("PF %s has no cxi device", filepath.Base(c.pfPCI))
	}
	vfIdx, err := vfIndex(c.pfPCI, c.pciPath)
	if err != nil {
		return nil, fmt.Errorf("PF %s: %w", pfName, err)
	}
	svcID, err := readParentSvcID(x.pfClassPath, pfName, vfIdx)
	if err != nil {
		return nil, fmt.Errorf("PF %s vf %d: %w", pfName, vfIdx, err)
	}
	dev := baseDevice(c)
	dev.FunctionType = FunctionVF
	dev.PFName = pfName
	dev.VFIndex = vfIdx
	dev.ParentSvcID = svcID
	return dev, nil
}

// dropUnlimitedParents drops the VFs of every parent without resource limits:
// the budget model needs the kernel to enforce a per-parent budget.
func (x *discoverer) dropUnlimitedParents(vfs []*Device) []*Device {
	if x.parentHasLimits == nil {
		return vfs
	}
	drop := make(map[*Device]bool)
	for _, p := range Parents(vfs) {
		ok, err := x.parentHasLimits(p.PFName, p.SvcID)
		var reason string
		switch {
		case err != nil:
			reason = fmt.Sprintf("cannot read its resource limits: %v", err)
		case !ok:
			reason = "it has no resource limits (Resource Limits: No)"
		default:
			continue
		}
		for _, d := range p.VFs {
			drop[d] = true
			x.logf("ERROR: skipping VF %s: parent service %d on PF %s: %s", d.Name, p.SvcID, p.PFName, reason)
		}
	}
	if len(drop) == 0 {
		return vfs
	}
	kept := vfs[:0:0]
	for _, d := range vfs {
		if !drop[d] {
			kept = append(kept, d)
		}
	}
	return kept
}

// warnPFsWithoutVFs logs every PF that has no VF at all.
func (x *discoverer) warnPFsWithoutVFs(cands []candidate) {
	hasVF := make(map[string]bool)
	for _, c := range cands {
		if c.isVF {
			hasVF[c.pfPCI] = true
		}
	}
	for _, c := range cands {
		if !c.isVF && !hasVF[c.pciPath] {
			x.logf("WARNING: --vf-only: PF %s has no SR-IOV VFs", c.name)
		}
	}
}

// vfIndex returns N for the PF's virtfnN link that resolves to vfPCI.
func vfIndex(pfPCI, vfPCI string) (int, error) {
	links, err := filepath.Glob(filepath.Join(pfPCI, "virtfn*"))
	if err != nil {
		return 0, err
	}
	for _, l := range links {
		target, err := filepath.EvalSymlinks(l)
		if err != nil || target != vfPCI {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(l), "virtfn"))
		if err != nil {
			continue
		}
		return n, nil
	}
	return 0, fmt.Errorf("no virtfn link resolves to %s", filepath.Base(vfPCI))
}

func readIntFile(path string, fallback int) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fallback
	}
	return v
}

// readPCISlot extracts PCI_SLOT_NAME from a uevent file.
func readPCISlot(ueventPath string) string {
	data, err := os.ReadFile(ueventPath)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "PCI_SLOT_NAME="); ok {
			return v
		}
	}
	return ""
}
