# Copilot Instructions — cxi-dra-driver

> Trust these instructions; they reflect the current state of this repository. Only explore further if something here is missing or appears stale.

## What this is

A Kubernetes Dynamic Resource Allocation (DRA) node driver for HPE Slingshot CXI PCIe devices. It runs as a per-node DaemonSet: discovers CXI devices via sysfs, publishes them as a `ResourceSlice`, programs a scoped CXI service (VNI + netns) via libcxi, and injects each device into its container via CDI and NRI. Full domain briefing: **read [CONTEXT.md](../CONTEXT.md) first.**

Related repository: **cxi-dra-integration-tests** exercises this driver end-to-end against a live cluster; it is not part of this workspace's build and does not need to be present to work on this repo.

## Folder structure

| Path | Role |
|------|------|
| `cmd/cxi-dra-driver/` | Driver daemon entry point — flags, wiring of all subsystems |
| `cmd/cxi-cdi-hook/` | Standalone OCI createContainer hook binary, installed on hosts; refreshes the ldconfig cache |
| `cmd/slingshot-cni/` | Host CNI plugin binary (CGO-free, `make build-cni`), bundled in the driver image and optionally installed onto nodes by the driver (`--cni-install-dir`); exec'd by containerd/Multus; thin wrapper over `internal/cni` |
| `internal/cxi/` | Hardware abstraction: device discovery, capacity, HSN pairing, CXI service CRUD (three build modes: `cxi` CGO, mock, stub) — see [internal/cxi/README.md](../internal/cxi/README.md) |
| `internal/driver/` | Core DRA plugin: `ClaimBinder`, consumable-capacity model + static `ResourceSlice` publishing, `Prepare`/`UnprepareResourceClaims`, namespace VNI allocator, orphan-service reaper, CNI hint-file writer — see [internal/driver/README.md](../internal/driver/README.md) |
| `internal/cdi/` | CDI spec generation (per-device spec + the shared `env` spec attached to every device), `cxi-cdi-hook` + ldconfig-runtime installation, containerd drop-in config |
| `internal/cni/` | Slingshot CNI `ADD`/`DEL`/`CHECK`: reads the driver's per-pod hint file, creates one L2 IPVLAN sub-interface per claimed device on its host HSN, delegates addressing to Whereabouts IPAM — see [docs/cni/cni-plugin.md](../docs/cni/cni-plugin.md) |
| `internal/netsysfs/` | Synthetic per-pod `/sys/class/net` tree so stock libfabric discovers the HSN interface |
| `internal/nri/` | NRI plugin: `RunPodSandbox`/`CreateContainer`/`StopPodSandbox` hooks |
| `deploy/helm/` | Recommended install path — parameterized Helm chart |
| `deploy/kubernetes/` | Raw manifests (DaemonSet, ClusterRole, DeviceClass, MutatingAdmissionPolicy) + test fixtures |
| `.github/` | AI-context files, CI workflows, issue/PR templates, dependabot config |

## Architecture

Scheduler (consumable-capacity accounting against the static `ResourceSlice`; the admission policy first converts legacy opaque config into `exactly.capacity.requests`) → `ClaimBinder` (live-hardware safety net, sets BindingConditions: `Ready=True` / `Ready=False` / `Unschedulable` `DeviceAtCapacity`) → kubelet `PrepareResourceClaims` (reads namespace VNI, stores allocation, writes the per-pod CNI hint file, returns CDI IDs) → CDI (`/dev/cxiN` + cgroup BPF) → CNI `ADD` (`slingshot-cni`, separate host process: L2 IPVLAN on the HSN + Whereabouts address, per device) → NRI `RunPodSandbox` (creates CXI service, builds synthetic sysfs) → NRI `CreateContainer` (injects `SLINGSHOT_*` env, bind-mounts sysfs). The CNI interface and the synthetic sysfs are complementary: libfabric discovers the HSN via the sysfs, IP consumers use the IPVLAN netdev. See [ARCHITECTURE.md](../ARCHITECTURE.md) for the full component map and data-flow narrative, [docs/pod-lifecycle-flow.txt](../docs/pod-lifecycle-flow.txt) for the annotated step-by-step, and the `dra-*.d2*` files for diagrams.

## Build commands

