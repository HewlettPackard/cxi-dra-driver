package cxi

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sysfsPFClassPath is the class directory holding each PF's vf/<N>/svc_id
// files, separate from the cxi_user class used for device discovery.
const sysfsPFClassPath = "/sys/class/cxi"

// Parent is one parent CXI service: the budget and VNI set shared by every VF
// assigned to it. It is identified by (PFName, SvcID); the same svc_id on two
// PFs is two distinct parents.
type Parent struct {
	// PFName is the CXI device name of the physical function.
	PFName string
	// SvcID is the parent service ID assigned to the VFs.
	SvcID int
	// VFs are the VFs assigned to this parent, in device index order.
	VFs []*Device
}

// Name is the parent's DRA device name, <pf>-svc<id>.
func (p Parent) Name() string {
	return fmt.Sprintf("%s-svc%d", p.PFName, p.SvcID)
}

// Parents groups VFs by (PF, parent svc_id). Parents are ordered by PF index,
// then svc_id; each parent's VFs are ordered by device index. Devices that are not
// VFs, or have no parent assigned, are ignored.
func Parents(vfs []*Device) []*Parent {
	type key struct {
		pf  string
		svc int
	}
	byKey := make(map[key]*Parent)
	var out []*Parent
	for _, d := range vfs {
		if d == nil || !d.IsVF() || d.ParentSvcID <= 0 {
			continue
		}
		k := key{d.PFName, d.ParentSvcID}
		p, ok := byKey[k]
		if !ok {
			p = &Parent{PFName: d.PFName, SvcID: d.ParentSvcID}
			byKey[k] = p
			out = append(out, p)
		}
		p.VFs = append(p.VFs, d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := cxiIndex(out[i].PFName), cxiIndex(out[j].PFName); a != b {
			return a < b
		}
		if out[i].PFName != out[j].PFName {
			return out[i].PFName < out[j].PFName
		}
		return out[i].SvcID < out[j].SvcID
	})
	for _, p := range out {
		sort.SliceStable(p.VFs, func(i, j int) bool {
			if p.VFs[i].Index != p.VFs[j].Index {
				return p.VFs[i].Index < p.VFs[j].Index
			}
			return p.VFs[i].Name < p.VFs[j].Name
		})
	}
	return out
}

// cxiIndex returns N for a "cxiN" name, or -1.
func cxiIndex(name string) int {
	suffix, ok := strings.CutPrefix(name, "cxi")
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(suffix)
	if err != nil {
		return -1
	}
	return n
}

// readParentSvcID reads the parent svc_id the PF's sysfs assigns to VF vfIdx.
// An unreadable file, a malformed value or 0 means no parent is assigned.
func readParentSvcID(classPath, pfName string, vfIdx int) (int, error) {
	path := filepath.Join(classPath, pfName, "vf", strconv.Itoa(vfIdx), "svc_id")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("parent service not assigned: cannot read %s: %w", path, err)
	}
	raw := strings.TrimSpace(string(data))
	id, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parent service not assigned: malformed svc_id %q in %s", raw, path)
	}
	if id <= 0 {
		return 0, fmt.Errorf("parent service not assigned: svc_id is %d in %s", id, path)
	}
	return id, nil
}
