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
                         │     └── DeviceNodes: /dev/cxiN  (cgroup BPF allowance)
                         ├── NRI plugin (RunPodSandbox)
                         │     └── creates CXI service: VNI + pod netns inode
                         ├── NRI plugin (CreateContainer)
                         │     └── injects FI_CXI_SVC_ID env vars
                         └── NRI plugin (StopPodSandbox)
                               └── destroys CXI service

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

Update the image reference in [deploy/kubernetes/daemonset.yaml](deploy/kubernetes/daemonset.yaml).

> **Note:** The `Dockerfile` uses `FROM scratch` with a linker-based entrypoint to avoid glibc version conflicts between build and runtime environments:
> ```
> ENTRYPOINT ["/lib/ld-linux-aarch64.so.1", "--library-path", "/usr/lib64:/lib64", "/cxi-dra-driver"]
> ```

## Deployment

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

### 3. Deploy the driver

```bash
make deploy
```

This applies RBAC, the `DeviceClass` (for CXI devices and VNIs), the `MutatingAdmissionPolicy`, and the `DaemonSet` in order.

### 4. Verify the ResourceSlice

```bash
kubectl get resourceslices -o wide
```

You should see one slice per node with devices `cxi0`, `cxi1`, … and attributes including `avail_txqs`, `avail_tles`, etc. which are refreshed periodically.

### 5. Run the test pod

```bash
kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl get pod cxi-test-3
kubectl exec -it cxi-test-3 -- fi_info -p cxi
```

The test pod claims one CXI device and one fabric VNI via the merged `cxi-quarter-with-vni` template. You should see CXI providers enumerated without `privileged: true`.

## Requesting CXI Resources

Each pod that uses a CXI device needs a **single** `ResourceClaimTemplate` that requests both the CXI device and a fabric VNI together. The CXI device and VNI are allocated as one atomic unit so that the scheduler's Unschedulable compaction mechanism works correctly (see [How scheduler placement works](#how-scheduler-placement-works) below).

Two templates are provided in [deploy/kubernetes/test/resourceclaim.yaml](deploy/kubernetes/test/resourceclaim.yaml):

| Template | TXQs | TGQs | LE pools | TLE pools | Use case |
|---|---|---|---|---|---|
| `cxi-quarter-with-vni` | 256 | 128 | 4 (4096 LEs) | 1 (512 TLEs) | Most jobs |
| `cxi-half-with-vni` | 512 | 256 | 8 (8192 LEs) | 2 (1024 TLEs) | Bandwidth-intensive jobs |

All resource fields are **required** — the driver has no defaults. Claims without an opaque config block will be rejected at prepare time with a clear error.

### Claim template structure

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: cxi-quarter-with-vni
  namespace: default
spec:
  spec:
    devices:
      requests:
        # CXI device request must be first — the MutatingAdmissionPolicy
        # injects avail_* capacity selectors into requests[0].
        - name: cxi
          exactly:
            deviceClassName: cxi-device
        - name: vni
          exactly:
            deviceClassName: cxi-vni
      config:
        - requests: ["cxi"]
          opaque:
            driver: cxi.slingshot.hpe.com
            parameters:
              txqs: 256
              tgqs: 128
              eqs: 511
              cts: 511
              ptes: 512
              acs: 255
              le_pools: 4    # 1 pool = 1024 LEs
              tle_pools: 1   # 1 pool = 512 TLEs
```

LEs and TLEs are expressed as **pool counts** due to hardware allocation granularity:
- `le_pools: 1` reserves 1024 LEs
- `tle_pools: 1` reserves 512 TLEs

Set `le_pools: 0` and `tle_pools: 0` if your job does not use triggered operations.

### Wiring the claim into a pod

```yaml
spec:
  resourceClaims:
    - name: cxi
      resourceClaimTemplateName: cxi-quarter-with-vni
  containers:
    - name: workload
      resources:
        claims:
          - name: cxi
