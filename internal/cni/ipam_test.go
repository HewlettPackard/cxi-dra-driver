package cni

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeWhereabouts compiles testdata/fakewhereabouts as a plugin named
// "whereabouts" into a temp dir, points CNI_PATH at it, and returns a
// function that reads back every invocation the fake recorded. Environment
// changes are process-wide, so tests using this must not run in parallel.
func fakeWhereabouts(t *testing.T) (invocations func() []map[string]string) {
	t.Helper()
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "whereabouts"), "./testdata/fakewhereabouts")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building fake whereabouts: %v\n%s", err, out)
	}
	logPath := filepath.Join(dir, "invocations.jsonl")
	t.Setenv("CNI_PATH", dir)
	t.Setenv("FAKE_WHEREABOUTS_LOG", logPath)
	// The parent's own CNI env must not leak into the delegate un-overridden.
	t.Setenv("CNI_IFNAME", "net1")
	t.Setenv("CNI_CONTAINERID", "ctr-1")

	return func() []map[string]string {
		f, err := os.Open(logPath)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		var recs []map[string]string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec map[string]string
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				t.Fatalf("parsing fake log line %q: %v", sc.Text(), err)
			}
			recs = append(recs, rec)
		}
		return recs
	}
}

const whereaboutsConf = `{"cniVersion":"1.0.0","name":"slingshot-hsn","type":"slingshot-cni","ipam":{"type":"whereabouts","range":"10.200.0.0/24"}}`

// The real delegate must exec the plugin found on CNI_PATH with CNI_COMMAND
// set per call, CNI_IFNAME overridden to the per-device interface name (not
// the parent invocation's), and the parent's stdin config forwarded intact.
func TestExecDelegate_ContractWithFakeWhereabouts(t *testing.T) {
	invocations := fakeWhereabouts(t)
	d := newIPAMDelegate("whereabouts")
	ctx := context.Background()

	result, err := d.Add(ctx, []byte(whereaboutsConf), "hsn0")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(result.IPs) != 1 || result.IPs[0].Address.String() != "10.200.0.42/24" {
		t.Errorf("Add result IPs = %v", result.IPs)
	}
	if len(result.Routes) != 1 || result.Routes[0].Dst.String() != "10.201.0.0/16" {
		t.Errorf("Add result routes = %v", result.Routes)
	}
	if err := d.Check(ctx, []byte(whereaboutsConf), "hsn0"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if err := d.Del(ctx, []byte(whereaboutsConf), "hsn1_inet"); err != nil {
		t.Fatalf("Del: %v", err)
	}

	recs := invocations()
	if len(recs) != 3 {
		t.Fatalf("got %d invocations, want 3: %v", len(recs), recs)
	}
	want := []struct{ cmd, ifName string }{{"ADD", "hsn0"}, {"CHECK", "hsn0"}, {"DEL", "hsn1_inet"}}
	for i, w := range want {
		if recs[i]["command"] != w.cmd || recs[i]["ifName"] != w.ifName {
			t.Errorf("invocation %d = %v, want command=%s ifName=%s", i, recs[i], w.cmd, w.ifName)
		}
		if recs[i]["containerID"] != "ctr-1" {
			t.Errorf("invocation %d containerID = %q, want inherited ctr-1", i, recs[i]["containerID"])
		}
		if recs[i]["ipamType"] != "whereabouts" {
			t.Errorf("invocation %d did not receive the ipam block: %v", i, recs[i])
		}
	}
}

func TestExecDelegate_PluginFailureIsSurfaced(t *testing.T) {
	fakeWhereabouts(t)
	conf := strings.Replace(whereaboutsConf, `"type":"whereabouts"`, `"type":"whereabouts","failIfName":"hsn0"`, 1)

	_, err := newIPAMDelegate("whereabouts").Add(context.Background(), []byte(conf), "hsn0")
	if err == nil || !strings.Contains(err.Error(), "forced failure for hsn0") {
		t.Fatalf("Add error = %v, want plugin's own failure message", err)
	}
}

func TestExecDelegate_MissingPluginOnPath(t *testing.T) {
	t.Setenv("CNI_PATH", t.TempDir())
	if _, err := newIPAMDelegate("whereabouts").Add(context.Background(), []byte(whereaboutsConf), "hsn0"); err == nil {
		t.Fatal("expected error when plugin is not on CNI_PATH")
	}
}
