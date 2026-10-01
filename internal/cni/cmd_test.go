package cni

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// fakeLinks is an in-memory linkManager. It records every call in order so
// tests can assert on the exact sequence of kernel-facing operations
// (creation order, rollback order, idempotent deletes) without a netns.
type fakeLinks struct {
	calls []string
	// ifaces maps in-pod interface name -> parent hsnIface for links that
	// currently "exist".
	ifaces map[string]string
	// taken lists names the "kernel" refuses, forcing the _inet fallback.
	taken []string
	// failCreate/failConfigure name an hsnIface whose op should fail.
	failCreate, failConfigure string
}

func newFakeLinks() *fakeLinks { return &fakeLinks{ifaces: map[string]string{}} }

func (f *fakeLinks) CreateIPVLAN(hsnIface, _, _ string) (string, error) {
	f.calls = append(f.calls, "create:"+hsnIface)
	if hsnIface == f.failCreate {
		return "", errors.New("injected create failure")
	}
	for _, name := range ifaceNameCandidates(hsnIface) {
		if _, exists := f.ifaces[name]; exists || slices.Contains(f.taken, name) {
			continue
		}
		f.ifaces[name] = hsnIface
		return name, nil
	}
	return "", errors.New("no free name")
}

func (f *fakeLinks) Find(_, hsnIface string) (string, error) {
	f.calls = append(f.calls, "find:"+hsnIface)
	for _, name := range ifaceNameCandidates(hsnIface) {
		if _, ok := f.ifaces[name]; ok {
			return name, nil
		}
	}
	return "", nil
}

func (f *fakeLinks) Delete(_, ifName string) error {
	f.calls = append(f.calls, "delete:"+ifName)
	delete(f.ifaces, ifName)
	return nil
}

func (f *fakeLinks) ConfigureAddress(_, ifName string, result *types100.Result) error {
	f.calls = append(f.calls, fmt.Sprintf("configure:%s:%s", ifName, result.IPs[0].Address.String()))
	if f.ifaces[ifName] == f.failConfigure {
		return errors.New("injected configure failure")
	}
	return nil
}

func (f *fakeLinks) Check(_, ifName string) error {
	f.calls = append(f.calls, "check:"+ifName)
	if _, ok := f.ifaces[ifName]; !ok {
		return fmt.Errorf("%s missing", ifName)
	}
	return nil
}

// fakeIPAM is an in-memory ipamDelegate that hands out a deterministic
// address per interface name and tracks live reservations.
type fakeIPAM struct {
	calls    []string
	reserved map[string]string
	failAdd  string // ifName whose Add should fail
	failDel  bool
}

func newFakeIPAM() *fakeIPAM { return &fakeIPAM{reserved: map[string]string{}} }

func (f *fakeIPAM) Add(_ context.Context, _ []byte, ifName string) (*types100.Result, error) {
	f.calls = append(f.calls, "add:"+ifName)
	if ifName == f.failAdd {
		return nil, errors.New("injected ipam failure")
	}
	addr := fmt.Sprintf("10.200.0.%d/24", 10+len(f.reserved))
	f.reserved[ifName] = addr
	ip, ipnet, _ := net.ParseCIDR(addr)
	ipnet.IP = ip
	return &types100.Result{
		CNIVersion: "1.0.0",
		IPs:        []*types100.IPConfig{{Address: *ipnet, Gateway: net.ParseIP("10.200.0.1")}},
	}, nil
}

func (f *fakeIPAM) Del(_ context.Context, _ []byte, ifName string) error {
	f.calls = append(f.calls, "del:"+ifName)
	if f.failDel {
		return errors.New("injected ipam del failure")
	}
	delete(f.reserved, ifName)
	return nil
}

func (f *fakeIPAM) Check(_ context.Context, _ []byte, ifName string) error {
	f.calls = append(f.calls, "check:"+ifName)
	if _, ok := f.reserved[ifName]; !ok {
		return fmt.Errorf("no reservation for %s", ifName)
	}
	return nil
}

// testPlugin wires a plugin to fresh fakes and a stdout buffer.
func testPlugin() (*plugin, *fakeLinks, *fakeIPAM, *bytes.Buffer) {
	log.SetOutput(io.Discard)
	links, ipam, out := newFakeLinks(), newFakeIPAM(), &bytes.Buffer{}
	p := &plugin{
		links:   links,
		newIPAM: func(string) ipamDelegate { return ipam },
		stdout:  out,
	}
	return p, links, ipam, out
}

