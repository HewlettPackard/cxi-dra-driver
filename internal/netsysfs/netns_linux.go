//go:build linux

package netsysfs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"golang.org/x/sys/unix"
)

// PodInterfaces returns the pod's real network interfaces and the exact symlink
// text of each /sys/class/net/<iface> entry, so Build can replicate them and
// avoid masking the pod's own NICs when the synthetic tree is mounted.
//
// A sysfs instance reflects the network namespace of whoever mounted it, not of
// whoever reads it, so simply reading the driver's /sys after setns would still
// show host interfaces. We therefore join the pod's netns on a dedicated,
// throwaway OS thread, take a private mount namespace, mount a fresh sysfs, and
// read it. netnsPath is typically /proc/<sandbox-pid>/ns/net.
func PodInterfaces(netnsPath string) ([]Iface, error) {
	type result struct {
		ifaces []Iface
		err    error
	}
	ch := make(chan result, 1)
	// Run the namespace switch on its own goroutine+thread. The thread is
	// permanently locked and never unlocked, so when the goroutine returns the
	// Go runtime destroys it instead of returning a thread still joined to the
	// pod's netns (and a private mount ns) to the scheduler's pool.
	go func() {
		runtime.LockOSThread()
		ifaces, err := enumInNetns(netnsPath)
		ch <- result{ifaces: ifaces, err: err}
	}()
	r := <-ch
	return r.ifaces, r.err
}

func enumInNetns(netnsPath string) ([]Iface, error) {
	nsFd, err := unix.Open(netnsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open netns %s: %w", netnsPath, err)
	}
	defer unix.Close(nsFd)

	if err := unix.Setns(nsFd, unix.CLONE_NEWNET); err != nil {
		return nil, fmt.Errorf("setns into pod netns: %w", err)
	}
	// Private mount namespace so the sysfs mount below is invisible to the host
	// and is torn down automatically when this thread dies.
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return nil, fmt.Errorf("unshare mount namespace: %w", err)
	}
	// Some hosts default the root mount to shared propagation; make it private
	// so our temporary sysfs mount cannot escape into other namespaces.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return nil, fmt.Errorf("making mounts private: %w", err)
	}

	tmp, err := os.MkdirTemp("", "cxi-netsysfs-")
	if err != nil {
		return nil, fmt.Errorf("creating temp mountpoint: %w", err)
	}
	defer os.RemoveAll(tmp)

	if err := unix.Mount("sysfs", tmp, "sysfs", 0, ""); err != nil {
		return nil, fmt.Errorf("mounting sysfs for pod netns: %w", err)
	}
	defer unix.Unmount(tmp, unix.MNT_DETACH)

	netDir := filepath.Join(tmp, "class", "net")
	entries, err := os.ReadDir(netDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", netDir, err)
	}

	ifaces := make([]Iface, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		target, err := os.Readlink(filepath.Join(netDir, name))
		if err != nil {
			// Not a symlink (or vanished); skip — we can only replicate links.
			continue
		}
		ifaces = append(ifaces, Iface{Name: name, Target: target})
	}
	return ifaces, nil
}
