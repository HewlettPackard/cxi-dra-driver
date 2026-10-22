package cdi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

const testDriver = "cxi.slingshot.hpe.com"

func TestEnvDeviceID(t *testing.T) {
	if got, want := EnvDeviceID(testDriver), testDriver+"/env=env"; got != want {
		t.Errorf("EnvDeviceID = %q, want %q", got, want)
	}
}

// absentDevNodes returns cfg with xpmem/gdrdrv/kdreg2 paths that do not exist, so
// the env spec emits no device nodes.
func absentDevNodes(t *testing.T, cfg CDIEnvConfig) CDIEnvConfig {
	dir := t.TempDir()
	cfg.XpmemPath = filepath.Join(dir, "xpmem")
	cfg.GdrDrvPath = filepath.Join(dir, "gdrdrv")
	cfg.KdReg2Path = filepath.Join(dir, "kdreg2")
	return cfg
}

func readEnvSpec(t *testing.T, cdiRoot string) specs.ContainerEdits {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cdiRoot, testDriver+"-env.json"))
	if err != nil {
		t.Fatalf("reading env spec: %v", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshalling env spec: %v", err)
	}
	if spec.Kind != testDriver+"/env" {
		t.Errorf("kind = %q, want %q", spec.Kind, testDriver+"/env")
	}
	if len(spec.Devices) != 1 || spec.Devices[0].Name != EnvName {
		t.Fatalf("devices = %v, want [{env}]", spec.Devices)
	}
	return spec.Devices[0].ContainerEdits
}

func mountsByContainerPath(edits specs.ContainerEdits) map[string]*specs.Mount {
	out := make(map[string]*specs.Mount, len(edits.Mounts))
	for _, m := range edits.Mounts {
		out[m.ContainerPath] = m
	}
	return out
}

func TestWriteEnvSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const libfabricHost = "/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1"
	cfg := absentDevNodes(t, CDIEnvConfig{
		Libs: []string{libfabricHost, "/usr/lib64/libcxi.so.1"},
		Env:  []string{"CXIP_SKIP_AMA_CHECK=1"},
	})

	if err := WriteEnvSpec(cdiRoot, testDriver, cfg); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}
	edits := readEnvSpec(t, cdiRoot)

	if len(edits.DeviceNodes) != 0 {
		t.Errorf("device nodes = %d, want 0 (all absent)", len(edits.DeviceNodes))
	}
	mounts := mountsByContainerPath(edits)
	if len(mounts) != 3 {
		t.Errorf("mounts = %v, want /dev/shm + 2 libs", edits.Mounts)
	}
	for containerPath, hostPath := range map[string]string{
		"/dev/shm":                 "/dev/shm",
		"/host/env/libfabric.so.1": libfabricHost,
		"/host/env/libcxi.so.1":    "/usr/lib64/libcxi.so.1",
	} {
		m, ok := mounts[containerPath]
		if !ok {
			t.Errorf("no mount at %s; mounts = %v", containerPath, edits.Mounts)
			continue
		}
		if m.HostPath != hostPath {
			t.Errorf("mount %s host path = %q, want %q", containerPath, m.HostPath, hostPath)
		}
	}
	if !reflect.DeepEqual(edits.Env, []string{"CXIP_SKIP_AMA_CHECK=1"}) {
		t.Errorf("env = %v", edits.Env)
	}
	if len(edits.Hooks) != 1 {
		t.Fatalf("hooks = %v, want one ldcache hook", edits.Hooks)
	}
	h := edits.Hooks[0]
	wantArgs := []string{DefaultHookPath, "update-ldcache", "--ldconfig-dir", DefaultLdconfigDir, "--folder", "/host/env"}
	if h.HookName != "createContainer" || h.Path != DefaultHookPath || !reflect.DeepEqual(h.Args, wantArgs) {
		t.Errorf("hook = %+v, want createContainer %s %v", h, DefaultHookPath, wantArgs)
	}
}

func TestWriteEnvSpecCustomLdcache(t *testing.T) {
	cdiRoot := t.TempDir()
	cfg := absentDevNodes(t, CDIEnvConfig{Libs: []string{"/usr/lib64/libcxi.so.1"}})
	cfg.HookPath = "/opt/cxi/bin/cxi-cdi-hook"
	cfg.LdconfigDir = "/opt/cxi/bin"
	if err := WriteEnvSpec(cdiRoot, testDriver, cfg); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}
	edits := readEnvSpec(t, cdiRoot)
	wantArgs := []string{"/opt/cxi/bin/cxi-cdi-hook", "update-ldcache", "--ldconfig-dir", "/opt/cxi/bin", "--folder", "/host/env"}
	if len(edits.Hooks) != 1 || edits.Hooks[0].Path != "/opt/cxi/bin/cxi-cdi-hook" ||
		!reflect.DeepEqual(edits.Hooks[0].Args, wantArgs) {
		t.Errorf("hooks = %+v, want %v", edits.Hooks, wantArgs)
	}

	disabled := false
	cfg.Ldcache.Enabled = &disabled
	if err := WriteEnvSpec(cdiRoot, testDriver, cfg); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}
	if edits := readEnvSpec(t, cdiRoot); len(edits.Hooks) != 0 {
		t.Errorf("hooks = %v, want none with ldcache disabled", edits.Hooks)
	}
}

