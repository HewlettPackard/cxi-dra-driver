# cxi-dra-driver

A Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) node driver for HPE Slingshot CXI (Cray eXascale Interconnect) PCIe devices.

The driver runs as a DaemonSet on each Slingshot-equipped node. It discovers CXI devices via sysfs, publishes them as a `ResourceSlice` so the scheduler can allocate them to pods, programs a scoped CXI service (VNI + netns) via libcxi, and injects each device into its container via CDI (Container Device Interface) and NRI (Node Resource Interface).

## Architecture

```
Scheduler ──allocates──► ResourceSlice (cxi0, cxi1, …)
                              │
                         DaemonSet (this driver)
                         ├── discovers /sys/class/cxi_user/*
                         ├── pairs CXI↔HSN via sysfs PCIe topology
                         ├── writes CDI specs → /var/run/cdi/
                         │     ├── DeviceNodes: /dev/cxiN  (cgroup BPF allowance)
                         │     └── env/nccl/aws-ofi-nccl profile specs (libraries + env
                         │         vars incl. CXIP_SKIP_AMA_CHECK; opt-in via cdi_profiles)
                         ├── ClaimBinder
                         │     └── gates pod binding via DRA BindingConditions
                         │         (Ready=True only when capacity is confirmed
                         │          and (namespace, device) slot is free)
                         ├── Polling reconciler
                         │     └── reconciles in-memory occupancy with live
                         │         CXI services every refresh tick — recovers
                         │         from driver restart, missed events, and
                         │         force-deleted pods
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

MutatingAdmissionPolicy (in-process in API server)
    └── intercepts ResourceClaim CREATE
          └── injects CEL selectors from opaque config parameters
                (e.g. avail_txqs >= 256 && avail_tles >= 512)
                so the scheduler bin-packs onto devices with sufficient headroom
```

## Requirements

- Kubernetes 1.34+ (stable `resource.k8s.io/v1` DRA API; `MutatingAdmissionPolicy` requires 1.32+)
- Nodes labelled `feature.node.kubernetes.io/cxi=true`
- Container runtime with CDI and NRI support (containerd ≥ 2.0)
- NRI enabled in containerd config (see below)
- Go 1.24+ and CGo toolchain with libcxi headers (to build with real hardware support)

## Building

The binary must be built on an ARM64 Slingshot node (e.g. `nid001040`) because it links against libcxi via CGo.

### 1. Copy host libraries into `bin/`

The image is built `FROM scratch` and bundles all required shared libraries. Copy them from the host before each build:

```bash
cp /usr/lib64/libcxi.so.1       bin/
cp /usr/lib64/libnl-3.so.200    bin/
cp /lib64/libresolv.so.2        bin/
cp /lib64/libc.so.6             bin/
cp /lib/ld-linux-aarch64.so.1   bin/
```

These paths are for RHEL/SUSE-based Slingshot nodes with glibc 2.38 on ARM64. Adjust if your node layout differs.

### 2. Build the binary

```bash
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build -tags cxi -o bin/cxi-dra-driver ./cmd/cxi-dra-driver
```

### 3. Build and push the container image

```bash
podman-hpc build -t docker.io/<your-registry>/cxi-dra-driver:dev .
podman-hpc push docker.io/<your-registry>/cxi-dra-driver:dev
```

Reference this image at install time — `--set image.repository=…,image.tag=…` for Helm, or the `image:` field in [deploy/kubernetes/daemonset.yaml](deploy/kubernetes/daemonset.yaml) for the kubectl path.

> **Note:** The `Dockerfile` uses `FROM scratch` with a linker-based entrypoint to avoid glibc version conflicts between build and runtime environments:
> ```
> ENTRYPOINT ["/lib/ld-linux-aarch64.so.1", "--library-path", "/usr/lib64:/lib64", "/cxi-dra-driver"]
> ```

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
| `image.repository` / `image.tag` | `docker.io/pirese/cxi-dra-driver` / `dev` | driver image |
| `driver.vniRange.start` / `.end` | `100` / `300` | per-namespace VNI pool |
| `driver.systemSvcIDs` | `[1,2,3,4]` | blessed system services (never reaped; their usage is reserved) |
| `admissionPolicy.excludedNamespaces` | `[kube-system]` | namespaces the policy ignores |

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