func testCmdArgs(t *testing.T, hintsDir, podUID string) *skel.CmdArgs {
	t.Helper()
	stdin, err := json.Marshal(map[string]any{
		"cniVersion":    "1.0.0",
		"name":          "slingshot-hsn",
		"type":          "slingshot-cni",
		"hintsDir":      hintsDir,
		"retryInterval": "5ms",
		"retryTimeout":  "200ms",
		"ipam":          map[string]any{"type": "whereabouts"},
	})
	if err != nil {
		t.Fatalf("marshal stdin config: %v", err)
	}
	return &skel.CmdArgs{
		ContainerID: "container-1",
		Netns:       "/var/run/netns/test",
		IfName:      "net1",
		Args:        "K8S_POD_UID=" + podUID,
		StdinData:   stdin,
	}
}

func mustWriteHint(t *testing.T, dir, podUID string, hints []Hint) {
	t.Helper()
	if err := writeHintFile(dir, podUID, hints); err != nil {
		t.Fatalf("writeHintFile: %v", err)
	}
}

var (
	oneDevice  = []Hint{{Device: "cxi0", HSNIface: "hsn0", VNI: 137}}
	twoDevices = []Hint{{Device: "cxi0", HSNIface: "hsn0", VNI: 137}, {Device: "cxi1", HSNIface: "hsn1", VNI: 137}}
)

func TestAdd_SingleDevice(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, links, ipam, out := testPlugin()

	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}

	want := []string{"create:hsn0", "configure:hsn0:10.200.0.10/24"}
	if !slices.Equal(links.calls, want) {
		t.Errorf("link calls = %v, want %v", links.calls, want)
	}
	if !slices.Equal(ipam.calls, []string{"add:hsn0"}) {
		t.Errorf("ipam calls = %v", ipam.calls)
	}

	var result types100.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parsing printed result %q: %v", out.String(), err)
	}
	if len(result.Interfaces) != 1 || result.Interfaces[0].Name != "hsn0" || result.Interfaces[0].Sandbox != "/var/run/netns/test" {
		t.Errorf("interfaces = %+v", result.Interfaces)
	}
	if len(result.IPs) != 1 || *result.IPs[0].Interface != 0 || result.IPs[0].Address.String() != "10.200.0.10/24" {
		t.Errorf("ips = %+v", result.IPs)
	}
}

// A count>1 claim yields one hint entry per device; ADD must configure every
// one of them and index each IP to its own interface in the result.
func TestAdd_MultiDevice(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", twoDevices)
	p, links, ipam, out := testPlugin()

	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}

	want := []string{"create:hsn0", "configure:hsn0:10.200.0.10/24", "create:hsn1", "configure:hsn1:10.200.0.11/24"}
	if !slices.Equal(links.calls, want) {
		t.Errorf("link calls = %v, want %v", links.calls, want)
	}
	if !slices.Equal(ipam.calls, []string{"add:hsn0", "add:hsn1"}) {
		t.Errorf("ipam calls = %v", ipam.calls)
	}

	var result types100.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parsing printed result: %v", err)
	}
	if len(result.Interfaces) != 2 || len(result.IPs) != 2 {
		t.Fatalf("interfaces=%d ips=%d, want 2/2", len(result.Interfaces), len(result.IPs))
	}
	for i, ip := range result.IPs {
		if *ip.Interface != i {
			t.Errorf("ips[%d].Interface = %d, want %d", i, *ip.Interface, i)
		}
	}
}

// When the preferred hsn_iface name is unavailable in the pod netns, the
// interface falls back to hsn_iface+"_inet" and IPAM is keyed on that name.
func TestAdd_NameFallback(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, links, ipam, out := testPlugin()
	links.taken = []string{"hsn0"}

	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, ok := links.ifaces["hsn0_inet"]; !ok {
		t.Errorf("expected hsn0_inet to exist, have %v", links.ifaces)
	}
	if !slices.Equal(ipam.calls, []string{"add:hsn0_inet"}) {
		t.Errorf("ipam calls = %v, want keyed on fallback name", ipam.calls)
	}
	var result types100.Result
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("parsing printed result: %v", err)
	}
	if len(result.Interfaces) != 1 || result.Interfaces[0].Name != "hsn0_inet" {
		t.Errorf("result interfaces = %+v, want fallback name", result.Interfaces)
	}
}

