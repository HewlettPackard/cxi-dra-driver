# internal/cxi

Hardware abstraction layer for CXI (HPE Slingshot NIC) devices. Everything else in the driver talks to hardware only through this package's exported types.

## Files

| File | Purpose |
|------|---------|
| `device.go` | `Device` struct (name, index, dev path, sysfs path, NUMA node, PCIe root, HSN interface, GPU PCIe root) and `Discover()`, which walks `/sys/class/cxi_user`, reads capacity, pairs each device with its HSN interface, and finds NUMA-local GPUs |
| `resources.go` | `ResourceSet` struct (the per-device hardware quota counters: `txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `le_pools`, `tle_pools`, `acs`) and its `Fits`/`Minus` operations — see [README.md](../../README.md#resource-field-reference) for the exact semantics of each field |
| `hsn.go` | `DiscoverHSN()` / `PairDevice()` — matches CXI PCIe roots to HSN netdev names via device topology |
| `gpu.go` | `GPUPCIeRootByNUMA()` — queries NVIDIA GPUs via sysfs, maps NUMA node → PCIe root for co-location hints |
| `service.go` | `ServiceManager` interface — CXI service (virtual network interface) CRUD: create, destroy, enable/disable |
| `capacity_cgo.go` / `capacity_stub.go` | Real (`tags cxi`, libcxi ioctls) vs. non-CGO fallback capacity reads |
| `service_cgo.go` / `service_mock.go` / `service_stub.go` | Real, synthetic (`--mock-devices`), and no-op service implementations |
| `mock.go` | `MockDevices(n)` — generates `n` synthetic devices for kind/local dev and tests |

## Build modes

This package has **three** implementations selected by build tag, never by runtime `if` branches in one file:

| Mode | Build tag | Files | Use |
|------|-----------|-------|-----|
| Real hardware | `tags cxi` | `*_cgo.go` | `make build` — requires libcxi headers on an ARM64 Slingshot node |
| Synthetic | (default, `--mock-devices N`) | `mock.go` | `make build-mock` — kind clusters, local dev |
| No-op | (default) | `*_stub.go` | Compiles cleanly when neither real nor mock devices are configured |

New hardware-facing code should follow this same three-file split — see [.github/instructions/code-style.instructions.md](../../.github/instructions/code-style.instructions.md).

## Testing

No unit tests exist in this package today — `capacity_cgo.go`/`service_cgo.go` require real hardware, and the mock/stub paths are exercised indirectly through `internal/driver`'s tests and the sibling `cxi-dra-integration-tests` repo. `mock.go` is the fixture to reuse rather than hand-rolling synthetic devices.