### 4. Verify the ResourceSlice

```bash
kubectl get resourceslices -o wide
```

You should see one slice per node with devices `cxi0`, `cxi1`, … and attributes including `avail_txqs`, `avail_tles`, etc. which are refreshed periodically.

### 5. Run a test workload

A Helm install already created the ResourceClaimTemplates — named `<prefix>-small`, `-medium`, …
where the prefix is the sanitized driver name (e.g. `cxi-slingshot-hpe-com-small`) — in the
`resourceClaimTemplates.namespace` (`default` by default). For the kubectl path, apply the raw
templates (short names like `cxi-small`) and the example pod instead:

```bash
kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml   # kubectl path only; Helm already renders these
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl get pod cxi-test-3
kubectl exec cxi-test-3 -- fi_info --provider cxi
```

The test pod claims one CXI device via the `cxi-small` template. Its fabric VNI is the one assigned to the `default` namespace by the namespace VNI allocator (visible as the `cxi.slingshot.hpe.com/vni` annotation on the namespace). You should see CXI providers enumerated without `privileged: true`.

## Requesting CXI Resources

Each pod that uses a CXI device needs a `ResourceClaimTemplate` that requests one CXI device. Pods do **not** request a VNI — VNIs are assigned per namespace by the leader-elected namespace VNI allocator and shared across every pod in the namespace, so each tenant gets exactly one cluster allocation that all of its pods communicate over.

Four templates are provided in [deploy/kubernetes/test/resourceclaim.yaml](deploy/kubernetes/test/resourceclaim.yaml):

| Template | TXQs | TGQs | LE pools | TLE pools | Pods/device | Use case |
|---|---|---|---|---|---|---|
| `cxi-small`  | 4    | 2   | 1 (1024 LEs)  | 1 (512 TLEs)  | 3 | Benchmarks, `fi_pingpong`, development, single-threaded tools |
| `cxi-medium` | 16   | 8   | 2 (2048 LEs)  | 1 (512 TLEs)  | 3 | Standard MPI, NCCL at moderate scale, typical HPC workloads |
| `cxi-large`  | 64   | 32  | 4 (4096 LEs)  | 2 (1024 TLEs) | 1 | Multi-threaded MPI, NCCL at scale, collectives-heavy workloads |
| `cxi-whole`  | 1019 | 507 | 14 (14336 LEs) | 3 (1536 TLEs) | 1 | Exclusive device — single demanding workload taking the whole NIC |

The `Pods/device` column assumes the device's blessed system services (declared via `--system-svc-ids`) reserve roughly one of its 4 TLE pools, leaving 3 available to claims, and the template values shown above. For mixed-template workloads the binding constraint is whichever resource saturates first; with the current templates `tle_pools` is the first to saturate. If your workloads follow Slingshot's "don't reserve LEs/TLEs unless you need them" guidance and use templates with `tle_pools: 0`, the per-device pod ceiling is set by some other resource (typically TXQs).

All resource fields **plus `cdi_profiles`** are required — the driver has no defaults. Claims without an opaque config block (or with any field missing) will be rejected at prepare time with a clear error.

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
        # CXI device request. The MutatingAdmissionPolicy injects avail_*
        # capacity selectors into requests[0].
        - name: cxi
          exactly:
            deviceClassName: cxi-device
      config:
        - requests: ["cxi"]
          opaque:
            driver: cxi.slingshot.hpe.com
            parameters:
              txqs: 4
              tgqs: 2
              eqs: 8
              cts: 4
              ptes: 4
              acs: 2
              le_pools: 1               # 1 pool = 1024 LEs
              tle_pools: 1              # 1 pool = 512 TLEs
              cdi_profiles: ["env"]     # see "CDI Profiles" below
