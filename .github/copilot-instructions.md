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
| `internal/cxi/` | Hardware abstraction: device discovery, capacity, HSN pairing, CXI service CRUD (three build modes: `cxi` CGO, mock, stub) — see [internal/cxi/README.md](../internal/cxi/README.md) |
| `internal/driver/` | Core DRA plugin: `ClaimBinder`, `ResourceSlice` publishing, `Prepare`/`UnprepareResourceClaims`, namespace VNI allocator, reconciler, reaper — see [internal/driver/README.md](../internal/driver/README.md) |
| `internal/cdi/` | CDI spec generation (device/env/NCCL/AWS-OFI-NCCL), hook + ldconfig-runtime installation, containerd drop-in config |
| `internal/netsysfs/` | Synthetic per-pod `/sys/class/net` tree so stock libfabric discovers the HSN interface |
| `internal/nri/` | NRI plugin: `RunPodSandbox`/`CreateContainer`/`StopPodSandbox` hooks |
| `deploy/helm/` | Recommended install path — parameterized Helm chart |
| `deploy/kubernetes/` | Raw manifests (DaemonSet, ClusterRole, DeviceClass, MutatingAdmissionPolicy) + test fixtures |
| `.github/` | AI-context files, CI workflow, issue/PR templates, CODEOWNERS |

## Architecture

Scheduler → admission policy (injects CEL capacity selectors) → `ClaimBinder` (fit-check, sets BindingConditions) → kubelet `PrepareResourceClaims` (reads namespace VNI, stores allocation, returns CDI IDs) → CDI (`/dev/cxiN` + cgroup BPF) → NRI `RunPodSandbox` (creates CXI service, builds synthetic sysfs) → NRI `CreateContainer` (injects `SLINGSHOT_*` env, bind-mounts sysfs). See [ARCHITECTURE.md](../ARCHITECTURE.md) for the full component map and data-flow narrative, [docs/pod-lifecycle-flow.txt](../docs/pod-lifecycle-flow.txt) for the annotated step-by-step, and the `dra-*.d2*` files for diagrams.

## Build commands

```bash
make build         # CGO_ENABLED=1, tags=cxi — requires libcxi headers on an ARM64 Slingshot node
make build-mock    # CGO_ENABLED=0 — no libcxi required, for kind/local dev
make build-hook    # cxi-cdi-hook is always CGO-free regardless of driver build mode
make vet           # go vet ./...
make image         # container build (FROM scratch; see Dockerfile ENTRYPOINT note in README)
```

## Test commands

```bash
go test ./...              # unit tests across internal/cdi, internal/driver, internal/netsysfs, cmd/cxi-cdi-hook
golangci-lint run          # lint (see .golangci.yml); enforced in CI
```

CI (`.github/workflows/build.yaml`) runs `go vet`, `go test ./...`, and `golangci-lint`. `internal/cxi` and `internal/nri` have no unit tests (hardware/cluster-dependent; exercised via `mock.go` and the sibling `cxi-dra-integration-tests` repo).

## Critical anti-patterns

- **Do not** build the real (`tags cxi`) binary anywhere but an ARM64 Slingshot node with libcxi headers — it links against `libcxi.so` via CGO. Use `build-mock` elsewhere.
- **Do not** assume `le_pools: 0` / `tle_pools: 0` means "no capacity" — it maps the service to a shared default kernel pool on a best-effort basis, not "unavailable" (see README § Resource field reference).
- **Do not** hand-roll a VNI per pod — VNIs are namespace-scoped and assigned once by the leader-elected namespace allocator (`internal/driver/vniallocator.go`); pods read the namespace's `cxi.slingshot.hpe.com/vni` annotation.
- **Do not** skip copying the exact host shared libraries into `bin/` before an image build — the image is `FROM scratch` and has no other way to resolve them at runtime; see README step 1.

## Environment variables / flags

Driver flags are defined in `cmd/cxi-dra-driver/main.go`: `--node-name`/`NODE_NAME`, `--pod-uid`/`POD_UID`, `--pod-namespace`/`POD_NAMESPACE` (required); `--driver-name`, `--vni-range-start`/`-end`, `--enable-cdi-env`, `--env-config`, `--cdi-hook-*`, `--cdi-ldconfig-*`, `--mock-devices`, `--system-svc-ids`. Full table in [CONTEXT.md](../CONTEXT.md).

## External dependencies

Kubernetes 1.34+ (stable `resource.k8s.io/v1`; `MutatingAdmissionPolicy` needs 1.32+), containerd ≥2.0 with CDI + NRI enabled, and libcxi (host-provided, not vendored) for the real build. No sibling-repo dependency in this direction — `cxi-dra-integration-tests` depends on this repo, not the reverse.

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

None hand-generated in source control. `bin/` (build output) and the host libraries copied there before an image build are gitignored build artifacts, not committed.

## Security

Report vulnerabilities per [SECURITY.md](../SECURITY.md). Sensitive paths (`internal/driver`, `internal/cxi`, `.github/`) have owners in [CODEOWNERS](CODEOWNERS).

## Maintenance matrix

| When you change... | Also update... |
|---------------------|----------------|
| A driver CLI flag or its default | `CONTEXT.md` env/flag table, this file's "Environment variables / flags" |
| A `make` target | `README.md`, this file's "Build commands" |
| A CXI/DRA domain gotcha (resource semantics, build-tag requirement) | `CONTEXT.md` anti-patterns, `.github/instructions/domain-patterns.instructions.md` |
| Add/remove/rename an agent, prompt, instruction, or skill | This file's customization index, `AGENTS.md` |
| `internal/driver` public API used across packages | `CONTEXT.md` domain model, `ARCHITECTURE.md` component map |
| A notable user-facing change | `CHANGELOG.md` |