```

See [deploy/kubernetes/test/pod.yaml](deploy/kubernetes/test/pod.yaml) for a complete example.

### Why CXI and VNI are in one claim

The driver uses Kubernetes `BindingConditions` to gate pod binding until device capacity is confirmed. When a CXI device is over-subscribed (the scheduler placed more claims on it than it can serve), the ClaimBinder sets `Unschedulable=True` on the device. The scheduler then deallocates the claim and retries on a less-loaded device.

Because the CXI device and VNI are in the **same** `ResourceClaim`, both allocations are cleared atomically on deallocation. If they were in separate claims, only the CXI claim would be deallocated while the VNI claim remained allocated, causing `"number of allocations 1 is smaller than number of claims 2"` Score errors on every subsequent retry.

### Multi-pod workloads sharing a VNI

The merged template gives each pod its own unique VNI, which is appropriate for independent pods that each need isolated fabric access. Support for multi-pod workloads that must communicate on a **shared** fabric service (same VNI across pods) is planned but not yet implemented via a merged template.

### How scheduler placement works

When a `ResourceClaim` with opaque CXI config is created, the `MutatingAdmissionPolicy` automatically injects CEL selectors into the claim. For example, a `cxi-quarter-with-vni` request becomes:

```
device.attributes["cxi.slingshot.hpe.com"]["avail_txqs"] >= 256 &&
device.attributes["cxi.slingshot.hpe.com"]["avail_tles"] >= 512 &&
...
```

These selectors steer subsequent scheduling rounds toward devices with available capacity. However, because the K8s DRA scheduler evaluates claims against a static snapshot of the `ResourceSlice` within a single scheduling batch, it may initially over-concentrate claims onto fewer devices than necessary (the custom `avail_*` attributes are not decremented between per-claim decisions in the same batch).

The driver handles this via **Unschedulable compaction**:

1. The scheduler allocates a batch of claims, possibly concentrating multiple claims on one device.
2. The `ClaimBinder` evaluates each allocation against real hardware capacity. It grants `Ready=True` to claims that fit within current capacity minus in-flight pending grants.
3. Claims that exceed the device's hardware capacity (device truly full) receive `Unschedulable=True`.
4. The scheduler sees `Unschedulable=True` in the claim's `BindingFailureConditions`, deallocates the entire claim (CXI + VNI together), and retries.
5. At retry time, the `ResourceSlice` has been republished with `avail_*` near zero for the full device, steering the retry allocation to a less-loaded device.

The system converges to the correct distribution (≤3 `cxi-quarter` pods per device) within one or two compaction cycles.

## Accounting for Pre-existing System Services

If a node has CXI services created by the OS or system software before the driver starts (e.g. HSN management services), their resource consumption should be declared as overhead so the driver does not overcommit.

Set the `--reserved-*` flags in the DaemonSet to deduct a fixed amount from the usable ceiling on every device on that node:

```yaml
# In daemonset.yaml, under containers[0].args:
- --reserved-txqs=64
- --reserved-tgqs=32
- --reserved-eqs=128
- --reserved-cts=128
- --reserved-ptes=64
- --reserved-acs=32
- --reserved-le-pools=1    # deducts 1024 LEs (1 pool)
- --reserved-tle-pools=1   # deducts 512 TLEs (1 pool)
```

These values are subtracted from the hardware total before any fit-check. A claim requesting more than `hardware_total - overhead` will be rejected immediately with a "will never fit on this hardware" error, before any libcxi call is attempted.

All reserved flags default to `0` (no overhead deducted).

## How Device Injection Works

Unprivileged containers require two things to open `/dev/cxiN`:

1. **The device node** present in the container's `/dev/`
2. **A cgroup BPF allowance** permitting access to the character device major:minor

Both are handled via CDI. The driver writes a CDI spec to `/var/run/cdi/` at startup containing a `DeviceNodes` entry for each CXI device. containerd resolves CDI devices during container spec construction — before the cgroup BPF program is compiled — so the allowance is baked in from the start. This is why NRI alone (which fires after cgroup policy is locked in) is insufficient for device access.

The CXI service (VNI scoping + netns binding) is created in the NRI `RunPodSandbox` hook, which fires after the pod network namespace is ready but before any container starts. The netns inode is obtained via `/proc/<sandbox-pid>/ns/net` + `fstat`, which returns the real nsfs inode. Note: `os.Stat` on the netns bind-mount path (e.g. `/var/run/netns/cni-...`) returns the wrong inode — the underlying tmpfs inode rather than the nsfs inode that the CXI kernel driver uses to scope services.

## CXI Service Programming

Each pod receives a CXI service scoped to its network namespace inode and a unique VNI. libfabric reads `FI_CXI_SVC_ID` (injected as an env var by the NRI plugin) and uses it instead of allocating its own service, ensuring the pod operates within its allocated fabric resources.

### Cluster-wide VNI pool

VNIs are managed cluster-wide via a dedicated `ResourceSlice` published by the VNI pool leader. One of the DaemonSet pods wins a Kubernetes `Lease`-based leader election and publishes a pool of VNI devices (each with a `type: vni` attribute) using the `cxi-vni` `DeviceClass`. These devices are visible to the scheduler across all nodes (`allNodes: true`).

When a pod's VNI `ResourceClaim` is allocated, the scheduler reserves one VNI device from this pool. The driver reads the allocated VNI number at prepare time and programs it into the CXI service. Released VNIs (when the claim is deleted) are automatically returned to the pool for reuse.

The VNI range is configured via `--vni-range-start` / `--vni-range-end` in the DaemonSet. Each VNI in the range is represented as a separate device (`vni-100`, `vni-101`, …). The kernel returns `-EEXIST` from `cxil_svc_set_vni_range` if two services attempt to claim the same VNI — the cluster-wide allocation prevents this from happening under normal operation.

## Project Layout

```
cmd/cxi-dra-driver/      Binary entrypoint and CLI flags
internal/cxi/            Sysfs device discovery, CXI↔HSN pairing, libcxi CGo wrapper
internal/cdi/            CDI spec generation (DeviceNodes)
internal/driver/         DRA kubelet plugin (ResourceSlice, Prepare/Unprepare, store)
internal/nri/            NRI plugin (RunPodSandbox, CreateContainer, StopPodSandbox)
deploy/kubernetes/       DaemonSet, DeviceClass, RBAC, MutatingAdmissionPolicy, test manifests
```

## Configuration Flags

| Flag | Default | Description |
|---|---|---|
| `--driver-name` | `cxi.slingshot.hpe.com` | DRA driver identifier |
| `--node-name` | `$NODE_NAME` | Kubernetes node name |
| `--pod-uid` | `$POD_UID` | Pod UID for rolling-update socket uniqueness |
| `--cdi-root` | `/var/run/cdi` | CDI spec output directory |
| `--plugin-dir` | `/var/lib/kubelet/plugins/cxi.slingshot.hpe.com` | Kubelet plugin socket directory |
| `--registrar-dir` | `/var/lib/kubelet/plugins_registry` | Plugin registrar directory |
| `--mock-devices` | `0` | Generate N synthetic devices (no hardware needed) |
| `--resource-refresh-interval` | `30s` | How often to republish the ResourceSlice with refreshed available-capacity attributes |
| `--vni-range-start` | `100` | First VNI in the cluster-wide pool |
| `--vni-range-end` | `300` | Last VNI in the cluster-wide pool (inclusive) |
| `--reserved-txqs` | `0` | TXQs reserved by pre-existing system services |
| `--reserved-tgqs` | `0` | TGQs reserved by pre-existing system services |
| `--reserved-eqs` | `0` | EQs reserved by pre-existing system services |
| `--reserved-cts` | `0` | CTs reserved by pre-existing system services |
| `--reserved-ptes` | `0` | PTEs reserved by pre-existing system services |
| `--reserved-acs` | `0` | ACs reserved by pre-existing system services |
| `--reserved-le-pools` | `0` | LE pools reserved by pre-existing system services (1 pool = 1024 LEs) |
| `--reserved-tle-pools` | `0` | TLE pools reserved by pre-existing system services (1 pool = 512 TLEs) |

## Device Attributes

Each device in the `ResourceSlice` exposes the following attributes, accessible in CEL selectors as `device.attributes["cxi.slingshot.hpe.com"]["<name>"]`:

| Attribute | Type | Description |
|---|---|---|
| `numa_node` | int | NUMA node the device is attached to |
| `pcie_root` | string | PCIe root complex slot (e.g. `"0000:81:00.0"`) |
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
kubectl apply -f deploy/kubernetes/deviceclass-vni.yaml  # VNI device class
kubectl apply -f deploy/kubernetes/admission-policy.yaml
kubectl apply -f deploy/kubernetes/daemonset.yaml        # add --mock-devices=2, remove nodeSelector

# Verify both device types appear
kubectl get resourceslices -o wide

# Run the test workload (merged CXI+VNI claim)
kubectl apply -f deploy/kubernetes/test/resourceclaim.yaml
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl logs cxi-test-3
```

