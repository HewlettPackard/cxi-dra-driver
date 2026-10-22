# Security Policy

## Reporting a vulnerability

This repository implements a Kubernetes DRA node driver with CGO access to host hardware (libcxi) and elevated container privileges (device injection, NRI hooks, host library installation). If you discover a security issue — e.g. a privilege-escalation path via the CDI/NRI hooks, an unsafe host library install, or a DRA binding bypass — report it privately to the repository maintainers rather than opening a public issue.

## Scope

- The driver runs as a privileged-adjacent DaemonSet with host filesystem access (`internal/cdi.InstallHook`, `InstallLdconfigRuntime`) and CGO bindings to libcxi — treat any change to host-install paths or bundled library selection as security-relevant.
- The `MutatingAdmissionPolicy` (`deploy/kubernetes/admission-policy.yaml`) runs in-process in the API server; a bug there can affect scheduling decisions cluster-wide.
- Static analysis: run `golangci-lint run` locally and review `gosec` findings on the host/CGO paths; it is not run in CI. No secret-scanning or SAST tooling is configured in the repo.
- Secrets (registry credentials, kubeconfigs) must never be committed.

## Response

Maintainers aim to acknowledge reports within 5 business days.