```

LEs and TLEs are expressed as **pool counts** due to hardware allocation granularity:
- `le_pools: 1` reserves 1024 LEs
- `tle_pools: 1` reserves 512 TLEs

A value of `0` for `le_pools` or `tle_pools` does **not** mean "no LEs/TLEs available" — the service is mapped to a shared default pool (`DEFAULT_LE_POOL_ID` / `DEFAULT_TLE_POOL_ID` in the kernel rgroup, see `cxi_rgroup.c:425`) which has `max_alloc` set to the device-wide total and `num_reserved=0`. The service can still consume LEs/TLEs from the default pool on a best-effort basis, but has no guaranteed capacity under contention. A value `>0` carves out exclusive reservation for the service. Slingshot's general guidance is to leave both at `0` for ordinary workloads and raise them only when the workload specifically depends on guaranteed receive-buffer capacity (large MPI with steady unexpected traffic) or triggered operations (NCCL allreduce, counter-driven MPI collectives).

### Resource field reference

The per-claim resource values map directly to fields in the per-service `cxi_svc_desc.resource_limits[]` quota in the kernel. The libfabric CXI provider (`shs-libfabric/prov/cxi/`) allocates resources from these quotas on demand via libcxi ioctls; allocations fail (and the application fails) when the quota is exhausted. The "shared default pool" model applies only to `le_pools` and `tle_pools` — all other fields are hard caps.

- **`txqs` — Transmit command queues.** The provider allocates **one shared TX CMDQ per endpoint** (`cxip_ep.c:85`; reused via ref-counting between the data TX context and the control TX path — both write to `ep_obj->txq`). Optional extras: one domain-level trigger CMDQ if any triggered operation is issued; one dedicated counter trigger CMDQ per counter if `FI_CXI_CNTR_TRIG_CMDQ=1`. A value of 2–4 covers a typical single-endpoint job with headroom; jobs that open multiple endpoints from one domain need proportionally more.

- **`tgqs` — Target (receive-side) command queues.** Same pattern as `txqs` — **one shared TGQ per endpoint** (`cxip_ep.c:85` with `transmit=false`), reused between the RX context and the control TGT path via `ep_obj->tgq`. A value of 1–2 is sufficient for a single-endpoint job.

- **`eqs` — Hardware event queues.** The provider allocates **exactly 4 hardware EQs per endpoint** via `cxil_alloc_evtq`: one RX-data EQ (`cxip_rxc.c:130`), one TX-data EQ (`cxip_txc.c:333`), one TX-control EQ and one TGT-control EQ (`cxip_ctrl.c:495`, called twice from `cxip_ep_ctrl_init`). 8 is a safe floor (one endpoint with headroom or two endpoints).

- **`cts` — Hardware counters.** Allocated only when the application explicitly opens a libfabric counter via `fi_cntr_open` (`cxip_cntr.c:730`). Basic point-to-point benchmarks like `fi_pingpong` need zero. NCCL and collective-heavy workloads use them extensively (64–256 is typical).

- **`ptes` — Portal table entries.** The provider allocates **2 unconditional PTEs per endpoint** — one control PTE in `cxip_ctrl.c:570` and one RX PTE in `cxip_msg_hpc.c:3946` (or `cxip_msg_rnr.c:374`) — plus one per memory region, one per rendezvous instance if the rendezvous protocol activates, and one per collective. A value of 2–4 covers single-endpoint jobs; raise for jobs with many MRs or collectives.

- **`acs` — Address contexts.** Allocated implicitly per `cxil_map` call for memory protection / IOMMU translation. The libfabric MR cache (`cxip_iomm.c:368`) aggregates repeated mappings of the same region, so typical jobs need only 2–4 ACs (EQ buffers + send/recv buffers). Frequent buffer reuse minimises AC pressure.

- **`le_pools` — List-entry pools** (1 pool = 1024 LEs). LEs are consumed by **every** `cxip_pte_append`: control messaging during `cxip_ep_ctrl_init` (`cxip_ctrl.c:206`), every receive append in the HPC/RNR paths (`cxip_msg_hpc.c:4218`, `cxip_msg_rnr.c:169`), every memory region (`cxip_mr.c`), rendezvous (`cxip_rdzv_pte.c`), and collectives (`cxip_coll.c`). See the shared-default-pool note above — a value of `0` means "use the shared default LE pool on a best-effort basis"; `>0` reserves N pools exclusively for guaranteed capacity.

- **`tle_pools` — Triggered list-entry pools** (1 pool = 512 TLEs). TLEs are consumed only by triggered commands — `cxi_cq_emit_trig_*` in `cxip_dom.c:2013, 2073, 2086` and counter-triggered events in `cxip_cntr.c:572`. A workload without triggered operations (e.g. `fi_pingpong`) needs zero. With `tle_pools: 0`, any triggered operations that do occur draw from the shared default pool on a best-effort basis; `>0` reserves dedicated TLE capacity.

### CDI Profiles

`cdi_profiles` is a required list of optional CDI specs to inject into the pod alongside the per-device `/dev/cxiN` node:

| Profile | Contents |
|---|---|
| `env`          | libcxi / libfabric / userspace runtime libraries (`ld.so.cache` refresh) plus env vars (notably `CXIP_SKIP_AMA_CHECK=1`) |
| `nccl`         | NCCL libraries from the configured Spack prefix |
| `aws-ofi-nccl` | AWS-OFI NCCL plugin and tuning env vars |

An empty list (`cdi_profiles: []`) means **no env injection** — use this when the container image already carries libfabric/libcxi (BYO runtime). The driver itself has no defaults; the claim must declare what it needs.

> **Note:** the `env` profile carries the env vars listed under `envVars` in the driver's env config (ConfigMap-driven). `CXIP_SKIP_AMA_CHECK=1` lives here so stock libfabric accepts the synthetic HSN netdev the driver injects (see [HSN Interface Injection](#hsn-interface-injection)). A pod that holds a CXI device but requests no `env` profile still gets the synthetic `/sys/class/net`, but **not** the AMA bypass — such a pod must supply its own libfabric configuration.

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

### Why pods carry exactly one ResourceClaim

The driver uses Kubernetes `BindingConditions` to gate pod binding until device capacity is confirmed. When a CXI device is over-subscribed (the scheduler placed more claims on it than it can serve), the ClaimBinder sets `Unschedulable=True` on the device. The scheduler then deallocates the claim and retries on a less-loaded device.

Because each pod carries a single CXI claim — VNI is namespace-scoped, not part of the claim — there is nothing else to leave in a partial-allocation state when the scheduler deallocates and retries. This structurally avoids the `"number of allocations N is smaller than number of claims"` Score error that would arise if VNI were a separate claim that survived the CXI deallocation.

### Multi-pod workloads sharing a VNI

Every pod in a namespace receives the same VNI, so any number of pods in the same namespace can form RDMA connections to each other on the Slingshot fabric without further coordination. This makes a namespace the unit of fabric tenancy: distinct namespaces are isolated; multiple jobs inside one namespace deliberately share the namespace's VNI.

### How scheduler placement works

When a `ResourceClaim` with opaque CXI config is created, the `MutatingAdmissionPolicy` automatically injects two CEL selectors into the claim. For a `cxi-small` request from namespace `ellis` these are:

```
# Selector 1: per-resource capacity check (one expression with all 8 fields)
device.attributes["cxi.slingshot.hpe.com"]["avail_txqs"] >= 4 &&
device.attributes["cxi.slingshot.hpe.com"]["avail_tles"] >= 512 &&
...

