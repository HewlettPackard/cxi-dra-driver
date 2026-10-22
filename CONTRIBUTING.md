# Contributing

## Branching and commits

- Target pull requests at `main`. `main` is the protected/deployed branch — never push directly to it.
- Keep commits scoped to one logical change.

## Before opening a PR

- Run `go test ./...`, `make vet`, and `golangci-lint run`. CI (`.github/workflows/build.yaml`) enforces only `go vet` and `go test`; `golangci-lint` is a local check, so run it yourself.
- If your change touches `internal/cxi`'s `_cgo.go` files, check that `make image` still builds (it compiles libcxi from pinned SHS sources in-container), or run `make build` on a Slingshot node with libcxi headers, or note in the PR that neither was possible.
- If your change affects pod lifecycle behavior (`internal/driver`, `internal/nri`, `internal/cdi`), verify it against the sibling `cxi-dra-integration-tests` suite — this repo's own CI cannot reach a live cluster.
- Use `.github/pull_request_template.md` to structure the description.

## Adding a new hardware-abstraction path

`internal/cxi` splits real/mock/stub implementations into separate files (`_cgo.go`, `mock.go`, `_stub.go`) rather than build-tag `if` branches in one file. Follow this pattern for any new hardware-facing code — see `.github/instructions/code-style.instructions.md`.

## Updating deployment manifests

Both `deploy/helm/cxi-dra-driver` and `deploy/kubernetes/` must stay in sync for any change to RBAC (including the VNI allocator's `<driverName>-vni-allocator-leader` Lease), the DeviceClass, or the MutatingAdmissionPolicies — the Helm chart is the recommended path but the raw manifests are kept as a lighter alternative (see README § Deployment).

## First task suggestions

New to this repo? A good first contribution is adding a unit test for an untested `internal/driver` function, or improving a godoc comment against the standard set in `.github/instructions/code-style.instructions.md`.
