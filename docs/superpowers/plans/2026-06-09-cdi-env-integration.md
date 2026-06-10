# CDI Environment Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expand the DRA driver's CDI spec generation to include the full CXI container environment — shared device nodes, library bind-mounts, and optional NCCL/AWS-OFI-NCCL support — using separate per-kind CDI spec files alongside the existing per-device specs.

**Architecture:** A new `internal/cdi/env.go` file provides three spec writers (`WriteEnvSpec`, `WriteNCCLSpec`, `WriteAWSofiNCCLSpec`) and corresponding DeviceID helpers. The driver calls these at startup via `Run()` and `prepare.go` appends the extra CDI device IDs to every allocation. Configuration is passed through `driver.Config.CDIEnvConfig`, surfaced as new CLI flags.

**Tech Stack:** Go 1.23+, `tags.cncf.io/container-device-interface/specs-go v0.8.0`, standard `encoding/json`, `syscall`, `os`

**Spec:** `docs/superpowers/specs/2026-06-09-cdi-env-integration-design.md`

---

## File Map

| Action | Path | Responsibility |
|--------|------|----------------|
| Create | `internal/cdi/env.go` | `CDIEnvConfig` struct, three spec writers, three removers, three DeviceID helpers |
| Create | `internal/cdi/env_test.go` | Tests for all functions in `env.go` |
| Modify | `internal/driver/driver.go` | Add `CDIEnvConfig` to `Config`; call env spec writers in `Run()` |
| Modify | `internal/driver/prepare.go` | Append extra CDI device IDs (`/env=env`, `/nccl=nccl`, `/aws-ofi-nccl=aws-ofi-nccl`) |
| Modify | `internal/driver/prepare_test.go` | Test CDI device ID injection with various `CDIEnvConfig` combinations |
| Modify | `cmd/cxi-dra-driver/main.go` | Add 9 new CLI flags; pass `CDIEnvConfig` via `driver.Config` |
| Modify | `deploy/kubernetes/daemonset.yaml` | Add Isambard flag values to `args` |

---

## Task 1: Create the integration branch

**Files:** none (git only)

- [ ] **Step 1: Create and switch to the new branch**

```bash
git checkout -b cdi-env-integration
```

Expected: `Switched to a new branch 'cdi-env-integration'`

---

## Task 2: `CDIEnvConfig` struct and DeviceID helpers

**Files:**
- Create: `internal/cdi/env.go`
- Create: `internal/cdi/env_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/cdi/env_test.go`:

```go
package cdi

import "testing"

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
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cdi/... -run TestDeviceIDs -v
```

Expected: compile error — `EnvDeviceID`, `NCCLDeviceID`, `AWSofiNCCLDeviceID` undefined.

- [ ] **Step 3: Implement `CDIEnvConfig` and DeviceID helpers**

Create `internal/cdi/env.go`:

```go
package cdi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	specs "tags.cncf.io/container-device-interface/specs-go"
)

// CDIEnvConfig holds configurable paths for environment CDI spec generation.
// XpmemPath and GdrDrvPath default to the standard paths when empty.
type CDIEnvConfig struct {
	LibfabricLib string // host path to libfabric.so.1; empty = skip mount

	NCCLPrefix string // Spack NCCL install dir; empty = skip NCCL spec

	AWSofiNCCLPrefix string // Spack aws-ofi-nccl install dir; empty = skip OFI spec
	HwlocLib         string
	PciaccessLib     string
	Libxml2Lib       string
	LibatomicLib     string
	LiblzmaLib       string
	LibiconvLib      string

	// Optional path overrides for /dev/xpmem and /dev/gdrdrv; used in tests.
	XpmemPath  string
	GdrDrvPath string
}

func EnvDeviceID(driverName string) string {
	return fmt.Sprintf("%s/env=env", driverName)
}

func NCCLDeviceID(driverName string) string {
	return fmt.Sprintf("%s/nccl=nccl", driverName)
}

func AWSofiNCCLDeviceID(driverName string) string {
	return fmt.Sprintf("%s/aws-ofi-nccl=aws-ofi-nccl", driverName)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cdi/... -run TestDeviceIDs -v
```