# Selector 2: per-namespace exclusion (literal namespace interpolated at admission time)
!device.attributes["cxi.slingshot.hpe.com"]["active_namespaces"].contains(",ellis,")
```

The scheduler ANDs both selectors. Selector 1 steers placement to devices with capacity for the request. Selector 2 prevents the scheduler from picking a device that already hosts another claim from the same namespace — necessary because SHS14 enforces VNI uniqueness per device at the kernel level (`cxil_alloc_svc` runs the RX-profile `vni_overlap_test` against the device's existing services and rejects a second service whose VNI matches an existing one with `-EEXIST`), and every claim in a namespace shares the namespace's VNI.

The `active_namespaces` attribute is a comma-padded sorted string (e.g. `,alpha,ellis,zeta,`) published per device on every `ResourceSlice` update. The leading and trailing commas let the CEL `.contains(",<NS>,")` perform exact, non-prefix matching — `,foo,` does not match `,foobar,`. An unoccupied device publishes the empty string.

The driver re-publishes the `ResourceSlice` immediately on every occupancy 0↔1 transition (a claim grant flips 0→1; the NRI plugin's `DestroyService` callback flips 1→0). Other claims in the same namespace see the updated `active_namespaces` within milliseconds and are steered away from the now-occupied device — no waiting for the next 10-second periodic refresh.

Because the K8s DRA scheduler evaluates claims against a static snapshot of the `ResourceSlice` within a single scheduling batch, it may still over-concentrate claims onto fewer devices than necessary in one batch (the custom `avail_*` attributes are not decremented between per-claim decisions in the same batch). The driver handles this via **Unschedulable compaction**, which is also the runtime safety net for the rare case where the admission-injected selector raced a stale `ResourceSlice`:

1. The scheduler allocates a batch of claims, possibly concentrating multiple claims on one device.
2. The `ClaimBinder` evaluates each allocation against real hardware capacity **and** against per-namespace device occupancy. It grants `Ready=True` to claims that fit within current capacity minus in-flight pending grants and whose `(namespace, device)` pair is not already held by another claim.
3. Claims that **would** fit the device's raw hardware capacity but not after subtracting in-flight pending grants (grants already made on this device whose CXI service is not yet created) receive `Ready=False` with reason `InsufficientCapacity`. Unlike the Unschedulable cases below, this is a *hold*, not a deallocation: the claim stays on the device and the binder re-evaluates it on a 2-second ticker and immediately whenever a service is torn down (`TriggerRetry`), so a transient pending backlog drains without bouncing the claim to another device.
4. Claims that exceed the device's hardware capacity even before counting pending grants receive `Unschedulable=True` with reason `DeviceAtCapacity`.
5. Claims whose `(namespace, device)` pair is already held by another claim in the same namespace receive `Unschedulable=True` with reason `NamespaceDeviceOccupied`. This should be rare with the admission-injected CEL selector in place; it is kept as a defense against admission-policy bypass or stale `ResourceSlice` race conditions.
6. The scheduler sees `Unschedulable=True` in the claim's `BindingFailureConditions`, deallocates the single CXI claim, and retries.
7. At retry time, the `ResourceSlice` has been republished — `avail_*` near zero for any device that became full, `active_namespaces` updated for the same-namespace case — steering the retry allocation to an actually eligible device.

The system converges to the correct distribution (≤3 `cxi-small`/`cxi-medium` pods per device, ≤1 `cxi-large`/`cxi-whole` pod per device, ≤1 claim per namespace per device on any one node) within one or two compaction cycles in the worst case, and typically zero (CEL pre-filter catches it at scheduling time).

The per-namespace occupancy index is released by three independent, idempotent paths that all converge on a single `removeOccupancyLocked` helper:

1. **NRI `StopPodSandbox`** (normal pod termination) — calls `RemoveOccupancy(ns, dev)` after `cxil_destroy_svc` succeeds. This is the hot path.
2. **`ClaimBinder` watch** — calls `RemoveOccupancyForClaim(uid)` when the scheduler clears `Status.Allocation` (PostFilter after a PreBind failure, Unschedulable signal, manual edit) or when a `ResourceClaim` is deleted. Resolves the UID through a reverse index so the binder doesn't need to know the device. This catches scheduler-side de-allocation that would never reach NRI.
3. **Polling reconciler** — runs once per `--resource-refresh-interval` tick before the periodic ResourceSlice republish. Enumerates live CXI services on every device via `cxil_get_svc_list` + `cxil_svc_get_vni_range`, reverse-maps each VNI to its namespace via the annotation cache, and reconciles the in-memory occupancy against hardware truth. Releases any slot not backed by either a pending grant or a live service; adopts any live service not yet tracked with a synthetic `hw:<dev>/<ns>` UID. This recovers from driver restart, missed watch events, force-deleted pods that bypassed NRI, and containerd `StopPodSandbox` short-circuits.

The reconciler is the hardware-aware authority and keeps the in-memory model converged on actual kernel state without race conditions: if a slot's hardware service still exists, the slot is kept; if it doesn't and no pending grant covers it, the slot is released. The other two paths give sub-second latency in the common cases; the reconciler bounds worst-case latency to one tick.

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

Only `/sys/class/net` is overmounted; `/sys/devices` is left untouched, so the replicated NIC symlinks still resolve. The HSN presents **only** as a sysfs directory under its host-native name — it is not a real netdev, so `ip link`, `ethtool`, netlink, and IP addressing all show nothing. This is sufficient because libfabric globs sysfs rather than using netlink, and the RDMA datapath is kernel-bypass and needs no netdev.

Two libfabric gates interact with the tree. The link gate (`cxip_if.c:142`) has no env bypass and is satisfied by `operstate=up`. The AMA gate (`cxip_if.c:580`) drops any device whose netdev lacks a recognised MAC unless `CXIP_SKIP_AMA_CHECK=1` is set; the synthetic tree omits a MAC, so the `env` CDI profile supplies that env var (see [CDI Profiles](#cdi-profiles)) — it is deliberately **not** hardcoded in NRI.

Injection is skipped in mock mode and when `--netns-sysfs-root` is empty. Reading the host HSN metadata is fatal (the services are rolled back and the kubelet retries the sandbox) because libfabric cannot bootstrap without it; replicating the pod's own NICs is best-effort and falls back to an HSN-only tree on failure.

## CXI Service Programming

Each pod receives a CXI service scoped to its network namespace inode and a unique VNI. libfabric reads `SLINGSHOT_SVC_IDS` (injected as an env var by the NRI plugin) and uses it instead of allocating its own service, ensuring the pod operates within its allocated fabric resources. `SLINGSHOT_DEVICES` and `SLINGSHOT_VNIS` carry the corresponding device name and VNI, aligned by position.

### Namespace-scoped VNI allocation

VNIs are assigned per **namespace** by a leader-elected controller that runs inside the DaemonSet. One DaemonSet pod wins a `Lease`-based election (`cxi-vni-allocator-leader` in `kube-system`) and runs a namespace watcher that scans for the `cxi.slingshot.hpe.com/vni` annotation. Any namespace that lacks one is patched with the lowest free VNI in the configured cluster-wide range.

When a pod is scheduled and the kubelet calls `PrepareResourceClaims`, the driver reads the VNI from the pod's namespace annotation and programs it into the CXI service it creates. Every pod in the same namespace therefore shares the same VNI and can form RDMA connections to every other pod in that namespace. Namespaces are isolated from each other on the fabric.

The VNI range is configured via `--vni-range-start` / `--vni-range-end` in the DaemonSet (default `0` disables the allocator, in which case the driver assumes namespaces are annotated by some other authority). The skip list (`kube-system`, `kube-public`, `kube-node-lease`) is hardcoded — system namespaces never receive a VNI.

Pre-existing annotations are respected: if a namespace already has a valid `cxi.slingshot.hpe.com/vni` annotation, the allocator leaves it untouched. This lets operators pre-assign VNIs to particular tenants.

## Project Layout

```
cmd/cxi-dra-driver/      Binary entrypoint and CLI flags
internal/cxi/            Sysfs device discovery, CXI↔HSN pairing, libcxi CGo wrapper
internal/cdi/            CDI spec generation (DeviceNodes; env/nccl/ofi specs; containerd CDI config drop-in)
internal/netsysfs/       Per-pod synthetic /sys/class/net tree builder for HSN sysfs injection
internal/driver/         DRA kubelet plugin (ResourceSlice, Prepare/Unprepare,
                         ClaimBinder, namespace VNI allocator, store)
