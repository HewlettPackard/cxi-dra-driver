# cxi-dra-driver

A Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) node driver for HPE Slingshot CXI (Cray eXascale Interconnect) PCIe devices.

The driver runs as a DaemonSet on each Slingshot-equipped node. It discovers CXI devices via sysfs, publishes them as a `ResourceSlice` so the scheduler can allocate them to pods, programs a scoped CXI service (VNI + netns) via libcxi, and injects each device into its container via CDI (Container Device Interface) and NRI (Node Resource Interface).

> **Contributing or exploring with an AI assistant?** Start with [CONTEXT.md](CONTEXT.md) (AI session briefing) and [ARCHITECTURE.md](ARCHITECTURE.md) (full component map and data flow). See [AGENTS.md](AGENTS.md) for the full Copilot customization index and [CONTRIBUTING.md](CONTRIBUTING.md) before opening a PR.

## Architecture

```
Scheduler ──allocates──► ResourceSlice (cxi0, cxi1, …)
                              │
                         DaemonSet (this driver)
                         ├── discovers /sys/class/cxi_user/*
                         ├── pairs CXI↔HSN via sysfs PCIe topology
                         ├── writes CDI specs → /var/run/cdi/
                         │     ├── DeviceNodes: /dev/cxiN  (cgroup BPF allowance)
                         │     └── env spec, attached to every device: kdreg2/gdrdrv/xpmem,
                         │         /dev/shm, host libfabric/libcxi under /host/env and
                         │         CXIP_SKIP_AMA_CHECK (libs/env with --enable-cdi-env)
                         ├── ClaimBinder
                         │     └── gates pod binding via DRA BindingConditions
                         │         (Ready=True only when capacity is confirmed)
                         ├── Orphan-service reaper
                         │     └── destroys CXI services whose owning pod is
                         │         gone every refresh tick — recovers from
                         │         driver restart, missed events, and
                         │         force-deleted pods that bypassed NRI
                         ├── Namespace VNI allocator (leader-elected)
                         │     └── assigns one VNI per namespace via annotation
                         ├── NRI plugin (RunPodSandbox)
                         │     └── creates CXI service (namespace VNI + pod netns inode)
                         │         and builds the per-pod synthetic /sys/class/net tree
                         │         (HSN sysfs injection so stock libfabric finds the device)
                         ├── NRI plugin (CreateContainer)
                         │     └── injects SLINGSHOT_DEVICES, SLINGSHOT_VNIS, SLINGSHOT_SVC_IDS env
                         │         vars and bind-mounts the synthetic /sys/class/net over the
                         │         container's own (HSN visible to stock libfabric)
                         └── NRI plugin (StopPodSandbox)
                               └── destroys CXI service and removes the synthetic sysfs tree

slingshot-cni (host CNI plugin, invoked by containerd/Multus — see docs/cni/cni-plugin.md)
    ├── reads the driver's per-pod hint file (/var/run/cxi/net-hints/<podUID>.json,
    │   written by PrepareResourceClaims before RunPodSandbox fires)
    ├── ADD: per claimed device, creates an L2 IPVLAN sub-interface on the paired
    │        host HSN, moves it into the pod netns as <hsn_iface> (or <hsn_iface>_inet),
    │        and delegates addressing to Whereabouts IPAM (cluster-wide HSN pool)
    └── DEL: releases the Whereabouts address and removes the interface (idempotent)
    (owns L3 addressing only — complementary to the NRI sysfs injection above,
     which is what stock libfabric's HSN discovery depends on)

kube-scheduler (DRA consumable capacity)
    └── charges each claim's capacity.requests (txqs … tles) against the device's
          published capacity, so claims share a NIC without overcommitting it

MutatingAdmissionPolicy (in-process in API server)
    └── intercepts ResourceClaim CREATE
          └── converts legacy opaque config (txqs …, le_pools, tle_pools)
                into capacity.requests
    └── intercepts Pod CREATE
          └── injects Multus slingshot-hsn network annotation
                for Pods referencing CXI ResourceClaimTemplates
```

## Requirements

- Kubernetes 1.36+ (DRA consumable capacity — the `DRAConsumableCapacity` feature gate — is beta and on by default from 1.36; on 1.34/1.35 enable it, along with `DRADeviceBindingConditions`, on the API server, scheduler and kubelet). `MutatingAdmissionPolicy` requires 1.32+
- Nodes labelled `feature.node.kubernetes.io/cxi=true`
- Container runtime with CDI and NRI support (containerd ≥ 2.0)
- NRI enabled in containerd config (see below)
- Docker or Podman (to build the image); Go 1.24+ only if building the binary outside a container

## Building

CI publishes multi-arch (`linux/amd64` + `linux/arm64`) images to
`ghcr.io/bristol-supercomputing/cxi-dra` on every push to `main` and every
`v*` tag, so most users never need to build anything.

### Container image (recommended)

```bash
make image                       # production image, both hardware paths real
make push
```

[Dockerfile](Dockerfile) is self-contained: it clones and builds libcxi from
public HPE Slingshot sources in a builder stage, then compiles the driver with
`-tags cxi` against it. There is nothing to stage in `bin/` and no dependency
on the host's Slingshot install, so this builds on a laptop or in CI just as
well as on a Slingshot node, for either architecture.

On a machine with no Docker Hub access, redirect the base images:

```bash
make image BUILD_ARGS='--build-arg GO_IMAGE=mirror.gcr.io/library/golang:1.24-bookworm \
                       --build-arg RUNTIME_IMAGE=mirror.gcr.io/library/debian:bookworm-slim'
```

> **Keeping libcxi in step with the nodes.** libcxi talks to the `cxi` kernel
> driver over a versioned ioctl ABI. The `CASSINI_HEADERS_REF`, `CXI_DRIVER_REF`
> and `LIBCXI_REF` build args in [Dockerfile](Dockerfile) pin
> `release/shs-14.0` (libcxi API 1.8.0, the first release carrying
> `cxil_svc_set_netns`). **When the nodes move to a new SHS release, repin
> these.** A skew does not fail the build — it surfaces at runtime as
> `cxil_open_device` or `cxil_svc_*` errors.

### Slingshot CNI plugin

`slingshot-cni` is a CGO-free host binary **bundled into the driver image** (the `Dockerfile` builds it in the builder stage, alongside `cxi-cdi-hook`). To build it standalone:

```bash
make build-cni                 # → bin/slingshot-cni for the build host's arch
make build-cni GOARCH=amd64    # cross-build, e.g. from an arm64 workstation for x86-64 nodes
```

