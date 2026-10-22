---
description: "CXI/DRA domain invariants and anti-patterns for the CXI DRA node driver."
applyTo: "**"
---

# Domain Patterns

Domain model and rationale: [CONTEXT.md](../../CONTEXT.md). Term definitions: the `cxi-dra-glossary` skill.

## Do

- Read the namespace's `cxi.slingshot.hpe.com/vni` annotation in `PrepareResourceClaims`; VNIs are namespace-scoped and assigned once by the leader-elected allocator (`internal/driver/vniallocator.go`). Pods never request a VNI.
- Treat capacity as DRA consumable capacity: the `ResourceSlice` is static and published once, the scheduler does the accounting (`consumedCapacity`), and `ClaimBinder` is only the live-hardware safety net (`Ready=True` / `Ready=False` / `Unschedulable` `DeviceAtCapacity`).
- Treat `le_pools: 0` / `tle_pools: 0` as "shared best-effort default pool", not "no capacity" (README § Resource field reference).
- Keep `slingshot-cni`'s IPVLAN interface and `internal/netsysfs`'s synthetic sysfs side by side: libfabric needs the sysfs, IP consumers need the netdev.
- Keep netlink calls behind `internal/cni`'s `linkManager` seam, override `CNI_IFNAME` per device when delegating to Whereabouts, and keep CNI `DEL` idempotent (missing hint/netns/link = success; only IPVLAN links are ever deleted).
- Keep the hint-file writer (`internal/driver.CNIHint`) and reader (`internal/cni.Hint`) as separate types that agree only on the JSON schema; update [docs/cni/cnidra-cni-hint-contract.md](../../docs/cni/cnidra-cni-hint-contract.md) with any schema change.
- Keep `deploy/helm/cxi-dra-driver` and `deploy/kubernetes/` in sync for RBAC, DeviceClass, and admission-policy changes. The VNI allocator's Lease is named `<driverName>-vni-allocator-leader`.

## Do not

- Do not build the real (`tags cxi`) binary without libcxi headers; use `make build-mock`, or `make image`, which builds libcxi from pinned SHS sources.
- Do not let the `Dockerfile` libcxi pins (`CASSINI_HEADERS_REF`, `CXI_DRIVER_REF`, `LIBCXI_REF`) drift from the SHS release on the nodes; skew shows up at runtime as `cxil_*` errors, not at build time.
- Do not reintroduce `avail_*`/`capacity_*`/`active_namespaces` attributes, per-namespace device occupancy, the occupancy reconciler, or periodic `ResourceSlice` republishing; SHS 14.0.1 lifted the one-namespace-per-device constraint.
- Do not add NCCL/aws-ofi-nccl CDI profiles back; k8s-modules injects them. The shared `env` spec is always attached to every device.
- Do not hand-roll a VNI per pod or per claim.
