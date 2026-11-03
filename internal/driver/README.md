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

## SR-IOV VF mode (`--vf-only`)

The driver publishes one thing and programs another. `views.go` splits them: `publishedDevices` is what the static `ResourceSlice` advertises (PFs in PF mode; in VF mode one **parent** per (PF, parent service), named `<pf>-svc<id>`, with `vfs` capacity), and `programmableDevices` is what services are created on (PFs, or the VFs). VFs are never published.

- **Prepare** (`vfselect.go`, `programmableFor`): under a per-parent lock, gives each allocation result the lowest free VF of its parent, records `PodAllocation{DeviceName: vf, ParentName, ShareID}` and writes the CNI hint for that VF. After the NRI plugin creates the service, `RecordSvcID` reports the VF as `vf` in the claim's device status. Results of one claim get distinct VFs, a re-Prepare keeps them, and a parent with no free VF fails Prepare (kubelet retries). The `AllocationStore` (plus VFs held by a pod child with no hint after a restart, see Restart) is the source of truth for "VF in use"; nothing in the Prepare hot path rescans hardware.
- **Live reads** (`views.go`): `liveAvailable` / `AvailableFor` read the parent's remaining budget from the hardware. `ClaimBinder` and the NRI fit check use it, and pending grants are keyed by (device, ShareID).
- **VNI**: the admission policy keeps the scheduler off parents whose VNI set misses the namespace VNI. Prepare and `ClaimBinder` re-check it (`VNINotInParentSet`) as a fallback for claims created before the namespace was annotated.
- **Restart** (`recoverVFState`): rebuilds VF occupancy from hint files, live claims and VF service lists; a VF with a pod child but no hint stays busy until the reaper removes the child.
- **Reaper**: iterates only the discovered VFs, ignores `--system-svc-ids`, never deletes a system-bit service (the VF Ethernet child), and warns about non-system VF children it cannot attribute to a pod.

## Testing

`capacity_test.go`, `driver_test.go`, `claimbinder_test.go`, `prepare_test.go`, `resourceslice_test.go`, `resourceslice_parent_test.go` (parent attributes, `vfs`, VNI forms, attribute+capacity limit), `store_test.go`, `vfselect_test.go`, `vfintegration_test.go`, `serviceoptions_test.go`, `reaper_test.go`, `vniallocator_test.go`, `vni_test.go`, and `cnihints_test.go` cover this package — the strongest unit-test coverage in the repository. `devicestatus.go` and `unprepare.go` currently have no dedicated tests; consider these first when adding coverage.
