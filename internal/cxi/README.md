# internal/cxi

Hardware abstraction layer for CXI (HPE Slingshot NIC) devices. Everything else in the driver talks to hardware only through this package's exported types.

## Files

| File | Purpose |
|------|---------|
| `device.go` | `Device` struct (name, index, dev path, sysfs path, NUMA node, PCI bus ID, HSN interface, capacity, reserved, and the SR-IOV fields `FunctionType`, `PFName`, `VFIndex`, `ParentSvcID`, `PodIfName`) and `Discover(DiscoverOptions)`, which walks `/sys/class/cxi_user`, classifies each device as a PF or a VF (`device/physfn` present), keeps PFs by default or VFs with `VFOnly` (never both), reads capacity, NUMA node and PCI bus ID, and pairs each device with its HSN interface. The `discoverer` type takes the sysfs directories and libcxi reads as seams for unit tests |
| `resources.go` | `ResourceSet` struct (the per-device hardware quota counters: `txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `le_pools`, `tle_pools`, `acs`) and its `Fits`/`Minus` operations — see [README.md](../../README.md#resource-field-reference) for the exact semantics of each field |
| `hsn.go` | `PairDevice()` — pairs a CXI device with the netdev (any name, e.g. `hsn3` for a PF, `enp197s0f1` for a VF) whose resolved `device` symlink equals the CXI device's PCI function path |
| `service.go` | `ServiceManager` interface — CXI service (virtual network interface) CRUD: create, destroy, enable/disable |
| `capacity_cgo.go` / `capacity_stub.go` | Real (`tags cxi`, libcxi ioctls) vs. non-CGO fallback capacity reads |
| `service_cgo.go` / `service_mock.go` / `service_stub.go` | Real, synthetic (`--mock-devices`), and no-op service implementations |
| `mock.go` | `MockDevices(n)` — generates `n` synthetic devices for kind/local dev and tests |
| `parent.go` | VF-mode read path (SR-IOV): `ReadParentBudget` (parent `res` + `VNISet`, `ErrParentUnlimited` when the parent has no resource limits), `ReadParentRemaining` (parent `res` − Σ `res` of every service on the parent's VFs, system children included, raw LE/TLE), `ReadVFSystemReservation` (Σ `res` of all system-bit services on a VF). The math is build-independent; only `readParentDescriptor`/`readVFServices` differ per mode (`service_cgo.go` / `capacity_stub.go`) |
| `vniset.go` | `VNISet` — a parent's VNI set in range or list form (a contiguous list is normalized to a range): `Contains`, `Covers`, `Range`, `List`, and `String` (`"min-max"` or the delimiter-wrapped `",40,42,"` used for the `vni_set` slice attribute) |
| `service_mock.go` (topology) | `SetMockTopology`/`ResetMockTopology` — the in-memory parents and per-VF service lists that the non-cxi build of the parent read path answers from |

## Build modes

This package has **three** implementations selected by build tag, never by runtime `if` branches in one file:

| Mode | Build tag | Files | Use |
|------|-----------|-------|-----|
| Real hardware | `tags cxi` | `*_cgo.go` | `make build` — requires libcxi headers on the build host (a Slingshot node); `make image` builds libcxi in-container |
| Synthetic | (default, `--mock-devices N`) | `mock.go` | `make build-mock` — kind clusters, local dev |
| No-op | (default) | `*_stub.go` | Compiles cleanly when neither real nor mock devices are configured |

New hardware-facing code should follow this same three-file split — see [.github/instructions/code-style.instructions.md](../../.github/instructions/code-style.instructions.md).

## Testing

VF child creation accepts `ServiceOptions{Parent: parent}`; existing calls without
options preserve PF behavior. The child uses the parent's `limits.les.max`
(`ParentDescriptor.LEMax`), reserves no LEs/TLEs, and restricts itself to one VNI.
The six tracked resources retain `res == max`. The mock serializes budget checks
and updates across sibling VFs, includes system reservations, rejects out-of-set
VNIs/overcommit, and releases reservations on deletion.
VF creation errors identify the failing operation; parent-budget and VNI/descriptor
diagnostics apply only to allocation errors, not later configuration failures.
The optional argument preserves existing PF calls, but implementations of the
`ServiceManager` interface must accept the variadic `ServiceOptions` parameter.

`service_failure_test.go` compiles the production C creation routine against fake
libcxi calls using `cc` (no hardware or libcxi headers needed). It forces open,
allocation, disable, netns-binding, enable, and destroy failures for PF and VF
requests. It checks operation ordering, destruction of the allocated service,
simulated reservation release, device closure, preservation of the original
error, and orphan warnings when destruction fails. These checks skip explicitly
if `cc` is unavailable. They do not prove real-kernel cleanup under failure.

`device_test.go` and `parents_test.go` cover PF/VF discovery, sysfs parent-service
resolution, and stable grouping by (PF, service ID). `parents_limits_test.go`
checks discovery's rejection of unlimited or unreadable parents through the
mock topology. `parents.go` defines `Parent` / `Parents()` and reads each VF's
assignment from `/sys/class/cxi/<pf>/vf/<N>/svc_id`.

`vniset_test.go` and `parent_test.go` cover `VNISet` and the VF-mode math (parent minus children across VFs, system children, LE/TLE, unlimited parent) against the mock topology; `parent_test.go` is `!cxi`-only because it drives `SetMockTopology`. `hsn_test.go` covers pairing. The rest of `capacity_cgo.go`/`service_cgo.go` requires real hardware, and the mock/stub paths are exercised indirectly through `internal/driver`'s tests and the sibling `cxi-dra-integration-tests` repo. `mock.go` is the fixture to reuse rather than hand-rolling synthetic devices.
