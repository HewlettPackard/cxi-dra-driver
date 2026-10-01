# Changelog

All notable changes to `cxi-dra-driver` will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) via its `v*` release tags.

## [Unreleased]

### Added
- `slingshot-cni` is now bundled into the driver image and can be installed onto nodes by the driver itself: new `--cni-install-dir` / `--cni-src` flags (default off), Helm `cni.install` / `cni.binDir`, plus an optional `NetworkAttachmentDefinition` render (`cni.nad.*`). Entirely opt-in — default renders are unchanged and `hostNetwork`/future SR-IOV remain first-class. `make build-cni` honours `GOARCH`; `make image` now depends on it. (SSHOTECO-105)
- `slingshot-cni` now produces a working HSN-backed pod interface end-to-end: on `ADD` it creates one L2 IPVLAN sub-interface per claimed CXI device on the paired host HSN, moves it into the pod netns as `<hsn_iface>` (falling back to `<hsn_iface>_inet`), and delegates addressing to the Whereabouts IPAM plugin per the standard CNI delegate contract; `DEL` releases the address and tears the interface down idempotently; `CHECK` verifies both. Multi-device (`count > 1`) claims are fully supported. The driver's `internal/netsysfs` sysfs injection is unchanged and runs alongside. New dependency: `github.com/vishvananda/netlink`. See `docs/cni/cni-plugin.md`.
- AI-readiness pass: `.github/copilot-instructions.md`, `CONTEXT.md`, `ARCHITECTURE.md`, `AGENTS.md`, `.github/instructions/*`, `.github/agents/*`, `.github/prompts/*`, `.github/skills/cxi-dra-glossary/`.
- Repository hygiene: `SECURITY.md`, `CONTRIBUTING.md`, `.github/CODEOWNERS`, `.github/pull_request_template.md`, `.github/ISSUE_TEMPLATE/bug_report.md`, `.gitleaks.toml`, `.github/dependabot.yml`, `.editorconfig`, `.golangci.yml`.
- CI: `go test ./...` and `golangci-lint run` steps in `.github/workflows/build.yaml` (previously `go vet` only).
- `internal/cxi/README.md` and `internal/driver/README.md` package maps.

### Removed
- `docs/superpowers/` (implementation plans and design specs for CDI env integration and namespace VNI allocation) — both features are fully implemented and merged (`internal/cdi/env.go`, `internal/driver/vniallocator.go`); the directory was stale, agentic-framework-specific TDD scaffolding no longer relevant once the work landed.

<!--
When adding an entry to [Unreleased], group it under Added / Changed / Fixed / Removed / Security.
When cutting a release, rename [Unreleased] to [X.Y.Z] - YYYY-MM-DD and add a new empty [Unreleased] above it.
-->
