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

	// Use non-existent paths so xpmem/gdrdrv device nodes are skipped.
	cfg := CDIEnvConfig{
		XpmemPath:    filepath.Join(t.TempDir(), "xpmem"),
		GdrDrvPath:   filepath.Join(t.TempDir(), "gdrdrv"),
		LibfabricLib: "/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1",
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
	// xpmem/gdrdrv absent → no device nodes.
	if len(spec.ContainerEdits.DeviceNodes) != 0 {
		t.Errorf("device nodes = %d, want 0 (both absent)", len(spec.ContainerEdits.DeviceNodes))
	}
	// libfabric should appear in mounts.
	found := false
	for _, m := range spec.ContainerEdits.Mounts {
		if m.HostPath == cfg.LibfabricLib {
			found = true
			break
		}
	}
	if !found {
		t.Error("libfabric mount not found in env spec")
	}
	// /dev/shm must be present.
	foundShm := false
	for _, m := range spec.ContainerEdits.Mounts {
		if m.HostPath == "/dev/shm" {
			foundShm = true
			break
		}
	}
	if !foundShm {
		t.Error("/dev/shm mount not found in env spec")
	}
	// ldcache hook must be present.
	if len(spec.ContainerEdits.Hooks) == 0 {
		t.Error("no hooks in env spec, want ldcache hook")
	}
}

func TestWriteEnvSpecNoLibfabric(t *testing.T) {
	cdiRoot := t.TempDir()
	cfg := CDIEnvConfig{
		XpmemPath:  filepath.Join(t.TempDir(), "xpmem"),
		GdrDrvPath: filepath.Join(t.TempDir(), "gdrdrv"),
	}
	if err := WriteEnvSpec(cdiRoot, "cxi.slingshot.hpe.com", cfg); err != nil {
		t.Fatalf("WriteEnvSpec without libfabric: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(cdiRoot, "cxi.slingshot.hpe.com-env.json"))
	var spec specs.Spec
	json.Unmarshal(data, &spec)
	for _, m := range spec.ContainerEdits.Mounts {
		if m.ContainerPath == "/usr/lib64/libfabric.so.1" {
			t.Error("libfabric mount found but LibfabricLib was empty")
		}
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
	// Second remove must be a no-op.
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
	if len(spec.ContainerEdits.Mounts) != 1 ||
		spec.ContainerEdits.Mounts[0].HostPath != cfg.NCCLPrefix ||
		spec.ContainerEdits.Mounts[0].ContainerPath != "/host/nccl" {
		t.Errorf("mounts = %v, want NCCL prefix → /host/nccl", spec.ContainerEdits.Mounts)
	}
	if len(spec.ContainerEdits.Hooks) == 0 {
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