```bash
make build         # CGO_ENABLED=1, tags=cxi — requires libcxi headers on the build host (a Slingshot node); `make image` builds libcxi in-container instead
make build-mock    # CGO_ENABLED=0 — no libcxi required, for kind/local dev
make build-hook    # cxi-cdi-hook is always CGO-free regardless of driver build mode
make build-cni     # slingshot-cni is always CGO-free; honours GOARCH (the image builds it itself)
make vet           # go vet ./...
make image         # production container build: self-contained multi-stage Dockerfile (builds libcxi from pinned SHS sources, driver, cxi-cdi-hook, slingshot-cni; Debian runtime; amd64/arm64)
make image-mock    # no-hardware image from Dockerfile.mock (used by make kind-load / deploy/kind/setup.sh)
```

## Test commands

```bash
go test ./...              # unit tests across internal/cdi, internal/cni, internal/driver, internal/netsysfs, cmd/cxi-cdi-hook
golangci-lint run          # lint; not run in CI
```

CI (`.github/workflows/build.yaml`) runs `go vet` and `go test ./...`, then builds the image natively per architecture and merges a multi-arch manifest to ghcr; it does **not** run `golangci-lint`. `.github/workflows/release-chart.yaml` lints/templates and publishes the Helm chart. `internal/cxi` and `internal/nri` have no unit tests (hardware/cluster-dependent; exercised via `mock.go` and the sibling `cxi-dra-integration-tests` repo). `internal/cni`'s real-netlink tests (`network_linux_test.go`) need Linux + root and skip otherwise — run them via a privileged container from a non-Linux workstation (see [docs/cni/cni-plugin.md](../docs/cni/cni-plugin.md) § Testing); its orchestration and IPAM-delegate tests run anywhere.

## Critical anti-patterns

- **Do not** build the real (`tags cxi`) binary anywhere without libcxi headers — it links against `libcxi.so` via CGO. Use `build-mock` elsewhere, or `make image`, which builds libcxi itself.
- **Do not** let the `Dockerfile`'s libcxi pins (`CASSINI_HEADERS_REF`, `CXI_DRIVER_REF`, `LIBCXI_REF`) drift from the SHS release on the nodes — a userspace/kernel skew surfaces at runtime as `cxil_*` errors, not at build time; nothing needs staging in `bin/` before `make image`.
- **Do not** reintroduce `avail_*`/`capacity_*` attributes, per-namespace device occupancy, or periodic `ResourceSlice` republishing — the slice is static with consumable capacity, and SHS 14.0.1 lifted the one-namespace-per-device constraint.
- **Do not** add NCCL/aws-ofi-nccl CDI profiles back — they are injected by k8s-modules; the shared `env` spec is always attached to every device.
- **Do not** assume `le_pools: 0` / `tle_pools: 0` means "no capacity" — it maps the service to a shared default kernel pool on a best-effort basis, not "unavailable" (see README § Resource field reference).
- **Do not** hand-roll a VNI per pod — VNIs are namespace-scoped and assigned once by the leader-elected namespace allocator (`internal/driver/vniallocator.go`); pods read the namespace's `cxi.slingshot.hpe.com/vni` annotation.
- **Do not** on SR-IOV VF mode (`--vf-only`) publish VFs as devices, key budget by VF, publish `hsn_iface` on a parent, or rely on an `Unschedulable` bounce for a static condition such as VNI coverage (the first-fit allocator re-picks the same parent; the `cxi-dra-vni-selector` admission policy decides it). The parent service is the device; `vfs` keeps VFs exclusive; the VF is chosen at Prepare under the per-parent lock.
- **Do not** treat `slingshot-cni`'s IPVLAN interface and `internal/netsysfs`'s synthetic sysfs as alternatives — both run; libfabric needs the sysfs, IP consumers need the netdev. Keep netlink calls behind `internal/cni`'s `linkManager` seam (`network_linux.go`/`network_other.go`), override `CNI_IFNAME` per device when delegating to Whereabouts, and keep CNI `DEL` idempotent (missing hint/netns/link = success; only IPVLAN links are ever deleted).

## Environment variables / flags

