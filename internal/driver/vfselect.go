package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// readActiveServices lists a device's services; a seam for tests.
var readActiveServices = cxi.ReadActiveServices

// getParentLock returns the mutex serialising VF selection under one parent,
// creating it if needed (modelled on the NRI plugin's getDeviceLock).
func (d *Driver) getParentLock(name string) *sync.Mutex {
	v, _ := d.parentLocks.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// lockParents takes the locks of every parent the results allocate, in name
// order so two claims spanning the same parents cannot deadlock. PF mode takes
// none. The returned func releases them.
func (d *Driver) lockParents(results []resourceapi.DeviceRequestAllocationResult) func() {
	var names []string
	for _, r := range results {
		if _, ok := d.parents.get(r.Device); ok && !slices.Contains(names, r.Device) {
			names = append(names, r.Device)
		}
	}
	sort.Strings(names)
	locks := make([]*sync.Mutex, 0, len(names))
	for _, n := range names {
		mu := d.getParentLock(n)
		mu.Lock()
		locks = append(locks, mu)
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}
}

// vfHolders maps each VF that is not free to who holds it: a stored pod or
// prepared-claim allocation (the in-memory AllocationStore), or a child found
// at startup without a hint file.
func (d *Driver) vfHolders() map[string][]string {
	held := d.store.DeviceHolders()
	d.recoveredMu.Lock()
	defer d.recoveredMu.Unlock()
	for vf, why := range d.recoveredBusy {
		held[vf] = append(held[vf], why)
	}
	return held
}

// shareIDOf returns the result's ShareID, "" when unset.
func shareIDOf(r resourceapi.DeviceRequestAllocationResult) string {
	if r.ShareID == nil {
		return ""
	}
	return string(*r.ShareID)
}

// programmableFor maps each of the claim's results to the device its service is
// created on. In PF mode that is the allocated device. In VF mode it is a VF of
// the allocated parent: the one recorded for this result if the claim was
// already prepared here (kubelet re-prepares), otherwise the lowest free one.
// Results on the same parent get distinct VFs. Callers hold lockParents.
func (d *Driver) programmableFor(claimUID types.UID, results []resourceapi.DeviceRequestAllocationResult) ([]string, error) {
	out := make([]string, len(results))
	if !d.vfOnly {
		for i, r := range results {
			out[i] = r.Device
		}
		return out, nil
	}

	prior := make(map[pendingKey]string)
	for _, a := range d.store.ClaimAllocs(claimUID) {
		prior[pendingKey{deviceName: a.ParentName, shareID: a.ShareID}] = a.DeviceName
	}
	held := d.vfHolders()
	taken := make(map[string]bool)
	for i, r := range results {
		if vf, ok := prior[pendingKey{deviceName: r.Device, shareID: shareIDOf(r)}]; ok {
			out[i] = vf
			taken[vf] = true
		}
	}
	for i, r := range results {
		if out[i] != "" {
			continue
		}
		p, ok := d.parents.get(r.Device)
		if !ok {
			return nil, fmt.Errorf("device %s is not a parent published by this driver", r.Device)
		}
		for _, vf := range p.VFs {
			if !taken[vf.Name] && len(held[vf.Name]) == 0 {
				out[i] = vf.Name
				taken[vf.Name] = true
				break
			}
		}
		if out[i] == "" {
			return nil, fmt.Errorf("parent %s has no free VF: %s", r.Device, describeVFs(p, held, taken))
		}
	}
	return out, nil
}

// describeVFs lists a parent's VFs with who holds each, for the no-free-VF error.
func describeVFs(p *parentInfo, held map[string][]string, taken map[string]bool) string {
	parts := make([]string, 0, len(p.VFs))
	for _, vf := range p.VFs {
		switch {
		case len(held[vf.Name]) > 0:
			parts = append(parts, fmt.Sprintf("%s (%s)", vf.Name, strings.Join(held[vf.Name], ", ")))
		case taken[vf.Name]:
			parts = append(parts, vf.Name+" (this claim)")
		default:
			parts = append(parts, vf.Name+" (free)")
		}
	}
	return strings.Join(parts, "; ")
}

// recoverVFState rebuilds, before the plugin serves, which VFs are taken after
// a driver restart (VF mode only). For every pod with a hint file that a claim
// allocated on this node still reserves, the pod's allocation is put back in
// the store so its VFs stay taken and Unprepare can release them. A VF with a
// pod child service but no hint file is marked busy until the reaper removes
// the child.
func (d *Driver) recoverVFState(ctx context.Context) {
	if !d.vfOnly {
		return
	}
	hints := d.readHintFiles()
	recovered := 0
	if len(hints) > 0 {
		claims, err := d.kubeClient.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
		if err != nil {
			// Fail closed: without the claims every hinted VF stays taken.
			d.logf("warning: VF recovery: listing ResourceClaims: %v; treating every VF in a hint file as busy", err)
			d.recoveredMu.Lock()
			for pod, hs := range hints {
				for _, h := range hs {
					d.recoveredBusy[h.Device] = "hint file of pod " + string(pod)
				}
			}
			d.recoveredMu.Unlock()
		} else {
			recovered = d.recoverFromClaims(claims.Items, hints)
		}
	}

	held := d.vfHolders()
	for _, vf := range d.programmableDevices() {
		if len(held[vf.Name]) > 0 {
			continue
		}
		svcs, err := readActiveServices(vf.Name)
		if err != nil {
			d.logf("warning: VF recovery: reading services on %s: %v", vf.Name, err)
			continue
		}
		if id, ok := podChild(svcs); ok {
			d.recoveredMu.Lock()
			d.recoveredBusy[vf.Name] = fmt.Sprintf("child svc %d without a hint file", id)
			d.recoveredMu.Unlock()
			d.logf("warning: VF recovery: %s has pod child svc %d but no hint file; busy until the reaper removes it", vf.Name, id)
		}
	}
	d.logf("VF recovery: %d pod allocation(s) restored from hint files, %d VF(s) busy without a hint", recovered, len(d.recoveredBusy))
}

// podChild returns the first non-system service on a VF.
func podChild(svcs []cxi.ServiceInfo) (int, bool) {
	for _, s := range svcs {
		if !s.IsSystem {
			return s.SvcID, true
		}
	}
	return 0, false
}

// readHintFiles returns the hint entries on disk per pod, keeping only entries
// for this node's VFs.
func (d *Driver) readHintFiles() map[types.UID][]CNIHint {
	out := make(map[types.UID][]CNIHint)
	if d.cniHintsDir == "" {
		return out
	}
	files, err := filepath.Glob(filepath.Join(d.cniHintsDir, "*.json"))
	if err != nil {
		d.logf("warning: VF recovery: listing %s: %v", d.cniHintsDir, err)
		return out
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			d.logf("warning: VF recovery: reading %s: %v", f, err)
			continue
		}
		var hs []CNIHint
		if err := json.Unmarshal(data, &hs); err != nil {
			d.logf("warning: VF recovery: parsing %s: %v", f, err)
			continue
		}
		var mine []CNIHint
		for _, h := range hs {
			if _, ok := d.parents.parentOf(h.Device); ok {
				mine = append(mine, h)
			}
		}
		if len(mine) > 0 {
			out[types.UID(strings.TrimSuffix(filepath.Base(f), ".json"))] = mine
		}
	}
	return out
}

