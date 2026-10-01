# Slingshot CNI Plugin

This document covers the `slingshot-cni` binary: what it does, how it fits
alongside the driver's existing HSN injection, its package layout, the
`ADD`/`DEL`/`CHECK` command flows, and its configuration. It complements
[cnidra-cni-hint-contract.md](./cnidra-cni-hint-contract.md), which
specifies the driver-side write contract this plugin reads against.

## What it does

`slingshot-cni` gives a pod a real, L3-addressed network interface backed by
each CXI device it claimed. For every device in the pod's hint file it:

1. creates an **L2 IPVLAN sub-interface** on the device's paired host HSN
   interface (`hsn_iface`),
2. moves it into the pod's network namespace under the HSN's own name
   (`hsn2`, falling back to `hsn2_inet` if that name is taken), and
3. delegates address assignment to the **Whereabouts IPAM plugin** per the
   standard CNI IPAM delegate contract, then applies the returned
   address/routes to the interface.

A pod that claimed two devices (`count: 2`) gets two interfaces, two
Whereabouts reservations, and one aggregated CNI result.

### Relationship to the driver's HSN sysfs injection

This plugin is **complementary to, not a replacement for**, the driver's
[internal/netsysfs](../../internal/netsysfs) synthetic `/sys/class/net`
injection (see README § *HSN Interface Injection*). The two mechanisms serve
different consumers and continue to run side by side:

| Mechanism | Owned by | Provides | Consumed by |
|---|---|---|---|
| Synthetic `/sys/class/net/<hsn>` tree | driver (NRI `RunPodSandbox` / `CreateContainer`) | sysfs metadata (`operstate`, `carrier`, `device → BDF`) | stock libfabric's CXI provider, which globs sysfs to discover the HSN; RDMA datapath is kernel-bypass |
| IPVLAN netdev + IP address | `slingshot-cni` (CNI `ADD`) | a real netdev with an L3 address | anything that needs IP over the HSN: out-of-band job launchers, control-plane traffic, tooling |

libfabric's HSN discovery does not depend on the IPVLAN interface, and the
IPVLAN interface does not depend on the synthetic sysfs. Note that the
container's `/sys/class/net` is bind-mounted with the synthetic tree, so the
IPVLAN interface created here is visible via netlink (`ip link`, `ip addr`)
but **not** under the container's overmounted `/sys/class/net` — this is
expected.

## Binary and package layout

Mirrors the `cmd/cxi-cdi-hook` + `internal/cdi` split already used in this
repo for the CDI hook:

| Path | Role |
|------|------|
| [cmd/slingshot-cni/main.go](../../cmd/slingshot-cni/main.go) | Thin binary entrypoint — wires `internal/cni`'s command functions into `skel.PluginMainFuncs` |
| [internal/cni/cmd.go](../../internal/cni/cmd.go) | `ADD`/`DEL`/`CHECK` orchestration: multi-device loop, rollback, idempotency |
| [internal/cni/config.go](../../internal/cni/config.go) | stdin network-config parsing (`NetConf`) |
| [internal/cni/hint.go](../../internal/cni/hint.go), [podargs.go](../../internal/cni/podargs.go) | hint-file read/validate, `CNI_ARGS` parsing |
| [internal/cni/network.go](../../internal/cni/network.go) | `linkManager` interface — the kernel-facing seam |
| [internal/cni/network_linux.go](../../internal/cni/network_linux.go) | real implementation via `vishvananda/netlink` (IPVLAN create/move/rename/addr/route) |
| [internal/cni/network_other.go](../../internal/cni/network_other.go) | non-Linux stub so the package builds and its platform-independent tests run everywhere |
| [internal/cni/ipam.go](../../internal/cni/ipam.go) | `ipamDelegate` interface and the real `invoke.Delegate*`-based implementation |