Expected: `PASS`

- [ ] **Step 5: Commit**

```bash
git add internal/cdi/env.go internal/cdi/env_test.go
git commit -m "cdi: add CDIEnvConfig struct and DeviceID helpers"
```

---

## Task 3: `WriteEnvSpec` and `RemoveEnvSpec`

**Files:**
- Modify: `internal/cdi/env.go`
- Modify: `internal/cdi/env_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cdi/env_test.go`:

```go
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
```

Add the `"path/filepath"` import to the test file (it already has `"testing"` and `"os"` once added in the next step; add all at once).

Update the import block at the top of `internal/cdi/env_test.go`:

```go
package cdi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	specs "tags.cncf.io/container-device-interface/specs-go"
)
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cdi/... -run 'TestWriteEnvSpec|TestRemoveEnvSpec' -v
```

Expected: compile error — `WriteEnvSpec`, `RemoveEnvSpec` undefined.

- [ ] **Step 3: Implement `WriteEnvSpec` and `RemoveEnvSpec`**

Append to `internal/cdi/env.go` (below the DeviceID helpers):

```go
// WriteEnvSpec writes the shared CXI environment CDI spec containing device
// nodes, library bind-mounts, and the ldcache hook that every CXI container
// needs. xpmem and gdrdrv device nodes are included only when present on the
// host; a missing libfabric path skips that mount with a warning.
func WriteEnvSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	xpmemPath := cfg.XpmemPath
	if xpmemPath == "" {
		xpmemPath = "/dev/xpmem"
	}
	gdrDrvPath := cfg.GdrDrvPath
	if gdrDrvPath == "" {
		gdrDrvPath = "/dev/gdrdrv"
	}

	var deviceNodes []*specs.DeviceNode
	for _, p := range []string{xpmemPath, gdrDrvPath} {
		if dn := optionalDeviceNode(p); dn != nil {
			deviceNodes = append(deviceNodes, dn)
		}
	}

	mounts := []*specs.Mount{
		{
			HostPath:      "/dev/shm",
			ContainerPath: "/dev/shm",
			Options:       []string{"bind", "rw", "nosuid", "private"},
		},
	}
	if cfg.LibfabricLib != "" {
		mounts = append(mounts, &specs.Mount{
			HostPath:      cfg.LibfabricLib,
			ContainerPath: "/usr/lib64/libfabric.so.1",
			Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
		})
	}
	for _, so := range []string{
		"/usr/lib64/libcxi.so.1",
		"/usr/lib64/libcurl.so.4",
		"/usr/lib64/libjson-c.so.5",
		"/usr/lib64/libatomic.so.1",
		"/usr/lib64/libnl-3.so.200",
		"/usr/lib64/libnghttp2.so.14",
		"/usr/lib64/libidn2.so.0",
		"/usr/lib64/libssh.so.4",
		"/usr/lib64/libpsl.so.5",
		"/usr/lib64/libssl.so.1.1",
		"/usr/lib64/libcrypto.so.1.1",
		"/usr/lib64/libgssapi_krb5.so.2",
		"/usr/lib64/libldap_r-2.4.so.2",
		"/usr/lib64/liblber-2.4.so.2",
		"/usr/lib64/libzstd.so.1",
		"/usr/lib64/libbrotlidec.so.1",
		"/usr/lib64/libz.so.1",
		"/usr/lib64/libunistring.so.2",
		"/usr/lib64/libjitterentropy.so.3",
		"/usr/lib64/libkrb5.so.3",
		"/usr/lib64/libk5crypto.so.3",
		"/lib64/libcom_err.so.2",
		"/usr/lib64/libkrb5support.so.0",
		"/usr/lib64/libsasl2.so.3",
		"/usr/lib64/libbrotlicommon.so.1",
		"/usr/lib64/libkeyutils.so.1",
		"/usr/lib64/libpcre.so.1",
		"/usr/lib64/libxpmem.so.0",
	} {
		mounts = append(mounts, &specs.Mount{
			HostPath:      so,
			ContainerPath: so,
			Options:       []string{"bind", "ro", "nosuid", "private"},
		})
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/env",
		Devices: []specs.Device{{Name: "env"}},
		ContainerEdits: specs.ContainerEdits{
			DeviceNodes: deviceNodes,
			Mounts:      mounts,
			Hooks:       []*specs.Hook{ldcacheHook("/usr/lib64")},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI env spec: %w", err)
	}
	return os.WriteFile(filepath.Join(cdiRoot, driverName+"-env.json"), data, 0o644)
}

// RemoveEnvSpec deletes the env CDI spec file.
func RemoveEnvSpec(cdiRoot, driverName string) error {
	return removeSpec(filepath.Join(cdiRoot, driverName+"-env.json"))
}

// optionalDeviceNode stats hostPath and returns a DeviceNode if the path
// exists on the host; returns nil without error if the path is absent.
func optionalDeviceNode(hostPath string) *specs.DeviceNode {
	var st syscall.Stat_t
	if err := syscall.Stat(hostPath, &st); err != nil {
		return nil
	}
	major := int64((uint64(st.Rdev) >> 8) & 0xfff)
	minor := int64((uint64(st.Rdev) & 0xff) | ((uint64(st.Rdev) >> 12) & 0xfff00))
	return &specs.DeviceNode{
		Path:        hostPath,
		HostPath:    hostPath,
		Type:        "c",
		Major:       major,
		Minor:       minor,
		Permissions: "rw",
	}
}

// ldcacheHook returns a createContainer hook that runs nvidia-cdi-hook
// update-ldcache for each provided folder.
func ldcacheHook(folders ...string) *specs.Hook {
	args := []string{"nvidia-cdi-hook", "update-ldcache"}
	for _, f := range folders {
		args = append(args, "--folder", f)
	}
	return &specs.Hook{
		HookName: "createContainer",
		Path:     "/usr/bin/nvidia-cdi-hook",
		Args:     args,
	}
}

// removeSpec removes a CDI spec file, treating a missing file as a no-op.
func removeSpec(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cdi/... -run 'TestWriteEnvSpec|TestRemoveEnvSpec' -v
```

