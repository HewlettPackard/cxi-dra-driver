---
description: "Go build, test, and CI conventions for the CXI DRA node driver."
applyTo: "**/*.go"
---

# Build & Test

- Module: `github.com/slingshot/cxi-dra-driver`, Go 1.24.
- Real hardware build (CGO, links libcxi): `make build` — requires an ARM64 Slingshot node with libcxi headers. Never run this on a laptop or generic CI runner.
- Hardware-free build (kind/local dev, tests, CI): `make build-mock` (driver) / `make build-hook` (hook binary — always CGO-free regardless of driver mode).
- Unit tests: `go test ./...` — also run in CI (`.github/workflows/build.yaml`).
- Lint: `golangci-lint run` (config in `.golangci.yml`, includes `gosec`) — also enforced in CI.
- `make vet` / `go vet ./...` — required.

## Do

- Add new hardware-facing logic to `internal/cxi` behind the existing three-way split: `_cgo.go` (`tags cxi`, real libcxi ioctls), `mock.go` (`--mock-devices`), `_stub.go` (default no-op) — see [internal/cxi/service_cgo.go](../../internal/cxi/service_cgo.go) and its `_mock`/`_stub` siblings as the template.
- Write unit tests for anything added to `internal/driver` or `internal/cdi` — these packages have the strongest existing test coverage and are covered by CI.
- Reference the exact flag name from [cmd/cxi-dra-driver/main.go](../../cmd/cxi-dra-driver/main.go) when documenting driver behavior — flags there are the single source of truth for env-var-equivalent overrides (`NODE_NAME`, `POD_UID`, `POD_NAMESPACE`).

## Do not

- Do not add test coverage expectations to `internal/nri` or the real (`_cgo.go`) paths of `internal/cxi` without a live cluster/hardware — these are integration-tested by the sibling `cxi-dra-integration-tests` repo, not unit-testable in isolation.
- Do not assume `go test ./...` needs `tags cxi` — the stub/mock build modes are the default and are what CI runs.
