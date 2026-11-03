---
name: cxi-dra-glossary
description: "Glossary of CXI/DRA/Kubernetes domain terms used throughout this driver. USE FOR: understanding source comments, README, and CONTEXT.md; writing new code that touches DRA/CXI/CDI/NRI/CNI concepts. Trigger keywords: DRA, CXI, CDI, NRI, CNI, ResourceClaim, ResourceSlice, consumable capacity, HSN, VNI, IPVLAN, Whereabouts, hint file, reaper, SR-IOV, VF, parent service, glossary."
---

# CXI DRA Glossary

| Term | Meaning |
|------|---------|
| **DRA** | Dynamic Resource Allocation — Kubernetes `resource.k8s.io` API this driver implements the node side of |
| **CXI** | HPE Slingshot NIC device family, exposed as `/dev/cxiN` and discovered via `/sys/class/cxi_user` |
| **ResourceClaim / ResourceClaimTemplate** | Kubernetes objects a pod uses to request a CXI device (e.g. `cxi-small`, `cxi-whole`) |
| **ResourceSlice** | Object this driver publishes per node (once, at startup), advertising devices with topology attributes (`hsn_iface`, `resource.kubernetes.io/numaNode`, `resource.kubernetes.io/pciBusID`) and consumable capacity |
| **Consumable capacity** | DRA feature (`DRAConsumableCapacity`, Kubernetes 1.36+) where each device publishes `txqs`/`tgqs`/`eqs`/`cts`/`ptes`/`acs`/`les`/`tles` and the scheduler charges each allocation's `consumedCapacity` against them; claims request it via `exactly.capacity.requests` (legacy opaque config is converted by the admission policy) |
| **ClaimBinder** | Watches ResourceClaims cluster-wide as a live-hardware safety net and sets DRA `Ready=True` / `Ready=False` / `Unschedulable` (`DeviceAtCapacity`) BindingConditions |
| **CDI** | Container Device Interface — injects `/dev/cxiN` and cgroup BPF device allowance per device, plus a shared `env` spec on every device (kdreg2/gdrdrv/xpmem nodes, `/dev/shm`, optional host libs at `/host/env`); NCCL/aws-ofi-nccl come from k8s-modules |
| **NRI** | Node Resource Interface — this driver's `RunPodSandbox`/`CreateContainer`/`StopPodSandbox` hooks |
| **HSN** | Slingshot High-Speed Network fabric; the physical interface (`hsnN` netdev) each CXI device is paired with |
| **HSN sysfs injection** | `internal/netsysfs`: a per-pod synthetic `/sys/class/net` overmount so stock libfabric discovers the HSN inside the pod; gives no real netdev |
| **Slingshot CNI / `slingshot-cni`** | Separate host CNI plugin (`cmd/slingshot-cni` + `internal/cni`) exec'd by containerd/Multus; gives the pod a real IP-addressed netdev per claimed device. Complementary to HSN sysfs injection, not a replacement |
| **CNI hint file** | `<--cni-hints-dir>/<podUID>.json`, written by `PrepareResourceClaims`; PF entries carry `{device, hsn_iface, vni}`, while VF entries also carry `schema_version: 1` and `ifname: cxiN`. `hsn_iface` is the host CNI parent; `ifname` is the preferred pod-side name |
| **IPVLAN (L2)** | Kernel virtual-interface type `slingshot-cni` creates on the host HSN and moves into the pod netns, named by `ifname` when set or otherwise `hsn_iface` (fallback `<name>_inet`) |
| **Whereabouts** | Cluster-wide CNI IPAM plugin `slingshot-cni` delegates to for HSN addresses; keys reservations on `(containerID, ifname)`, hence the per-device `CNI_IFNAME` override |
| **VNI** | Virtual Network Identifier — assigned once per namespace (not per pod/claim) by the leader-elected namespace allocator |
| **SR-IOV / VF mode** | `--vf-only` (Helm `driver.vfOnly`): the node's NIC exposes SR-IOV Virtual Functions and the driver publishes parents instead of PFs; never mixed with PF mode on a node |
| **PF / VF** | Physical Function (`cxiN` with no `device/physfn`, paired with its HSN netdev, e.g. `hsn3`) / Virtual Function (a `cxiN` whose `device/physfn` points at its PF; its netdev is named by PCI function, e.g. `enp197s0f1`). Pods get a CXI service on a VF |
| **Parent service** | CXI service on a PF (`Parent Service: Yes`) that reserves a resource budget and a VNI set for the VFs assigned to it (`/sys/class/cxi/<pf>/vf/<N>/svc_id`, `0` = unassigned). It must carry resource limits. Created by the administrator before the driver starts; the driver never creates, resizes or destroys it |
| **Parent device** | What the driver publishes in VF mode: one DRA device per parent service, `<pf>-svc<id>`, with the parent's budget (less the Ethernet system children) as consumable capacity plus a `vfs` capacity (one per allocation). VFs are not published; the VF is picked at Prepare |
| **Ethernet system child** | System-bit service the kernel creates on a VF while its netdev is up; it reserves resources (about 660 LEs, 8 PTEs, 1 TXQ/TGQ/EQ/AC measured on SHS 15.0.0) from the parent; the driver measures it on one VF with its netdev up at startup and deducts it for every VF. Never reaped |
| **`pf_netdev` / `hsn_iface` / `ifname`** | `pf_netdev`: a parent's PF netdev attribute, informational. `hsn_iface`: PF devices' attribute and the CNI hint field naming the **host** link the IPVLAN attaches to (PF netdev in PF mode, VF netdev such as `enp197s0f1` in VF mode); never published on a parent. `ifname`: optional hint field naming the **pod-side** interface (the VF's `cxiN` in VF mode; absent in PF mode, where `hsn_iface` is used) and the synthetic sysfs entry |
| **VNI selector** | `cxi-dra-vni-selector` `MutatingAdmissionPolicy` that appends a CEL selector from the namespace VNI annotation so the scheduler only allocates parents whose VNI set covers it; required for multi-parent VF nodes |
| **Reaper** | Backstop that destroys orphaned CXI services (pod netns gone for several consecutive cycles) left behind by pods that didn't tear down cleanly; runs every `--resource-refresh-interval` |
| **TXQ/TGQ/EQ/CT/PTE/LE/TLE/AC** | Per-device hardware quota fields, requested as `exactly.capacity.requests` (legacy opaque `le_pools`/`tle_pools` config is converted by the admission policy: pools × 1024 = `les`, × 512 = `tles`) — see README § Resource field reference for exact semantics |
| **`tags cxi` / mock / stub** | The three build modes of `internal/cxi` — real libcxi ioctls, synthetic devices (`--mock-devices`), and no-op, respectively |

See `CONTEXT.md` for how these concepts map to the actual packages.
