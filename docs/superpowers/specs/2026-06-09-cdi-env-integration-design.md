# CDI Environment Integration Design

**Date:** 2026-06-09
**Branch:** cdi-env-integration (to be created)
**Status:** Approved

## Background

The DRA driver currently generates a minimal CDI spec per CXI device — just the `/dev/cxiN` device node and a sysfs bind-mount. To run CXI-based workloads (e.g. MPI with libfabric, NCCL over Slingshot), containers also need:

- Additional device nodes: `/dev/xpmem`, `/dev/gdrdrv`
- The full libcxi/libfabric library stack bind-mounted from the host
- Optionally: NCCL libraries and the AWS OFI NCCL plugin with its tuning env vars

The reference configuration for these requirements lives in `tmp/CDI/` (three CDI yaml files used in the existing podman setup). This work integrates that configuration into the DRA driver programmatically.

`tmp/CDI/` is gitignored (`/tmp/` in `.gitignore`) and remains a reference only.

## Approach

**Separate "environment" CDI spec files** written at driver startup alongside the existing per-device specs. Specifically:

- `<cdiRoot>/<driverName>-env.json` — always written; contains shared device nodes, library mounts, and ldcache hook
- `<cdiRoot>/<driverName>-nccl.json` — written only when `--nccl-prefix` is set
- `<cdiRoot>/<driverName>-aws-ofi-nccl.json` — written only when `--aws-ofi-nccl-prefix` is set

`PrepareResourceClaims` appends the corresponding CDI device IDs to every allocation so kubelet injects these environments into the container alongside the per-device ID.

This approach avoids duplicate mounts (a problem if the monolithic approach were used with multi-device allocations) and maps cleanly onto a future upgrade path where NCCL/OFI become selectable DeviceClasses.

## New CLI Flags

All new flags added to `cmd/cxi-dra-driver/main.go`.

### Base CXI environment

| Flag | Description | Default |
|------|-------------|---------|
| `--libfabric-lib` | Host path to `libfabric.so.1` | `""` |

If omitted, the libfabric mount is skipped and a warning is logged. The env spec is still written.

### NCCL (optional)

| Flag | Description |
|------|-------------|
| `--nccl-prefix` | Path to NCCL Spack install dir; mounted whole into `/host/nccl` |

Env spec written only when this flag is set.

### AWS OFI NCCL (optional)

| Flag | Description |
|------|-------------|
| `--aws-ofi-nccl-prefix` | Path to aws-ofi-nccl Spack dir; `lib/libnccl-net.so` extracted from it |
| `--hwloc-lib` | Host path to `libhwloc.so.15` |
| `--pciaccess-lib` | Host path to `libpciaccess.so.0` |
| `--libxml2-lib` | Host path to `libxml2.so.2` |
| `--libatomic-lib` | Host path to `libatomic.so.1` (gcc-runtime) |
| `--liblzma-lib` | Host path to `liblzma.so.5` |
| `--libiconv-lib` | Host path to `libiconv.so.2` |

Spec written only when `--aws-ofi-nccl-prefix` is set.

## CDI Package Changes

### New file: `internal/cdi/env.go`

```go
type CDIEnvConfig struct {
    LibfabricLib       string

    NCCLPrefix         string

    AWSofiNCCLPrefix   string
    HwlocLib           string
    PciaccessLib       string
    Libxml2Lib         string
    LibatomicLib       string
    LiblzmaLib         string
    LibiconvLib        string
}

func WriteEnvSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error
func WriteNCCLSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error
func WriteAWSofiNCCLSpec(cdiRoot, driverName string, cfg CDIEnvConfig) error

func RemoveEnvSpec(cdiRoot, driverName string) error
func RemoveNCCLSpec(cdiRoot, driverName string) error
func RemoveAWSofiNCCLSpec(cdiRoot, driverName string) error
```

CDI device IDs exposed as constants/helpers:

```go
func EnvDeviceID(driverName string) string        // "<driverName>/env=env"
func NCCLDeviceID(driverName string) string       // "<driverName>/nccl=nccl"
func AWSofiNCCLDeviceID(driverName string) string // "<driverName>/aws-ofi-nccl=aws-ofi-nccl"
```

### `WriteEnvSpec` contents

Device `env`, top-level `containerEdits`:

- **DeviceNodes:** `/dev/xpmem`, `/dev/gdrdrv` — statted at write time; each skipped individually if not present on the host (non-fatal, debug log)
- **Mounts:** `/dev/shm`, all standard system libs from `cxi.yaml` (`libcxi.so.1`, ~28 others at `/usr/lib64/`), plus libfabric if `--libfabric-lib` is set
- **Hook:** `nvidia-cdi-hook update-ldcache --folder /usr/lib64` (createContainer)

### `WriteNCCLSpec` contents

Device `nccl`, top-level `containerEdits`:

- **Mounts:** `<nccl-prefix>` → `/host/nccl` (bind, ro)
- **Hook:** `nvidia-cdi-hook update-ldcache --folder /host/nccl/lib` (createContainer)

### `WriteAWSofiNCCLSpec` contents

Device `aws-ofi-nccl`, top-level `containerEdits`:

- **Mounts:** `<prefix>/lib/libnccl-net.so` → `/usr/lib/libnccl-net.so`, `/usr/lib64/libgdrapi.so.2.5` → `/usr/lib/libgdrapi.so`, all 6 dependency `.so` mounts
- **Env vars:** All NCCL and `FI_CXI_*` tuning constants from `aws-ofi-nccl.yaml` (hardcoded — these are tuning values, not paths)
- **Hook:** `nvidia-cdi-hook update-ldcache --folder /usr/lib --folder /usr/lib64` (createContainer)

## Driver Changes

### `internal/driver/driver.go`

- `Driver` struct gains `cdiEnvConfig cdi.CDIEnvConfig`
- `Run()`: after per-device `cdi.WriteSpec` calls, call `cdi.WriteEnvSpec`; conditionally call `cdi.WriteNCCLSpec` and `cdi.WriteAWSofiNCCLSpec`
- No cleanup on shutdown — specs are node-lifetime objects and the driver leaves the JSON files in place (consistent with existing per-device CDI specs).

### `internal/driver/prepare.go`

When building CDI device IDs for a claim, append after the existing per-device ID:
- `cdi.EnvDeviceID(driverName)` — always
- `cdi.NCCLDeviceID(driverName)` — if `cfg.NCCLPrefix != ""`
- `cdi.AWSofiNCCLDeviceID(driverName)` — if `cfg.AWSofiNCCLPrefix != ""`

### `internal/driver/unprepare.go`

No changes — env/nccl/aws-ofi-nccl specs are node-lifetime objects, not per-allocation.

## Deployment

### `deploy/kubernetes/daemonset.yaml`

New flags added to the driver container args, populated with Isambard values:

```yaml
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

### `deploy/kind/daemonset-mock.yaml`

No changes — mock setup runs without real hardware; env spec gracefully skips absent device nodes.

## Future: Option C (selectable capability classes)

The three spec files produced by this design map directly onto future DeviceClasses:
- `env` → base CXI device class (already exists as `cxi-device`)
- `nccl` → new `cxi-nccl` DeviceClass
- `aws-ofi-nccl` → new `cxi-aws-ofi-nccl` DeviceClass

When option C is implemented, the env spec IDs injected in `prepare.go` are replaced by CDI IDs driven by the user's ResourceClaim selections. No structural changes to the spec files themselves are needed.