Bundling it does **not** install it on nodes — that is opt-in at deploy time (`cni.install=true`, see [Optional: Slingshot CNI](#optional-slingshot-cni)). Sites that use `hostNetwork` (or, in future, SR-IOV VFs) for HSN access simply leave it off.


Reference this image at install time — `--set image.repository=…,image.tag=…` for Helm, or the `image:` field in [deploy/kubernetes/daemonset.yaml](deploy/kubernetes/daemonset.yaml) for the kubectl path.

### Mock image (kind / no hardware)

```bash
make kind-reload                 # build mock image, load into kind, restart DS
```

[Dockerfile.mock](Dockerfile.mock) builds without the `cxi` tag, so the service
calls are stubs. Run the driver with `--mock-devices N` so it uses
`cxi.MockServiceManager` and stays off the hardware paths.

### Binary directly on a Slingshot node

Requires the CGo toolchain and `cray-libcxi-devel` installed:

```bash
make build                       # CGO_ENABLED=1, -tags cxi
```

The cgo directives in [internal/cxi/service_cgo.go](internal/cxi/service_cgo.go)
expect the node layout (`-I/usr/include`, `-L/usr/lib64 -lcxi`); the container
build installs libcxi to those same paths so both routes compile identically.

## Deployment

The [Helm chart](deploy/helm/cxi-dra-driver) is the recommended install path: it renders the
workload (DaemonSet or Deployment), `DeviceClass`, RBAC, `MutatingAdmissionPolicy`, the
`env.json` ConfigMap, and the ResourceClaimTemplates as one driver-name-parameterized release,
so multiple driver versions can be installed side-by-side. The raw manifests under
[deploy/kubernetes](deploy/kubernetes) (applied by `make deploy`) are kept as a lighter-weight
alternative and for [mock mode](#mock-mode-kind--no-hardware).

Steps 1 and 2 are node prerequisites for **either** install path.

### 1. Enable NRI in containerd

NRI must be enabled in the containerd config on each node. For k3s/RKE2 nodes, edit `/var/lib/rancher/k3s/agent/etc/containerd/config.toml` and ensure:

```toml
[plugins."io.containerd.nri.v1.nri"]
  disable = false
  disable_connections = false
  plugin_registration_timeout = "5s"
  plugin_request_timeout = "2s"
  socket_path = "/var/run/nri/nri.sock"
  state_dir = "/var/run/nri"
```

Restart containerd after making changes.

### 2. Label Slingshot nodes

```bash
kubectl label node <node-name> feature.node.kubernetes.io/cxi=true
```

### 3. Install the driver

#### With Helm (recommended)

```bash
# Default: DaemonSet, driver name cxi.slingshot.hpe.com, namespace kube-system
helm install cxi deploy/helm/cxi-dra-driver \
  --set image.repository=docker.io/<your-registry>/cxi-dra-driver \
  --set image.tag=dev
```

The runtime-library host paths under `envConfig.json` in
[values.yaml](deploy/helm/cxi-dra-driver/values.yaml) are **site-specific** (the default
`libfabric` path is a developer home directory). Supply your own values file before installing on
a real cluster:

```bash
helm install cxi deploy/helm/cxi-dra-driver -f my-site-values.yaml
```

Most-used values (full list in the [chart README](deploy/helm/cxi-dra-driver/README.md)):

| Value | Default | Purpose |
|---|---|---|
| `driverName` | `cxi.slingshot.hpe.com` | DRA driver name; drives every owned object name |
| `workload.kind` | `DaemonSet` | `DaemonSet` (one pod per node) or `Deployment` |
| `workload.replicas` | `10` | replica count (Deployment only) |
| `image.repository` / `image.tag` | `ghcr.io/bristol-supercomputing/cxi-dra` / `main` | driver image (pin a commit-sha or `v*` tag for a reproducible install) |
| `driver.vniRange.start` / `.end` | `100` / `300` | per-namespace VNI pool |
| `driver.systemSvcIDs` | `[1,2,3,4]` | blessed system services (never reaped; their usage is reserved) |
| `admissionPolicy.excludedNamespaces` | `[kube-system]` | namespaces the policy ignores |
| `resourceClaimTemplates.namespaces` | `[default]` | namespaces to render the templates into (one copy each) |

Because every object keys off `driverName`, a second install under a different name is fully
isolated, and pod anti-affinity keeps the two versions on different nodes — install once more for
A/B version testing, or switch to Deployment mode:

```bash
helm install cxi-next deploy/helm/cxi-dra-driver --set driverName=cxi.next.hpe.com --set image.tag=next
helm install cxi      deploy/helm/cxi-dra-driver --set workload.kind=Deployment --set workload.replicas=10
```

Dry-run a render before applying to a live cluster:

```bash
helm lint deploy/helm/cxi-dra-driver
helm template t deploy/helm/cxi-dra-driver | kubectl apply --dry-run=client -f -
```

#### With kubectl (alternative)

Set the image in [deploy/kubernetes/daemonset.yaml](deploy/kubernetes/daemonset.yaml), then apply
the env ConfigMap and the core manifests:

```bash
kubectl apply -f deploy/kubernetes/configmap.yaml   # env.json ConfigMap (cxi-dra-driver-env)
make deploy                                          # clusterrole, deviceclass, admission-policy, daemonset
```

`make deploy` applies the `ClusterRole`/RBAC, the `DeviceClass`, the `MutatingAdmissionPolicy`, and
the `DaemonSet` in order. It does **not** apply `configmap.yaml`, which the DaemonSet mounts as
`cxi-dra-driver-env` for `--enable-cdi-env` — apply it first (as above) or the pods will not start.

#### Optional: Slingshot CNI

The driver works without any CNI involvement — pods reach the HSN via `hostNetwork` (and in future via SR-IOV VFs). If you want each CXI-claiming pod to get its own IP-addressed HSN interface instead, enable the bundled [`slingshot-cni`](#hsn-ip-interface-slingshot-cni). It is off by default and everything below renders nothing until you opt in.

Prerequisites not installed by this chart: **Multus** (secondary-network attachment; provides the `NetworkAttachmentDefinition` CRD) and **Whereabouts** (the `whereabouts` binary on each node's `CNI_PATH` plus its CRDs / `ip-reconciler`).

```bash
# cni.install     — driver installs slingshot-cni into /opt/cni/bin on every node at startup
# cni.nad.create  — render a NetworkAttachmentDefinition "slingshot-hsn" into the claim-template namespaces
# cni.nad.ipam.*  — Whereabouts HSN pool (any Whereabouts option goes under cni.nad.ipam)
helm install cxi deploy/helm/cxi-dra-driver \
  --set cni.install=true \
  --set cni.nad.create=true \
  --set cni.nad.ipam.range=10.200.0.0/16
```

| Value | Default | Purpose |
|---|---|---|
| `cni.install` | `false` | install the bundled binary into `cni.binDir` via `--cni-install-dir` (atomic, re-done on every driver start so upgrades roll it; left in place on uninstall) |
| `cni.binDir` | `/opt/cni/bin` | node CNI plugin directory, mounted into the driver pod only when `cni.install=true` |
| `cni.nad.create` | `false` | render the NAD (`cni.nad.name`, into `cni.nad.namespaces` or, if empty, `resourceClaimTemplates.namespaces`) |
| `cni.nad.ipam` | `{type: whereabouts, range: 10.200.0.0/16}` | passed through verbatim as the NAD `ipam` block; `type` is required |

Pods then opt in with `k8s.v1.cni.cncf.io/networks: <namespace>/slingshot-hsn`; pods without the annotation are unaffected. For the kubectl path, uncomment the `--cni-install-dir` / `cni-bin` lines in [deploy/kubernetes/daemonset.yaml](deploy/kubernetes/daemonset.yaml) and apply [deploy/kubernetes/cni/networkattachmentdefinition.yaml](deploy/kubernetes/cni/networkattachmentdefinition.yaml).

### 4. Verify the ResourceSlice

```bash
kubectl get resourceslices -o wide
```

You should see one slice per node with devices `cxi0`, `cxi1`, …, each with `allowMultipleAllocations: true` and a `capacity` entry per CXI resource (`txqs` … `tles`, sized to the usable ceiling). The slice is static: it is published once at driver startup, and how much of each device is in use is tracked by the scheduler in each claim's `consumedCapacity`, not in the slice.

### 5. Run a test workload

A Helm install already created the ResourceClaimTemplates — named `<prefix>-small`, `-medium`, …
where the prefix is the sanitized driver name (e.g. `cxi-slingshot-hpe-com-small`). Each template
is rendered once into **every** namespace in `resourceClaimTemplates.namespaces` (default
`[default]`), so a single install can stamp the templates into all of your tenant namespaces — e.g.
`--set 'resourceClaimTemplates.namespaces={default,team-a,team-b}'`. The singular
`resourceClaimTemplates.namespace` remains as a fallback, used only when that list is empty. For the
kubectl path, apply the raw templates (short names like `cxi-small`) and the example pod instead:

```bash
kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml   # kubectl path only; Helm already renders these
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl get pod cxi-test-3
kubectl exec cxi-test-3 -- fi_info --provider cxi
```

The test pod claims one CXI device via the `cxi-small` template. Its fabric VNI is the one assigned to the `default` namespace by the namespace VNI allocator (visible as the `cxi.slingshot.hpe.com/vni` annotation on the namespace). You should see CXI providers enumerated without `privileged: true`.

## Requesting CXI Resources

Each pod that uses a CXI device needs a `ResourceClaimTemplate` that requests one CXI device. Pods do **not** request a VNI — VNIs are assigned per namespace by the leader-elected namespace VNI allocator and shared across every pod in the namespace, so each tenant gets exactly one cluster allocation that all of its pods communicate over.

Six templates are provided in [deploy/kubernetes/test/resourceclaim.yaml](deploy/kubernetes/test/resourceclaim.yaml) (and rendered by the Helm chart). The four single-device sizes are:

| Template | TXQs | TGQs | LEs | TLEs | Pods/device | Use case |
|---|---|---|---|---|---|---|
| `cxi-small`  | 4    | 2   | 1024 (1 pool)  | 512 (1 pool)  | 3 | Benchmarks, `fi_pingpong`, development, single-threaded tools |
| `cxi-medium` | 16   | 8   | 2048 (2 pools) | 512 (1 pool)  | 3 | Standard MPI, NCCL at moderate scale, typical HPC workloads |
| `cxi-large`  | 64   | 32  | 4096 (4 pools) | 1024 (2 pools) | 1 | Multi-threaded MPI, NCCL at scale, collectives-heavy workloads |
| `cxi-whole`  | 1019 | 507 | 14336 (14 pools) | 1536 (3 pools) | 1 | Exclusive device — single demanding workload taking the whole NIC |

`cxi-2-small` and `cxi-2-whole` request two devices (`count: 2`) with the `small` and `whole` resource profiles respectively.

The `Pods/device` column assumes the device's blessed system services (declared via `--system-svc-ids`) reserve roughly one of its 4 TLE pools, leaving 3 available to claims, and the template values shown above. For mixed-template workloads the binding constraint is whichever resource saturates first; with the current templates `tles` is the first to saturate. If your workloads follow Slingshot's "don't reserve LEs/TLEs unless you need them" guidance and use templates with `tles: "0"`, the per-device pod ceiling is set by some other resource (typically TXQs).

Resources are requested as DRA **consumable capacity**: every device publishes `txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `acs`, `les` and `tles` as capacity, and the scheduler charges each allocation's `capacity.requests` against it, so it never places more on a NIC than its usable ceiling. A resource left out of the request consumes the device default (`txqs` 4, `tgqs` 2, `eqs` 8, `cts` 4, `ptes` 4, `acs` 2, `les`/`tles` 0). Claims written for the old opaque-config API are converted to capacity requests by the `MutatingAdmissionPolicy`; with the policy disabled, the driver rejects any whose opaque config asks for more than was allocated, rather than under-provision them. There are no CDI parameters: every device gets the env spec (see [Host runtime injection](#host-runtime-injection-env)).

### Claim template structure

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: cxi-small
  namespace: default
spec:
  spec:
    devices:
      requests:
        - name: cxi
          exactly:
            deviceClassName: cxi-device
            capacity:
              requests:
                txqs: "4"
                tgqs: "2"
                eqs: "8"
                cts: "4"
                ptes: "4"
                acs: "2"
                les: "1024"             # 1 pool
                tles: "512"             # 1 pool
```

LEs and TLEs are absolute counts, but the hardware allocates them in **pools** (1024 LEs, 512 TLEs). The devices publish a `requestPolicy` step of one pool, so the scheduler rounds a request up to whole pools — `les: "1500"` consumes 2048 — and the allocation's `consumedCapacity` (what the CXI service is programmed with) records the rounded amount.

A value of `0` for `les` or `tles` does **not** mean "no LEs/TLEs available" — the service is mapped to a shared default pool (`DEFAULT_LE_POOL_ID` / `DEFAULT_TLE_POOL_ID` in the kernel rgroup, see `cxi_rgroup.c:425`) which has `max_alloc` set to the device-wide total and `num_reserved=0`. The service can still consume LEs/TLEs from the default pool on a best-effort basis, but has no guaranteed capacity under contention. A value `>0` carves out exclusive reservation for the service. Slingshot's general guidance is to leave both at `0` for ordinary workloads and raise them only when the workload specifically depends on guaranteed receive-buffer capacity (large MPI with steady unexpected traffic) or triggered operations (NCCL allreduce, counter-driven MPI collectives).

### Resource field reference

The per-claim resource values map directly to fields in the per-service `cxi_svc_desc.resource_limits[]` quota in the kernel. The libfabric CXI provider (`shs-libfabric/prov/cxi/`) allocates resources from these quotas on demand via libcxi ioctls; allocations fail (and the application fails) when the quota is exhausted. The "shared default pool" model applies only to `les` and `tles` — all other fields are hard caps.

- **`txqs` — Transmit command queues.** The provider allocates **one shared TX CMDQ per endpoint** (`cxip_ep.c:85`; reused via ref-counting between the data TX context and the control TX path — both write to `ep_obj->txq`). Optional extras: one domain-level trigger CMDQ if any triggered operation is issued; one dedicated counter trigger CMDQ per counter if `FI_CXI_CNTR_TRIG_CMDQ=1`. A value of 2–4 covers a typical single-endpoint job with headroom; jobs that open multiple endpoints from one domain need proportionally more.

- **`tgqs` — Target (receive-side) command queues.** Same pattern as `txqs` — **one shared TGQ per endpoint** (`cxip_ep.c:85` with `transmit=false`), reused between the RX context and the control TGT path via `ep_obj->tgq`. A value of 1–2 is sufficient for a single-endpoint job.

- **`eqs` — Hardware event queues.** The provider allocates **exactly 4 hardware EQs per endpoint** via `cxil_alloc_evtq`: one RX-data EQ (`cxip_rxc.c:130`), one TX-data EQ (`cxip_txc.c:333`), one TX-control EQ and one TGT-control EQ (`cxip_ctrl.c:495`, called twice from `cxip_ep_ctrl_init`). 8 is a safe floor (one endpoint with headroom or two endpoints).

- **`cts` — Hardware counters.** Allocated only when the application explicitly opens a libfabric counter via `fi_cntr_open` (`cxip_cntr.c:730`). Basic point-to-point benchmarks like `fi_pingpong` need zero. NCCL and collective-heavy workloads use them extensively (64–256 is typical).

- **`ptes` — Portal table entries.** The provider allocates **2 unconditional PTEs per endpoint** — one control PTE in `cxip_ctrl.c:570` and one RX PTE in `cxip_msg_hpc.c:3946` (or `cxip_msg_rnr.c:374`) — plus one per memory region, one per rendezvous instance if the rendezvous protocol activates, and one per collective. A value of 2–4 covers single-endpoint jobs; raise for jobs with many MRs or collectives.

- **`acs` — Address contexts.** Allocated implicitly per `cxil_map` call for memory protection / IOMMU translation. The libfabric MR cache (`cxip_iomm.c:368`) aggregates repeated mappings of the same region, so typical jobs need only 2–4 ACs (EQ buffers + send/recv buffers). Frequent buffer reuse minimises AC pressure.

- **`les` — List entries** (allocated in pools of 1024). LEs are consumed by **every** `cxip_pte_append`: control messaging during `cxip_ep_ctrl_init` (`cxip_ctrl.c:206`), every receive append in the HPC/RNR paths (`cxip_msg_hpc.c:4218`, `cxip_msg_rnr.c:169`), every memory region (`cxip_mr.c`), rendezvous (`cxip_rdzv_pte.c`), and collectives (`cxip_coll.c`). See the shared-default-pool note above — a value of `0` means "use the shared default LE pool on a best-effort basis"; `>0` reserves whole pools exclusively for guaranteed capacity.

- **`tles` — Triggered list entries** (allocated in pools of 512). TLEs are consumed only by triggered commands — `cxi_cq_emit_trig_*` in `cxip_dom.c:2013, 2073, 2086` and counter-triggered events in `cxip_cntr.c:572`. A workload without triggered operations (e.g. `fi_pingpong`) needs zero. With `tles: "0"`, any triggered operations that do occur draw from the shared default pool on a best-effort basis; `>0` reserves dedicated TLE capacity.

### Host runtime injection (env)

Like the NVIDIA DRA driver, the driver injects the host runtime it owns with every device and leaves nothing to choose per claim. Each prepared CXI device gets its own `/dev/cxiN` spec plus the shared `env` CDI spec (referenced once per claim, however many devices it has). The env spec carries:

- `/dev/kdreg2` (libfabric's MR-cache monitor), `/dev/gdrdrv` (GDRCopy) and `/dev/xpmem` device nodes, each only when present on the host, and a `/dev/shm` bind — **always**, even without `--enable-cdi-env`;
- with `--enable-cdi-env` and `--env-config`: the listed host libraries (libfabric, libcxi and their dependencies), bind-mounted read-only at `/host/env/<basename>` — nothing is written over the image's own files — plus the listed env vars.

A `createContainer` hook (`cxi-cdi-hook update-ldcache --folder /host/env`) rebuilds the container's `ld.so.cache` with `/host/env` included. The driver installs `cxi-cdi-hook` and a self-contained `ldconfig` (with its dynamic linker and libc) onto each node at startup (`--cdi-hook-path`, `--cdi-ldconfig-dir`; see [internal/cdi/hookinstall.go](internal/cdi/hookinstall.go)), so **nothing needs to be pre-installed on the node** — no nvidia-container-toolkit. The driver does **not** touch `LD_LIBRARY_PATH`, which the loader consults before the ldcache; an image that points it at a bundled libfabric (NGC: `/opt/amazon/efa/lib`) is left to k8s-modules (below).

**Not provided here:**

- **NCCL and aws-ofi-nccl** (libraries, tuning env vars, `LD_LIBRARY_PATH` handling) are injected by **k8s-modules**, which defines a cluster-wide `Module` custom resource. Pods load modules by annotation:

  ```yaml
  metadata:
    annotations:
      modules.brics.ac.uk/load: aws-ofi-nccl-1.18.0   # comma-separated; a family name loads its default
  ```

  A claim that still sets `cdi_profiles` to anything other than `[]` / `["env"]` is rejected with an error pointing there; the field is otherwise deprecated and ignored.

**Configuring** (the `envConfig.json` Helm value / `env.json` ConfigMap):

```json
{
  "ldcache": {"enabled": true},
  "libs":    ["/opt/cray/libfabric/2.3.1/lib64/libfabric.so.1", "/usr/lib64/libcxi.so.1"],
  "env":     ["CXIP_SKIP_AMA_CHECK=1"]
}
```

| Field | Meaning |
|---|---|
| `ldcache` | The ldcache hook; `enabled` defaults to true. The hook and ldconfig locations are driver flags, not config fields. |
| `libs` | Absolute host file paths, mounted at `/host/env/<basename>`. Required, and basenames must be unique. |
| `env` | `KEY=value` env vars. |

> **Note:** `CXIP_SKIP_AMA_CHECK=1` lives in the env config so stock libfabric accepts the synthetic HSN netdev the driver injects (see [HSN Interface Injection](#hsn-interface-injection)). With `--enable-cdi-env` off, a pod still gets the synthetic `/sys/class/net` and the device nodes, but **not** the AMA bypass — it must supply its own libfabric configuration.

### Wiring the claim into a pod

```yaml
spec:
  resourceClaims:
    - name: cxi
      resourceClaimTemplateName: cxi-small
  containers:
    - name: workload
      resources:
        claims:
          - name: cxi
```

See [deploy/kubernetes/test/pod.yaml](deploy/kubernetes/test/pod.yaml) for a complete example.

### Slingshot CNI auto-attachment

When the optional Slingshot CNI network is enabled (`cni.nad.create=true`; the
kubectl path applies [deploy/kubernetes/cni/admission-policy.yaml](deploy/kubernetes/cni/admission-policy.yaml)),
pods that reference a CXI `ResourceClaimTemplate` do not need to add the
Multus network-selection annotation manually. Without it, pods opt in with the
annotation themselves (see [Optional: Slingshot CNI](#optional-slingshot-cni)).

At Pod CREATE admission time, a second `MutatingAdmissionPolicy`
(`<fullname>-slingshot-network-injection`) checks `spec.resourceClaims` for a CXI claim-template name. Helm-rendered
CXI `ResourceClaimTemplate` objects follow the naming convention:

`<fullname>-<template>`

For example:

`cxi-slingshot-hpe-com-small`

When a matching claim is found, the admission policy automatically injects:

```yaml
metadata:
  annotations:
    k8s.v1.cni.cncf.io/networks: <namespace>/slingshot-hsn
``` 

### Why pods carry exactly one ResourceClaim

The driver uses Kubernetes `BindingConditions` to gate pod binding until device capacity is confirmed. When a CXI device is over-subscribed (the scheduler placed more claims on it than it can serve), the ClaimBinder sets `Unschedulable=True` on the device. The scheduler then deallocates the claim and retries on a less-loaded device.

Because each pod carries a single CXI claim — VNI is namespace-scoped, not part of the claim — there is nothing else to leave in a partial-allocation state when the scheduler deallocates and retries. This structurally avoids the `"number of allocations N is smaller than number of claims"` Score error that would arise if VNI were a separate claim that survived the CXI deallocation.

### Multi-pod workloads sharing a VNI

Every pod in a namespace receives the same VNI, so any number of pods in the same namespace can form RDMA connections to each other on the Slingshot fabric without further coordination. This makes a namespace the unit of fabric tenancy: distinct namespaces are isolated; multiple jobs inside one namespace deliberately share the namespace's VNI.

### How scheduler placement works

Each device is published with `allowMultipleAllocations: true` and one consumable `capacity` per CXI resource, valued at the device's usable ceiling (hardware total minus the blessed system services):

```yaml
- name: cxi0
  allowMultipleAllocations: true
  capacity:
    txqs: { value: "1019", requestPolicy: { default: "4", validRange: { min: "0" } } }
    les:  { value: "14336", requestPolicy: { default: "0", validRange: { min: "0", step: "1024" } } }
    # … tgqs, eqs, cts, ptes, acs, tles
```

The scheduler allocates a claim to a device only if, for every resource, the request (rounded up by the `requestPolicy`) plus what earlier allocations of that device consumed fits within `value`. It records the amount in the claim's `status.allocation.devices.results[].consumedCapacity` alongside a per-allocation `shareID`, and the driver programs the CXI service from `consumedCapacity`. The accounting is atomic inside the scheduler, so a batch of claims cannot over-concentrate onto one NIC.

There is no per-namespace exclusion: SHS14's duplicate-VNI constraint has been lifted (the kernel now permits multiple services sharing a VNI on one device), so any number of claims from the same namespace may co-locate on a device as long as its capacity allows. Fabric tenancy is still namespace-scoped — every pod in a namespace shares the namespace's VNI — but that no longer restricts placement.

The scheduler only sees what Kubernetes allocated. The `ClaimBinder` is the safety net for everything else — CXI services created outside Kubernetes, and claims allocated before the devices published capacity (their `consumedCapacity` is empty, so the scheduler does not count them):

1. The scheduler allocates a claim; because the device publishes `bindingConditions: [Ready]` the pod is not bound yet.
2. The `ClaimBinder` evaluates the allocation against live hardware capacity. It grants `Ready=True` to claims that fit within current capacity minus in-flight pending grants.
3. Claims that **would** fit the device's raw hardware capacity but not after subtracting in-flight pending grants (grants already made on this device whose CXI service is not yet created) receive `Ready=False` with reason `InsufficientCapacity`. This is a *hold*, not a deallocation: the binder re-evaluates it on a 2-second ticker and immediately whenever a service is torn down (`TriggerRetry`).
4. Claims that exceed the device's hardware capacity even before counting pending grants receive `Unschedulable=True` with reason `DeviceAtCapacity`.
5. The scheduler sees `Unschedulable=True` in the claim's `BindingFailureConditions`, deallocates the single CXI claim, and retries. Its accounting does not include the out-of-band consumption that filled the device, so the retry may pick the same device until that consumption goes away.

With only Kubernetes-managed services on the NIC, steps 3–5 do not occur: the scheduler's accounting already guarantees the hardware fits.

In-flight pending grants are released by two independent, idempotent paths:

1. **NRI `RunPodSandbox`** — calls `RemovePending(uid)` once the CXI service backing the claim has been created, so the grant is now reflected in the live hardware capacity and no longer needs a pending reservation.
2. **`ClaimBinder` watch** — calls `RemovePending(uid)` when the scheduler clears `Status.Allocation` (PostFilter after a PreBind failure, Unschedulable signal, manual edit) or when a `ResourceClaim` is deleted. This catches scheduler-side de-allocation that would never reach NRI.

Actual hardware capacity is always read live from the device (`cxil_get_svc_list`), so once a service exists the pending reservation is redundant; the pending map exists only to bridge the window between a grant and its service creation within a single scheduling batch.

## Accounting for Pre-existing System Services

If a node has CXI services created by the OS or system software before the driver starts (e.g. the default service and the Ethernet service(s)), their resource consumption must be accounted for so the driver does not overcommit a device.

List their svc_ids in the `--system-svc-ids` flag on the DaemonSet (comma-separated, applied uniformly to every device on the node). Obtain them from `cxi_service list` on a node:

```yaml
# In daemonset.yaml, under containers[0].args:
- --system-svc-ids=1,2,3,4
```

At discovery time the driver reads each blessed service's actual `resource_limits[]` from the kernel and sums them into a per-device reserved set. The usable ceiling for claims is then `hardware_total − reserved`. A claim requesting more than the usable ceiling is rejected immediately with a "request will never fit on this hardware" error, before any libcxi call is attempted.

Blessed services are also protected from teardown: the NRI plugin and the orphan-service reaper never delete a service whose svc_id is in this set, so a stale in-memory mapping can never reap a system service. An empty list (the default) blesses nothing — the driver reserves no capacity and may reap pre-existing services it does not recognise.

## How Device Injection Works

Unprivileged containers require two things to open `/dev/cxiN`:

1. **The device node** present in the container's `/dev/`
2. **A cgroup BPF allowance** permitting access to the character device major:minor

Both are handled via CDI. The driver writes a CDI spec to `/var/run/cdi/` at startup containing a `DeviceNodes` entry for each CXI device. containerd resolves CDI devices during container spec construction — before the cgroup BPF program is compiled — so the allowance is baked in from the start. This is why NRI alone (which fires after cgroup policy is locked in) is insufficient for device access.

The CXI service (VNI scoping + netns binding) is created in the NRI `RunPodSandbox` hook, which fires after the pod network namespace is ready but before any container starts. The netns inode is obtained via `/proc/<sandbox-pid>/ns/net` + `fstat`, which returns the real nsfs inode. Note: `os.Stat` on the netns bind-mount path (e.g. `/var/run/netns/cni-...`) returns the wrong inode — the underlying tmpfs inode rather than the nsfs inode that the CXI kernel driver uses to scope services.

## HSN Interface Injection

Stock (unpatched) libfabric's CXI provider enumerates HSN devices by reading their `/sys/class/net/<hsn>` metadata. Inside a pod the HSN interfaces (`hsn0`, `hsn1`, …) live in the host network namespace and are not visible, so libfabric warns `CXI netdev not found` and fails to bootstrap. The driver fixes this **without patching libfabric and without exposing a real netdev** — the goal is zero libfabric patches.

In `RunPodSandbox` the NRI plugin builds a per-pod synthetic `/sys/class/net` tree under `--netns-sysfs-root` (`/var/run/cxi/netns/<podUID>/net`); in `CreateContainer` it bind-mounts that tree read-only over the container's `/sys/class/net`. The tree contains:

- a symlink per real pod NIC, enumerated by entering the pod netns and mounting a fresh sysfs, so the pod keeps visibility of its own interfaces; and
- one `hsnN/` directory per allocated HSN with exactly the metadata libfabric reads — `operstate=up`, `carrier=1`, `speed`, and `device → …/<BDF>` (the BDF of the paired CXI device, matched via PCIe topology).

Only `/sys/class/net` is overmounted; `/sys/devices` is left untouched, so the replicated NIC symlinks still resolve. The HSN presents **only** as a sysfs directory under its host-native name — the synthetic entry is not a real netdev, so on its own `ip link`, `ethtool`, netlink, and IP addressing show nothing for it. This is sufficient for libfabric, which globs sysfs rather than using netlink, and the RDMA datapath is kernel-bypass and needs no netdev. Pods that also need **IP** over the HSN get a real netdev from the separate [Slingshot CNI plugin](#hsn-ip-interface-slingshot-cni), which runs alongside this mechanism.

Two libfabric gates interact with the tree. The link gate (`cxip_if.c:142`) has no env bypass and is satisfied by `operstate=up`. The AMA gate (`cxip_if.c:580`) drops any device whose netdev lacks a recognised MAC unless `CXIP_SKIP_AMA_CHECK=1` is set; the synthetic tree omits a MAC, so the env CDI spec supplies that env var (see [Host runtime injection](#host-runtime-injection-env)) — it is deliberately **not** hardcoded in NRI.

Injection is skipped in mock mode and when `--netns-sysfs-root` is empty. Reading the host HSN metadata is fatal (the services are rolled back and the kubelet retries the sandbox) because libfabric cannot bootstrap without it; replicating the pod's own NICs is best-effort and falls back to an HSN-only tree on failure.

## HSN IP Interface (Slingshot CNI)

The sysfs injection above is what stock libfabric needs; it deliberately does not give the pod an IP-addressed netdev on the HSN. Workloads that also need IP over the fabric (out-of-band launchers, control-plane traffic, tooling) get one from **`slingshot-cni`** ([cmd/slingshot-cni](cmd/slingshot-cni), [internal/cni](internal/cni)) — a standalone host CNI plugin exec'd by containerd/Multus, separate from the driver DaemonSet. The two mechanisms are complementary and both run for a pod that has the network attached.

How it works, per pod:

1. `PrepareResourceClaims` writes a **hint file** `<--cni-hints-dir>/<podUID>.json` with one `{device, hsn_iface, vni}` entry per claimed device — the only handoff from the driver daemon to the CNI process. Kubelet waits for prepare to return before `RunPodSandbox`, so the file always exists before CNI `ADD` fires ([contract](docs/cni/cnidra-cni-hint-contract.md)).
2. On CNI **`ADD`**, for every entry: an **L2 IPVLAN** sub-interface is created on the paired host HSN, moved into the pod netns as `<hsn_iface>` (e.g. `hsn2`; falls back to `hsn2_inet` if that name is taken), and given an address by the **Whereabouts** IPAM plugin from a cluster-wide HSN pool. A `count: 2` claim yields two interfaces and two reservations. Any partial failure is fully unwound.
3. On CNI **`DEL`**, each reservation is released and each interface removed — best-effort and idempotent.

Minimal `NetworkAttachmentDefinition` config (full field reference in [docs/cni/cni-plugin.md](docs/cni/cni-plugin.md)):

```json
{
  "cniVersion": "1.0.0",
  "name": "slingshot-hsn",
  "type": "slingshot-cni",
  "ipam": { "type": "whereabouts", "range": "10.200.0.0/16" }
}
```

`ipam.type` is required. Node prerequisites: `slingshot-cni` on `CNI_PATH` (installed by the driver when `cni.install=true` / `--cni-install-dir` is set, or by hand), `whereabouts` on `CNI_PATH` plus Whereabouts's cluster-side install, Multus (or equivalent) to attach the network, and the driver's `--cni-hints-dir` matching the config's `hintsDir` (both default to `/var/run/cxi/net-hints`). Inside the container the IPVLAN link is visible via `ip link`/`ip addr` but not under the overmounted `/sys/class/net` — that is expected. Install details: [Optional: Slingshot CNI](#optional-slingshot-cni).

## CXI Service Programming

Each pod receives a CXI service scoped to its network namespace inode and its namespace's VNI. libfabric reads `SLINGSHOT_SVC_IDS` (injected as an env var by the NRI plugin) and uses it instead of allocating its own service, ensuring the pod operates within its allocated fabric resources. `SLINGSHOT_DEVICES` and `SLINGSHOT_VNIS` carry the corresponding device name and VNI, aligned by position.

### Namespace-scoped VNI allocation

VNIs are assigned per **namespace** by a leader-elected controller that runs inside the DaemonSet. One DaemonSet pod wins a `Lease`-based election (`<driver-name>-vni-allocator-leader`, e.g. `cxi.slingshot.hpe.com-vni-allocator-leader`, in the driver's own namespace) and runs a namespace watcher that scans for the `cxi.slingshot.hpe.com/vni` annotation. Any namespace that lacks one is patched with the lowest free VNI in the configured cluster-wide range.

When a pod is scheduled and the kubelet calls `PrepareResourceClaims`, the driver reads the VNI from the pod's namespace annotation and programs it into the CXI service it creates. Every pod in the same namespace therefore shares the same VNI and can form RDMA connections to every other pod in that namespace. Namespaces are isolated from each other on the fabric.

The VNI range is configured via `--vni-range-start` / `--vni-range-end` in the DaemonSet (default `0` disables the allocator, in which case the driver assumes namespaces are annotated by some other authority). The skip list (`kube-system`, `kube-public`, `kube-node-lease`) is hardcoded — system namespaces never receive a VNI.

Pre-existing annotations are respected: if a namespace already has a valid `cxi.slingshot.hpe.com/vni` annotation, the allocator leaves it untouched. This lets operators pre-assign VNIs to particular tenants.

## Project Layout

```
cmd/cxi-dra-driver/      Binary entrypoint and CLI flags
cmd/cxi-cdi-hook/        Standalone OCI createContainer hook binary (ldconfig cache refresh)
cmd/slingshot-cni/       Slingshot CNI plugin binary (host-side; IPVLAN-on-HSN + Whereabouts IPAM)
internal/cxi/            Sysfs device discovery, CXI↔HSN pairing, libcxi CGo wrapper
internal/cdi/            CDI spec generation (DeviceNodes; shared env spec; containerd CDI config drop-in), host install of cxi-cdi-hook/ldconfig/slingshot-cni
internal/cni/            slingshot-cni logic: hint-file read, L2 IPVLAN on the HSN, Whereabouts IPAM delegation
internal/netsysfs/       Per-pod synthetic /sys/class/net tree builder for HSN sysfs injection
internal/driver/         DRA kubelet plugin (static consumable-capacity ResourceSlice, Prepare/Unprepare,
                         ClaimBinder, namespace VNI allocator, orphan-service reaper, CNI hint writer, store)
internal/nri/            NRI plugin (RunPodSandbox, CreateContainer, StopPodSandbox)
deploy/kubernetes/       DaemonSet, DeviceClass, RBAC, MutatingAdmissionPolicy, ConfigMap, test manifests; cni/ holds the optional NAD + network-injection policy
deploy/helm/             Helm chart (cxi-dra-driver)
deploy/kind/             kind mock-mode test harness (no hardware)
docs/                    pod-lifecycle-flow.txt, cni/ (plugin reference, driver-to-CNI hint contract)
```

## Configuration Flags

| Flag | Default | Description |
|---|---|---|
| `--driver-name` | `cxi.slingshot.hpe.com` | DRA driver identifier |
| `--node-name` | `$NODE_NAME` | Kubernetes node name |
| `--pod-uid` | `$POD_UID` | Pod UID for rolling-update socket uniqueness |
| `--pod-namespace` | `$POD_NAMESPACE` | Namespace this pod runs in, used for the VNI-allocator leader-election lease |
| `--cdi-root` | `/var/run/cdi` | CDI spec output directory |
| `--plugin-dir` | `/var/lib/kubelet/plugins/cxi.slingshot.hpe.com` | Kubelet plugin socket directory |
| `--registrar-dir` | `/var/lib/kubelet/plugins_registry` | Plugin registrar directory |
| `--kubeconfig` | `""` | Path to a kubeconfig (empty uses in-cluster config) |
| `--mock-devices` | `0` | Generate N synthetic devices (no hardware needed) |
| `--resource-refresh-interval` | `10s` | How often the orphan-service reaper checks for CXI services whose pod is gone |
| `--vni-range-start` | `0` | First VNI in the namespace-VNI allocator's pool (0 disables the allocator) |
| `--vni-range-end` | `0` | Last VNI in the pool (inclusive) |
| `--system-svc-ids` | `""` | Comma-separated svc_ids of blessed system services: never deleted, and their reserved resources are deducted from each device's usable ceiling |
| `--vf-only` | `false` | Discover and publish SR-IOV VFs only; PFs are skipped before any libcxi read, so a PF is never opened. The default publishes PFs only and skips VFs (`device/physfn` present); the two are never mixed. With no usable VF the driver logs an error and publishes an empty slice. `--system-svc-ids` is ignored (with a warning), because its PF svc_ids would match pod services on a VF. In VF mode the `ResourceSlice` publishes one device per parent service, `<pf>-svc<id>`, with the parent's budget less one Ethernet-child reservation per VF (LEs/TLEs 0); at Prepare each claim result is given the lowest free VF of its parent, which the pod's CDI device, NRI service and CNI hint use. A parent with no free VF fails Prepare (kubelet retries). Part of SR-IOV; do not use it in production yet |
| `--netns-sysfs-root` | `/var/run/cxi/netns` | Host directory for the per-pod synthetic `/sys/class/net` trees (empty disables HSN sysfs injection) |
| `--cni-hints-dir` | `/var/run/cxi/net-hints` | Host directory for per-pod CNI hint files read by `slingshot-cni` (empty disables the hint-file write/delete); must match the `hintsDir` in the plugin's network config. See [docs/cni/cnidra-cni-hint-contract.md](docs/cni/cnidra-cni-hint-contract.md) |
| `--cni-install-dir` | `""` (disabled) | Host CNI plugin directory (e.g. `/opt/cni/bin`, must be a hostPath mounted at the same path) to install the bundled `slingshot-cni` into at startup; requires `--cni-hints-dir`. Opt-in — the driver works without the CNI |
| `--cni-src` | `/slingshot-cni` | Path of the `slingshot-cni` binary bundled in the image |
| `--enable-cdi-env` | `false` | Add the `--env-config` host libs and env vars to the env CDI spec (the spec itself is always attached) |
| `--env-config` | `""` | Path to the JSON CDI env config file (required when `--enable-cdi-env`) |
| `--containerd-config-path` | `""` | Path to write the containerd CDI config drop-in (empty disables) |
| `--cdi-hook-src` | `/cxi-cdi-hook` | Path of the `cxi-cdi-hook` binary bundled in the image |
| `--cdi-hook-path` | `/var/run/cxi/bin/cxi-cdi-hook` | Host path the hook is installed at (installed only with `--enable-cdi-env`); referenced by the generated CDI hooks |
| `--cdi-ldconfig-dir` | `/var/run/cxi/bin` | Host directory the bundled ldconfig runtime (ldconfig, dynamic linker, libc) is installed into (only with `--enable-cdi-env`) |
| `--cdi-ldconfig-src` / `--cdi-ldconfig-linker-src` / `--cdi-ldconfig-libc-src` | `/ldconfig-runtime/ldconfig` / `/ldconfig-runtime/ld.so` / `/ldconfig-runtime/libc.so.6` | Paths of the ldconfig runtime files bundled in the image |

## Device Attributes

Each device in the `ResourceSlice` exposes the following driver-specific attributes, accessible in CEL selectors as `device.attributes["cxi.slingshot.hpe.com"]["<name>"]`:

| Attribute | Type | Description |
|---|---|---|
| `type` | string | Always `"device"`; matched by the DeviceClass selector |
| `hsn_iface` | string | Associated HSN network interface (e.g. `"hsn2"`) |

The per-resource `capacity_*` (hardware total) and `avail_*` (live headroom) attributes are no longer published: placement is governed by consumable capacity below, and the `ClaimBinder` reads live headroom from the hardware directly. To see how much of a device is allocated, sum `consumedCapacity` across the claims allocated to it.

Each device also publishes consumable capacity, requested under `spec.devices.requests[].exactly.capacity.requests`. Values are the usable ceiling (hardware total minus the `--system-svc-ids` reservation):

| Capacity | Default per allocation | Step | Description |
|---|---|---|---|
| `txqs` | 4 | — | Transmit command queues |
| `tgqs` | 2 | — | Target command queues |
| `eqs` | 8 | — | Event queues |
| `cts` | 4 | — | Counters |
| `ptes` | 4 | — | Portal table entries |
| `acs` | 2 | — | Address contexts |
| `les` | 0 | 1024 | List entries (one LE pool per step) |
| `tles` | 0 | 512 | Triggered list entries (one TLE pool per step) |

Each device also exposes the Kubernetes [standard device attributes](https://kubernetes.io/docs/reference/node/dra-standard-device-attributes/) under the `resource.kubernetes.io` domain, letting CEL selectors and `matchAttribute` constraints compare CXI devices against devices from other DRA drivers without driver-specific knowledge. `numaNode` is omitted when the device's NUMA affinity is unknown:

| Attribute | Type | Description |
|---|---|---|
| `resource.kubernetes.io/numaNode` | int | NUMA node the device is attached to |
| `resource.kubernetes.io/pciBusID` | string | PCI bus address in extended BDF form (e.g. `"0000:81:00.0"`) |

Example — claim a device on NUMA node 1 with a custom CEL selector:

```yaml
devices:
  requests:
    - name: cxi
      exactly:
        deviceClassName: cxi-device
        selectors:
          - cel:
              expression: 'device.attributes["resource.kubernetes.io"].numaNode == 1'
```

## Mock Mode (kind / no hardware)

The driver supports `--mock-devices=N` which generates N synthetic CXI devices without real hardware. The full scheduling → allocation → prepare loop runs normally.

```bash
# Create a kind cluster with DRA enabled
kind create cluster --name cxi-dra-dev --config deploy/kind/kind-config.yaml

# Build and load image (no CGo needed in mock mode)
podman build -f Dockerfile.mock -t cxi-dra-driver:dev .
kind load docker-image cxi-dra-driver:dev --name cxi-dra-dev

# Deploy
kubectl apply -f deploy/kubernetes/clusterrole.yaml
kubectl apply -f deploy/kubernetes/deviceclass.yaml      # CXI device class
kubectl apply -f deploy/kubernetes/admission-policy.yaml
kubectl apply -f deploy/kubernetes/daemonset.yaml        # add --mock-devices=2, remove nodeSelector

# Verify the CXI ResourceSlice
kubectl get resourceslices -o wide

# Run the test workload (single CXI-only claim — VNI comes from the namespace)
kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl logs cxi-test-3
```

## Known Limitations / Planned Work

- **Hot-plug** — no fsnotify watcher on `/sys/class/cxi_user`; driver restart required if devices appear/disappear
- **VNI exhaustion handling** — if the configured `[--vni-range-start, --vni-range-end]` is exhausted, the namespace VNI allocator logs the error per reconcile attempt but does not emit an Event on the affected namespace; operators must read the DaemonSet logs to find out why new namespaces aren't getting a VNI

## Troubleshooting

### `fi_info -p cxi` returns `-61 (FI_ENODATA)`

Check in order:

1. **Device accessible** — run inside the pod:
   ```bash
   python3 -c "import os; os.open('/dev/cxi0', os.O_RDWR)"
   ```
   If EPERM, CDI device injection failed — check `/var/run/cdi/` specs have `deviceNodes` entries.

2. **Service netns correct** — on the host:
   ```bash
   cxi_service list -v
   ```
   The `netns=` value should match the pod's netns inode:
   ```bash
   stat /var/run/netns/cni-<id>   # Inode field
   ```

3. **NRI fired**:
   ```bash
   kubectl logs -n kube-system daemonset/cxi-dra-driver
   ```
   Should show `cxi-nri: creating CXI service` lines when the pod started.

4. **Env vars injected** — inside the pod:
   ```bash
   env | grep SLINGSHOT
   ```
   Should show `SLINGSHOT_DEVICES=cxi0`, `SLINGSHOT_VNIS=<n>`, `SLINGSHOT_SVC_IDS=<n>`.

### Claim rejected: "opaque config requests … exceed the scheduler-allocated capacity"

The claim still carries legacy opaque resource config, but it was allocated without matching capacity requests, so the scheduler charged only the defaults. This happens when the `MutatingAdmissionPolicy` is disabled or did not match the claim (it only converts `requests[0]` using `config[0]`). Move the resources to `spec.devices.requests[].exactly.capacity.requests` — see [deploy/kubernetes/test/resourceclaim.yaml](deploy/kubernetes/test/resourceclaim.yaml).

### Claim rejected: "has neither consumed capacity nor CXI opaque resource config"

The claim was allocated against a device that published no capacity (a driver older than consumable-capacity support), and it has no opaque config to fall back on. Recreate the claim once every node runs the current driver.

### Claim rejected: "exceed usable ceiling"

The requested resources exceed `hardware_total - reserved_by_blessed_services`, where the reserved amount is read from the live `resource_limits` of the services listed in `--system-svc-ids`. Either reduce the request, trim a blessed service's reservation, or drop it from `--system-svc-ids` if it should not be counted as overhead.

### EPERM opening `/dev/cxi0`

The CDI spec must contain `deviceNodes` for containerd to add the cgroup BPF allowance. Verify:

```bash
cat /var/run/cdi/cxi.slingshot.hpe.com-cxi-cxi0.json | grep -A8 deviceNodes
```

### Wrong netns inode in CXI service

The driver uses `/proc/<sandbox-pid>/ns/net` + `fstat` to obtain the nsfs inode. `os.Stat` on the netns bind-mount path returns the underlying tmpfs inode, not the nsfs inode — ensure you are running a current image.

### NRI plugin not firing

Verify NRI is enabled in containerd config and the `/var/run/nri/` socket directory is mounted into the driver pod. Check logs for `Started plugin 00-cxi-dra-driver`. If the pod was already running when the driver restarted, delete and recreate it — NRI only fires for new container creation events.
