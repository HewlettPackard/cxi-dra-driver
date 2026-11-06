//go:build linux

package cni

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/types"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// These tests drive the real netlinkManager against a throwaway named netns
// and a dummy parent link standing in for an HSN interface. They need
// CAP_NET_ADMIN/CAP_SYS_ADMIN and so skip unless run as root — CI containers
// typically qualify; contributor laptops typically do not.

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to create network namespaces and links")
	}
}

// newTestNetNS creates a named netns (as `ip netns add` would) and returns
// its /var/run/netns path. netns.NewNamed switches the calling thread into
// the new namespace, so it runs on a locked, throwaway goroutine that
// restores the original namespace before exiting.
func newTestNetNS(t *testing.T) string {
	t.Helper()
	name := "cxi-cni-" + filepath.Base(t.TempDir())
	errCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		orig, err := netns.Get()
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = orig.Close() }()
		ns, err := netns.NewNamed(name)
		if err != nil {
			errCh <- err
			return
		}
		_ = ns.Close()
		errCh <- netns.Set(orig)
	}()
	if err := <-errCh; err != nil {
		t.Fatalf("creating netns %s: %v", name, err)
	}
	t.Cleanup(func() { _ = netns.DeleteNamed(name) })
	return filepath.Join("/var/run/netns", name)
}