// A failure on the second device must unwind the first: release its IPAM
// reservation and delete its interface, leaving nothing behind.
func TestAdd_RollbackOnSecondDeviceIPAMFailure(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", twoDevices)
	p, links, ipam, out := testPlugin()
	ipam.failAdd = "hsn1"

	err := p.add(testCmdArgs(t, dir, "pod"))
	if err == nil || !strings.Contains(err.Error(), "cxi1") {
		t.Fatalf("add error = %v, want failure naming device cxi1", err)
	}
	if out.Len() != 0 {
		t.Errorf("failed ADD must not print a result, got %s", out.String())
	}
	if len(links.ifaces) != 0 {
		t.Errorf("leaked interfaces after rollback: %v", links.ifaces)
	}
	if len(ipam.reserved) != 0 {
		t.Errorf("leaked IPAM reservations after rollback: %v", ipam.reserved)
	}
	// Failed device: its half-created link is deleted with no IPAM release
	// (nothing was reserved). Then the completed device is unwound.
	wantLinks := []string{"create:hsn0", "configure:hsn0:10.200.0.10/24", "create:hsn1", "delete:hsn1", "delete:hsn0"}
	if !slices.Equal(links.calls, wantLinks) {
		t.Errorf("link calls = %v, want %v", links.calls, wantLinks)
	}
	if !slices.Equal(ipam.calls, []string{"add:hsn0", "add:hsn1", "del:hsn0"}) {
		t.Errorf("ipam calls = %v", ipam.calls)
	}
}

func TestAdd_RollbackOnConfigureFailure(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, links, ipam, _ := testPlugin()
	links.failConfigure = "hsn0"

	if err := p.add(testCmdArgs(t, dir, "pod")); err == nil {
		t.Fatal("expected configure failure")
	}
	if len(links.ifaces) != 0 || len(ipam.reserved) != 0 {
		t.Errorf("leaked state: ifaces=%v reserved=%v", links.ifaces, ipam.reserved)
	}
	if !slices.Equal(ipam.calls, []string{"add:hsn0", "del:hsn0"}) {
		t.Errorf("ipam calls = %v, want reservation released", ipam.calls)
	}
}

func TestAdd_CreateFailureFirstDevice(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, links, ipam, _ := testPlugin()
	links.failCreate = "hsn0"

	if err := p.add(testCmdArgs(t, dir, "pod")); err == nil {
		t.Fatal("expected create failure")
	}
	if len(ipam.calls) != 0 {
		t.Errorf("IPAM must not be called when link creation fails, got %v", ipam.calls)
	}
}

func TestDel_ReleasesAndRemovesEveryDevice(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", twoDevices)
	p, links, ipam, _ := testPlugin()
	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}
	links.calls, ipam.calls = nil, nil

	if err := p.del(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("del: %v", err)
	}
	if len(links.ifaces) != 0 || len(ipam.reserved) != 0 {
		t.Errorf("leaked state: ifaces=%v reserved=%v", links.ifaces, ipam.reserved)
	}
	wantLinks := []string{"find:hsn0", "delete:hsn0", "find:hsn1", "delete:hsn1"}
	if !slices.Equal(links.calls, wantLinks) {
		t.Errorf("link calls = %v, want %v", links.calls, wantLinks)
	}
	if !slices.Equal(ipam.calls, []string{"del:hsn0", "del:hsn1"}) {
		t.Errorf("ipam calls = %v", ipam.calls)
	}
}

// DEL must release IPAM under the name ADD actually used, even when that was
// the _inet fallback.
func TestDel_UsesFallbackNameForIPAMRelease(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, links, ipam, _ := testPlugin()
	links.taken = []string{"hsn0"}
	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}
	ipam.calls = nil

	if err := p.del(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("del: %v", err)
	}
	if !slices.Equal(ipam.calls, []string{"del:hsn0_inet"}) {
		t.Errorf("ipam calls = %v, want release under fallback name", ipam.calls)
	}
}