Expected: `PASS`

- [ ] **Step 5: Commit**

```bash
git add internal/cdi/env.go internal/cdi/env_test.go
git commit -m "cdi: add WriteEnvSpec and RemoveEnvSpec"
```

---

## Task 4: `WriteNCCLSpec` and `RemoveNCCLSpec`

**Files:**
- Modify: `internal/cdi/env.go`
- Modify: `internal/cdi/env_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cdi/env_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cdi/... -run 'TestWriteNCCLSpec|TestRemoveNCCLSpec' -v
```

Expected: compile error — `WriteNCCLSpec`, `RemoveNCCLSpec` undefined.

- [ ] **Step 3: Implement `WriteNCCLSpec` and `RemoveNCCLSpec`**

Append to `internal/cdi/env.go`:

```go
// WriteNCCLSpec writes the NCCL CDI spec, mounting the whole NCCL Spack prefix
// into /host/nccl so the container's ldcache can find the NCCL libraries.
func WriteNCCLSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/nccl",
		Devices: []specs.Device{{Name: "nccl"}},
		ContainerEdits: specs.ContainerEdits{
			Mounts: []*specs.Mount{
				{
					HostPath:      cfg.NCCLPrefix,
					ContainerPath: "/host/nccl",
					Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
				},
			},
			Hooks: []*specs.Hook{ldcacheHook("/host/nccl/lib")},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI NCCL spec: %w", err)
	}
	return os.WriteFile(filepath.Join(cdiRoot, driverName+"-nccl.json"), data, 0o644)
}

// RemoveNCCLSpec deletes the NCCL CDI spec file.
func RemoveNCCLSpec(cdiRoot, driverName string) error {
	return removeSpec(filepath.Join(cdiRoot, driverName+"-nccl.json"))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./internal/cdi/... -run 'TestWriteNCCLSpec|TestRemoveNCCLSpec' -v
```

Expected: `PASS`

- [ ] **Step 5: Commit**

```bash
git add internal/cdi/env.go internal/cdi/env_test.go
git commit -m "cdi: add WriteNCCLSpec and RemoveNCCLSpec"
```

---