The binary is built independently of the driver DaemonSet image (`make
build-cni`, CGO-free) — it runs on the host as a CNI plugin invoked by
containerd/Multus, not inside the driver's own container. It needs
`CAP_NET_ADMIN` (it is exec'd by containerd as root, as all CNI plugins are).

## Command reference

All three commands start the same way:

1. Parse the stdin network config (`NetConf`). `ipam.type` is required.
2. Parse `CNI_ARGS` for `K8S_POD_UID`.
3. Read `<hintsDir>/<podUID>.json`, retrying on a bounded interval/timeout
   (default `100ms`/`5s`) if it isn't there yet — a safety net for the driver
   crashing between claim allocation and `PrepareResourceClaims`, not the
   common path (see the ordering guarantee in the hint contract).
4. Validate every hint entry (`device`, `hsn_iface`, positive `vni`).

### `ADD`

For each hint entry, in order:

| Step | Operation | On failure |
|---|---|---|
| a | `LinkByName(hsn_iface)` in the host netns → parent | fail; unwind previous devices |
| b | `LinkAdd` IPVLAN (mode L2, parent = HSN) under a temporary host-side name (`cxi<containerID[:8]>`) | fail; unwind |
| c | `LinkSetNsFd` → move into `CNI_NETNS` | delete link; unwind |
| d | Inside the pod netns: rename to `hsn_iface`; if the kernel refuses (name in use), rename to `hsn_iface_inet`; `LinkSetUp` | delete link; unwind |
| e | Exec the IPAM plugin (`ipam.type`, found on `CNI_PATH`) with `CNI_COMMAND=ADD`, the **unmodified stdin config**, and `CNI_IFNAME` overridden to the name chosen in (d) | delete link; unwind |
| f | Apply every returned IP to the interface; add every returned route (using the IP's gateway when the route has none) | IPAM `DEL` + delete link; unwind |

"Unwind" releases the IPAM reservation and deletes the interface of every
device already completed in this invocation, in reverse order, so a failed
`ADD` leaves nothing behind for kubelet's retry to collide with.

On success the plugin prints one CNI 1.0.0 result: one `interfaces[]` entry
per device (`name` = chosen in-pod name, `sandbox` = `CNI_NETNS`), one
`ips[]` entry per IPAM address with `interface` indexing its device, and the
union of IPAM routes.

**Why `CNI_IFNAME` is overridden per device.** Whereabouts keys each
reservation on `(containerID, ifname)`. The `CNI_IFNAME` containerd/Multus
passes to `slingshot-cni` is a single name (e.g. `net1`); reusing it for
every device of a `count: 2` pod would make the second reservation overwrite
the first. Presenting each device under its own in-pod name keeps the
reservations distinct and lets `DEL` release each one precisely.

### `DEL`

Best-effort and idempotent, per the CNI spec — the pod is going away
regardless:

- A **missing hint file** is logged and `DEL` returns success (the driver may
  already have removed it, or crashed mid-teardown). A hint file that exists
  but is **malformed** is still an error, since that indicates a bug.
- For each entry: find the interface under either candidate name (so the IPAM
  release uses the same `ifname` `ADD` registered — the preferred name is
  used if the interface is already gone), exec IPAM `DEL`, then delete the
  interface. A missing interface or a vanished netns is treated as success.
- Every entry is attempted even if an earlier one fails; all errors are
  joined and returned at the end.

`DEL` only ever deletes **IPVLAN** links. If the pod's netns has a non-IPVLAN
interface under the HSN's name (the very situation the `_inet` fallback
exists for), it is left untouched.

### `CHECK`

For each entry: locate the interface under either candidate name, verify it
is an IPVLAN link and is up, then exec IPAM `CHECK` for that name. The first
failure is returned.

## Configuration

A `NetworkAttachmentDefinition` for Multus (or a raw conflist entry):

```json
{
  "cniVersion": "1.0.0",
  "name": "slingshot-hsn",
  "type": "slingshot-cni",
  "hintsDir": "/var/run/cxi/net-hints",
  "retryInterval": "100ms",
  "retryTimeout": "5s",
  "ipam": {
    "type": "whereabouts",
    "range": "10.200.0.0/16",
    "exclude": ["10.200.0.0/30"]
  }
}
```

| Field | Required | Default | Meaning |
|---|---|---|---|
| `type` | yes | — | must be `slingshot-cni` |
| `ipam.type` | **yes** | — | IPAM delegate plugin name, found on `CNI_PATH`; `whereabouts` is the supported choice. The whole `ipam` block is forwarded to it verbatim, so any Whereabouts option (`range`, `exclude`, `range_start`, …) goes here. |
| `hintsDir` | no | `/var/run/cxi/net-hints` | must match the driver's `--cni-hints-dir` |
| `retryInterval` / `retryTimeout` | no | `100ms` / `5s` | hint-file polling bounds |

Prerequisites on each node:

- `slingshot-cni` and `whereabouts` binaries on `CNI_PATH` (typically
  `/opt/cni/bin`).
- Whereabouts's own cluster-side install (CRDs, `ip-reconciler`) — see its
  documentation. The HSN pool is cluster-wide, so use a single `range` across
  all nodes.
- The driver running with `--cni-hints-dir` non-empty (the default).

## Testing

| File | Covers | Runs |
|---|---|---|
| [cmd_test.go](../../internal/cni/cmd_test.go) | `ADD`/`DEL`/`CHECK` orchestration against in-memory fakes: single- and multi-device result shape, `_inet` fallback, rollback ordering on IPAM / configure / create failure, `DEL` idempotency and per-device continuation, missing-vs-malformed hint handling | everywhere |
| [ipam_test.go](../../internal/cni/ipam_test.go) | the real IPAM delegate exec'ing a fake `whereabouts` binary (built from [testdata/fakewhereabouts](../../internal/cni/testdata/fakewhereabouts)) — verifies `CNI_COMMAND`, per-device `CNI_IFNAME` override, inherited `CNI_CONTAINERID`, stdin forwarding, result parsing, and error surfacing | everywhere (needs `go` on `PATH`) |
| [network_linux_test.go](../../internal/cni/network_linux_test.go) | the real `netlink` implementation against a throwaway named netns and a dummy parent: create/move/rename/up, L2 mode, address and route application, `Find`/`Check`/`Delete`, idempotent delete, `_inet` fallback without disturbing the pod's own link, vanished-netns handling | Linux as root only (skips otherwise) |

To run the root-gated tests from a non-Linux workstation:

```bash
podman run --rm --privileged -v "$PWD":/src -w /src golang:1.24-alpine \
  go test ./internal/cni/
```