// DEL is idempotent: a second DEL (interfaces already gone) still succeeds
// and still asks IPAM to release, under the preferred name.
func TestDel_Idempotent(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, links, ipam, _ := testPlugin()

	if err := p.del(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("del on never-added pod: %v", err)
	}
	if !slices.Equal(links.calls, []string{"find:hsn0", "delete:hsn0"}) {
		t.Errorf("link calls = %v", links.calls)
	}
	if !slices.Equal(ipam.calls, []string{"del:hsn0"}) {
		t.Errorf("ipam calls = %v", ipam.calls)
	}
}

// A failing IPAM release must not stop the interface from being removed,
// nor the remaining devices from being attempted; the error is still
// surfaced at the end.
func TestDel_ContinuesPastIPAMFailure(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", twoDevices)
	p, links, ipam, _ := testPlugin()
	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}
	ipam.failDel = true

	if err := p.del(testCmdArgs(t, dir, "pod")); err == nil {
		t.Fatal("expected IPAM release error to be surfaced")
	}
	if len(links.ifaces) != 0 {
		t.Errorf("interfaces must still be removed: %v", links.ifaces)
	}
}

func TestDel_MissingHintFileIsSuccess(t *testing.T) {
	dir := t.TempDir()
	p, links, ipam, _ := testPlugin()

	if err := p.del(testCmdArgs(t, dir, "no-such-pod")); err != nil {
		t.Fatalf("del with missing hint: %v", err)
	}
	if len(links.calls) != 0 || len(ipam.calls) != 0 {
		t.Errorf("nothing should be attempted without a hint: links=%v ipam=%v", links.calls, ipam.calls)
	}
}

func TestDel_MalformedHintIsError(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", []Hint{{Device: "cxi0"}})
	p, _, _, _ := testPlugin()

	if err := p.del(testCmdArgs(t, dir, "pod")); err == nil {
		t.Fatal("expected validation error for incomplete hint")
	}
}

func TestCheck_HealthyAfterAdd(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", twoDevices)
	p, links, ipam, _ := testPlugin()
	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}
	links.calls, ipam.calls = nil, nil

	if err := p.check(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("check: %v", err)
	}
	if !slices.Equal(links.calls, []string{"find:hsn0", "check:hsn0", "find:hsn1", "check:hsn1"}) {
		t.Errorf("link calls = %v", links.calls)
	}
	if !slices.Equal(ipam.calls, []string{"check:hsn0", "check:hsn1"}) {
		t.Errorf("ipam calls = %v", ipam.calls)
	}
}

func TestCheck_FailsWhenInterfaceMissing(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, _, _, _ := testPlugin()

	if err := p.check(testCmdArgs(t, dir, "pod")); err == nil {
		t.Fatal("expected check to fail with no interface present")
	}
}

func TestCheck_FailsWhenIPAMReservationGone(t *testing.T) {
	dir := t.TempDir()
	mustWriteHint(t, dir, "pod", oneDevice)
	p, _, ipam, _ := testPlugin()
	if err := p.add(testCmdArgs(t, dir, "pod")); err != nil {
		t.Fatalf("add: %v", err)
	}
	delete(ipam.reserved, "hsn0")

	if err := p.check(testCmdArgs(t, dir, "pod")); err == nil {
		t.Fatal("expected check to fail when IPAM has no reservation")
	}
}

func TestAdd_MissingHintFile(t *testing.T) {
	dir := t.TempDir()
	p, _, _, _ := testPlugin()
	if err := p.add(testCmdArgs(t, dir, "no-such-pod")); err == nil {
		t.Fatal("expected error for missing hint file")
	}
}

func TestAdd_MissingPodUID(t *testing.T) {
	dir := t.TempDir()
	p, _, _, _ := testPlugin()
	args := testCmdArgs(t, dir, "unused")
	args.Args = "IgnoreUnknown=1"
	if err := p.add(args); err == nil {
		t.Fatal("expected error for missing K8S_POD_UID")
	}
}

// The exported entrypoints must wire the real dependencies; on any platform
// they should at least get as far as config/hint validation.
func TestExportedCmds_ValidateBeforeNetworking(t *testing.T) {
	dir := t.TempDir()
	for name, fn := range map[string]func(*skel.CmdArgs) error{"add": CmdAdd, "check": CmdCheck} {
		if err := fn(testCmdArgs(t, dir, "no-such-pod")); err == nil {
			t.Errorf("%s: expected hint-resolution error", name)
		}
	}
	if err := CmdDel(testCmdArgs(t, dir, "no-such-pod")); err != nil {
		t.Errorf("del with missing hint: %v", err)
	}
}