## Task 5: `WriteAWSofiNCCLSpec` and `RemoveAWSofiNCCLSpec`

**Files:**
- Modify: `internal/cdi/env.go`
- Modify: `internal/cdi/env_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cdi/env_test.go`:

```go
func TestWriteAWSofiNCCLSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{
		AWSofiNCCLPrefix: "/spack/aws-ofi-nccl-1.18.0",
		HwlocLib:         "/spack/hwloc/lib/libhwloc.so.15",
		PciaccessLib:     "/spack/pciaccess/lib/libpciaccess.so.0",
		Libxml2Lib:       "/spack/libxml2/lib/libxml2.so.2",
		LibatomicLib:     "/spack/gcc-runtime/lib/libatomic.so.1",
		LiblzmaLib:       "/spack/xz/lib/liblzma.so.5",
		LibiconvLib:      "/spack/libiconv/lib/libiconv.so.2",
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

	// libnccl-net.so must be present in mounts.
	wantHostPath := cfg.AWSofiNCCLPrefix + "/lib/libnccl-net.so"
	found := false
	for _, m := range spec.ContainerEdits.Mounts {
		if m.HostPath == wantHostPath && m.ContainerPath == "/usr/lib/libnccl-net.so" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("libnccl-net.so mount not found; mounts = %v", spec.ContainerEdits.Mounts)
	}

	// Env vars must include NCCL_NET.
	foundEnv := false
	for _, e := range spec.ContainerEdits.Env {
		if e == "NCCL_NET=AWS Libfabric" {
			foundEnv = true
			break
		}
	}
	if !foundEnv {
		t.Error("NCCL_NET env var not found in aws-ofi-nccl spec")
	}

	// ldcache hook must be present.
	if len(spec.ContainerEdits.Hooks) == 0 {
		t.Error("no hooks in aws-ofi-nccl spec, want ldcache hook")
	}
}

func TestRemoveAWSofiNCCLSpec(t *testing.T) {
	cdiRoot := t.TempDir()
	const driver = "cxi.slingshot.hpe.com"
	cfg := CDIEnvConfig{AWSofiNCCLPrefix: "/spack/aws-ofi-nccl"}
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
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/cdi/... -run 'TestWriteAWSofiNCCLSpec|TestRemoveAWSofiNCCLSpec' -v
```

Expected: compile error — `WriteAWSofiNCCLSpec`, `RemoveAWSofiNCCLSpec` undefined.

- [ ] **Step 3: Implement `WriteAWSofiNCCLSpec` and `RemoveAWSofiNCCLSpec`**

Append to `internal/cdi/env.go`:

