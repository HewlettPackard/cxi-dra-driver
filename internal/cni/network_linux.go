//go:build linux

package cni

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// netlinkManager is the real linkManager, backed by vishvananda/netlink.
// Operations inside the pod netns go through netlink.NewHandleAt, which binds
// a netlink socket to that namespace without switching the calling OS
// thread's own namespace — unlike internal/netsysfs, which must setns because
// it mounts a real sysfs.
type netlinkManager struct{}

func newLinkManager() linkManager { return netlinkManager{} }

// tempLinkName is the host-side name the IPVLAN link is created under before
// being moved into the pod netns. It cannot be created directly under its
// final name because that name belongs to the HSN parent in the host netns.
func tempLinkName(containerID string) string {
	if len(containerID) > 8 {
		containerID = containerID[:8]
	}
	return "cxi" + containerID
}

func (netlinkManager) CreateIPVLAN(hsnIface, ifName, containerID, nsPath string) (string, error) {
	parent, err := netlink.LinkByName(hsnIface)
	if err != nil {
		return "", fmt.Errorf("looking up HSN parent interface %s: %w", hsnIface, err)
	}

	tmpName := tempLinkName(containerID)
	link := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{Name: tmpName, ParentIndex: parent.Attrs().Index},
		Mode:      netlink.IPVLAN_MODE_L2,
	}
	if err := netlink.LinkAdd(link); err != nil {
		return "", fmt.Errorf("creating IPVLAN sub-interface on %s: %w", hsnIface, err)
	}

	nsHandle, err := netns.GetFromPath(nsPath)
	if err != nil {
		_ = netlink.LinkDel(link)
		return "", fmt.Errorf("opening netns %s: %w", nsPath, err)
	}
	defer func() { _ = nsHandle.Close() }()

	if err := netlink.LinkSetNsFd(link, int(nsHandle)); err != nil {
		_ = netlink.LinkDel(link)
		return "", fmt.Errorf("moving IPVLAN sub-interface into netns %s: %w", nsPath, err)
	}

	var usedName string
	err = withHandle(nsPath, func(h *netlink.Handle) error {
		moved, err := h.LinkByName(tmpName)
		if err != nil {
			return fmt.Errorf("finding %s inside netns: %w", tmpName, err)
		}
		var renameErr error
		for _, candidate := range ifaceNameCandidates(ifName) {
			if renameErr = h.LinkSetName(moved, candidate); renameErr == nil {
				usedName = candidate
				break
			}
		}
		if renameErr != nil {
			return fmt.Errorf("renaming IPVLAN sub-interface to %s (or fallback): %w", ifName, renameErr)
		}
		// Before the link comes up, so it never answers ARP with the defaults.
		if err := setARPSysctls(nsPath, usedName); err != nil {
			return err
		}
		if err := h.LinkSetUp(moved); err != nil {
			return fmt.Errorf("setting %s up: %w", usedName, err)
		}
		return nil
	})
	if err != nil {
		// The link may be under tmpName or a candidate name by now; try all.
		_ = withHandle(nsPath, func(h *netlink.Handle) error {
			l, _, ferr := findLink(h, append([]string{tmpName}, ifaceNameCandidates(ifName)...))
			if ferr != nil || l == nil {
				return ferr
			}
			return h.LinkDel(l)
		})
		return "", err
	}
	return usedName, nil
}

// Per-interface ARP sysctls applied to every IPVLAN the plugin creates.
// A pod claiming several devices has one IPVLAN per HSN, all in the same L2
// domain and subnet. With the kernel defaults (arp_ignore=0) every interface
// answers ARP for every local IP, so a peer can cache the MAC of the wrong
// interface and its traffic is then dropped (intermittent 100% loss).
const (
	// arpIgnoreTargetOnly answers an ARP request only if the target IP is
	// configured on the interface that received it.
	arpIgnoreTargetOnly = "1"
	// arpAnnounceBestLocal restricts the ARP source address to one in the
	// target's subnet on the outgoing interface, so announcements never
	// advertise another interface's IP.
	arpAnnounceBestLocal = "2"
)

// setARPSysctls writes arp_ignore and arp_announce for ifName inside the netns
// at nsPath.
func setARPSysctls(nsPath, ifName string) error {
	// The kernel exposes a "." in an interface name as "/" in sysctl paths.
	dir := filepath.Join("/proc/sys/net/ipv4/conf", strings.ReplaceAll(ifName, ".", "/"))
	return inNetNS(nsPath, func() error {
		for _, s := range []struct{ name, value string }{
			{"arp_ignore", arpIgnoreTargetOnly},
			{"arp_announce", arpAnnounceBestLocal},
		} {
			if err := os.WriteFile(filepath.Join(dir, s.name), []byte(s.value), 0o644); err != nil {
				return fmt.Errorf("setting %s=%s on %s: %w", s.name, s.value, ifName, err)
			}
		}
		return nil
	})
}

