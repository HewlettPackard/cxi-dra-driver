# CONTEXT.md — AI Session Briefing

## Project overview

This repository implements the **CXI DRA driver** — a Kubernetes Dynamic Resource Allocation node driver for HPE Slingshot CXI PCIe devices. It is the production driver exercised end-to-end by the sibling `cxi-dra-integration-tests` repository (not the reverse).

| Layer | Technology |
|-------|-----------|
| Language | Go 1.24 |
| Kubernetes API | `resource.k8s.io/v1` (DRA), `k8s.io/dynamic-resource-allocation` kubelet plugin framework |
| Hardware integration | CGO bindings to libcxi (`tags cxi` build); CDI (Container Device Interface); NRI (Node Resource Interface) via containerd |
| Deployment | Helm chart (`deploy/helm/cxi-dra-driver`, recommended) or raw manifests (`deploy/kubernetes/`) |
| CI | GitHub Actions (`.github/workflows/build.yaml`) — `go vet`, `go test ./...`, `golangci-lint`, multi-arch (amd64/arm64) image build/push |

## Annotated layout

```
.
├── cmd/
│   ├── cxi-dra-driver/     Driver daemon entry point (flags, subsystem wiring)
│   ├── cxi-cdi-hook/       Standalone host-installed OCI createContainer hook (ldconfig refresh)
│   └── slingshot-cni/      Standalone host CNI plugin binary (CGO-free); logic in internal/cni
├── internal/
│   ├── cxi/                Hardware layer: device discovery, capacity, HSN pairing, service CRUD (cxi/mock/stub build modes)
│   ├── driver/             DRA plugin: ClaimBinder, ResourceSlice publishing, Prepare/UnprepareResourceClaims, VNI allocator, reconciler, reaper, CNI hint-file writer
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
├── Dockerfile / Dockerfile.ci   Production (FROM scratch) and multi-arch CI build images
└── Makefile                  build / build-mock / build-hook / build-cni / vet / image / push / deploy / kind-load targets
```

## Domain model

| Concept | Meaning |
|---------|---------|
| **DRA** | Kubernetes `resource.k8s.io` API for allocating specialized hardware (here, CXI NICs) via `ResourceClaim`/`ResourceClaimTemplate` |
| **CXI** | HPE Slingshot NIC device family, exposed as `/dev/cxiN`; discovered via `/sys/class/cxi_user` |
| **ResourceSlice** | Object this driver publishes per node advertising devices + capacity attributes (`avail_txqs`, `avail_tles`, `active_namespaces`, …) |
| **ClaimBinder** | Watches ResourceClaims cluster-wide, performs a real fit-check, sets DRA BindingConditions (`Ready`/`Unschedulable`) |
| **PrepareResourceClaims / UnprepareResourceClaims** | Kubelet plugin hooks; prepare reads the namespace's VNI annotation, stores a `PodAllocation`, returns CDI device IDs |
| **VNI (namespace-scoped)** | One VNI per namespace, assigned by a leader-elected `vniallocator` and stored as the `cxi.slingshot.hpe.com/vni` namespace annotation — pods never request their own VNI |
| **CDI** | Injects `/dev/cxiN`, optional env/NCCL/AWS-OFI-NCCL library specs, and cgroup BPF device allowance |
| **NRI** | `RunPodSandbox` creates the CXI service + synthetic sysfs tree; `CreateContainer` injects `SLINGSHOT_DEVICES`/`SLINGSHOT_VNIS`/`SLINGSHOT_SVC_IDS` env vars; `StopPodSandbox` tears both down |
| **Slingshot CNI (`slingshot-cni`)** | Separate host CNI plugin (`cmd/slingshot-cni` + `internal/cni`), exec'd by containerd/Multus. `ADD` creates one **L2 IPVLAN** sub-interface per claimed device on the host `hsn_iface`, moves it into the pod netns as `<hsn_iface>` (fallback `<hsn_iface>_inet`), and delegates addressing to **Whereabouts** IPAM; `DEL` releases and removes idempotently. Owns L3 addressing only — complementary to the NRI sysfs injection, which is what libfabric's HSN discovery depends on |
| **CNI hint file** | `<--cni-hints-dir>/<podUID>.json`, written atomically by `PrepareResourceClaims` (`internal/driver/cnihints.go`), one `{device, hsn_iface, vni}` per claimed device. The only handoff from the driver daemon to the CNI process; ordering vs. CNI `ADD` is guaranteed by kubelet (prepare returns before `RunPodSandbox`). See `docs/cni/cnidra-cni-hint-contract.md` |
| **Reconciler** | Poll-driven safety net rebuilding in-memory occupancy from live CXI services (covers driver restarts, missed events) |
| **Reaper** | Deletes orphaned CXI services left behind by pods that didn't tear down cleanly |
| **Build tags: `cxi` / mock / stub** | `internal/cxi` has three implementations — `_cgo.go` (real libcxi ioctls, `tags cxi`), `mock.go` (`--mock-devices N`, no hardware), `_stub.go` (no-op, default non-cxi build) — so the driver, its tests, and `build-mock` all work without libcxi |

## Architectural patterns and anti-patterns