// With --enable-cdi-env unset the driver writes the env spec from a zero
// config: it must still carry the gdrdrv and kdreg2 nodes and /dev/shm so
// every CXI device gets them, but no lib mounts, hook, or env.
func TestWriteEnvSpecZeroConfigInjectsDeviceNodes(t *testing.T) {
	cdiRoot := t.TempDir()
	// A regular file is enough: optionalDeviceNode only needs stat to succeed
	// (Rdev is 0 for a regular file, but the entry is still emitted).
	devDir := t.TempDir()
	gdrdrv := filepath.Join(devDir, "gdrdrv")
	kdreg2 := filepath.Join(devDir, "kdreg2")
	for _, p := range []string{gdrdrv, kdreg2} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatalf("creating fake %s: %v", p, err)
		}
	}
	cfg := absentDevNodes(t, CDIEnvConfig{})
	cfg.GdrDrvPath = gdrdrv
	cfg.KdReg2Path = kdreg2
	if err := WriteEnvSpec(cdiRoot, testDriver, cfg); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}
	edits := readEnvSpec(t, cdiRoot)
	var got []string
	for _, dn := range edits.DeviceNodes {
		got = append(got, dn.HostPath)
	}
	if want := []string{gdrdrv, kdreg2}; !reflect.DeepEqual(got, want) {
		t.Errorf("device nodes = %v, want %v", got, want)
	}
	if len(edits.Mounts) != 1 || edits.Mounts[0].ContainerPath != "/dev/shm" {
		t.Errorf("mounts = %v, want only /dev/shm", edits.Mounts)
	}
	if len(edits.Hooks) != 0 || len(edits.Env) != 0 {
		t.Errorf("hooks = %v env = %v, want none", edits.Hooks, edits.Env)
	}
}

// Specs of the removed nccl/aws-ofi-nccl profiles must be deleted so their
// device IDs stop resolving on nodes upgraded in place.
func TestWriteEnvSpecRemovesLegacyProfiles(t *testing.T) {
	cdiRoot := t.TempDir()
	var stale []string
	for _, name := range []string{"nccl", "aws-ofi-nccl"} {
		p := filepath.Join(cdiRoot, testDriver+"-"+name+".json")
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, p)
	}
	if err := WriteEnvSpec(cdiRoot, testDriver, absentDevNodes(t, CDIEnvConfig{})); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}
	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("legacy spec %s still present", p)
		}
	}
	if _, err := os.Stat(filepath.Join(cdiRoot, testDriver+"-env.json")); err != nil {
		t.Errorf("env spec not written: %v", err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoadConfig(t *testing.T) {
	path := writeConfig(t, `{
		"ldcache": {"enabled": false},
		"libs": [
			"/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1",
			"/usr/lib64/libcxi.so.1"
		],
		"env": ["CXIP_SKIP_AMA_CHECK=1"]
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	wantLibs := []string{"/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1", "/usr/lib64/libcxi.so.1"}
	if !reflect.DeepEqual(cfg.Libs, wantLibs) {
		t.Errorf("libs = %v, want %v", cfg.Libs, wantLibs)
	}
	if !reflect.DeepEqual(cfg.Env, []string{"CXIP_SKIP_AMA_CHECK=1"}) {
		t.Errorf("env = %v", cfg.Env)
	}
	if cfg.Ldcache.enabled() {
		t.Error("ldcache enabled, want disabled")
	}
}

func TestLoadConfigRejects(t *testing.T) {
	tests := []struct {
		name, body, wantErr string
	}{
		{"empty libs", `{"libs":[]}`, "libs is empty"},
		{"no libs", `{}`, "libs is empty"},
		{"legacy profiles schema", `{"profiles":{"env":{"libs":["/a"]}}}`, "unknown field"},
		{"legacy ldLibraryPath", `{"ldLibraryPath":{"mode":"prepend"},"libs":["/a"]}`, "unknown field"},
		{"legacy rename object", `{"libs":[{"path":"/a/libx.so","as":"liby.so"}]}`, "cannot unmarshal"},
		{"relative lib", `{"libs":["lib/libcxi.so.1"]}`, "must be an absolute file path"},
		{"root lib", `{"libs":["/"]}`, "must be an absolute file path"},
		{"collision", `{"libs":["/a/libz.so.1","/b/libz.so.1"]}`, "both mount at /host/env/libz.so.1"},
		{"hook path moved to driver flags", `{"ldcache":{"hookPath":"/a"},"libs":["/a"]}`, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tt.body))
			if err == nil {
				t.Fatal("LoadConfig accepted invalid config")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

// The shipped non-Helm ConfigMap must stay loadable under the current schema.
func TestDeployConfigMapLoads(t *testing.T) {
	data, err := os.ReadFile("../../deploy/kubernetes/configmap.yaml")
	if err != nil {
		t.Fatalf("reading configmap: %v", err)
	}
	_, body, ok := strings.Cut(string(data), "env.json: |\n")
	if !ok {
		t.Fatal("env.json key not found in configmap")
	}
	cfg, err := LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig(deploy/kubernetes/configmap.yaml): %v", err)
	}
	if !slicesContainPrefix(cfg.Env, "CXIP_SKIP_AMA_CHECK=") {
		t.Errorf("deploy configmap env %v lacks CXIP_SKIP_AMA_CHECK", cfg.Env)
	}
}

func slicesContainPrefix(list []string, prefix string) bool {
	for _, s := range list {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
