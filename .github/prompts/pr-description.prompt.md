---
description: "Drafts a PR description for a pending change in cxi-dra-driver, mapping the diff to affected components and flagging risk."
agent: 'agent'
---

# Draft a PR description

1. Run `git diff main...HEAD` (or equivalent) to see the full change set.
2. Map changed files to components: `cmd/cxi-dra-driver`, `cmd/cxi-cdi-hook`, `cmd/slingshot-cni`, `internal/cxi`, `internal/driver`, `internal/cdi`, `internal/cni`, `internal/netsysfs`, `internal/nri`, `deploy/`, `.github/`.
3. Confirm `go test ./...`, `make vet`, and `make lint` were run; note if a `tags cxi` build was needed and whether it was possible in this environment.
4. Flag as higher-risk any change touching `internal/driver/claimbinder.go`/`vniallocator.go`, resource-field semantics in `internal/cxi/resources.go`, the `Dockerfile` (libcxi pins, ldconfig runtime), or `.github/workflows/build.yaml` / `release-chart.yaml`.
5. Use `.github/pull_request_template.md` as the description skeleton.
6. Reference the `pr-preparation` agent for a fuller review pass before submitting.