// newDummyParent creates a dummy link in the test's own netns to act as the
// HSN parent.
func newDummyParent(t *testing.T, name string) {
	t.Helper()
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
	if err := netlink.LinkAdd(link); err != nil {
		t.Fatalf("creating dummy parent %s: %v", name, err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		t.Fatalf("bringing up dummy parent %s: %v", name, err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })
}

func linksIn(t *testing.T, nsPath string) map[string]netlink.Link {
	t.Helper()
	out := map[string]netlink.Link{}
	err := withHandle(nsPath, func(h *netlink.Handle) error {
		links, err := h.LinkList()
		if err != nil {
			return err
		}
		for _, l := range links {
			out[l.Attrs().Name] = l
		}
		return nil
	})
	if err != nil {
		t.Fatalf("listing links in %s: %v", nsPath, err)
	}
	return out
}

func TestNetlinkManager_CreateConfigureCheckDelete(t *testing.T) {
	requireRoot(t)
	const parent = "hsnT0"
	newDummyParent(t, parent)
	nsPath := newTestNetNS(t)
	m := newLinkManager()

	name, err := m.CreateIPVLAN(parent, parent, "abcdef1234567890", nsPath)
	if err != nil {
		t.Fatalf("CreateIPVLAN: %v", err)
	}
	if name != parent {
		t.Errorf("in-pod name = %q, want preferred %q", name, parent)
	}
	link, ok := linksIn(t, nsPath)[parent]
	if !ok {
		t.Fatalf("%s not present in pod netns", parent)
	}
	if link.Type() != "ipvlan" {
		t.Errorf("link type = %s, want ipvlan", link.Type())
	}
	if ipv, ok := link.(*netlink.IPVlan); ok && ipv.Mode != netlink.IPVLAN_MODE_L2 {
		t.Errorf("ipvlan mode = %v, want L2", ipv.Mode)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Error("link is not up")
	}
	if _, leaked := linksIn(t, "/proc/self/ns/net")[tempLinkName("abcdef1234567890")]; leaked {
		t.Error("temporary link leaked in host netns")
	}

	ip, ipnet, _ := net.ParseCIDR("10.200.0.42/24")
	ipnet.IP = ip
	_, routeDst, _ := net.ParseCIDR("10.201.0.0/16")
	result := &types100.Result{
		IPs:    []*types100.IPConfig{{Address: *ipnet, Gateway: net.ParseIP("10.200.0.1")}},
		Routes: []*types.Route{{Dst: *routeDst}},
	}
	if err := m.ConfigureAddress(nsPath, name, result); err != nil {
		t.Fatalf("ConfigureAddress: %v", err)
	}
	err = withHandle(nsPath, func(h *netlink.Handle) error {
		l, err := h.LinkByName(name)
		if err != nil {
			return err
		}
		addrs, err := h.AddrList(l, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		if len(addrs) != 1 || addrs[0].IPNet.String() != "10.200.0.42/24" {
			t.Errorf("addresses = %v, want [10.200.0.42/24]", addrs)
		}
		routes, err := h.RouteList(l, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		var found bool
		for _, r := range routes {
			if r.Dst != nil && r.Dst.String() == "10.201.0.0/16" && r.Gw.Equal(net.ParseIP("10.200.0.1")) {
				found = true
			}
		}
		if !found {
			t.Errorf("route 10.201.0.0/16 via 10.200.0.1 not found in %v", routes)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("inspecting configured link: %v", err)
	}

	if got, err := m.Find(nsPath, parent); err != nil || got != name {
		t.Errorf("Find = (%q, %v), want (%q, nil)", got, err, name)
	}
	if err := m.Check(nsPath, name); err != nil {
		t.Errorf("Check: %v", err)
	}

	if err := m.Delete(nsPath, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, still := linksIn(t, nsPath)[name]; still {
		t.Error("link still present after Delete")
	}
	if err := m.Delete(nsPath, name); err != nil {
		t.Errorf("second Delete must be a no-op, got %v", err)
	}
	if got, err := m.Find(nsPath, parent); err != nil || got != "" {
		t.Errorf("Find after delete = (%q, %v), want (\"\", nil)", got, err)
	}
	if err := m.Check(nsPath, name); err == nil {
		t.Error("Check must fail once the link is gone")
	}
}

// readARPSysctl reads a per-interface IPv4 sysctl as seen from inside nsPath.
func readARPSysctl(t *testing.T, nsPath, ifName, name string) string {
	t.Helper()
	var val string
	err := inNetNS(nsPath, func() error {
		b, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv4/conf", ifName, name))
		val = strings.TrimSpace(string(b))
		return err
	})
	if err != nil {
		t.Fatalf("reading %s on %s: %v", name, ifName, err)
	}
	return val
}

// Several IPVLANs of one pod share an L2 domain, so each must answer ARP only
// for its own address; the kernel defaults make every interface answer for all.
func TestNetlinkManager_CreateIPVLANSetsARPSysctls(t *testing.T) {
	requireRoot(t)
	nsPath := newTestNetNS(t)
	m := newLinkManager()

	var names []string
	for i, parent := range []string{"hsnT4", "hsnT5"} {
		newDummyParent(t, parent)
		name, err := m.CreateIPVLAN(parent, parent, fmt.Sprintf("ctr%d", i), nsPath)
		if err != nil {
			t.Fatalf("CreateIPVLAN(%s): %v", parent, err)
		}
		names = append(names, name)
	}
	for _, name := range names {
		if got := readARPSysctl(t, nsPath, name, "arp_ignore"); got != "1" {
			t.Errorf("%s arp_ignore = %q, want 1", name, got)
		}
		if got := readARPSysctl(t, nsPath, name, "arp_announce"); got != "2" {
			t.Errorf("%s arp_announce = %q, want 2", name, got)
		}
	}
}

// If the pod netns already has an interface under the HSN's name, the
// IPVLAN falls back to the _inet suffix.
func TestNetlinkManager_NameFallback(t *testing.T) {
	requireRoot(t)
	const parent = "hsnT1"
	newDummyParent(t, parent)
	nsPath := newTestNetNS(t)

	err := withHandle(nsPath, func(h *netlink.Handle) error {
		return h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: parent}})
	})
	if err != nil {
		t.Fatalf("pre-creating colliding link: %v", err)
	}

	m := newLinkManager()
	name, err := m.CreateIPVLAN(parent, parent, "ctr", nsPath)
	if err != nil {
		t.Fatalf("CreateIPVLAN: %v", err)
	}
	if name != parent+inetSuffix {
		t.Errorf("in-pod name = %q, want %q", name, parent+inetSuffix)
	}
	if got, _ := m.Find(nsPath, parent); got != name {
		t.Errorf("Find = %q, want %q", got, name)
	}

	// Teardown must remove only our IPVLAN, never the pod's own link that
	// happens to sit under the preferred name.
	if err := m.Delete(nsPath, name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	links := linksIn(t, nsPath)
	if _, still := links[name]; still {
		t.Errorf("%s still present after Delete", name)
	}
	if l, ok := links[parent]; !ok || l.Type() != "dummy" {
		t.Errorf("pod's own %s link was disturbed: %v", parent, l)
	}
}

// Every teardown-side operation must treat a vanished netns as success.
func TestNetlinkManager_GoneNetNSIsNotAnError(t *testing.T) {
	requireRoot(t)
	m := newLinkManager()
	gone := "/var/run/netns/cxi-cni-does-not-exist"

	if err := m.Delete(gone, "hsn0"); err != nil {
		t.Errorf("Delete on missing netns: %v", err)
	}
	if name, err := m.Find(gone, "hsn0"); err != nil || name != "" {
		t.Errorf("Find on missing netns = (%q, %v), want (\"\", nil)", name, err)
	}
	if _, err := m.CreateIPVLAN("no-such-parent", "cxi0", "ctr", gone); err == nil {
		t.Error("CreateIPVLAN with a missing parent must fail")
	}
}

func TestNetlinkManager_CustomName(t *testing.T) {
	requireRoot(t)
	const parent = "enp197s0f1np0"
	newDummyParent(t, parent)
	for _, tt := range []struct {
		name     string
		fallback bool
	}{
		{"cxi4", false},
		{"cxi4", true},
		{"1234567890", true},
	} {
		t.Run(fmt.Sprintf("%s/fallback=%t", tt.name, tt.fallback), func(t *testing.T) {
			nsPath := newTestNetNS(t)
			if tt.fallback {
				if err := withHandle(nsPath, func(h *netlink.Handle) error {
					return h.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: tt.name}})
				}); err != nil {
					t.Fatal(err)
				}
			}
			m := newLinkManager()
			name, err := m.CreateIPVLAN(parent, tt.name, "custom", nsPath)
			if err != nil {
				t.Fatalf("CreateIPVLAN: %v", err)
			}
			wantName := tt.name
			if tt.fallback {
				wantName += inetSuffix
			}
			if name != wantName {
				t.Fatalf("created %q, want %q", name, wantName)
			}
			if got, err := m.Find(nsPath, tt.name); err != nil || got != wantName {
				t.Fatalf("Find = (%q, %v), want %q", got, err, wantName)
			}
			if err := m.Check(nsPath, name); err != nil {
				t.Fatalf("Check: %v", err)
			}
			if err := m.Delete(nsPath, name); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if err := m.Delete(nsPath, name); err != nil {
				t.Fatalf("second Delete: %v", err)
			}
			if tt.fallback {
				if err := m.Delete(nsPath, tt.name); err != nil {
					t.Fatal(err)
				}
				if link := linksIn(t, nsPath)[tt.name]; link == nil || link.Type() != "dummy" {
					t.Fatalf("non-IPVLAN link was disturbed: %v", link)
				}
			}
			if got, err := m.Find(nsPath, tt.name); err != nil || got != "" {
				t.Errorf("Find after Delete = (%q, %v)", got, err)
			}
			if err := m.Check(nsPath, name); err == nil {
				t.Error("Check after Delete must fail")
			}
		})
	}
}
