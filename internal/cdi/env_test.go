package cdi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

func TestDeviceIDs(t *testing.T) {
	const driver = "cxi.slingshot.hpe.com"
	tests := []struct {
		name string
		fn   func(string) string
		want string
	}{
		{"env", EnvDeviceID, driver + "/env=env"},
		{"nccl", NCCLDeviceID, driver + "/nccl=nccl"},
		{"aws-ofi-nccl", AWSofiNCCLDeviceID, driver + "/aws-ofi-nccl=aws-ofi-nccl"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(driver); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteEnvSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"

	const libfabricHost = "/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1"
	// Use non-existent paths so xpmem/gdrdrv device nodes are skipped.
	cfg := CDIEnvConfig{
		XpmemPath:  filepath.Join(t.TempDir(), "xpmem"),
		GdrDrvPath: filepath.Join(t.TempDir(), "gdrdrv"),
		EnvMounts: []MountSpec{
			{HostPath: libfabricHost, ContainerPath: "/usr/lib64/libfabric.so.1"},
			{HostPath: "/usr/lib64/libcxi.so.1", ContainerPath: "/usr/lib64/libcxi.so.1"},
			{HostPath: "/opt/xpmem/lib64/libxpmem.so.0", ContainerPath: "/usr/lib64/libxpmem.so.0"},
		},
	}

	if err := WriteEnvSpec(cdiRoot, driver, cfg); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}

	specPath := filepath.Join(cdiRoot, driver+"-env.json")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("reading spec: %v", err)
	}

	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshalling spec: %v", err)
	}

	if spec.Kind != driver+"/env" {
		t.Errorf("kind = %q, want %q", spec.Kind, driver+"/env")
	}
	if len(spec.Devices) != 1 || spec.Devices[0].Name != "env" {
		t.Errorf("devices = %v, want [{env}]", spec.Devices)
	}
	if len(spec.Devices[0].ContainerEdits.DeviceNodes) != 0 {
		t.Errorf("device nodes = %d, want 0 (both absent)", len(spec.Devices[0].ContainerEdits.DeviceNodes))
	}
	found := false
	for _, m := range spec.Devices[0].ContainerEdits.Mounts {
		if m.HostPath == libfabricHost {
			found = true
			break
		}
	}
	if !found {
		t.Error("libfabric mount not found in env spec")
	}
	foundShm := false
	for _, m := range spec.Devices[0].ContainerEdits.Mounts {
		if m.HostPath == "/dev/shm" {
			foundShm = true
			break
		}
	}
	if !foundShm {
		t.Error("/dev/shm mount not found in env spec")
	}
	// The libxpmem.so.0 mount has a host path different from its container path —
	// confirm both halves survive the round-trip through json.
	foundXpmem := false
	for _, m := range spec.Devices[0].ContainerEdits.Mounts {
		if m.HostPath == "/opt/xpmem/lib64/libxpmem.so.0" && m.ContainerPath == "/usr/lib64/libxpmem.so.0" {
			foundXpmem = true
			break
		}
	}
	if !foundXpmem {
		t.Error("libxpmem.so.0 mount with split host/container path not found")
	}
	if len(spec.Devices[0].ContainerEdits.Hooks) == 0 {
		t.Error("no hooks in env spec, want ldcache hook")
	}
}

func TestRemoveEnvSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{
		XpmemPath:  filepath.Join(t.TempDir(), "xpmem"),
		GdrDrvPath: filepath.Join(t.TempDir(), "gdrdrv"),
	}
	if err := WriteEnvSpec(cdiRoot, driver, cfg); err != nil {
		t.Fatalf("WriteEnvSpec: %v", err)
	}
	if err := RemoveEnvSpec(cdiRoot, driver); err != nil {
		t.Fatalf("RemoveEnvSpec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cdiRoot, driver+"-env.json")); !os.IsNotExist(err) {
		t.Error("env spec file still exists after RemoveEnvSpec")
	}
	if err := RemoveEnvSpec(cdiRoot, driver); err != nil {
		t.Errorf("RemoveEnvSpec on absent file: %v", err)
	}
}

func TestWriteNCCLSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{NCCLPrefix: "/spack/nccl-2.29.2"}

	if err := WriteNCCLSpec(cdiRoot, driver, cfg); err != nil {
		t.Fatalf("WriteNCCLSpec: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cdiRoot, driver+"-nccl.json"))
	if err != nil {
		t.Fatalf("reading NCCL spec: %v", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if spec.Kind != driver+"/nccl" {
		t.Errorf("kind = %q, want %q", spec.Kind, driver+"/nccl")
	}
	if len(spec.Devices) != 1 || spec.Devices[0].Name != "nccl" {
		t.Errorf("devices = %v, want [{nccl}]", spec.Devices)
	}
	if len(spec.Devices[0].ContainerEdits.Mounts) != 1 ||
		spec.Devices[0].ContainerEdits.Mounts[0].HostPath != cfg.NCCLPrefix ||
		spec.Devices[0].ContainerEdits.Mounts[0].ContainerPath != "/host/nccl" {
		t.Errorf("mounts = %v, want NCCL prefix → /host/nccl", spec.Devices[0].ContainerEdits.Mounts)
	}
	if len(spec.Devices[0].ContainerEdits.Hooks) == 0 {
		t.Error("no hooks in NCCL spec, want ldcache hook")
	}
}

func TestRemoveNCCLSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{NCCLPrefix: "/spack/nccl"}
	if err := WriteNCCLSpec(cdiRoot, driver, cfg); err != nil {
		t.Fatalf("WriteNCCLSpec: %v", err)
	}
	if err := RemoveNCCLSpec(cdiRoot, driver); err != nil {
		t.Fatalf("RemoveNCCLSpec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cdiRoot, driver+"-nccl.json")); !os.IsNotExist(err) {
		t.Error("nccl spec file still exists after RemoveNCCLSpec")
	}
	if err := RemoveNCCLSpec(cdiRoot, driver); err != nil {
		t.Errorf("RemoveNCCLSpec on absent file: %v", err)
	}
}

func TestWriteAWSofiNCCLSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{
		AWSofiMounts: []MountSpec{
			{HostPath: "/spack/aws-ofi-nccl-1.18.0/lib/libnccl-net.so", ContainerPath: "/usr/lib/libnccl-net.so"},
			{HostPath: "/spack/hwloc/lib/libhwloc.so.15", ContainerPath: "/usr/lib/libhwloc.so.15"},
		},
		AWSofiEnv: []string{"NCCL_NET=AWS Libfabric", "FI_CXI_RX_MATCH_MODE=hybrid"},
	}

	if err := WriteAWSofiNCCLSpec(cdiRoot, driver, cfg); err != nil {
		t.Fatalf("WriteAWSofiNCCLSpec: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cdiRoot, driver+"-aws-ofi-nccl.json"))
	if err != nil {
		t.Fatalf("reading spec: %v", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if spec.Kind != driver+"/aws-ofi-nccl" {
		t.Errorf("kind = %q, want %q", spec.Kind, driver+"/aws-ofi-nccl")
	}
	if len(spec.Devices) != 1 || spec.Devices[0].Name != "aws-ofi-nccl" {
		t.Errorf("devices = %v, want [{aws-ofi-nccl}]", spec.Devices)
	}

	found := false
	for _, m := range spec.Devices[0].ContainerEdits.Mounts {
		if m.HostPath == "/spack/aws-ofi-nccl-1.18.0/lib/libnccl-net.so" && m.ContainerPath == "/usr/lib/libnccl-net.so" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("libnccl-net.so mount not found; mounts = %v", spec.Devices[0].ContainerEdits.Mounts)
	}

	foundEnv := false
	for _, e := range spec.Devices[0].ContainerEdits.Env {
		if e == "NCCL_NET=AWS Libfabric" {
			foundEnv = true
			break
		}
	}
	if !foundEnv {
		t.Error("NCCL_NET env var not found in aws-ofi-nccl spec")
	}

	if len(spec.Devices[0].ContainerEdits.Hooks) == 0 {
		t.Error("no hooks in aws-ofi-nccl spec, want ldcache hook")
	}
}

func TestRemoveAWSofiNCCLSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{
		AWSofiMounts: []MountSpec{
			{HostPath: "/spack/aws-ofi-nccl/lib/libnccl-net.so", ContainerPath: "/usr/lib/libnccl-net.so"},
		},
	}
	if err := WriteAWSofiNCCLSpec(cdiRoot, driver, cfg); err != nil {
		t.Fatalf("WriteAWSofiNCCLSpec: %v", err)
	}
	if err := RemoveAWSofiNCCLSpec(cdiRoot, driver); err != nil {
		t.Fatalf("RemoveAWSofiNCCLSpec: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cdiRoot, driver+"-aws-ofi-nccl.json")); !os.IsNotExist(err) {
		t.Error("aws-ofi-nccl spec file still exists after remove")
	}
	if err := RemoveAWSofiNCCLSpec(cdiRoot, driver); err != nil {
		t.Errorf("RemoveAWSofiNCCLSpec on absent file: %v", err)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env.json")
	if err := os.WriteFile(path, []byte(`{
		"envMounts": [
			{"hostPath": "/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1", "containerPath": "/usr/lib64/libfabric.so.1"},
			{"hostPath": "/opt/xpmem/lib64/libxpmem.so.0",                "containerPath": "/usr/lib64/libxpmem.so.0"}
		],
		"ncclPrefix": "/spack/nccl",
		"awsOfiMounts": [
			{"hostPath": "/spack/aws-ofi-nccl/lib/libnccl-net.so", "containerPath": "/usr/lib/libnccl-net.so"}
		],
		"awsOfiEnv": ["NCCL_NET=AWS Libfabric"]
	}`), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.EnvMounts) != 2 || cfg.EnvMounts[1].HostPath != "/opt/xpmem/lib64/libxpmem.so.0" {
		t.Errorf("EnvMounts = %v", cfg.EnvMounts)
	}
	if cfg.NCCLPrefix != "/spack/nccl" {
		t.Errorf("NCCLPrefix = %q", cfg.NCCLPrefix)
	}
	if len(cfg.AWSofiMounts) != 1 || len(cfg.AWSofiEnv) != 1 {
		t.Errorf("aws-ofi fields = %v / %v", cfg.AWSofiMounts, cfg.AWSofiEnv)
	}
}

func TestLoadConfigRejectsEmptyMounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(path, []byte(`{"envMounts":[]}`), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("LoadConfig accepted empty envMounts")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(path, []byte(`{
		"envMounts":[{"hostPath":"/a","containerPath":"/a"}],
		"libfabricLib":"/opt/cray/libfabric.so.1"
	}`), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("LoadConfig accepted unknown field libfabricLib")
	}
}