internal/nri/            NRI plugin (RunPodSandbox, CreateContainer, StopPodSandbox)
deploy/kubernetes/       DaemonSet, DeviceClass, RBAC, MutatingAdmissionPolicy, ConfigMap, test manifests
deploy/helm/             Helm chart (cxi-dra-driver)
deploy/kind/             kind mock-mode test harness (no hardware)
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
| `--mock-devices` | `0` | Generate N synthetic devices (no hardware needed) |
| `--resource-refresh-interval` | `10s` | How often to republish the ResourceSlice with refreshed available-capacity attributes |
| `--vni-range-start` | `0` | First VNI in the namespace-VNI allocator's pool (0 disables the allocator) |
| `--vni-range-end` | `0` | Last VNI in the pool (inclusive) |
| `--system-svc-ids` | `""` | Comma-separated svc_ids of blessed system services: never deleted, and their reserved resources are deducted from each device's usable ceiling |
| `--netns-sysfs-root` | `/var/run/cxi/netns` | Host directory for the per-pod synthetic `/sys/class/net` trees (empty disables HSN sysfs injection) |
| `--enable-cdi-env` | `false` | Write the `env`/`nccl`/`ofi` CDI specs and inject their device IDs into pods |
| `--env-config` | `""` | Path to the JSON CDI env config file (required when `--enable-cdi-env`) |
| `--containerd-config-path` | `""` | Path to write the containerd CDI config drop-in (empty disables) |