- **Do** read the namespace's VNI annotation in `PrepareResourceClaims` rather than trusting a per-claim VNI — see `internal/driver/vniallocator.go`.
- **Do** treat `le_pools: 0` / `tle_pools: 0` as "shared best-effort pool", not "zero capacity" — see README § Resource field reference.
- **Do not** build the real (`tags cxi`) binary off an ARM64 Slingshot node — it CGO-links against `libcxi.so.1`, which is host-provided, not vendored.
- **Do not** skip copying host libraries (`libcxi.so.1`, `libnl-3.so.200`, etc.) into `bin/` before an image build — the image is `FROM scratch` and bundles them; see README step 1.
- **Do not** assume this repo is standalone — the sibling `cxi-dra-integration-tests` repository is the only thing that exercises `internal/nri` and the real `internal/cxi` hardware path end-to-end; this repo's own CI cannot verify pod-lifecycle behavior.
- **Do not** treat `slingshot-cni`'s IPVLAN interface as a replacement for `internal/netsysfs` (or vice versa) — libfabric reads the synthetic sysfs, IP consumers use the IPVLAN netdev; both run. Inside the container the IPVLAN link shows under `ip link` but **not** under the overmounted `/sys/class/net`; that is expected.
- **Do not** share a Go type between the hint-file writer (`internal/driver.CNIHint`) and reader (`internal/cni.Hint`) — they are different processes that agree only on the JSON schema.
- **Do not** call the IPAM delegate with the parent invocation's `CNI_IFNAME` — Whereabouts keys reservations on `(containerID, ifname)`, so a multi-device pod needs `CNI_IFNAME` overridden per interface (`internal/cni/ipam.go`), or reservations collide.
- **Do not** let CNI `DEL` delete anything but IPVLAN links, or fail on a missing hint file / vanished netns — DEL must be idempotent and must never wedge teardown of a pod that is going away regardless.
- **Do not** put netlink calls outside the `linkManager` seam in `internal/cni` — `network_linux.go` / `network_other.go` mirror the `_linux`/`_other` split in `internal/netsysfs`, keeping the orchestration unit-testable on any platform.

## Build, test, and run commands

```bash
make build          # CGO_ENABLED=1, tags=cxi — real hardware build, ARM64 Slingshot node only
make build-mock      # CGO_ENABLED=0 — no libcxi, for kind/local dev
make build-cni       # CGO_ENABLED=0 — slingshot-cni host CNI plugin binary (ships separately from the driver image)
go test ./...        # unit tests (also run in CI)
golangci-lint run    # lint (also run in CI; config in .golangci.yml)
make vet             # go vet ./...
make image / push    # container build via Dockerfile / push to ghcr.io/slingshot/cxi-dra-driver
make deploy          # kubectl apply of raw manifests (clusterrole, deviceclass, admission-policy, daemonset)
make kind-load / kind-reload   # load a locally-built image into a kind cluster and restart the DaemonSet
```

See [README.md](README.md) for the full Helm-based install path and per-value reference.

## Testing strategy

- Unit tests (`go test ./...`, stdlib `testing` package only — no third-party assertion library) live alongside their packages: `internal/cdi` (3 files), `internal/driver` (8 files), `internal/cni` (6 files), `internal/netsysfs` (1 file), `cmd/cxi-cdi-hook` (1 file).
- `internal/cni` is tested in three layers: `cmd_test.go` drives `ADD`/`DEL`/`CHECK` against in-memory `linkManager`/`ipamDelegate` fakes (any platform); `ipam_test.go` execs a fake `whereabouts` binary built from `testdata/fakewhereabouts` to verify the real CNI delegate contract (any platform, needs `go` on `PATH`); `network_linux_test.go` drives the real netlink code against a throwaway named netns and a dummy parent (Linux **as root** only — skips otherwise; run it via a privileged container from a non-Linux workstation, see `docs/cni/cni-plugin.md`).
- `internal/cxi` and `internal/nri` have no unit tests — `internal/cxi`'s real code paths are hardware-dependent (exercised via `mock.go`/`--mock-devices` at the integration level); `internal/nri`'s plugin lifecycle is integration-tested against a live cluster by the sibling `cxi-dra-integration-tests` repo.
- Mocking: `internal/cxi/mock.go` and `service_mock.go` provide synthetic devices and no-op service CRUD for tests and `--mock-devices` kind/dev runs — reuse these instead of hand-rolling fakes.
- CI (`.github/workflows/build.yaml`) runs `go vet`, `go test ./...`, and `golangci-lint` on every PR; it does not and cannot reach a live cluster.

## Known gaps and constraints

- `internal/cxi` and `internal/nri` have zero unit test coverage; both are only exercised by the sibling integration-test suite against a live cluster.
- `slingshot-cni` against a **real** HSN parent and a **real** Whereabouts install is not covered by any automated test here — the root-gated netns tests use a dummy parent and a fake IPAM binary. Live-cluster verification via `cxi-dra-integration-tests` is required.
- `slingshot-cni` does not persist which in-pod name (`<hsn_iface>` vs `<hsn_iface>_inet`) `ADD` chose; `DEL`/`CHECK` re-derive it by probing. Ambiguous only if both names exist as IPVLAN links from a prior life.
- No CodeQL/SAST scanning is configured yet beyond `golangci-lint`'s `gosec` linter.
- This file, `AGENTS.md`, `ARCHITECTURE.md`, `.github/copilot-instructions.md`, and the `.github/instructions/*.md`/`.github/agents/*.md` files were added as part of an AI-readiness pass (see `ai-readiness-report.md`) — keep them in sync with `README.md` as the source of truth for commands and flags.
