package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)

// reapGraceCycles is the number of consecutive reconcile cycles a service must
// look orphaned — pod netns gone, not blessed, not kernel-system, not tracked
// by the NRI plugin — before the reaper destroys it. At the default 10s refresh
// interval this is ~30s of confirmed-dead netns, comfortably longer than the
// window between cxil_alloc_svc and the kernel recording the pod netns during a
// normal pod start, so a pod mid-start is never mistaken for an orphan.
const reapGraceCycles = 3

// netnsInodeMask keeps only the low 32 bits of an inode. The kernel stores a
// service's netns as an unsigned int (see cxi_create_service, which truncates
// the inode when calling cxil_svc_set_netns), so live inodes read from /proc
// must be truncated the same way before comparison. Real nsfs inodes sit well
// below 2^32, so this is a no-op for genuine values but keeps the two sides
// bit-for-bit comparable regardless.
const netnsInodeMask = 0xFFFFFFFF

// reapKey identifies one service on one device, used to track how many
// consecutive cycles it has looked orphaned.
type reapKey struct {
	deviceName string
	svcID      int
}

// reapOrphanServices destroys CXI services whose owning pod is gone. A
// service leaked by a force-deleted pod (NRI StopPodSandbox never fired), a
// driver restart, or out-of-band creation would otherwise hold its VNI and
// resources on the device forever.
//
// A service is preserved if ANY of these hold, so the reaper only ever deletes
// a service it is certain is an orphan:
//   - its svc_id is blessed (--system-svc-ids),
//   - its kernel is_system_svc bit is set (Ethernet/default services),
//   - the NRI plugin still owns it for a live pod (LiveSvcIDs),
//   - the netns the kernel recorded for it is still live on the node,
//   - that netns cannot be determined (read error or 0) — indeterminate state
//     must never lead to a delete.
//
// Even once a service's netns is confirmed dead, it is destroyed only after
// reapGraceCycles consecutive confirmations.
//
// Inert in mock mode (no real services) and until SetLiveSvcIDsFunc has wired
// the NRI live-svc-id source — without it the reaper cannot tell a live pod's
// service from an orphan and must do nothing.
func (d *Driver) reapOrphanServices() {
	if d.mockDevices != nil {
		return
	}
	if d.liveSvcIDs == nil {
		return
	}

	live, err := liveNetnsInodes("/proc")
	if err != nil {
		d.logf("warning: reaper enumerating live netns: %v — skipping reap pass", err)
		return
	}
	// A non-empty live set is a sanity floor: the driver's own netns alone
	// guarantees at least one entry on any running node. An empty set means
	// enumeration silently produced nothing, and reaping against it would
	// destroy every service — refuse the whole pass.
	if len(live) == 0 {
		d.logf("warning: reaper found zero live netns inodes — skipping reap pass")
		return
	}

	blessed := make(map[int]bool, len(d.systemSvcIDs))
	for _, id := range d.systemSvcIDs {
		blessed[id] = true
	}
	nriLive := d.liveSvcIDs()

	seen := make(map[reapKey]bool)
	for _, dev := range d.programmableDevices() {
		svcs, err := cxi.ReadActiveServices(dev.Name)
		if err != nil {
			d.logf("warning: reaper reading services for %s: %v", dev.Name, err)
			continue
		}
		d.releaseRecovered(dev.Name, svcs)
		for _, svc := range svcs {
			key := reapKey{deviceName: dev.Name, svcID: svc.SvcID}
			seen[key] = true

			// Services we must never delete: blessed, kernel-system, or still
			// owned by the NRI plugin for a running pod. Reset any miss count.
			if blessed[svc.SvcID] || svc.IsSystem || nriLive[dev.Name][svc.SvcID] {
				delete(d.reaperMisses, key)
				continue
			}

			// Resolve the netns the kernel recorded for this service. An error
			// or a zero inode is indeterminate (e.g. a service mid-creation,
			// before cxil_svc_set_netns) and is treated as live.
			ns, err := cxi.ReadServiceNetns(dev.Name, svc.SvcID)
			if err != nil || ns == 0 {
				delete(d.reaperMisses, key)
				continue
			}
			if live[ns&netnsInodeMask] {
				delete(d.reaperMisses, key)
				continue
			}

			// netns confirmed dead this cycle. Require reapGraceCycles
			// consecutive confirmations before destroying.
			d.reaperMisses[key]++
			if d.reaperMisses[key] < reapGraceCycles {
				d.logf("reaper: service svc_id=%d on %s has dead netns inode=%d (%d/%d) — deferring",
					svc.SvcID, dev.Name, ns, d.reaperMisses[key], reapGraceCycles)
				continue
			}

			d.logf("reaper: destroying orphan service svc_id=%d on %s (netns inode=%d gone for %d cycles)",
				svc.SvcID, dev.Name, ns, d.reaperMisses[key])
			if err := d.reapSvcMgr.DeleteService(dev.Name, svc.SvcID); err != nil {
				d.logf("warning: reaper destroying svc_id=%d on %s: %v", svc.SvcID, dev.Name, err)
				continue
			}
			delete(d.reaperMisses, key)
		}
	}

	// Drop miss counts for services that no longer appear on any device — they
	// were destroyed (by us, the NRI plugin, or out of band) — so the map can't
	// grow without bound over the driver's lifetime.
	for key := range d.reaperMisses {
		if !seen[key] {
			delete(d.reaperMisses, key)
		}
	}
}

// liveNetnsInodes enumerates every process under procRoot and returns the set
// of network-namespace nsfs inodes currently in use on the node, each masked to
// its low 32 bits to match the truncated value the kernel stores per service.
// The driver runs with hostPID=true, so procRoot ("/proc") reflects the host
// PID namespace and therefore covers every pod's netns, not just the driver's.
//
// Per-process errors (the process exited between readdir and open, or its
// /proc/<pid>/ns/net is unreadable) are skipped: they are expected on a busy
// node and a single missing process must not shrink the live set. Only a
// failure to read procRoot itself is returned as an error, since that would
// yield a dangerously incomplete set that the caller must not reap against.
func liveNetnsInodes(procRoot string) (map[uint64]bool, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", procRoot, err)
	}
	out := make(map[uint64]bool)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// /proc/<pid> directories have all-numeric names; skip the rest.
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		ino, err := netnsInodeAt(filepath.Join(procRoot, e.Name(), "ns", "net"))
		if err != nil {
			continue
		}
		out[ino&netnsInodeMask] = true
	}
	return out, nil
}

// netnsInodeAt opens path and returns its inode via fstat. This mirrors the NRI
// plugin's netnsInode: stat() on an nsfs bind-mount can report the mountpoint's
// inode rather than the namespace's, so the namespace file must be opened and
// the open descriptor fstat'd to read the true netns inode.
func netnsInodeAt(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return uint64(st.Ino), nil
}
