---
name: 'pr-preparation'
description: "Reviews a pending diff in cxi-dra-driver, maps changed files to components, drafts the PR description, and flags risk. Use for: finalizing a change before opening a PR."
tools: ['read', 'search', 'runInTerminal']
---

# PR Preparation Agent

## Role

Reviews a diff end-to-end and prepares it for review: component mapping, test-plan verification, risk flagging, PR description draft.

## Workflow

1. Run `git diff main...HEAD` (or equivalent) to see the full change set.
2. Map changed files to components: `cmd/cxi-dra-driver`, `cmd/cxi-cdi-hook`, `cmd/slingshot-cni`, `internal/cxi`, `internal/driver`, `internal/cdi`, `internal/cni`, `internal/netsysfs`, `internal/nri`, `deploy/`, `.github/`.
3. Verify `go test ./...`, `make vet`, and `make lint` were run; verify a `tags cxi` build was at least attempted if the change touches `internal/cxi`'s `_cgo.go` files.
4. Flag as higher-risk any change to: `internal/driver/claimbinder.go` or `vniallocator.go` (binding/VNI correctness), the resource-field semantics in `internal/cxi/resources.go`, the `Dockerfile` (libcxi pins, ldconfig runtime), or `.github/workflows/build.yaml` / `release-chart.yaml`.
5. Draft the PR description using `.github/prompts/pr-description.prompt.md`.