```go
// WriteAWSofiNCCLSpec writes the AWS OFI NCCL CDI spec, including the OFI
// plugin library, GDRCopy API library, Spack dependency libs, and the NCCL/
// libfabric tuning env vars from the Isambard podman configuration.
func WriteAWSofiNCCLSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error {
	if err := os.MkdirAll(cdiRoot, 0o755); err != nil {
		return err
	}

	mounts := []*specs.Mount{
		{
			HostPath:      cfg.AWSofiNCCLPrefix + "/lib/libnccl-net.so",
			ContainerPath: "/usr/lib/libnccl-net.so",
			Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
		},
		{
			HostPath:      "/usr/lib64/libgdrapi.so.2.5",
			ContainerPath: "/usr/lib/libgdrapi.so",
			Options:       []string{"bind", "ro", "nosuid", "nodev", "private"},
		},
	}

	type depLib struct {
		hostPath  string
		soName    string
	}
	for _, dep := range []depLib{
		{cfg.HwlocLib, "libhwloc.so.15"},
		{cfg.PciaccessLib, "libpciaccess.so.0"},
		{cfg.Libxml2Lib, "libxml2.so.2"},
		{cfg.LibatomicLib, "libatomic.so.1"},
		{cfg.LiblzmaLib, "liblzma.so.5"},
		{cfg.LibiconvLib, "libiconv.so.2"},
	} {
		if dep.hostPath == "" {
			continue
		}
		mounts = append(mounts, &specs.Mount{
			HostPath:      dep.hostPath,
			ContainerPath: "/usr/lib/" + dep.soName,
			Options:       []string{"bind", "rw", "nosuid", "nodev", "private"},
		})
	}

	spec := &specs.Spec{
		Version: cdiVersion,
		Kind:    driverName + "/aws-ofi-nccl",
		Devices: []specs.Device{{Name: "aws-ofi-nccl"}},
		ContainerEdits: specs.ContainerEdits{
			Env: []string{
				"NCCL_NET=AWS Libfabric",
				"NCCL_CROSS_NIC=1",
				"NCCL_NET_GDR_LEVEL=PHB",
				"NCCL_SOCKET_IFNAME=hsn",
				"NCCL_MIN_NCHANNELS=4",
				"NCCL_GDRCOPY_ENABLE=1",
				"NCCL_NET_FORCE_FLUSH=0",
				"FI_MR_CACHE_MONITOR=userfaultfd",
				"FI_CXI_DISABLE_HOST_REGISTER=1",
				"FI_HMEM_CUDA_USE_GDRCOPY=1",
				"FI_CXI_DEFAULT_CQ_SIZE=131072",
				"FI_CXI_DEFAULT_TX_SIZE=2048",
				"FI_CXI_RDZV_PROTO=alt_read",
				"FI_CXI_RDZV_THRESHOLD=0",
				"FI_CXI_RDZV_GET_MIN=0",
				"FI_CXI_RDZV_EAGER_SIZE=0",
				"FI_CXI_DISABLE_NON_INJECT_MSG_IDC=1",
				"FI_CXI_RX_MATCH_MODE=hybrid",
			},
			Mounts: mounts,
			Hooks:  []*specs.Hook{ldcacheHook("/usr/lib", "/usr/lib64")},
		},
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling CDI aws-ofi-nccl spec: %w", err)
	}
	return os.WriteFile(filepath.Join(cdiRoot, driverName+"-aws-ofi-nccl.json"), data, 0o644)
}

// RemoveAWSofiNCCLSpec deletes the AWS OFI NCCL CDI spec file.
func RemoveAWSofiNCCLSpec(cdiRoot, driverName string) error {
	return removeSpec(filepath.Join(cdiRoot, driverName+"-aws-ofi-nccl.json"))
}
```

- [ ] **Step 4: Run the full CDI test suite**

```bash
go test ./internal/cdi/... -v
```

Expected: all tests `PASS`

- [ ] **Step 5: Commit**

```bash
git add internal/cdi/env.go internal/cdi/env_test.go
git commit -m "cdi: add WriteAWSofiNCCLSpec and RemoveAWSofiNCCLSpec"
```

---

## Task 6: Driver `Config` changes and env spec writing in `Run()`

**Files:**
- Modify: `internal/driver/driver.go`

- [ ] **Step 1: Add `CDIEnvConfig` to `Config` and `Driver`, update `New()` and `Run()`**

In `internal/driver/driver.go`, make these changes:

Add `cdiEnvConfig cdi.CDIEnvConfig` to the `Driver` struct after `overhead`:

```go
type Driver struct {
	nodeName        string
	podUID          string
	driverName      string
	cdiRoot         string
	pluginDir       string
	registrarDir    string
	kubeClient      kubernetes.Interface
	mockDevices     []*cxi.Device
	devices         []*cxi.Device
	store           *AllocationStore
	overhead        cxi.ResourceSet
	refreshInterval time.Duration
	cdiEnvConfig    cdi.CDIEnvConfig
}
```

Add `CDIEnvConfig cdi.CDIEnvConfig` to `Config`:

```go
type Config struct {
	Overhead        cxi.ResourceSet
	RefreshInterval time.Duration
	CDIEnvConfig    cdi.CDIEnvConfig
}
```

In `New()`, add `cdiEnvConfig: cfg.CDIEnvConfig` to the returned struct:

```go
return &Driver{
	nodeName:        nodeName,
	podUID:          podUID,
	driverName:      driverName,
	cdiRoot:         cdiRoot,
	pluginDir:       pluginDir,
	registrarDir:    registrarDir,
	kubeClient:      kubeClient,
	mockDevices:     mockDevices,
	store:           NewAllocationStore(),
	overhead:        cfg.Overhead,
	refreshInterval: cfg.RefreshInterval,
	cdiEnvConfig:    cfg.CDIEnvConfig,
}
```

