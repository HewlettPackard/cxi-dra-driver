# CONTEXT.md — AI Session Briefing

## Project overview

This repository implements the **CXI DRA driver** — a Kubernetes Dynamic Resource Allocation node driver for HPE Slingshot CXI PCIe devices. It is the production driver exercised end-to-end by the sibling `cxi-dra-integration-tests` repository (not the reverse).

| Layer | Technology |
|-------|-----------|
| Language | Go 1.24 |
| Kubernetes API | `resource.k8s.io/v1` (DRA), `k8s.io/dynamic-resource-allocation` kubelet plugin framework |
| Hardware integration | CGO bindings to libcxi (`tags cxi` build); CDI (Container Device Interface); NRI (Node Resource Interface) via containerd |
| Deployment | Helm chart (`deploy/helm/cxi-dra-driver`, recommended) or raw manifests (`deploy/kubernetes/`) |
| CI | GitHub Actions — `build.yaml` (`go vet`, `go test ./...`, per-architecture native image builds merged into a multi-arch (amd64/arm64) manifest on ghcr) and `release-chart.yaml` (Helm chart lint/publish); `golangci-lint` is not run in CI |

## Annotated layout

```
.
├── cmd/
│   ├── cxi-dra-driver/     Driver daemon entry point (flags, subsystem wiring)
│   ├── cxi-cdi-hook/       Standalone host-installed OCI createContainer hook (ldconfig refresh)
│   └── slingshot-cni/      Standalone host CNI plugin binary (CGO-free); logic in internal/cni
├── internal/
│   ├── cxi/                Hardware layer: device discovery, capacity, HSN pairing, service CRUD (cxi/mock/stub build modes)
│   ├── driver/             DRA plugin: ClaimBinder, consumable-capacity ResourceSlice publishing, Prepare/UnprepareResourceClaims, VNI allocator, orphan-service reaper, CNI hint-file writer
│   ├── cdi/                CDI spec generation, hook + ldconfig-runtime installation, containerd drop-in config
│   ├── cni/                Slingshot CNI ADD/DEL/CHECK: hint-file consumption, L2 IPVLAN-on-HSN link management, Whereabouts IPAM delegation
│   ├── netsysfs/           Synthetic per-pod /sys/class/net tree for stock libfabric HSN discovery
│   └── nri/                NRI plugin: RunPodSandbox / CreateContainer / StopPodSandbox
├── deploy/
│   ├── helm/cxi-dra-driver/  Parameterized Helm chart (recommended install path)
│   └── kubernetes/           Raw manifests + deploy/kubernetes/test fixtures
├── docs/
│   ├── pod-lifecycle-flow.txt  Annotated ASCII flow: schedule → bind → prepare → CDI → NRI → running
│   └── cni/                    cni-plugin.md (slingshot-cni architecture + command reference), cnidra-cni-hint-contract.md (driver→CNI hint-file contract)
├── dra-architecture.d2.txt, dra-current-state.d2, dra-sequences.d2   Component + sequence diagrams
├── ARCHITECTURE.md          Consolidated component map and data-flow reference
├── Dockerfile / Dockerfile.mock   Production image (self-contained multi-stage, multi-arch; builds libcxi, the driver, cxi-cdi-hook and slingshot-cni, bundles an ldconfig runtime) and the no-hardware mock image for kind
└── Makefile                  build / build-mock / build-hook / build-cni / vet / image / image-mock / push / deploy / kind-load targets
```

## Domain model

VF service creation passes the discovered `cxi.Parent` through
`Driver.ServiceOptionsFor` and NRI to `ServiceManager.CreateService` as optional
`ServiceOptions`. `ParentDescriptor.LEMax` is the parent's LE maximum, distinct
from its reserved LE budget: children use that maximum and reserve zero LEs/TLEs.
PF calls without options retain their existing behavior.