// inNetNS runs fn on an OS thread joined to the netns at nsPath. It is needed
// for /proc/sys/net, which is resolved against the network namespace of the
// opening thread (netlink handles don't help here). fn runs on a throwaway
// goroutine whose thread is never unlocked, so the runtime discards it instead
// of returning a thread still joined to the pod netns to the pool — the same
// approach as internal/netsysfs.
func inNetNS(nsPath string, fn func() error) error {
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		nsFd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			errCh <- fmt.Errorf("opening netns %s: %w", nsPath, err)
			return
		}
		defer func() { _ = unix.Close(nsFd) }()
		if err := unix.Setns(nsFd, unix.CLONE_NEWNET); err != nil {
			errCh <- fmt.Errorf("joining netns %s: %w", nsPath, err)
			return
		}
		errCh <- fn()
	}()
	return <-errCh
}

func (netlinkManager) Find(nsPath, ifName string) (string, error) {
	var found string
	err := withHandle(nsPath, func(h *netlink.Handle) error {
		link, name, err := findLink(h, ifaceNameCandidates(ifName))
		if err != nil {
			return err
		}
		if link != nil {
			found = name
		}
		return nil
	})
	if isNetNSGone(err) {
		return "", nil
	}
	return found, err
}

func (netlinkManager) Delete(nsPath, ifName string) error {
	err := withHandle(nsPath, func(h *netlink.Handle) error {
		link, _, err := findLink(h, []string{ifName})
		if err != nil || link == nil {
			return err
		}
		if err := h.LinkDel(link); err != nil {
			return fmt.Errorf("deleting %s: %w", ifName, err)
		}
		return nil
	})
	if isNetNSGone(err) {
		return nil
	}
	return err
}

func (netlinkManager) ConfigureAddress(nsPath, ifName string, result *types100.Result) error {
	return withHandle(nsPath, func(h *netlink.Handle) error {
		link, err := h.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("finding %s to configure: %w", ifName, err)
		}
		var gw net.IP
		for _, ip := range result.IPs {
			if err := h.AddrAdd(link, &netlink.Addr{IPNet: &ip.Address}); err != nil {
				return fmt.Errorf("adding address %s to %s: %w", ip.Address.String(), ifName, err)
			}
			if gw == nil && ip.Gateway != nil {
				gw = ip.Gateway
			}
		}
		for _, route := range result.Routes {
			routeGW := route.GW
			if routeGW == nil {
				routeGW = gw
			}
			r := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &route.Dst, Gw: routeGW}
			if err := h.RouteAdd(r); err != nil {
				return fmt.Errorf("adding route %s via %s on %s: %w", route.Dst.String(), routeGW, ifName, err)
			}
		}
		return nil
	})
}

func (netlinkManager) Check(nsPath, ifName string) error {
	return withHandle(nsPath, func(h *netlink.Handle) error {
		link, err := h.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("finding %s: %w", ifName, err)
		}
		if link.Type() != "ipvlan" {
			return fmt.Errorf("%s is a %s link, not ipvlan", ifName, link.Type())
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("%s is not up", ifName)
		}
		return nil
	})
}

// withHandle runs fn with a netlink handle bound to the netns at nsPath.
func withHandle(nsPath string, fn func(*netlink.Handle) error) error {
	nsHandle, err := netns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("opening netns %s: %w", nsPath, err)
	}
	defer func() { _ = nsHandle.Close() }()

	h, err := netlink.NewHandleAt(nsHandle)
	if err != nil {
		return fmt.Errorf("binding netlink handle to netns %s: %w", nsPath, err)
	}
	defer h.Close()
	return fn(h)
}

// findLink returns the first of candidates present in h's netns as an IPVLAN
// link, and its name, or (nil, "", nil) if none is. Non-IPVLAN links are
// skipped, not matched: the pod may legitimately own an interface under the
// preferred name (which is exactly why the _inet fallback exists), and
// Delete must never remove a link this plugin did not create. Only "not
// found" is swallowed.
func findLink(h *netlink.Handle, candidates []string) (netlink.Link, string, error) {
	for _, name := range candidates {
		link, err := h.LinkByName(name)
		if err == nil {
			if link.Type() == "ipvlan" {
				return link, name, nil
			}
			continue
		}
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return nil, "", fmt.Errorf("looking up %s: %w", name, err)
		}
	}
	return nil, "", nil
}

// isNetNSGone reports whether err means the netns path no longer exists —
// the pod is already fully torn down, which DEL must treat as success.
func isNetNSGone(err error) bool {
	return err != nil && errors.Is(err, unix.ENOENT)
}
