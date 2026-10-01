# Architecture

This document is the canonical, consolidated architecture reference for `cxi-dra-driver`. It supersedes having to cross-reference the README's architecture section, the three `.d2` diagram files, and `docs/pod-lifecycle-flow.txt` independently — those sources still exist and are linked below for diagram detail, but this file is the single place that maps components to source paths, describes the full data flow, and documents initialization ordering.

For the AI-session domain-model summary (terminology, anti-patterns, known gaps), see [CONTEXT.md](CONTEXT.md). For build/deploy instructions, see [README.md](README.md).

## 1. System context

`cxi-dra-driver` is a Kubernetes **Dynamic Resource Allocation (DRA)** node driver. It runs as a DaemonSet (one pod per Slingshot-equipped node) and is responsible for:

1. Discovering CXI (HPE Slingshot NIC) hardware on the node.
2. Advertising that hardware to the Kubernetes scheduler via `ResourceSlice` objects.
3. Deciding, for each `ResourceClaim` the scheduler allocates to this node, whether the node actually has room (a second, authoritative fit-check beyond the scheduler's stale view).
4. Programming the hardware (a CXI "service" — effectively a virtual NIC scoped to a VNI and network namespace) when a claiming pod starts.
5. Injecting the device, libraries, and environment variables the pod's container needs to use the hardware, via CDI and NRI.
6. Tearing all of the above down when the pod stops, and cleaning up anything that leaks.

The repository also ships **`slingshot-cni`**, a host-side CNI plugin (bundled in the driver image, optionally installed onto each node's `CNI_PATH` by the driver at startup via `--cni-install-dir`) that gives each claiming pod a real, IP-addressed netdev on the HSN. The driver hands it per-pod state through a host hint file (see §3.4 and §3.5b); it owns L3 addressing only and is complementary to the driver's sysfs-based HSN injection. Its use is opt-in — `hostNetwork` pods (and, in future, SR-IOV VFs) remain first-class ways to reach the HSN, and the driver behaves identically with the CNI absent.

It does **not** implement scheduling itself (that's the Kubernetes DRA scheduler plugin, out of tree) and it does **not** contain end-to-end tests (that's the sibling `cxi-dra-integration-tests` repository, which deploys this driver to a live cluster and exercises it).

## 2. Component map

| Component | Source path | Responsibility |
|-----------|-------------|-----------------|
| Driver entry point | `cmd/cxi-dra-driver/main.go` | Parses ~23 CLI flags, builds the Kubernetes client, and wires every other component together (see §5, Initialization ordering) |
| CDI hook binary | `cmd/cxi-cdi-hook/main.go` | Standalone binary, installed onto the host by `internal/cdi`, that refreshes the container's ldconfig cache after CDI bind-mounts libraries in |
| Slingshot CNI binary | `cmd/slingshot-cni/main.go` | CGO-free host CNI plugin (`make build-cni`), bundled in the driver image and optionally installed onto nodes by the driver (`--cni-install-dir`, reusing `internal/cdi.InstallHook`); exec'd by containerd/Multus — wires `internal/cni`'s `CmdAdd`/`CmdDel`/`CmdCheck` into `skel.PluginMainFuncs` |
| Hardware layer | `internal/cxi/` | Device discovery (`device.go`), capacity reads (`capacity_cgo.go`/`capacity_stub.go`), HSN pairing (`hsn.go`), GPU co-location (`gpu.go`), CXI service CRUD (`service*.go`) — see [internal/cxi/README.md](internal/cxi/README.md) |
| DRA plugin core | `internal/driver/` | `ClaimBinder`, `ResourceSlice` publishing, `Prepare`/`UnprepareResourceClaims`, `AllocationStore`, VNI allocator, reconciler, reaper, CNI hint-file writer (`cnihints.go`) — see [internal/driver/README.md](internal/driver/README.md) |
| CDI integration | `internal/cdi/` | Generates CDI specs (`spec.go`, `env.go`), loads the env config (`config.go`), installs the hook binary and ldconfig runtime onto the host (`hookinstall.go`), writes the containerd drop-in config (`containerd.go`) |
| Synthetic network sysfs | `internal/netsysfs/` | Builds a per-pod replacement `/sys/class/net` tree (`build.go`) and bind-mounts it over the pod's own (`netns_linux.go`) so stock libfabric can discover the HSN interface |
| NRI plugin | `internal/nri/` | `Plugin` (`plugin.go`) — hooks into containerd's pod lifecycle (`RunPodSandbox`, `CreateContainer`, `StopPodSandbox`) to create/destroy the CXI service and inject env vars |
| Slingshot CNI logic | `internal/cni/` | CNI `ADD`/`DEL`/`CHECK` orchestration (`cmd.go`), hint-file read/validate (`hint.go`, `podargs.go`), stdin config (`config.go`), L2 IPVLAN link management behind the `linkManager` seam (`network.go`, `network_linux.go`, `network_other.go`), Whereabouts IPAM delegation behind `ipamDelegate` (`ipam.go`) — see [docs/cni/cni-plugin.md](docs/cni/cni-plugin.md) |
| Kubernetes admission | `deploy/kubernetes/admission-policy.yaml` | `MutatingAdmissionPolicy`, evaluated in-process by the API server — injects CEL capacity selectors into `ResourceClaim` requests at CREATE time |
| Deployment | `deploy/helm/cxi-dra-driver/` (recommended), `deploy/kubernetes/` (raw manifests) | DaemonSet/Deployment, RBAC, `DeviceClass`, admission policy, `ResourceClaimTemplate`s |

## 3. Data flow: pod lifecycle end-to-end

This section is the prose equivalent of [docs/pod-lifecycle-flow.txt](docs/pod-lifecycle-flow.txt) and [dra-sequences.d2](dra-sequences.d2)'s "Node Boot" and pod-lifecycle sequences — read those files for the annotated ASCII/diagram form; this section explains the same flow with component ownership made explicit.

### 3.1 Admission (API server, in-process)

A user (or a `ResourceClaimTemplate`-backed Deployment) creates a `ResourceClaim`. The `MutatingAdmissionPolicy` (`deploy/kubernetes/admission-policy.yaml`) intercepts the CREATE and injects CEL selectors derived from the claim's opaque `parameters` block (e.g. `avail_txqs >= 256 && avail_tles >= 512`) directly into `requests[0]`. This means the scheduler's normal device-selector matching does the first-pass capacity filtering — no driver code runs at this stage.

### 3.2 Scheduling (kube-scheduler, out of tree)

The DRA "structured parameters" scheduler plugin reads every node's `ResourceSlice` (published by this driver — see §3.6), applies the CEL selectors from §3.1, and picks a node + device. It writes the allocation into the claim's status and waits for the driver's `BindingConditions` to report `Ready=True` before letting the pod bind.

### 3.3 Binding gate (`internal/driver/claimbinder.go`)

`ClaimBinder` watches `ResourceClaim`s cluster-wide (not just ones on this node) and, for any claim the scheduler tentatively allocated to a device on this node, re-checks capacity against this node's **live** occupancy — the `ResourceSlice` the scheduler read might already be stale. Based on that check it sets one of:

- `Ready=True` — the pod may proceed to kubelet binding.
- `Ready=False` — not yet decided; the claim stays pending.
- `Unschedulable=True` (`DeviceAtCapacity`) — the device genuinely has no room; the scheduler must pick another device/node.
- `Unschedulable=True` (`NamespaceDeviceOccupied`) — another pod in the same namespace already holds this device (the per-namespace occupancy invariant — see CONTEXT.md).

`ClaimBinder` maintains a pending map and a per-namespace occupancy map in memory to make this decision without round-tripping to the kernel on every event.

### 3.4 Kubelet prepare hook (`internal/driver/prepare.go`)

Once bound, kubelet calls this driver's `PrepareResourceClaims` gRPC hook (via the `k8s.io/dynamic-resource-allocation` kubelet-plugin framework wired up in `driver.go`). This function:

1. Reads the pod's namespace `cxi.slingshot.hpe.com/vni` annotation (written by the VNI allocator — see §3.8). It does **not** read a per-claim VNI; VNIs are namespace-scoped.
2. Performs a static ceiling check against the requested `ResourceSet` (from the claim's opaque config) and the device's total capacity.
3. Stores a `PodAllocation` (device, HSN interface, VNI, resources) in the in-memory `AllocationStore` (`internal/driver/store.go`), keyed by pod UID.
4. Writes the pod's **CNI hint file** `<--cni-hints-dir>/<podUID>.json` (`cnihints.go`) — a JSON array with one `{device, hsn_iface, vni}` entry per claimed device — atomically (temp file + rename). This is the only handoff to `slingshot-cni` (§3.5b), which runs as a separate process and cannot see the `AllocationStore`. A write failure fails the claim so kubelet retries rather than starting a sandbox with no hint. Contract: [docs/cni/cnidra-cni-hint-contract.md](docs/cni/cnidra-cni-hint-contract.md).
5. Returns the CDI device IDs kubelet should ask the container runtime to attach — these reference the CDI specs `internal/cdi` already wrote (see §3.5).

Because kubelet waits for this hook to return before calling CRI `RunPodSandbox`, the hint file is guaranteed to exist before CNI `ADD` fires — ordering is a kubelet sequencing guarantee, not a race the plugin has to defend against (it still has a bounded retry as a safety net for driver crashes).

### 3.5 CDI (container runtime, driven by specs this driver wrote ahead of time)

Before any of the above, at driver startup (see §5) and per-device as devices are discovered, `internal/cdi` writes CDI spec files to `/var/run/cdi/` (`spec.go`) describing: the `/dev/cxiN` device node plus its `/sys/class/cxi_user/<name>` sysfs path, and — if `--enable-cdi-env=true` — additional "profile" specs (`env.go`) bundling libcxi/libfabric runtime libraries and environment variables (notably `CXIP_SKIP_AMA_CHECK=1`). The container runtime (containerd, with CDI support enabled) reads the CDI IDs returned by `PrepareResourceClaims` and performs the actual bind-mounts/device-node creation as part of standard OCI container creation — no driver code runs at this exact step, but the specs it wrote are what makes it possible.

### 3.5b CNI `ADD` (`slingshot-cni`, exec'd by containerd/Multus during `RunPodSandbox`)

When containerd sets up the sandbox's network — before this driver's NRI `RunPodSandbox` hook in §3.6 fires — it invokes every CNI plugin in the pod's network attachment list. If a `NetworkAttachmentDefinition` of `type: slingshot-cni` is attached, `internal/cni.CmdAdd` runs **as a separate host process** with no access to the driver's memory:

1. Parses its stdin config (`ipam.type` is mandatory) and `CNI_ARGS` (`K8S_POD_UID`), then reads the hint file from §3.4, retrying briefly if absent.
2. For **every** hint entry (a `count > 1` claim yields several): looks up the host HSN netdev named `hsn_iface`, creates an **L2 IPVLAN** sub-interface on it, moves it into the pod netns, renames it to `hsn_iface` (falling back to `hsn_iface_inet` if that name is taken), and brings it up (`network_linux.go`, via `vishvananda/netlink`).
3. Execs the configured IPAM plugin (**Whereabouts**) with the unmodified stdin config and `CNI_IFNAME` overridden to the in-pod name — Whereabouts keys reservations on `(containerID, ifname)`, so each device needs its own — and applies the returned address and routes (`ipam.go`).
4. Prints one aggregated CNI 1.0.0 result. Any failure partway unwinds every interface and reservation already made in this invocation.

This produces a real netdev with an IP on the HSN, for anything that needs IP over the fabric. It is **complementary to, not a replacement for**, the sysfs injection in §3.6 step 4: stock libfabric discovers the HSN by globbing `/sys/class/net`, which inside the container is the driver's synthetic overmount — the IPVLAN interface is visible via netlink (`ip link`/`ip addr`) but not under that overmounted path, and libfabric does not need it to be. Full command reference: [docs/cni/cni-plugin.md](docs/cni/cni-plugin.md).

### 3.6 NRI `RunPodSandbox` (`internal/nri/plugin.go`)

containerd calls this driver's NRI plugin at `RunPodSandbox`, once the pod's network namespace exists. The plugin:

1. Reads the `PodAllocation` `PrepareResourceClaims` stored in `AllocationStore`.
2. Creates the CXI service on the hardware (`internal/cxi.ServiceManager` — `cxil_alloc_svc` → `cxil_svc_set_netns` → `cxil_svc_enable`), scoped to the pod's netns and the namespace's VNI.
3. Performs a **live** fit-check (the hardware might have less room than the static check in §3.4 assumed, if concurrent pods started since).
4. Builds a synthetic `/sys/class/net` tree (`internal/netsysfs.Build`) replicating the pod's real interfaces plus a synthetic HSN entry with `operstate=up`/`carrier=1`/a `device` symlink, and bind-mounts it over the pod's own `/sys/class/net` (`netns_linux.go`) — this is required because stock (unpatched) libfabric expects to find the HSN interface there, and the real `/sys/class/net` is read-only and namespace-scoped.

A per-device mutex in the plugin serializes concurrent pod starts targeting the same device to prevent races in steps 2–4.

### 3.7 NRI `CreateContainer` (`internal/nri/plugin.go`)

Immediately before the container process starts, the plugin injects `SLINGSHOT_DEVICES`, `SLINGSHOT_VNIS`, and `SLINGSHOT_SVC_IDS` environment variables (comma-separated, one entry per claimed device) and ensures the synthetic sysfs tree from §3.6 is bind-mounted into this specific container.

### 3.8 Namespace VNI allocation (leader-elected, runs continuously, not per-pod)

Independently of any single pod's lifecycle, `internal/driver/vniallocator.go` runs as a leader-elected controller (via `vnipool.go`'s lease management) watching `Namespace` objects. For every namespace that claims a CXI device but has no VNI annotation yet, it assigns the lowest free VNI from the configured `--vni-range-start`/`--vni-range-end` pool and writes it as the `cxi.slingshot.hpe.com/vni` annotation. This is what §3.4 reads. Only one VNI is ever assigned per namespace — every pod in that namespace shares it.

### 3.9 Continuous safety nets (run on a timer, not part of the request-response flow above)

- **Reconciler** (`internal/driver/reconciler.go`): on `--resource-refresh-interval` (default 10s), re-reads live CXI service state from the hardware and rebuilds the in-memory occupancy map and `ResourceSlice`, closing the gap between event-driven updates and reality — chiefly relevant after a driver restart or a missed Kubernetes event.
- **Reaper** (`internal/driver/reaper.go`): periodically scans for CXI services that have no corresponding live pod (owner UID doesn't exist / doesn't match) and deletes them — cleans up after pods that were force-deleted or otherwise didn't get a clean `StopPodSandbox`.

### 3.10 Pod teardown (`internal/nri/plugin.go`, `StopPodSandbox`)

On pod stop, the plugin destroys the CXI service and removes the synthetic sysfs tree it created in §3.6. `internal/driver/unprepare.go`'s `UnprepareResourceClaims` removes the `PodAllocation` from the `AllocationStore` and deletes the pod's CNI hint file (best-effort — a removal failure never blocks claim release) around the same time via the kubelet hook path.

Independently, containerd invokes CNI `DEL` for the sandbox, and `internal/cni.CmdDel` releases each device's Whereabouts reservation (under the in-pod name `ADD` actually chose) and deletes its IPVLAN interface. `DEL` is best-effort and idempotent per the CNI spec: a missing hint file, an already-gone interface, or a vanished netns all count as success, every device is attempted even if an earlier one fails, and only IPVLAN links are ever deleted — a pod's own interface that happens to share the HSN's name is never touched. There is no ordering dependency between CNI `DEL` and the driver's hint-file removal; if the hint is already gone, `DEL` logs and returns success, leaving any stragglers to Whereabouts's own `ip-reconciler`.

## 4. Component interaction diagram

```
Scheduler ──allocates──► ResourceSlice (cxi0, cxi1, …)
                              │
                         DaemonSet (this driver)
                         ├── internal/cxi        discovers /sys/class/cxi_user/*, pairs CXI↔HSN
                         ├── internal/cdi         writes CDI specs → /var/run/cdi/
                         ├── internal/driver/claimbinder.go
                         │     gates pod binding via DRA BindingConditions
                         ├── internal/driver/reconciler.go + reaper.go
                         │     poll-driven occupancy safety nets
                         ├── internal/driver/vniallocator.go   (leader-elected)
                         │     assigns one VNI per namespace via annotation
                         ├── internal/nri (RunPodSandbox)
                         │     creates CXI service + synthetic /sys/class/net tree
                         ├── internal/nri (CreateContainer)
                         │     injects SLINGSHOT_* env, bind-mounts sysfs
                         └── internal/nri (StopPodSandbox)
                               destroys CXI service, removes synthetic sysfs tree
                              │
                              │ PrepareResourceClaims writes /var/run/cxi/net-hints/<podUID>.json
                              ▼
                         slingshot-cni (host CNI plugin; cmd/slingshot-cni + internal/cni)
                         ├── ADD   per device: L2 IPVLAN on host <hsn_iface> → pod netns as
                         │         <hsn_iface> (or <hsn_iface>_inet) → Whereabouts IPAM address
                         ├── DEL   release Whereabouts address, delete IPVLAN (idempotent)
                         └── CHECK verify link up + IPAM state

MutatingAdmissionPolicy (in-process in API server)
    injects CEL selectors (avail_txqs >= N && avail_tles >= M) from claim opaque config
```

See [dra-architecture.d2.txt](dra-architecture.d2.txt) and [dra-current-state.d2](dra-current-state.d2) for the rendered diagram form (the latter also color-codes implemented vs. not-yet-implemented components).

## 5. Initialization ordering (`cmd/cxi-dra-driver/main.go`)

At process startup, `main.go` performs, in order:

1. Parse CLI flags (node name, pod UID/namespace, driver name, VNI range, CDI config, mock-device count, etc.).
2. Build the Kubernetes client (in-cluster config, or `--kubeconfig` for local/dev use).
3. `internal/cxi.Discover()` — walk sysfs, read hardware capacity (or generate `--mock-devices` synthetic devices), pair each device with its HSN interface, resolve NUMA-local GPU co-location.
4. `internal/driver.New(...)` — construct the `Driver`, which in turn writes the initial CDI specs for every discovered device (and, if `--enable-cdi-env=true`, the env/NCCL/AWS-OFI-NCCL profile specs).
5. `internal/nri.NewPlugin(...)` — connect to containerd's NRI socket.
6. `driver.kubeletplugin.Start()` — start the DRA gRPC server and the kubelet plugin registrar.
7. `driver.PublishResources()` — publish the initial `ResourceSlice`(s) for this node.
8. `ClaimBinder.Run()` — start watching `ResourceClaim`s cluster-wide.
9. If `--vni-range-start`/`-end` are non-zero: start the namespace VNI allocator (leader election + namespace informer).
10. Register signal handlers (SIGTERM/SIGINT) for graceful shutdown of all of the above.

See [dra-sequences.d2](dra-sequences.d2) for this same sequence rendered as a diagram, with the exact actor names (`os`, `main`, `cxi_pkg`, `driver`, `nri`, `binder`, `vni_allocator`, `kubelet`, `apiserver`).

## 6. Deployment topology

Two install paths render the same logical objects (see README § Deployment for full instructions):

- **Helm** (`deploy/helm/cxi-dra-driver/`) — recommended; every object name is parameterized by `driverName`, so multiple driver versions can be installed side-by-side (e.g. for A/B testing) without collision. Supports `workload.kind: DaemonSet` (default) or `Deployment`.
- **Raw manifests** (`deploy/kubernetes/`) — a lighter-weight alternative applied via `make deploy`; also used for [mock mode](README.md#mock-mode-kind--no-hardware) in kind clusters. `configmap.yaml` (the `--enable-cdi-env` env config) is **not** applied by `make deploy` and must be applied separately first.

Both paths render: the DaemonSet/Deployment, `ClusterRole`/RBAC, `DeviceClass`, the `MutatingAdmissionPolicy`, and `ResourceClaimTemplate`s (`cxi-small`/`cxi-medium`/`cxi-large`/`cxi-whole`).

## 7. Concurrency model

| Component | Mechanism | Why |
|-----------|-----------|-----|
| `internal/nri.Plugin` | One mutex per device | Serializes concurrent `RunPodSandbox`/`StopPodSandbox` calls that target the same CXI device, so service creation, the live fit-check, and synthetic-sysfs construction never race for the same hardware |
| `internal/driver.AllocationStore` | Single `sync.RWMutex`-guarded map | Multiple goroutines (kubelet prepare/unprepare hooks, NRI hooks, reconciler) read and write `PodAllocation`s concurrently |
| `internal/driver.ClaimBinder` | In-memory pending map + per-namespace occupancy map, both mutex-guarded | Claim-watch events, the reconciler, and the reaper all need a consistent view of "what's currently occupying this device" without round-tripping to the kernel per decision |
| `internal/driver/vniallocator.go` | Kubernetes lease-based leader election (`vnipool.go`) | Only one driver replica across the whole cluster may assign VNIs at a time — this is a cluster-wide decision, not a per-node one, even though the driver itself runs as a per-node DaemonSet |

## 8. Failure modes and recovery

| Scenario | What happens | Recovery mechanism |
|----------|---------------|---------------------|
| Driver pod restarts (crash, node drain, upgrade) | In-memory `AllocationStore`, pending map, and occupancy map are lost | The reconciler (`internal/driver/reconciler.go`) re-reads live CXI service state from the hardware on the next `--resource-refresh-interval` tick and rebuilds occupancy; already-running pods are unaffected since their CXI services are still live on the node |
| Kubernetes event missed (informer resync gap, API server hiccup) | `ClaimBinder`'s in-memory view of a claim can drift from the API server's | The reconciler's periodic hardware re-read is the ground truth, not the event stream — drift self-heals on the next tick |
| Pod force-deleted (`kubectl delete --force`, node failure) | `StopPodSandbox` never runs, so the CXI service and synthetic sysfs tree for that pod are never explicitly torn down | The reaper (`internal/driver/reaper.go`) periodically finds CXI services with no live owning pod and deletes them |
| Live fit-check fails at `RunPodSandbox` (§3.6) after `PrepareResourceClaims`'s static check (§3.4) already passed | A concurrent pod on the same node consumed the remaining capacity between the two checks | `RunPodSandbox` fails the sandbox creation; kubelet retries pod creation, by which point `ResourceSlice`/`ClaimBinder` should reflect the new occupancy and the scheduler can pick a different device/node |
| Two namespaces need VNIs simultaneously | Race on VNI assignment | Leader election on the VNI allocator ensures only one process assigns VNIs cluster-wide; the underlying namespace annotation write is a single atomic API server update |
| CNI `ADD` fails partway through a multi-device pod (IPVLAN creation, IPAM, or address application) | Some devices already have an interface and a Whereabouts reservation | `internal/cni.CmdAdd` unwinds every interface and reservation it created in that invocation before returning the error; containerd fails `RunPodSandbox` and kubelet retries from a clean slate |
| Driver removed the hint file before CNI `DEL` ran (or crashed and restarted mid-teardown) | `CmdDel` cannot map devices to interfaces | `DEL` logs and returns success rather than wedging pod teardown; the pod netns is destroyed with the sandbox (taking the IPVLAN links with it) and Whereabouts's `ip-reconciler` garbage-collects any reservation left behind |

## 9. Extension points — "how to add X"

- **Add a new hardware quota field** (alongside `txqs`/`tgqs`/`eqs`/etc.): extend `ResourceSet` in `internal/cxi/resources.go` (`Fits`/`Minus`), the opaque `parameters` schema claims supply, the static ceiling check in `internal/driver/prepare.go`, and the README's § Resource field reference table. This is the most common kind of change and touches exactly these four places.
- **Add a new CDI profile** (alongside `env`, `nccl`, `aws-ofi-nccl`): add a spec-writer function in `internal/cdi/env.go` following the existing profile functions, register it so `cdi_profiles` in a `ResourceClaimTemplate` can select it, and document the new profile in the README's CDI Profiles table.
- **Add a new build mode to `internal/cxi`** (beyond `_cgo`/`mock`/`_stub`): create `<name>_<mode>.go` files following the existing suffix convention (see `.github/instructions/code-style.instructions.md`) rather than adding runtime branching to an existing file — this keeps CGO-only code out of builds that don't need it.
- **Add a new specialist Copilot agent**: see `.github/agents/*.agent.md` for the existing pattern (role, key constraints, workflow) and register it in `AGENTS.md` and the customization index in `.github/copilot-instructions.md`.

## 10. Observability

Logging is the only observability signal today (structured via the standard library and the Kubernetes client-go logger conventions) — there is no metrics endpoint (e.g. Prometheus `/metrics`) and no tracing integration. Driver DaemonSet logs (`kubectl -n kube-system logs ds/cxi-dra-driver`) are the primary debugging surface; see [.github/prompts/debug-driver.prompt.md](.github/prompts/debug-driver.prompt.md) for the most common triage flows. This is a known gap, not a design choice — adding a metrics endpoint for occupancy, fit-check outcomes, and reconciler/reaper activity would be a reasonable first observability improvement.

## 11. Package complexity summary

| Package | Files | Godoc coverage | Unit tests | Notes |
|---------|-------|-----------------|------------|-------|
| `cmd/cxi-dra-driver` | 1 | Package-level only (it's `main`) | None (integration-tested via CI image + sibling repo) | Wiring/orchestration only, no business logic |
| `cmd/cxi-cdi-hook` | 2 | Yes | `main_test.go` | Small, standalone, CGO-free by design |
| `cmd/slingshot-cni` | 1 | Package-level only (it's `main`) | None (logic lives in `internal/cni`) | Thin `skel.PluginMainFuncs` wiring; CGO-free (`make build-cni`) |
| `internal/cxi` | 11 | Yes | None | Hardware-dependent; see [internal/cxi/README.md](internal/cxi/README.md) |
| `internal/driver` | 14 | Yes | 7 of 14 files | Strongest test coverage in the repo; see [internal/driver/README.md](internal/driver/README.md) |
| `internal/cdi` | 8 | Yes | 3 of 8 files | Spec generation and host installation logic |
| `internal/cni` | 10 (+ `testdata/fakewhereabouts`) | Yes, including package-level rationale | 6 of 10 files | Three test layers: orchestration with fakes (any platform), real IPAM delegate exec'ing a fake `whereabouts` binary (any platform), real netlink against a throwaway netns (`network_linux_test.go`, Linux root only — skips otherwise) |
| `internal/netsysfs` | 4 | Yes, including package-level rationale | 1 of 4 files (`build_test.go`) | `netns_linux.go` is platform-specific and untested by design |
| `internal/nri` | 1 | Yes | None | Integration-tested only, by the sibling `cxi-dra-integration-tests` repo |

## 12. Key data types

| Type | File | Key fields | Purpose |
|------|------|-----------|---------|
| `Device` | `internal/cxi/device.go` | Name, index, dev path, sysfs path, NUMA node, PCIe root, HSN interface, GPU PCIe root | The in-memory representation of one discovered CXI NIC; produced by `Discover()` and consumed by every other package |
| `ResourceSet` | `internal/cxi/resources.go` | `txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `le_pools`, `tle_pools`, `acs` | The hardware quota a claim requests or a device has available; see §12 for how it flows through the system |
| `PodAllocation` | `internal/driver/store.go` | Device, HSN interface, VNI, `ResourceSet` | What `PrepareResourceClaims` decided for one pod; keyed by pod UID in `AllocationStore` |
| `AllocationStore` | `internal/driver/store.go` | `map[types.UID]PodAllocation` + `sync.RWMutex` | Thread-safe lookup shared by the kubelet prepare/unprepare hooks, the NRI plugin, and the reconciler |
| `CNIHint` / `cni.Hint` | `internal/driver/cnihints.go` / `internal/cni/hint.go` | `device`, `hsn_iface`, `vni` | One claimed device's entry in the per-pod hint file. Deliberately two separate Go types that agree only on the JSON schema — the writer (driver daemon) and reader (`slingshot-cni` process) are different binaries on different lifecycles and must not share code |
| `linkManager` / `ipamDelegate` | `internal/cni/network.go` / `internal/cni/ipam.go` | interfaces | The two seams `CmdAdd`/`CmdDel`/`CmdCheck` are written against: kernel-facing netlink operations and IPAM plugin exec. Real implementations are Linux-only / exec-based; tests substitute fakes so orchestration runs on any platform |
| `Driver` | `internal/driver/driver.go` | Devices, pending grants, occupancy, references to `ClaimBinder`/`AllocationStore` | Top-level type wiring every other `internal/driver` file together |
| CDI `Spec` (from `tags.cncf.io/container-device-interface`) | `internal/cdi/spec.go`, `env.go` | Device nodes, mounts, env vars | The on-disk `/var/run/cdi/*.json` contract between this driver and the container runtime |

## 13. Resource field implementation reference

The hardware quota fields a `ResourceClaimTemplate` requests (`txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `le_pools`, `tle_pools`, `acs`) are defined once in `internal/cxi/resources.go`'s `ResourceSet` and flow through the system as follows — see the README's § Resource field reference for the full semantic explanation of each field including kernel-source line citations for the libfabric CXI provider.

```
ResourceClaimTemplate.spec.devices.config[].opaque.parameters   (user-authored YAML)
        │
        ▼  read by the MutatingAdmissionPolicy at claim CREATE (§3.1)
CEL selector on requests[0]   (avail_txqs >= N && avail_tles >= M && …)
        │
        ▼  matched against ResourceSlice attributes at schedule time (§3.2)
Scheduler allocation
        │
        ▼  re-verified against live occupancy (§3.3)
ClaimBinder.Fits(ResourceSet)   — internal/cxi/resources.go
        │
        ▼  static ceiling check at kubelet prepare time (§3.4)
PrepareResourceClaims → AllocationStore.PodAllocation.ResourceSet
        │
        ▼  live re-check at NRI RunPodSandbox (§3.6)
ServiceManager.Create(ResourceSet)  — internal/cxi/service*.go → libcxi ioctls
```

Every field is a hard cap enforced by the kernel's per-service quota **except** `le_pools`/`tle_pools`, which fall back to a shared best-effort default pool when `0` — see the anti-pattern note in [CONTEXT.md](CONTEXT.md).

## 14. Testing architecture

This repository sits at the bottom of a two-repository test pyramid:

1. **Unit tests** (this repo, `go test ./...`) — fast, offline, no cluster or hardware. Cover `internal/driver` (state machines: claim binding, VNI allocation, occupancy reconciliation), `internal/cdi` (spec generation, config parsing), `internal/netsysfs` (synthetic tree construction), and `cmd/cxi-cdi-hook` (ldcache refresh logic). Run automatically in CI (`.github/workflows/build.yaml`).
2. **Integration / end-to-end tests** (sibling `cxi-dra-integration-tests` repo, not present in this workspace by default) — deploy this driver's built image to a live Kubernetes cluster and exercise `internal/nri`, the real (`tags cxi`) hardware path of `internal/cxi`, and the full pod-lifecycle flow from §3. This repo's own CI **cannot** run these — there is no live-cluster access from GitHub Actions.

There is no in-repo `examples/` directory or smoke-test harness; `deploy/kubernetes/test/` (a sample `ResourceClaim` + pod) plays that role for manual verification during development, not as an automated example suite.

## 15. CI/CD pipeline

`.github/workflows/build.yaml` runs on every push to `main`, every `v*` tag, and every PR touching `Dockerfile`, `go.mod`, `go.sum`, `cmd/**`, or `internal/**`:

1. Checkout, set up Go (version read from `go.mod`).
2. `go vet ./...`.
3. `go test ./...`.
4. `golangci-lint run` (config: `.golangci.yml`, includes `gosec`).
5. Set up Docker Buildx + QEMU (for cross-arch emulation).
6. Log in to `ghcr.io` (skipped on PRs — no push credentials needed to just build).
7. Compute image tags/labels from git ref, semver tag, and short SHA (`docker/metadata-action`).
8. Build (and, outside of PRs, push) a multi-arch (`linux/amd64` + `linux/arm64`) image from `Dockerfile.ci`, with GitHub Actions layer caching.

Steps 2–4 are the only automated correctness gates; the image build itself does not run any tests inside the container.

## 16. Historical note

CDI env integration (§3.5) and namespace VNI allocation (§3.8) were once tracked as implementation plans under `docs/superpowers/`; both features are complete and live, and that directory has been removed now that this document's own descriptions cover the same ground.

## Appendix A: Full file index (packages without their own README)

`internal/cxi` and `internal/driver` have their own per-file breakdown in [internal/cxi/README.md](internal/cxi/README.md) and [internal/driver/README.md](internal/driver/README.md). The remaining packages are small enough to index here directly rather than adding a README each.

### `internal/cdi/`

| File | Purpose |
|------|---------|
| `config.go` | `LoadConfig()` — reads and validates the JSON CDI env config mounted from a ConfigMap (`envMounts`/`awsOfiMounts`) |
| `containerd.go` | Writes the containerd CDI plugin drop-in config so the runtime auto-discovers specs under `/var/run/cdi/` |
| `containerd_test.go` | Tests for `containerd.go` |
| `env.go` | Writes the optional `env`/`nccl`/`aws-ofi-nccl` CDI profile specs (shared device nodes, library mounts, the ldcache-refresh hook) |
| `env_test.go` | Tests for spec writers and device-ID helpers |
| `hookinstall.go` | `InstallHook()` copies the `cxi-cdi-hook` binary from this container onto the host; `InstallLdconfigRuntime()` installs ldconfig + the dynamic linker + libc onto the host |
| `hookinstall_test.go` | Tests for hook and ldconfig-runtime installation |
| `spec.go` | `WriteSpec()`/`RemoveSpec()` — the per-device CDI spec exposing `/dev/cxiN` and its sysfs path |

### `internal/netsysfs/`

| File | Purpose |
|------|---------|
| `build.go` | `Build()` — constructs the synthetic `/sys/class/net` tree: replicates the pod's real interfaces as symlinks and adds HSN entries with `operstate`/`carrier`/`speed`/`device` metadata; carries an extensive package-level comment explaining why this exists |
| `build_test.go` | Tests tree construction, symlink replication, and HSN metadata synthesis |
| `netns_linux.go` | `MountNetSysfs()` — Linux-specific bind-mount of the synthetic tree over the pod's `/sys/class/net` |
| `netns_other.go` | Stub for non-Linux build targets |

### `internal/nri/`

| File | Purpose |
|------|---------|
| `plugin.go` | `Plugin` — implements the NRI `RunPodSandbox`/`CreateContainer`/`StopPodSandbox` hooks described in §3.6–§3.7 and §3.10; the single file in this package, but the most concurrency-sensitive one (see §7) |

### `cmd/`

| File | Purpose |
|------|---------|
| `cmd/cxi-dra-driver/main.go` | Driver entry point; see §5 for the full initialization sequence it performs |
| `cmd/cxi-cdi-hook/main.go` | The `update-ldcache` OCI hook subcommand, installed onto hosts by `internal/cdi.InstallHook` |
| `cmd/cxi-cdi-hook/main_test.go` | Tests the ldcache-refresh logic against a mocked OCI state |

## Appendix B: Related documents

| Document | Content |
|----------|---------|
| [README.md](README.md) | Build/install quickstart, full Helm values reference, resource-field semantics with kernel source citations |
| [CONTEXT.md](CONTEXT.md) | AI-session domain model, anti-patterns, known gaps — the condensed companion to this document |
| [docs/pod-lifecycle-flow.txt](docs/pod-lifecycle-flow.txt) | Annotated ASCII rendering of §3 with explicit decision points (`Ready=True/False`, `Unschedulable`, callback triggers) |
| [dra-architecture.d2.txt](dra-architecture.d2.txt) | Component diagram matching §2/§4 |
| [dra-current-state.d2](dra-current-state.d2) | Same component diagram, color-coded by implementation status |
| [dra-sequences.d2](dra-sequences.d2) | Sequence diagram for the node-boot flow in §5 |