Driver flags are defined in `cmd/cxi-dra-driver/main.go`: `--node-name`/`NODE_NAME`, `--pod-uid`/`POD_UID`, `--pod-namespace`/`POD_NAMESPACE` (required); `--driver-name`, `--vni-range-start`/`-end`, `--resource-refresh-interval` (orphan-service reaper period), `--enable-cdi-env`, `--env-config`, `--cdi-hook-*`, `--cdi-ldconfig-*` (`--cdi-ldconfig-src`/`-linker-src`/`-libc-src` default to `/ldconfig-runtime/{ldconfig,ld.so,libc.so.6}`, bundled in the image), `--mock-devices`, `--system-svc-ids` (ignored with `--vf-only`), `--vf-only` (SR-IOV VF mode: publish one device per parent service instead of PFs, never mixed; Helm `driver.vfOnly`), `--cni-hints-dir` (default `/var/run/cxi/net-hints`; must match the `hintsDir` in the `slingshot-cni` network config), `--cni-install-dir` (default empty = disabled; opt-in host install of the bundled `slingshot-cni` into e.g. `/opt/cni/bin`, Helm `cni.install`), `--cni-src` (default `/slingshot-cni`). Full table in [README.md](../README.md#configuration-flags). `slingshot-cni` itself takes no flags — it is configured via its CNI stdin config (`ipam.type` required; `hintsDir`, `retryInterval`, `retryTimeout` optional), see [docs/cni/cni-plugin.md](../docs/cni/cni-plugin.md).

## External dependencies

Kubernetes 1.36+ (stable `resource.k8s.io/v1`; DRA consumable capacity — the `DRAConsumableCapacity` feature gate — is beta and on by default from 1.36, enable it plus `DRADeviceBindingConditions` on 1.34/1.35; `MutatingAdmissionPolicy` needs 1.32+), containerd ≥2.0 with CDI + NRI enabled, and libcxi (built from pinned SHS sources inside the `Dockerfile`; the host needs only the matching `cxi` kernel driver) for the real build. `slingshot-cni` additionally needs the **Whereabouts** IPAM plugin binary on the node's `CNI_PATH` plus its cluster-side install (CRDs, `ip-reconciler`), and Multus (or equivalent) to attach the `slingshot-cni` network to pods; Go-side it depends on `github.com/vishvananda/netlink`. No sibling-repo dependency in this direction — `cxi-dra-integration-tests` depends on this repo, not the reverse.

## When to use which agent

| Task | Use |
|------|-----|
| Add/modify a driver flag, package, or fix a bug | `coding` agent |
| Self-review before opening a PR, or triage an open PR's diff | `code-review` agent |
| Finalize a change and draft the PR | `pr-preparation` agent, `pr-description` prompt |
| Update README/CONTEXT/ARCHITECTURE/AGENTS after a code change | `documentation` agent |
| Triage a stuck claim, stuck pod, or leaked CXI service | `debug-driver` prompt |
| Look up a DRA/CXI/CDI/NRI term | `cxi-dra-glossary` skill |

## Copilot customization index

| Type | Files |
|------|-------|
| Instructions | `.github/instructions/build-test.instructions.md`, `code-style.instructions.md`, `domain-patterns.instructions.md` |
| Prompts | `.github/prompts/pr-description.prompt.md`, `debug-driver.prompt.md` |
| Skills | `.github/skills/cxi-dra-glossary/` |
| Agents | `.github/agents/coding.agent.md`, `pr-preparation.agent.md`, `documentation.agent.md`, `code-review.agent.md` |

See [AGENTS.md](../AGENTS.md) for the full, bidirectional index.

## Generated / auto-modified files

None hand-generated in source control. `bin/` (build output) is a gitignored build artifact, not committed.

## Security

Report vulnerabilities per [SECURITY.md](../SECURITY.md). Treat `internal/driver`, `internal/cxi`, `internal/cdi` host-install paths, and `.github/` as security-relevant; the repo has no `CODEOWNERS` file.

## Maintenance matrix

| When you change... | Also update... |
|---------------------|----------------|
| A driver CLI flag or its default | `CONTEXT.md` env/flag table, this file's "Environment variables / flags" |
| A `make` target | `README.md`, this file's "Build commands" |
| A CXI/DRA domain gotcha (resource semantics, build-tag requirement) | `CONTEXT.md` anti-patterns, `.github/instructions/domain-patterns.instructions.md` |
| Add/remove/rename an agent, prompt, instruction, or skill | This file's customization index, `AGENTS.md` |
| `internal/driver` public API used across packages | `CONTEXT.md` domain model, `ARCHITECTURE.md` component map |
| A notable user-facing change | `CHANGELOG.md` |
| RBAC, `DeviceClass`, or admission-policy content (incl. the VNI allocator Lease name `<driverName>-vni-allocator-leader`) | both `deploy/helm/cxi-dra-driver/` and `deploy/kubernetes/` (and `deploy/kind/test-vni.sh`) |
| `Dockerfile` libcxi pins, bundled ldconfig runtime, or build workflows | `README.md` § Building, `ARCHITECTURE.md` § CI/CD pipeline |
