## Summary

<!-- What does this change do and why? Link the ticket/issue if one exists. -->

## Components touched

<!-- cmd/cxi-dra-driver, cmd/cxi-cdi-hook, internal/cxi, internal/driver, internal/cdi, internal/netsysfs, internal/nri, deploy/, .github/ -->

## Test plan

- [ ] `go test ./...`
- [ ] `make vet`
- [ ] `golangci-lint run`
- [ ] `make build` on real hardware (describe on PR), if this touches `internal/cxi`'s `_cgo.go` files
- [ ] Verified against `cxi-dra-integration-tests`

## Risk / rollback

<!-- Higher-risk areas: internal/driver/claimbinder.go or vniallocator.go (binding/VNI correctness),
     internal/cxi/resources.go (resource-field semantics), Dockerfile bundled-library list,
     .github/workflows/build.yaml. Note rollback plan for higher-risk changes. -->