## Device Attributes

Each device in the `ResourceSlice` exposes the following attributes, accessible in CEL selectors as `device.attributes["cxi.slingshot.hpe.com"]["<name>"]`:

| Attribute | Type | Description |
|---|---|---|
| `numa_node` | int | NUMA node the device is attached to |
| `pcie_root` | string | PCIe root complex slot of the CXI device itself (e.g. `"0000:81:00.0"`) |
| `hsn_iface` | string | Associated HSN network interface (e.g. `"hsn2"`) |
| `capacity_txqs` | int | Hardware total TXQs |
| `capacity_tgqs` | int | Hardware total TGQs |
| `capacity_eqs` | int | Hardware total EQs |
| `capacity_cts` | int | Hardware total CTs |
| `capacity_ptes` | int | Hardware total PTEs |
| `capacity_tles` | int | Hardware total TLEs |
| `capacity_les` | int | Hardware total LEs |
| `capacity_acs` | int | Hardware total ACs |
| `avail_txqs` | int | Currently available TXQs (refreshed periodically) |
| `avail_tgqs` | int | Currently available TGQs |
| `avail_eqs` | int | Currently available EQs |
| `avail_cts` | int | Currently available CTs |
| `avail_ptes` | int | Currently available PTEs |
| `avail_tles` | int | Currently available TLEs |
| `avail_les` | int | Currently available LEs |
| `avail_acs` | int | Currently available ACs |

