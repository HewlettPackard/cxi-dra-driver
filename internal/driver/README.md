# internal/driver

The core DRA (Dynamic Resource Allocation) plugin. This package owns device publishing, claim binding, pod allocation state, and the two safety nets (reconciler, reaper) that keep in-memory state consistent with the cluster and the hardware.

## Files

| File | Purpose |
|------|---------|
| `driver.go` | `Driver` — top-level type; owns devices, pending grants, occupancy; wires the other files together and runs their lifecycles |
| `claimbinder.go` | `ClaimBinder` — watches `ResourceClaim`s cluster-wide, runs the real capacity fit-check, and sets DRA `Ready`/`Unschedulable` BindingConditions |
| `resourceslice.go` | Builds and publishes the per-node `ResourceSlice`, including live `avail_*` attributes and `active_namespaces` |
| `devicestatus.go` | Publishes per-device status onto the `ResourceSlice` |
| `prepare.go` | `PrepareResourceClaims` — kubelet plugin hook; reads the namespace's VNI annotation, performs the static ceiling check, stores a `PodAllocation`, returns CDI device IDs |
| `unprepare.go` | `UnprepareResourceClaims` — kubelet plugin hook; removes the `PodAllocation` on pod deletion |
| `cnihints.go` | Writes/deletes the per-pod CNI hint file (the Slingshot CNI plugin's read-side contract) and the observability-only pod annotation — see [docs/cnidra-cni-hint-contract.md](../../docs/cnidra-cni-hint-contract.md) |
| `store.go` | `AllocationStore` — thread-safe in-memory map of pod UID → `PodAllocation` (device, HSN, VNI, resources) |
| `reconciler.go` | Poll-driven safety net: periodically rebuilds occupancy from live CXI services, covering driver restarts and missed events |
| `reaper.go` | Periodically scans for CXI services with no owning pod and deletes them |
| `vniallocator.go` | Leader-elected namespace controller — assigns the lowest free VNI to each namespace and writes the `cxi.slingshot.hpe.com/vni` annotation |
| `vnipool.go` | Shared leader-election and lease-management plumbing used by the VNI allocator |
| `vni.go` | Legacy VNI utilities kept for the (now namespace-annotation-based) allocation model |

## Key invariant: namespace-scoped VNI

Pods never carry their own VNI. `vniallocator.go` assigns exactly one VNI per namespace (leader-elected, so it's safe to run multiple driver replicas), and `prepare.go` reads that namespace annotation at claim-prepare time. See [CONTEXT.md](../../CONTEXT.md) for the full domain model.

## Testing

`driver_test.go`, `claimbinder_test.go`, `prepare_test.go`, `reconciler_test.go`, `store_test.go`, `vniallocator_test.go`, `vni_test.go`, and `cnihints_test.go` cover this package — the strongest unit-test coverage in the repository. `resourceslice.go`, `devicestatus.go`, `unprepare.go`, and `reaper.go` currently have no dedicated tests; consider these first when adding coverage.
