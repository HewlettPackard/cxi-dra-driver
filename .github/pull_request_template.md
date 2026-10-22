## Summary

<!-- What does this change do and why? Link the ticket/issue if one exists. -->

## Components touched

<!-- cmd/cxi-dra-driver, cmd/cxi-cdi-hook, cmd/slingshot-cni, internal/cxi, internal/driver, internal/cdi, internal/cni, internal/netsysfs, internal/nri, deploy/, .github/ -->

## Test plan

- [ ] `go test ./...`
- [ ] `make vet`
- [ ] `golangci-lint run` (local only; not run in CI)
- [ ] `make image` or `make build` (describe on PR), if this touches `internal/cxi`'s `_cgo.go` files
- [ ] Verified against `cxi-dra-integration-tests`

## Risk / rollback

<!-- Higher-risk areas: internal/driver/claimbinder.go or vniallocator.go (binding/VNI correctness),
     internal/cxi/resources.go (resource-field semantics), Dockerfile (libcxi pins, ldconfig runtime),
     .github/workflows/build.yaml / release-chart.yaml. Note rollback plan for higher-risk changes. -->
