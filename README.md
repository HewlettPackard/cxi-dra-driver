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
                         │     └── Mounts: sysfs subtrees
                         ├── NRI plugin (RunPodSandbox)
                         │     └── creates CXI service: VNI + pod netns inode
                         ├── NRI plugin (CreateContainer)
                         │     └── injects FI_CXI_SVC_ID env vars
                         └── NRI plugin (StopPodSandbox)
                               └── destroys CXI service
```

## Requirements

- Kubernetes 1.34+ (stable `resource.k8s.io/v1` DRA API)
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

This applies RBAC, the `DeviceClass`, and the `DaemonSet` in order.

### 4. Verify the ResourceSlice

```bash
kubectl get resourceslices -o wide
```

You should see one slice per node with devices `cxi0`, `cxi1`, … and attributes `numa_node`, `pcie_root`, `hsn_iface`.

### 5. Run the test pod

```bash
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl get pod cxi-test
kubectl exec -it cxi-test -- fi_info -p cxi
```

You should see CXI providers enumerated without `privileged: true`.

## How Device Injection Works

Unprivileged containers require two things to open `/dev/cxiN`:

1. **The device node** present in the container's `/dev/`
2. **A cgroup BPF allowance** permitting access to the character device major:minor

Both are handled via CDI. The driver writes a CDI spec to `/var/run/cdi/` at startup containing a `DeviceNodes` entry for each CXI device. containerd resolves CDI devices during container spec construction — before the cgroup BPF program is compiled — so the allowance is baked in from the start. This is why NRI alone (which fires after cgroup policy is locked in) is insufficient for device access.

The CXI service (VNI scoping + netns binding) is created in the NRI `RunPodSandbox` hook, which fires after the pod network namespace is ready but before any container starts. The netns inode is obtained via `/proc/<sandbox-pid>/ns/net` + `fstat`, which returns the real nsfs inode. Note: `os.Stat` on the netns bind-mount path (e.g. `/var/run/netns/cni-...`) returns the wrong inode — the underlying tmpfs inode rather than the nsfs inode that the CXI kernel driver uses to scope services.

## CXI Service Programming

Each pod receives a CXI service scoped to its network namespace inode and VNI. libfabric reads `FI_CXI_SVC_ID` (injected as an env var by the NRI plugin) and uses it instead of allocating its own service, ensuring the pod operates within its allocated fabric resources.

Current VNI allocation uses a static allocator (`VNI=1`). A cluster-wide VNI pool allocator is planned.

## Project Layout

```
cmd/cxi-dra-driver/      Binary entrypoint and CLI flags
internal/cxi/            Sysfs device discovery, CXI↔HSN pairing, libcxi CGo wrapper
internal/cdi/            CDI spec generation (DeviceNodes + sysfs mounts)
internal/driver/         DRA kubelet plugin (ResourceSlice, Prepare/Unprepare, store)
internal/nri/            NRI plugin (RunPodSandbox, CreateContainer, StopPodSandbox)
deploy/kubernetes/       DaemonSet, DeviceClass, RBAC, test manifests
```

## Configuration Flags

| Flag | Default | Description |
|---|---|---|
| `--driver-name` | `cxi.slingshot.hpe.com` | DRA driver identifier |
| `--cdi-root` | `/var/run/cdi` | CDI spec output directory |
| `--plugin-dir` | `/var/lib/kubelet/plugins/cxi.slingshot.hpe.com` | Kubelet plugin socket directory |
| `--registrar-dir` | `/var/lib/kubelet/plugins_registry` | Plugin registrar directory |
| `--mock-devices` | `0` | Generate N synthetic devices (no hardware needed) |
| `--mock-services` | `false` | Use no-op service manager (skips libcxi calls) |

## Device Attributes

Each device in the `ResourceSlice` exposes the following attributes:

| Attribute | Type | Example | Source |
|---|---|---|---|
| `numa_node` | int | `1` | `/sys/class/cxi_user/cxi0/device/numa_node` |
| `pcie_root` | string | `"0000:81:00.0"` | `uevent PCI_SLOT_NAME` |
| `hsn_iface` | string | `"hsn2"` | sysfs PCIe topology walk |

Example — claim a CXI device on NUMA node 1:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: cxi-numa1
spec:
  spec:
    devices:
      requests:
        - name: cxi
          deviceClassName: cxi-device
          selectors:
            - cel:
                expression: "device.attributes['numa_node'].int == 1"
```

