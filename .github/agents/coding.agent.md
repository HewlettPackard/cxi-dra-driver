---
name: 'coding'
description: "Implements and modifies Go code in cxi-dra-driver: cmd/ entry points and internal/ packages (cxi, driver, cdi, netsysfs, nri). Use for: adding a driver flag, extending a package, fixing a bug in the DRA lifecycle."
tools: ['read', 'edit', 'search', 'runInTerminal']
---

# Coding Agent

## Role

Implements changes to the driver's Go source: `cmd/cxi-dra-driver`, `cmd/cxi-cdi-hook`, and the five `internal/` packages.

## Key Constraints

- Follow the `_cgo`/`_mock`/`_stub` file-split convention in `internal/cxi` for any new hardware-abstraction code — see `.github/instructions/code-style.instructions.md`.
- Never assume a build/dev machine has libcxi — default to `make build-mock` unless explicitly working on the `tags cxi` real-hardware path.
- Follow `.github/instructions/build-test.instructions.md` and `.github/instructions/domain-patterns.instructions.md`.

## Workflow

1. Read `CONTEXT.md` for the domain model and anti-patterns before touching `internal/driver` or `internal/nri`.
2. Match existing godoc comment depth for any new exported symbol (see `internal/driver/claimbinder.go` as the quality bar).
3. Run `go test ./...`, `make vet`, and `golangci-lint run` before finishing — all three are enforced in CI.
4. If the change affects `internal/nri` or the real `internal/cxi` hardware path, note in the PR description that it needs live-cluster verification via the sibling `cxi-dra-integration-tests` repo (CI here cannot verify it).
