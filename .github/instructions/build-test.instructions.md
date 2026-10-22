---
description: "Go build, test, and CI conventions for the CXI DRA node driver."
applyTo: "**/*.go"
---

# Build & Test

- Module: `github.com/slingshot/cxi-dra-driver`, Go 1.24.
- Real hardware build (CGO, links libcxi): `make build` — requires libcxi headers on the build host (a Slingshot node). Never run this on a laptop or generic CI runner; `make image` builds libcxi from pinned SHS sources inside the container and needs no host libraries or staged `bin/` artifacts.
- Hardware-free build (kind/local dev, tests, CI): `make build-mock` (driver) / `make build-hook` (hook binary — always CGO-free regardless of driver mode) / `make build-cni` (`slingshot-cni` host CNI plugin — always CGO-free, honours `GOARCH`; the production image builds it itself and the driver optionally installs it onto nodes). `make image-mock` builds the no-hardware image from `Dockerfile.mock` (used by `make kind-load`).
- Unit tests: `go test ./...` — also run in CI (`.github/workflows/build.yaml`).
- Lint: `golangci-lint run` — run it locally; it is **not** run in CI.
- `make vet` / `go vet ./...` — required.

## Do

- Add new hardware-facing logic to `internal/cxi` behind the existing three-way split: `_cgo.go` (`tags cxi`, real libcxi ioctls), `mock.go` (`--mock-devices`), `_stub.go` (default no-op) — see [internal/cxi/service_cgo.go](../../internal/cxi/service_cgo.go) and its `_mock`/`_stub` siblings as the template.
- Write unit tests for anything added to `internal/driver` or `internal/cdi` — these packages have the strongest existing test coverage and are covered by CI.
- In `internal/cni`, keep netlink calls behind the `linkManager` interface (`network.go`; real impl `network_linux.go`, stub `network_other.go`) and IPAM exec behind `ipamDelegate` (`ipam.go`), and test orchestration changes in `cmd_test.go` against the in-memory fakes so they run on any platform. Anything touching the real netlink path needs a case in `network_linux_test.go` (root-only; run via `podman run --rm --privileged -v "$PWD":/src -w /src golang:1.24-alpine go test ./internal/cni/` from macOS). IPAM-contract changes go in `ipam_test.go`, which execs the fake plugin in `testdata/fakewhereabouts`.
- Reference the exact flag name from [cmd/cxi-dra-driver/main.go](../../cmd/cxi-dra-driver/main.go) when documenting driver behavior — flags there are the single source of truth for env-var-equivalent overrides (`NODE_NAME`, `POD_UID`, `POD_NAMESPACE`).

## Do not

- Do not add test coverage expectations to `internal/nri` or the real (`_cgo.go`) paths of `internal/cxi` without a live cluster/hardware — these are integration-tested by the sibling `cxi-dra-integration-tests` repo, not unit-testable in isolation.
- Do not expect `internal/cni`'s `network_linux_test.go` to run on a macOS/unprivileged CI runner — it skips unless root on Linux; and do not treat its dummy-parent + fake-IPAM coverage as a substitute for live verification against a real HSN and real Whereabouts.
- Do not assume `go test ./...` needs `tags cxi` — the stub/mock build modes are the default and are what CI runs.