| Concept | Meaning |
|---------|---------|
| **DRA** | Kubernetes `resource.k8s.io` API for allocating specialized hardware (here, CXI NICs) via `ResourceClaim`/`ResourceClaimTemplate` |
| **CXI** | HPE Slingshot NIC device family, exposed as `/dev/cxiN`; discovered via `/sys/class/cxi_user` |
| **ResourceSlice** | Object this driver publishes per node advertising devices; static, published once at startup. Each device has `allowMultipleAllocations` and one consumable capacity per CXI resource (`txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `acs`, `les`, `tles`, at the usable ceiling) plus topology attributes (`type`, `function_type`, `hsn_iface` on PFs, `resource.kubernetes.io/numaNode`, `resource.kubernetes.io/pciBusID`). In `--vf-only` mode the device is a parent service: it also publishes a `vfs` capacity (one VF per allocation), `pf_name`, `pf_netdev`, `parent_svc_id` and its VNI set (`vni_min`/`vni_max` or `vni_set`), has 0 LEs/TLEs, and takes the PF's NUMA node and PCI address; a `cxi-dra-vni-selector` admission policy keeps the scheduler off parents that do not cover the namespace VNI. The scheduler does the capacity accounting (needs `DRAConsumableCapacity`, Kubernetes 1.36+); the old `avail_*`/`capacity_*`/`active_namespaces` attributes are gone |
| **ClaimBinder** | Watches ResourceClaims cluster-wide as a safety net for consumption the scheduler cannot see; checks live hardware capacity and sets DRA BindingConditions — `Ready=True`, `Ready=False` (`InsufficientCapacity`, a hold), or `Unschedulable=True` (`DeviceAtCapacity`) |
| **PrepareResourceClaims / UnprepareResourceClaims** | Kubelet plugin hooks; prepare reads the namespace's VNI annotation, stores a `PodAllocation`, returns CDI device IDs |
| **VNI (namespace-scoped)** | One VNI per namespace, assigned by a leader-elected `vniallocator` and stored as the `cxi.slingshot.hpe.com/vni` namespace annotation — pods never request their own VNI |
| **CDI** | Injects `/dev/cxiN` and the cgroup BPF device allowance per device, plus one shared `env` spec attached to every CXI device (kdreg2/gdrdrv/xpmem nodes, `/dev/shm`; with `--enable-cdi-env`, host libs bind-mounted at `/host/env/<basename>` + env vars + the `cxi-cdi-hook` ldcache hook). NCCL/aws-ofi-nccl are injected by k8s-modules, not this driver |
| **NRI** | `RunPodSandbox` creates the CXI service + synthetic sysfs tree (VF entries are named after the pod-side `cxiN` interface while metadata comes from the host VF netdev); `CreateContainer` injects `SLINGSHOT_DEVICES`/`SLINGSHOT_VNIS`/`SLINGSHOT_SVC_IDS` env vars; `StopPodSandbox` tears both down |
| **Slingshot CNI (`slingshot-cni`)** | Host CNI plugin (`cmd/slingshot-cni` + `internal/cni`), bundled in the driver image and **optionally** installed onto nodes by the driver (`--cni-install-dir`, Helm `cni.install=true`, default off — `hostNetwork` remains first-class), exec'd by containerd/Multus. `ADD` creates one **L2 IPVLAN** sub-interface per claimed device on the host `hsn_iface`, moves it into the pod netns as the optional `ifname` (otherwise `hsn_iface`, fallback `<name>_inet`; base name at most 10 bytes), and delegates addressing to **Whereabouts** IPAM; `DEL` releases and removes idempotently. Owns L3 addressing only — complementary to the NRI sysfs injection, which is what libfabric's HSN discovery depends on |
| **CNI hint file** | `<--cni-hints-dir>/<podUID>.json`, written atomically by `PrepareResourceClaims` (`internal/driver/cnihints.go`), one `{device, hsn_iface, vni}` per PF device; VF entries also include `schema_version: 1` and `ifname: cxiN`. `hsn_iface` is always the host link used as the CNI parent. The CNI accepts schema versions through `1`; the PF writer remains unversioned and byte-compatible. The only handoff from the driver daemon to the CNI process; ordering vs. CNI `ADD` is guaranteed by kubelet (prepare returns before `RunPodSandbox`). See `docs/cni/cnidra-cni-hint-contract.md` |
| **Reaper** | Backstop that destroys orphaned CXI services (pod netns gone for several consecutive cycles) left behind by pods that didn't tear down cleanly; runs every `--resource-refresh-interval` and never touches blessed (PF mode only; ignored with `--vf-only`, where svc_ids are per-VF) or kernel-system services (including the VF Ethernet child), and in VF mode warns about VF children it cannot attribute to a pod |
| **Build tags: `cxi` / mock / stub** | `internal/cxi` has three implementations — `_cgo.go` (real libcxi ioctls, `tags cxi`), `mock.go` (`--mock-devices N`, no hardware), `_stub.go` (no-op, default non-cxi build) — so the driver, its tests, and `build-mock` all work without libcxi |
| **Parent device / `vfs` capacity** | In `--vf-only` mode the DRA device is a **parent service**, `<pf>-svc<id>`, not a VF. Its static ceiling is the parent's `res` minus one Ethernet system child per VF (LEs/TLEs published as 0), shared by every VF assigned to it; the synthetic `vfs` capacity (value = VFs assigned, one per allocation) keeps VFs exclusive. Pods never pick a VF: `Prepare` assigns the lowest free one under a per-parent lock. Enabled with `--vf-only` (Helm `driver.vfOnly`); `--system-svc-ids` is ignored there |
| **PF / VF mode (`--vf-only`)** | `cxi.Discover` classifies each `cxiN` as an SR-IOV VF (`device/physfn` present) or a PF, and never publishes both: the default publishes PFs and skips VFs; `--vf-only` publishes VFs and skips PFs before any libcxi read. Each discovered VF has `FunctionType`, `PFName`, `VFIndex`, and its assigned `ParentSvcID` read from `/sys/class/cxi/<pf>/vf/<N>/svc_id`; VFs with unreadable, malformed, or non-positive IDs, or parents without resource limits, are skipped and logged. VFs set `PodIfName` to their `cxiN` name; PFs leave it empty. `cxi.Parent` / `cxi.Parents()` group VFs by (PF, parent service ID). `ReadParentBudget`, `ReadParentRemaining`, and `ReadVFSystemReservation` expose read-only parent and VF resource data. In VF mode the driver publishes **parents**, not VFs (`publishedDevices`), and services are created on VFs (`programmableDevices`): Prepare maps each allocation result to the lowest free VF of its parent under a per-parent lock (`internal/driver/vfselect.go`), records it as `PodAllocation{DeviceName: vf, ParentName, ShareID}`, and rebuilds VF occupancy at startup from hint files, live claims and VF service lists (`recoverVFState`) |



## Architectural patterns and anti-patterns

- **Do** read the namespace's VNI annotation in `PrepareResourceClaims` rather than trusting a per-claim VNI — see `internal/driver/vniallocator.go`.
- **Do** treat `le_pools: 0` / `tle_pools: 0` as "shared best-effort pool", not "zero capacity" — see README § Resource field reference.
- **Do not** build the real (`tags cxi`) binary off a machine without libcxi headers — it CGO-links against `libcxi.so`. The production `Dockerfile` builds libcxi from pinned SHS sources itself, so `make image` works anywhere; for a bare `make build`, use a Slingshot node or `make build-mock`.
- **Do not** let the `Dockerfile`'s libcxi pins (`CASSINI_HEADERS_REF`, `CXI_DRIVER_REF`, `LIBCXI_REF`) drift from the SHS release on the nodes — a userspace/kernel skew surfaces at runtime as `cxil_*` errors, not at build time.
- **Do not** reintroduce `avail_*` CEL selectors, per-namespace device occupancy, or a periodic `ResourceSlice` republish — capacity is consumable and static; the scheduler accounts for it and `ClaimBinder` is only the live-hardware safety net.
- **Do not** add NCCL/aws-ofi-nccl CDI profiles back — k8s-modules injects them; the shared `env` spec is always attached to every device.
- **Do not** put `hsn_iface` on a parent device — that attribute promises "the netdev your pod gets", which a parent (spanning several VF netdevs) cannot promise; only PF devices publish it. VF mode names the pod interface after the chosen VF's `cxiN` via the hint's `ifname`.
- **Do not** publish VFs as devices on the current model — consumable capacity is strictly per device, so a budget shared by several VF devices cannot be expressed. The parent is the device and `vfs` keeps VFs exclusive; publishing VFs against a shared parent counter is the planned KEP-5941 follow-up, and the published/programmable split (`internal/driver/views.go`, `resourceslice.go`) keeps that flip local.
- **Do not** key budget, pending grants or capacity accounting by VF — the shared budget is the parent's, and the allocated device name (the parent) is already the key the scheduler, pending map and `ClaimBinder` use. Only NRI/reaper service state is keyed (VF name, svc ID), because service IDs are local to each VF.
- **Do not** rely on an `Unschedulable` bounce for a static condition (such as a parent whose VNI set misses the namespace VNI): the first-fit allocator does not exclude a device that just failed binding, so the claim loops allocate → bounce → deallocate on the same parent. Decide static conditions at admission (the `cxi-dra-vni-selector` policy); keep `Unschedulable` for transient ones.
- **Do not** let the driver create, resize or destroy parents or VFs, and do not accept a parent without resource limits — the kernel then enforces no per-parent budget. Provisioning is the administrator's job, done before the driver starts.
- **Do** keep `deploy/helm/` and `deploy/kubernetes/` in sync — e.g. the VNI allocator's Lease is named `<driverName>-vni-allocator-leader`, and the raw `ClusterRole` must name that same Lease.
- **Do not** assume this repo is standalone — the sibling `cxi-dra-integration-tests` repository is the only thing that exercises `internal/nri` and the real `internal/cxi` hardware path end-to-end; this repo's own CI cannot verify pod-lifecycle behavior.
- **Do not** treat `slingshot-cni`'s IPVLAN interface as a replacement for `internal/netsysfs` (or vice versa) — libfabric reads the synthetic sysfs, IP consumers use the IPVLAN netdev; both run. Inside the container the IPVLAN link shows under `ip link` but **not** under the overmounted `/sys/class/net`; that is expected.
- **Do not** share a Go type between the hint-file writer (`internal/driver.CNIHint`) and reader (`internal/cni.Hint`) — they are different processes that agree only on the JSON schema.
- **Do not** call the IPAM delegate with the parent invocation's `CNI_IFNAME` — Whereabouts keys reservations on `(containerID, ifname)`, so a multi-device pod needs `CNI_IFNAME` overridden per interface (`internal/cni/ipam.go`), or reservations collide.
- **Do not** let CNI `DEL` delete anything but IPVLAN links, or fail on a missing hint file / vanished netns — DEL must be idempotent and must never wedge teardown of a pod that is going away regardless.
- **Do not** put netlink calls outside the `linkManager` seam in `internal/cni` — `network_linux.go` / `network_other.go` mirror the `_linux`/`_other` split in `internal/netsysfs`, keeping the orchestration unit-testable on any platform.

## Build, test, and run commands

```bash
make build          # CGO_ENABLED=1, tags=cxi — real hardware build; needs libcxi headers on the build host (`make image` builds libcxi itself)
make build-mock      # CGO_ENABLED=0 — no libcxi, for kind/local dev
make build-cni       # CGO_ENABLED=0 — slingshot-cni host CNI plugin binary; honours GOARCH; the image build compiles it itself (`make image` does not depend on this target)
go test ./...        # unit tests (also run in CI)
golangci-lint run    # lint (local only — not run in CI; use GOOS=linux on a non-Linux workstation)
make vet             # go vet ./...
make image / push    # production container build via Dockerfile (self-contained; nothing to stage in bin/) / push to $(IMAGE):$(TAG)
make image-mock      # no-hardware image via Dockerfile.mock (for kind)
make deploy          # kubectl apply of raw manifests (clusterrole, deviceclass, admission-policy, daemonset)
make kind-load / kind-reload   # load a locally-built image into a kind cluster and restart the DaemonSet
```

See [README.md](README.md) for the full Helm-based install path and per-value reference.

## Testing strategy

- Unit tests (`go test ./...`, stdlib `testing` package only — no third-party assertion library) live alongside their packages: `internal/cdi` (3 files), `internal/driver` (9 files), `internal/cni` (6 files), `internal/netsysfs` (1 file), `cmd/cxi-cdi-hook` (1 file).
- `internal/cni` is tested in three layers: `cmd_test.go` drives `ADD`/`DEL`/`CHECK` against in-memory `linkManager`/`ipamDelegate` fakes (any platform); `ipam_test.go` execs a fake `whereabouts` binary built from `testdata/fakewhereabouts` to verify the real CNI delegate contract (any platform, needs `go` on `PATH`); `network_linux_test.go` drives the real netlink code against a throwaway named netns and a dummy parent (Linux **as root** only — skips otherwise; run it via a privileged container from a non-Linux workstation, see `docs/cni/cni-plugin.md`).
- `internal/nri` has no unit tests, and `internal/cxi`'s libcxi paths are hardware-dependent (exercised via `mock.go`/`--mock-devices` at the integration level); `internal/cxi`'s sysfs discovery and HSN pairing are unit-tested against a fake sysfs tree. `internal/nri`'s plugin lifecycle is integration-tested against a live cluster by the sibling `cxi-dra-integration-tests` repo.
- Mocking: `internal/cxi/mock.go` and `service_mock.go` provide synthetic devices and no-op service CRUD for tests and `--mock-devices` kind/dev runs — reuse these instead of hand-rolling fakes.
- CI (`.github/workflows/build.yaml`) runs `go vet` and `go test ./...` on every PR, then builds the image natively per architecture (it does not run `golangci-lint`); it does not and cannot reach a live cluster.

## Known gaps and constraints

- `internal/cxi` and `internal/nri` have zero unit test coverage; both are only exercised by the sibling integration-test suite against a live cluster.
- `slingshot-cni` against a **real** HSN parent and a **real** Whereabouts install is not covered by any automated test here — the root-gated netns tests use a dummy parent and a fake IPAM binary. Live-cluster verification via `cxi-dra-integration-tests` is required.
- `slingshot-cni` does not persist which in-pod name (`<hsn_iface>` vs `<hsn_iface>_inet`) `ADD` chose; `DEL`/`CHECK` re-derive it by probing. Ambiguous only if both names exist as IPVLAN links from a prior life.
- No CodeQL/SAST scanning runs in CI, and `golangci-lint` is not run in CI either; there is no `.golangci.yml`, `.github/CODEOWNERS`, or `.gitleaks.toml` in the repo. `GOOS=linux golangci-lint run` (default linters) is clean; a plain run on a non-Linux workstation still reports two `staticcheck` SA4023 findings at `internal/nri/plugin.go:471` because `netsysfs.PodInterfaces` is an always-erroring stub there — a false positive, so lint with `GOOS=linux`.
- This file, `AGENTS.md`, `ARCHITECTURE.md`, `.github/copilot-instructions.md`, and the `.github/instructions/*.md`/`.github/agents/*.md` files were added as part of an AI-readiness pass — keep them in sync with `README.md` as the source of truth for commands and flags.