In `Run()`, after the `for _, dev := range d.devices` loop (the existing per-device `WriteSpec` calls), add:

```go
	if err := cdi.WriteEnvSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
		return fmt.Errorf("writing CDI env spec: %w", err)
	}
	if d.cdiEnvConfig.NCCLPrefix != "" {
		if err := cdi.WriteNCCLSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
			return fmt.Errorf("writing CDI NCCL spec: %w", err)
		}
	}
	if d.cdiEnvConfig.AWSofiNCCLPrefix != "" {
		if err := cdi.WriteAWSofiNCCLSpec(d.cdiRoot, d.driverName, d.cdiEnvConfig); err != nil {
			return fmt.Errorf("writing CDI AWS OFI NCCL spec: %w", err)
		}
	}
```

- [ ] **Step 2: Build to verify no compile errors**

```bash
go build ./...
```

Expected: success (no output)

- [ ] **Step 3: Run existing driver tests to verify nothing is broken**

```bash
go test ./internal/driver/... -v
```

Expected: all tests `PASS`

- [ ] **Step 4: Commit**

```bash
git add internal/driver/driver.go
git commit -m "driver: write CDI env/nccl/aws-ofi-nccl specs at startup"
```

---

## Task 7: `prepare.go` — inject extra CDI device IDs

**Files:**
- Modify: `internal/driver/prepare.go`
- Modify: `internal/driver/prepare_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/driver/prepare_test.go`:

```go
func TestPrepareClaimCDIDeviceIDs(t *testing.T) {
	tests := []struct {
		name    string
		cfg     cdi.CDIEnvConfig
		wantIDs []string
	}{
		{
			name: "base only",
			cfg:  cdi.CDIEnvConfig{},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
			},
		},
		{
			name: "with NCCL",
			cfg:  cdi.CDIEnvConfig{NCCLPrefix: "/spack/nccl"},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/nccl=nccl",
			},
		},
		{
			name: "with AWS OFI NCCL",
			cfg:  cdi.CDIEnvConfig{AWSofiNCCLPrefix: "/spack/ofi"},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/aws-ofi-nccl=aws-ofi-nccl",
			},
		},
		{
			name: "with both",
			cfg:  cdi.CDIEnvConfig{NCCLPrefix: "/spack/nccl", AWSofiNCCLPrefix: "/spack/ofi"},
			wantIDs: []string{
				testDriver + "/cxi=cxi0",
				testDriver + "/env=env",
				testDriver + "/nccl=nccl",
				testDriver + "/aws-ofi-nccl=aws-ofi-nccl",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDriver()
			d.cdiEnvConfig = tt.cfg

			claim := &resourceapi.ResourceClaim{
				ObjectMeta: metav1.ObjectMeta{UID: types.UID("claim-cdi")},
				Spec: resourceapi.ResourceClaimSpec{
					Devices: resourceapi.DeviceClaim{Config: cxiConfig()},
				},
				Status: resourceapi.ResourceClaimStatus{
					Allocation: &resourceapi.AllocationResult{
						Devices: resourceapi.DeviceAllocationResult{
							Results: []resourceapi.DeviceRequestAllocationResult{cxiResult("cxi0")},
						},
					},
					ReservedFor: []resourceapi.ResourceClaimConsumerReference{
						{Resource: "pods", UID: "pod-x"},
					},
				},
			}
			vniByPod := map[types.UID]int{"pod-x": 50}

			res := d.prepareClaim(claim, vniByPod)
			if res.Err != nil {
				t.Fatalf("prepareClaim: %v", res.Err)
			}
			if len(res.Devices) != 1 {
				t.Fatalf("got %d devices, want 1", len(res.Devices))
			}

			gotIDs := res.Devices[0].CDIDeviceIDs
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("CDIDeviceIDs = %v, want %v", gotIDs, tt.wantIDs)
			}
			for i, want := range tt.wantIDs {
				if gotIDs[i] != want {
					t.Errorf("CDIDeviceIDs[%d] = %q, want %q", i, gotIDs[i], want)
				}
			}
		})
	}
}
```