// recoverFromClaims restores the store entries of hinted pods that a claim
// allocated on this node still reserves, and returns how many it restored.
// Hints of pods no claim reserves are logged and leave their VFs free.
func (d *Driver) recoverFromClaims(claims []resourceapi.ResourceClaim, hints map[types.UID][]CNIHint) int {
	restored := make(map[types.UID]bool)
	for i := range claims {
		claim := &claims[i]
		if claim.Status.Allocation == nil {
			continue
		}
		var results []resourceapi.DeviceRequestAllocationResult
		for _, r := range claim.Status.Allocation.Devices.Results {
			if _, ok := d.parents.get(r.Device); ok && r.Driver == d.driverName && r.Pool == d.nodeName {
				results = append(results, r)
			}
		}
		if len(results) == 0 {
			continue
		}
		for _, ref := range claim.Status.ReservedFor {
			pod := types.UID(ref.UID)
			hs, ok := hints[pod]
			if ref.Resource != "pods" || !ok {
				continue
			}
			allocs := d.allocsFromHints(claim, results, hs)
			if len(allocs) == 0 {
				continue
			}
			d.store.Set(pod, allocs)
			d.store.SetClaimPod(claim.UID, pod)
			if d.store.ClaimAllocs(claim.UID) == nil {
				d.store.SetClaimAllocs(claim.UID, allocs)
			}
			restored[pod] = true
			d.logf("VF recovery: pod %s of claim %s/%s holds %v", pod, claim.Namespace, claim.Name, deviceNames(allocs))
		}
	}
	for pod := range hints {
		if !restored[pod] {
			d.logf("VF recovery: hint file of pod %s is not reserved by any claim on this node; its VFs stay free", pod)
		}
	}
	return len(restored)
}

// allocsFromHints pairs a pod's hinted VFs with the claim's results on their
// parents, in order (results on one parent are interchangeable).
func (d *Driver) allocsFromHints(claim *resourceapi.ResourceClaim, results []resourceapi.DeviceRequestAllocationResult, hs []CNIHint) []PodAllocation {
	used := make([]bool, len(results))
	var out []PodAllocation
	for _, h := range hs {
		p, _ := d.parents.parentOf(h.Device)
		for i, r := range results {
			if used[i] || r.Device != p.Name() {
				continue
			}
			used[i] = true
			requested, err := d.resolveClaim(claim, r)
			if err != nil {
				d.logf("warning: VF recovery: claim %s/%s device %s: %v", claim.Namespace, claim.Name, r.Device, err)
			}
			pa := d.podAllocationBase(h.Device)
			pa.VNI = h.VNI
			pa.Resources = requested
			pa.ClaimUID, pa.ClaimNamespace, pa.ClaimName = claim.UID, claim.Namespace, claim.Name
			pa.ParentName, pa.ShareID = r.Device, shareIDOf(r)
			out = append(out, pa)
			break
		}
	}
	return out
}

func deviceNames(allocs []PodAllocation) []string {
	out := make([]string, len(allocs))
	for i, a := range allocs {
		out[i] = a.DeviceName
	}
	return out
}

// releaseRecovered frees a recovered-busy VF once it no longer has a pod child.
// Called by the reaper with each VF's current service list.
func (d *Driver) releaseRecovered(vf string, svcs []cxi.ServiceInfo) {
	if _, child := podChild(svcs); child {
		return
	}
	d.recoveredMu.Lock()
	defer d.recoveredMu.Unlock()
	if why, ok := d.recoveredBusy[vf]; ok {
		delete(d.recoveredBusy, vf)
		d.logf("VF %s is free again (was busy: %s)", vf, why)
	}
}