## Known Limitations / Planned Work

- **HSN interface injection** — libfabric warns `CXI netdev not found` because HSN interfaces (`hsn0`, `hsn1`, …) live in the host network namespace. Fix requires pre-creating the mountpoint directory on each node so the CDI spec can bind-mount over it
- **Service reconciliation on restart** — services surviving a driver restart are not re-tracked; orphaned services persist until node reboot
- **Hot-plug** — no fsnotify watcher on `/sys/class/cxi_user`; driver restart required if devices appear/disappear
- **VNI pool persistence** — VNI allocation is backed by `ResourceSlice` objects, so assignments survive individual driver pod restarts. However, if the entire DaemonSet is deleted and recreated, in-flight claims that were allocated but not yet released may reference VNIs that no longer appear in the re-published pool until the claims are deleted

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
   env | grep FI_CXI
   ```
   Should show `FI_CXI_SVC_ID=<n>`.

### Claim rejected: "has no CXI resource config"

All resource fields are required. Ensure your `ResourceClaimTemplate` has an `opaque` config block with all eight fields (`txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `acs`, `le_pools`, `tle_pools`). See [deploy/kubernetes/test/resourceclaim.yaml](deploy/kubernetes/test/resourceclaim.yaml) for complete examples.

### Claim rejected: "exceed usable ceiling"

The requested resources exceed `hardware_total - reserved_overhead`. Either reduce the request or lower the `--reserved-*` flags if the overhead was set too conservatively.

### EPERM opening `/dev/cxi0`

The CDI spec must contain `deviceNodes` for containerd to add the cgroup BPF allowance. Verify:

```bash
cat /var/run/cdi/cxi.slingshot.hpe.com-cxi-cxi0.json | grep -A8 deviceNodes
```

### Wrong netns inode in CXI service

The driver uses `/proc/<sandbox-pid>/ns/net` + `fstat` to obtain the nsfs inode. `os.Stat` on the netns bind-mount path returns the underlying tmpfs inode, not the nsfs inode — ensure you are running a current image.

### NRI plugin not firing

Verify NRI is enabled in containerd config and the `/var/run/nri/` socket directory is mounted into the driver pod. Check logs for `Started plugin 00-cxi-dra-driver`. If the pod was already running when the driver restarted, delete and recreate it — NRI only fires for new container creation events.