Add `"github.com/slingshot/cxi-dra-driver/internal/cdi"` to the import block of `internal/driver/prepare_test.go`. The full import block becomes:

```go
import (
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
)
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./internal/driver/... -run TestPrepareClaimCDIDeviceIDs -v
```

Expected: `FAIL` — CDI IDs slice has only 1 element (the device ID), but wants 2+.

- [ ] **Step 3: Implement CDI ID injection in `prepareClaim`**

In `internal/driver/prepare.go`, replace the `kubeletDevices = append(...)` block inside the allocation loop with:

```go
		cdiIDs := []string{
			cdi.DeviceID(d.driverName, alloc.Device),
			cdi.EnvDeviceID(d.driverName),
		}
		if d.cdiEnvConfig.NCCLPrefix != "" {
			cdiIDs = append(cdiIDs, cdi.NCCLDeviceID(d.driverName))
		}
		if d.cdiEnvConfig.AWSofiNCCLPrefix != "" {
			cdiIDs = append(cdiIDs, cdi.AWSofiNCCLDeviceID(d.driverName))
		}

		kubeletDevices = append(kubeletDevices, kubeletplugin.Device{
			Requests:     []string{alloc.Request},
			PoolName:     alloc.Pool,
			DeviceName:   alloc.Device,
			CDIDeviceIDs: cdiIDs,
		})
```

- [ ] **Step 4: Run all driver tests**

```bash
go test ./internal/driver/... -v
```

Expected: all tests `PASS`

- [ ] **Step 5: Commit**

```bash
git add internal/driver/prepare.go internal/driver/prepare_test.go
git commit -m "driver: inject env/nccl/aws-ofi-nccl CDI device IDs in prepare"
```

---

## Task 8: New CLI flags in `main.go`

**Files:**
- Modify: `cmd/cxi-dra-driver/main.go`

- [ ] **Step 1: Add the 9 new flags and pass `CDIEnvConfig` to `driver.New()`**

In `cmd/cxi-dra-driver/main.go`, add these flag declarations inside the `var (...)` block, after the existing `reservedTLEPools` declaration:

```go
		// CDI environment configuration: libraries and optional capability packs.
		libfabricLib     = flag.String("libfabric-lib", "", "Host path to libfabric.so.1 for CDI env spec (empty = skip mount)")
		ncclPrefix       = flag.String("nccl-prefix", "", "Spack NCCL install dir; enables NCCL CDI spec when set")
		awsOfiNCCLPrefix = flag.String("aws-ofi-nccl-prefix", "", "Spack aws-ofi-nccl install dir; enables AWS OFI NCCL CDI spec when set")
		hwlocLib         = flag.String("hwloc-lib", "", "Host path to libhwloc.so.15 (aws-ofi-nccl dependency)")
		pciaccessLib     = flag.String("pciaccess-lib", "", "Host path to libpciaccess.so.0 (aws-ofi-nccl dependency)")
		libxml2Lib       = flag.String("libxml2-lib", "", "Host path to libxml2.so.2 (aws-ofi-nccl dependency)")
		libatomicLib     = flag.String("libatomic-lib", "", "Host path to libatomic.so.1 (aws-ofi-nccl dependency)")
		liblzmaLib       = flag.String("liblzma-lib", "", "Host path to liblzma.so.5 (aws-ofi-nccl dependency)")
		libiconvLib      = flag.String("libiconv-lib", "", "Host path to libiconv.so.2 (aws-ofi-nccl dependency)")
```

In the `driver.New(...)` call, add `CDIEnvConfig` to `driver.Config`:

```go
	d := driver.New(
		*nodeName, *podUID, *driverName, *cdiRoot, *pluginDir, *registrarDir,
		kubeClient, mockDevList,
		driver.Config{
			Overhead:        overhead,
			RefreshInterval: *refreshInterval,
			CDIEnvConfig: cdi.CDIEnvConfig{
				LibfabricLib:     *libfabricLib,
				NCCLPrefix:       *ncclPrefix,
				AWSofiNCCLPrefix: *awsOfiNCCLPrefix,
				HwlocLib:         *hwlocLib,
				PciaccessLib:     *pciaccessLib,
				Libxml2Lib:       *libxml2Lib,
				LibatomicLib:     *libatomicLib,
				LiblzmaLib:       *liblzmaLib,
				LibiconvLib:      *libiconvLib,
			},
		},
	)
```

