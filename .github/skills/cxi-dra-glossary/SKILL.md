---
name: cxi-dra-glossary
description: "Glossary of CXI/DRA/Kubernetes domain terms used throughout this driver. USE FOR: understanding source comments, README, and CONTEXT.md; writing new code that touches DRA/CXI/CDI/NRI/CNI concepts. Trigger keywords: DRA, CXI, CDI, NRI, CNI, ResourceClaim, ResourceSlice, HSN, VNI, IPVLAN, Whereabouts, hint file, reconciler, reaper, glossary."
---

# CXI DRA Glossary

| Term | Meaning |
|------|---------|
| **DRA** | Dynamic Resource Allocation — Kubernetes `resource.k8s.io` API this driver implements the node side of |
| **CXI** | HPE Slingshot NIC device family, exposed as `/dev/cxiN` and discovered via `/sys/class/cxi_user` |
| **ResourceClaim / ResourceClaimTemplate** | Kubernetes objects a pod uses to request a CXI device (e.g. `cxi-small`, `cxi-whole`) |
| **ResourceSlice** | Object this driver publishes per node, advertising devices and live capacity attributes (`avail_txqs`, `avail_tles`, `active_namespaces`) |
| **ClaimBinder** | Watches ResourceClaims cluster-wide, runs the real fit-check, and sets DRA `Ready`/`Unschedulable` BindingConditions |
| **CDI** | Container Device Interface — injects `/dev/cxiN`, optional env/NCCL/AWS-OFI-NCCL library specs, and cgroup BPF device allowance |
| **NRI** | Node Resource Interface — this driver's `RunPodSandbox`/`CreateContainer`/`StopPodSandbox` hooks |
| **HSN** | Slingshot High-Speed Network fabric; the physical interface (`hsnN` netdev) each CXI device is paired with |
| **HSN sysfs injection** | `internal/netsysfs`: a per-pod synthetic `/sys/class/net` overmount so stock libfabric discovers the HSN inside the pod; gives no real netdev |
| **Slingshot CNI / `slingshot-cni`** | Separate host CNI plugin (`cmd/slingshot-cni` + `internal/cni`) exec'd by containerd/Multus; gives the pod a real IP-addressed netdev per claimed device. Complementary to HSN sysfs injection, not a replacement |
| **CNI hint file** | `<--cni-hints-dir>/<podUID>.json`, written by `PrepareResourceClaims`, one `{device, hsn_iface, vni}` per device; the only handoff from the driver daemon to the CNI process |
| **IPVLAN (L2)** | Kernel virtual-interface type `slingshot-cni` creates on the host HSN and moves into the pod netns, named `<hsn_iface>` (fallback `<hsn_iface>_inet`) |
| **Whereabouts** | Cluster-wide CNI IPAM plugin `slingshot-cni` delegates to for HSN addresses; keys reservations on `(containerID, ifname)`, hence the per-device `CNI_IFNAME` override |
| **VNI** | Virtual Network Identifier — assigned once per namespace (not per pod/claim) by the leader-elected namespace allocator |
| **Reconciler** | Poll-driven safety net rebuilding in-memory device occupancy from live CXI services (covers driver restarts, missed events) |
| **Reaper** | Cleans up orphaned CXI services left behind by pods that didn't tear down cleanly |
| **TXQ/TGQ/EQ/CT/PTE/LE/TLE/AC** | Per-device hardware quota fields requested via `ResourceClaimTemplate` opaque parameters — see README § Resource field reference for exact semantics |
| **`tags cxi` / mock / stub** | The three build modes of `internal/cxi` — real libcxi ioctls, synthetic devices (`--mock-devices`), and no-op, respectively |

See `CONTEXT.md` for how these concepts map to the actual packages.