The `avail_*` attributes are used by the `MutatingAdmissionPolicy` to steer initial placement and by the `ClaimBinder`'s Unschedulable compaction cycle to redirect retried claims away from full devices. The `ClaimBinder`'s live fit-check against hardware capacity (via `ReadAvailableCapacity`) is the authoritative gate for granting `Ready=True`.

Example — claim a device on NUMA node 1 with a custom CEL selector:

```yaml
devices:
  requests:
    - name: cxi
      exactly:
        deviceClassName: cxi-device
        selectors:
          - cel:
              expression: 'device.attributes["cxi.slingshot.hpe.com"]["numa_node"] == 1'
```

## Co-locating CXI NICs with NVIDIA GPUs

On GH200-class nodes each CXI NIC, GPU, and NUMA node map 1:1, and you usually want a
pod's CXI NIC to sit on the same PCIe locality as its GPU. DRA expresses this with a
[`matchAttribute`](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
constraint: every device across the listed requests must share the same value for a
*fully-qualified* attribute.

For a cross-driver match the attribute name and value must be identical on both sides.
The NVIDIA DRA driver publishes the standardized attribute
`resource.kubernetes.io/pcieRoot` (value `pci<domain>:<bus>`, e.g. `pci0009:00`). NVIDIA
does **not** publish `numa_node`, and the NIC and GPU sit under different PCIe roots, so
NUMA is the only shared locality key. This driver therefore discovers, for each CXI
device, the NVIDIA GPU on the same NUMA node and republishes that GPU's PCIe root under
the same standardized name:

| Attribute | Type | Description |
|---|---|---|
| `resource.kubernetes.io/pcieRoot` | string | PCIe root (`pci<domain>:<bus>`) of the NVIDIA GPU on this CXI device's NUMA node. Matches the value the NVIDIA driver publishes. Only present when a GPU is found on the device's NUMA node. |

The value is computed exactly as Kubernetes' own `deviceattribute.GetPCIeRootAttributeByPCIBusID`
helper does (resolve `/sys/bus/pci/devices/<bdf>` and take the leading `pci<domain>:<bus>`
component), so the two drivers' values are byte-for-byte comparable.

A single claim then requests one GPU and one CXI NIC and constrains them to the same root:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: gpu-cxi
spec:
  devices:
    config:
      - requests: ["cxi"]
        opaque:
          driver: cxi.slingshot.hpe.com   # must match this install's driverName
          parameters: { txqs: 19, tgqs: 57, eqs: 37, cts: 47, ptes: 36, acs: 18, le_pools: 1, tle_pools: 1, cdi_profiles: ["env", "aws-ofi-nccl"] }
    constraints:
      - matchAttribute: resource.kubernetes.io/pcieRoot
        requests: ["gpu", "cxi"]
    requests:
      - name: gpu
        exactly:
          deviceClassName: gpu.nvidia.com
          allocationMode: ExactCount
          count: 1
          selectors:
            - cel: { expression: 'device.driver == "gpu.nvidia.com"' }   # keep a selector so the MutatingAdmissionPolicy does not inject CXI selectors into the GPU request
      - name: cxi
        exactly:
          deviceClassName: cxi-device
          allocationMode: ExactCount
          count: 1
```

The scheduler allocates a GPU and the CXI NIC that share a PCIe root — e.g. `gpu-3` with
`cxi3`, both on `pci0039:00` (NUMA node 3).

Caveats:

- The CXI NIC and its NUMA-local GPU must both be **free on the same node**; the constraint
  removes the scheduler's freedom to pick an unrelated NIC, so a busy GPU or a CXI device
  already in use by another pod **in the same namespace** (see the per-namespace-per-device
  restriction above) can make an otherwise-valid node unschedulable.
- `resource.kubernetes.io/pcieRoot` is only published on nodes running a driver build that
  includes it; the constraint is unsatisfiable on nodes that predate the attribute.

> **Deprecation note:** this republish-the-GPU's-root approach is a stopgap. Once the NVIDIA
> DRA driver exposes NUMA topology through the standardized device-attributes KEP (the same
> effort that defines `resource.kubernetes.io/pcieRoot` in `k8s.io/dynamic-resource-allocation/deviceattribute`),
> both drivers can `matchAttribute` directly on a shared NUMA/topology attribute and this
> NUMA→GPU-PCIe-root inference can be removed.

## Mock Mode (kind / no hardware)

The driver supports `--mock-devices=N` which generates N synthetic CXI devices without real hardware. The full scheduling → allocation → prepare loop runs normally.

```bash
# Create a kind cluster with DRA enabled
kind create cluster --name cxi-dra-dev --config deploy/kind/kind-config.yaml

# Build and load image (no CGo needed in mock mode)
podman build -f Dockerfile.ci -t cxi-dra-driver:dev .
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
- **Reconciler API-server load** — the polling reconciler calls `Namespaces().List()` once per `--resource-refresh-interval` tick to build the VNI → namespace map. This is cheap at expected scale (tens of namespaces, 10 s tick) but should be migrated to an informer-backed `NamespaceLister` if the cluster grows large

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

### Claim rejected: "has no CXI resource config"

All resource fields are required. Ensure your `ResourceClaimTemplate` has an `opaque` config block with all eight fields (`txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `acs`, `le_pools`, `tle_pools`). See [deploy/kubernetes/test/resourceclaim.yaml](deploy/kubernetes/test/resourceclaim.yaml) for complete examples.

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
