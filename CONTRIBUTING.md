# Contributing

## Branching and commits

- Target pull requests at `main`. `main` is the protected/deployed branch — never push directly to it.
- Keep commits scoped to one logical change.

## Before opening a PR

- Run `go test ./...`, `make vet`, and `golangci-lint run` — all three are enforced in CI (`.github/workflows/build.yaml`).
- If your change touches `internal/cxi`'s `_cgo.go` files, attempt a `make build` on an ARM64 Slingshot node with libcxi headers, or note in the PR that this wasn't possible.
- If your change affects pod lifecycle behavior (`internal/driver`, `internal/nri`, `internal/cdi`), verify it against the sibling `cxi-dra-integration-tests` suite — this repo's own CI cannot reach a live cluster.
- Use `.github/pull_request_template.md` to structure the description.

## Adding a new hardware-abstraction path

`internal/cxi` splits real/mock/stub implementations into separate files (`_cgo.go`, `mock.go`, `_stub.go`) rather than build-tag `if` branches in one file. Follow this pattern for any new hardware-facing code — see `.github/instructions/code-style.instructions.md`.

## Updating deployment manifests

Both `deploy/helm/cxi-dra-driver` and `deploy/kubernetes/` must stay in sync for any change to RBAC, the DeviceClass, or the MutatingAdmissionPolicy — the Helm chart is the recommended path but the raw manifests are kept as a lighter alternative (see README § Deployment).

## First task suggestions

New to this repo? A good first contribution is adding a unit test for an untested `internal/driver` function, or improving a godoc comment against the standard set in `.github/instructions/code-style.instructions.md`.
