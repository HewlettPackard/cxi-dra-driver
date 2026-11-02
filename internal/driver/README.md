# internal/driver

The core DRA (Dynamic Resource Allocation) plugin. This package owns device publishing (a static `ResourceSlice` with consumable capacity), claim binding, pod allocation state, and the orphan-service reaper that keeps the node's CXI services consistent with the pods that exist.

## Files

| File | Purpose |
|------|---------|
| `driver.go` | `Driver` — top-level type; owns devices and pending grants; wires the other files together, publishes the `ResourceSlice` once, and runs the reaper ticker |
| `claimbinder.go` | `ClaimBinder` — watches `ResourceClaim`s cluster-wide as a live-hardware safety net and sets DRA BindingConditions: `Ready=True`, `Ready=False` (`InsufficientCapacity`), or `Unschedulable` (`DeviceAtCapacity`; `VNINotInParentSet` in VF mode) |
| `capacity.go` | Consumable-capacity model: capacity names, per-allocation request defaults, `buildCapacity` (usable ceiling → `DeviceCapacity` with request policy; LE/TLE step of one pool) and `resourceSetFromConsumed` (an allocation's `consumedCapacity` → `ResourceSet`) |
| `resourceslice.go` | Builds the static per-node `ResourceSlice`: topology attributes (`type`, `function_type`, `hsn_iface` on PFs, `resource.kubernetes.io/numaNode`, `resource.kubernetes.io/pciBusID`), consumable capacity, and the `Ready`/`Unschedulable` binding conditions. A VF-mode parent also gets `pf_name`, `pf_netdev`, `parent_svc_id`, the VNI set (`vni_min`/`vni_max` or `vni_set`) and a `vfs` capacity |
| `devicestatus.go` | Publishes per-device status (VNI, svc_id) onto the `ResourceClaim` status for operator introspection (`RecordSvcID`, called by the NRI plugin) |
| `prepare.go` | `PrepareResourceClaims` — kubelet plugin hook; reads the namespace's VNI annotation, resolves the request from the allocation's `consumedCapacity`, performs the static ceiling check, stores a `PodAllocation`, writes the CNI hint file, returns CDI device IDs; `recordSharedClaimPods` does the same for pods that reserve an already-prepared claim (called from the claim watch) |
| `unprepare.go` | `UnprepareResourceClaims` — kubelet plugin hook; removes the `PodAllocation` and deletes the pod's CNI hint file (best-effort) on pod deletion |
| `cnihints.go` | Writes/deletes the per-pod CNI hint file (the read-side contract for `slingshot-cni`, see `internal/cni`) and the observability-only pod annotation — see [docs/cni/cnidra-cni-hint-contract.md](../../docs/cni/cnidra-cni-hint-contract.md) |
| `store.go` | `AllocationStore` — thread-safe in-memory map of pod UID → `PodAllocation` (device, HSN, VNI, resources; in VF mode also the parent and ShareID) |
| `views.go` | Published vs programmable devices: PFs in PF mode; in VF mode parents are published (`parentSet`, ceiling from the parent budget) and VFs are programmed. `liveAvailable` / `AvailableFor` read the parent's remaining budget |
| `vfselect.go` | VF mode: per-parent locking and lowest-free-VF selection at Prepare (`programmableFor`), and startup recovery of VF occupancy from hint files, claims and VF service lists (`recoverVFState`) |
| `reaper.go` | Backstop that runs every `--resource-refresh-interval`: destroys CXI services whose pod netns is gone for several consecutive cycles, never touching blessed (PF mode only; ignored with `--vf-only`), kernel-system (incl. the VF Ethernet child), or NRI-tracked live services. In VF mode it warns (first sight, then every `reapWarnEveryCycles`) about non-system VF children that no live pod or hint file accounts for |
| `vniallocator.go` | Leader-elected namespace controller — assigns the lowest free VNI to each namespace and writes the `cxi.slingshot.hpe.com/vni` annotation |
| `vnipool.go` | Shared leader-election and lease-management plumbing used by the VNI allocator; the Lease is named `<driverName>-vni-allocator-leader` |
| `vni.go` | VNI bounds and validation (`MinVNI`/`MaxVNI`, `ValidVNI`, `ValidateVNIRange`) guarding the uint16 hardware field |

## Key invariant: namespace-scoped VNI

Pods never carry their own VNI. `vniallocator.go` assigns exactly one VNI per namespace (leader-elected, so it's safe to run multiple driver replicas), and `prepare.go` reads that namespace annotation at claim-prepare time. See [CONTEXT.md](../../CONTEXT.md) for the full domain model.

## Testing

`capacity_test.go`, `driver_test.go`, `claimbinder_test.go`, `prepare_test.go`, `resourceslice_test.go`, `store_test.go`, `vfselect_test.go`, `reaper_test.go`, `vniallocator_test.go`, `vni_test.go`, and `cnihints_test.go` cover this package — the strongest unit-test coverage in the repository. `devicestatus.go` and `unprepare.go` currently have no dedicated tests; consider these first when adding coverage.