Add the `cdi` package import to `cmd/cxi-dra-driver/main.go`. The import block becomes:

```go
import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
	"github.com/slingshot/cxi-dra-driver/internal/cxi"
	"github.com/slingshot/cxi-dra-driver/internal/driver"
	"github.com/slingshot/cxi-dra-driver/internal/nri"
)
```

- [ ] **Step 2: Build to verify no compile errors**

```bash
go build ./...
```

Expected: success (no output)

- [ ] **Step 3: Commit**

```bash
git add cmd/cxi-dra-driver/main.go
git commit -m "main: add CDI env/nccl/aws-ofi-nccl CLI flags"
```

---

## Task 9: Update `deploy/kubernetes/daemonset.yaml`

**Files:**
- Modify: `deploy/kubernetes/daemonset.yaml`

- [ ] **Step 1: Add the Isambard flag values to the daemonset `args`**

In `deploy/kubernetes/daemonset.yaml`, replace the `args` block:

```yaml
          args:
            - --driver-name=cxi.slingshot.hpe.com
            - --cdi-root=/var/run/cdi
            - --plugin-dir=/var/lib/kubelet/plugins/cxi.slingshot.hpe.com
            - --registrar-dir=/var/lib/kubelet/plugins_registry
```

with:

```yaml
          args:
            - --driver-name=cxi.slingshot.hpe.com
            - --cdi-root=/var/run/cdi
            - --plugin-dir=/var/lib/kubelet/plugins/cxi.slingshot.hpe.com
            - --registrar-dir=/var/lib/kubelet/plugins_registry
            - --libfabric-lib=/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1
            - --nccl-prefix=/projects/public/brics/dev/apps/linux-neoverse_v2/nccl-2.29.2-1-yjntbzz5uuuashekge53wqizfqum6swy
            - --aws-ofi-nccl-prefix=/projects/public/brics/dev/apps/linux-neoverse_v2/aws-ofi-nccl-1.18.0-6pbztrbjhvrmtu5r6nq2silgputmmcyk
            - --hwloc-lib=/projects/public/brics/dev/apps/linux-neoverse_v2/hwloc-2.12.2-xbbx7czzg3oczc3trftvj2dat3t6qj3o/lib/libhwloc.so.15
            - --pciaccess-lib=/projects/public/brics/dev/apps/linux-neoverse_v2/libpciaccess-0.17-yisowb26ojigtiis73t5dukyrvx7r6rk/lib/libpciaccess.so.0
            - --libxml2-lib=/projects/public/brics/dev/apps/linux-neoverse_v2/libxml2-2.13.5-xr3mgbb5y3midpndczeurgot35yosxkd/lib/libxml2.so.2
            - --libatomic-lib=/projects/public/brics/dev/apps/linux-neoverse_v2/gcc-runtime-14.3.0-4aynqrbkrzeranfuqhyfg7wktmszdenq/lib/libatomic.so.1
            - --liblzma-lib=/projects/public/brics/dev/apps/linux-neoverse_v2/xz-5.6.3-iqien4jp4hkpdks2rzyflpvgw3b72zqc/lib/liblzma.so.5
            - --libiconv-lib=/projects/public/brics/dev/apps/linux-neoverse_v2/libiconv-1.18-4ok6y76vboaxcwzvxwnwhc6uaxzookdp/lib/libiconv.so.2
```

- [ ] **Step 2: Run the full test suite one final time**

```bash
go test ./... 2>&1 | grep -E '^(ok|FAIL|---)'
```

Expected: all packages `ok`, no `FAIL`

- [ ] **Step 3: Commit**

```bash
git add deploy/kubernetes/daemonset.yaml
git commit -m "deploy: add CDI env/nccl/aws-ofi-nccl flags to daemonset (Isambard)"
```