## Mock Mode (kind / no hardware)

The driver supports `--mock-devices=N` which generates N synthetic CXI devices without real hardware. `/dev/null` is used as the device node stand-in and temporary sysfs directories are created under `/tmp/mock-cxi/`. The full scheduling → allocation → prepare loop runs normally.

```bash
# Create a kind cluster with CDI enabled
kind create cluster --name cxi-dra-dev --config deploy/kind/kind-config.yaml

# Build and load image (no CGo needed in mock mode)
docker build -t cxi-dra-driver:dev .
kind load docker-image cxi-dra-driver:dev --name cxi-dra-dev

# Deploy with mock devices
# Edit daemonset.yaml: add --mock-devices=2, remove nodeSelector
kubectl apply -f deploy/kubernetes/clusterrole.yaml
kubectl apply -f deploy/kubernetes/deviceclass.yaml
kubectl apply -f deploy/kubernetes/daemonset.yaml

# Verify
kubectl get resourceslices -o wide
kubectl apply -f deploy/kubernetes/test/pod.yaml
kubectl logs cxi-test
```

## Known Limitations / Planned Work

- **VNI pool** — currently hardcoded `VNI=1`; cluster-wide VNI allocator planned
- **HSN interface injection** — libfabric warns `CXI netdev not found` because HSN interfaces (`hsn0`, `hsn1`, …) live in the host network namespace. The relevant metadata (`speed`, `link`) is available at `/sys/class/cxi_user/cxi0/device/cxi/cxi0/device/properties/` inside the container but libfabric looks for it at `/sys/class/cxi_user/hsn0/device/properties/`. Fix requires pre-creating the mountpoint directory on each node (e.g. via a DaemonSet initContainer) so the CDI spec can bind-mount over it
- **Real device capacity** — `Capacity` values in `ResourceSlice` are currently hardcoded defaults; populating from `cxil_devinfo` via CGo is planned
- **Hot-plug** — no fsnotify watcher on `/sys/class/cxi_user`; driver restart required if devices appear/disappear
- **Multi-node VNI coordination** — VNI allocation is per-node; cross-node uniqueness not enforced

## Troubleshooting

### `fi_info -p cxi` returns `-61 (FI_ENODATA)`

Check in order:

1. **Device accessible**: run inside the pod:
   ```bash
   python3 -c "import os; os.open('/dev/cxi0', os.O_RDWR)"
   ```
   If EPERM, CDI device injection failed — check `/var/run/cdi/` specs have `deviceNodes` entries (see below).

2. **Service netns correct**: on the host:
   ```bash
   cxi_service list -v
   ```
   The `netns=` value should match the pod's netns inode. Get the expected inode via:
   ```bash
   # find the netns path from crictl, then:
   stat /var/run/netns/cni-<id>   # Inode field
   ```

3. **NRI fired**:
   ```bash
   kubectl logs -n kube-system daemonset/cxi-dra-driver
   ```
   Should show `cxi-nri: creating CXI service` lines when the pod started.

4. **Env vars injected**: inside the pod:
   ```bash
   env | grep FI_CXI
   ```
   Should show `FI_CXI_SVC_ID=<n>`.

### EPERM opening `/dev/cxi0`

The CDI spec must contain `deviceNodes` (not just `mounts`) for containerd to add the cgroup BPF allowance. Verify:

```bash
cat /var/run/cdi/cxi.slingshot.hpe.com-cxi-cxi0.json | grep -A8 deviceNodes
```

If missing, the driver is running an old image. Check:
```bash
kubectl get pod -n kube-system -l app=cxi-dra-driver \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].imageID}{"\n"}{end}'
```

### Wrong netns inode in CXI service

The driver uses `/proc/<sandbox-pid>/ns/net` + `fstat` to obtain the nsfs inode. If services show a mismatched `netns=`, verify the driver is running the current image. `os.Stat` on the netns bind-mount path returns the underlying tmpfs inode, not the nsfs inode — this was a known bug that has been fixed.

### NRI plugin not firing

Verify NRI is enabled in containerd config and the `/var/run/nri/` socket directory is mounted into the driver pod. Check logs for `Started plugin 00-cxi-dra-driver`. If the pod was already running when the driver restarted, delete and recreate it — NRI only fires for new container creation events.
