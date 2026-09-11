---
description: "Go documentation and naming conventions for the CXI DRA node driver."
applyTo: "**/*.go"
---

# Code Style

- Exported types and functions carry a godoc-style `//` comment explaining what they do and, for non-obvious logic, why — see [internal/driver/claimbinder.go](../../internal/driver/claimbinder.go) (`ClaimBinder`) or [internal/nri/plugin.go](../../internal/nri/plugin.go) (`Plugin`) as the reference quality bar. Match this level of detail for new exported symbols.
- Package-level "why this exists" doc comments are expected for packages with non-obvious rationale — see [internal/netsysfs/build.go](../../internal/netsysfs/build.go).
- File naming mirrors responsibility, not type: `capacity_cgo.go`/`capacity_stub.go`, `service_cgo.go`/`service_mock.go`/`service_stub.go` — keep the `_cgo`/`_mock`/`_stub` suffix convention for any new hardware-abstraction split rather than inventing a new pattern.
- No magic numbers for hardware quota fields (`txqs`, `tgqs`, `eqs`, `cts`, `ptes`, `le_pools`, `tle_pools`, `acs`) — name or comment the constant with the hardware reason, following the existing style in `internal/cxi/resources.go`.
- Run `golangci-lint run` before submitting — see `.golangci.yml` for the enabled linters (includes `gosec` for the security-relevant host/CGO paths).

## Do not

- Do not add TODO/FIXME comments as a substitute for follow-up tracking — the codebase currently has none; open an issue instead.
- Do not collapse the `_cgo`/`_mock`/`_stub` split into a single file with build-tag `if` branches — the separate-file pattern keeps CGO-only code out of non-cxi builds entirely.
