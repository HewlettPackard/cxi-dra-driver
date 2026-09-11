---
description: "Walks through triaging a stuck CXI DRA claim, pod, or leaked service — the driver's most common failure modes."
agent: 'agent'
---

# Debug a driver failure

Use this when a pod is stuck `Pending`/`ContainerCreating`, a `ResourceClaim` never reaches `Ready`, or a CXI service appears leaked.

1. **Claim stuck / pod Pending** — check `kubectl describe resourceclaim <name>` for the `Unschedulable` reason:
   - `DeviceAtCapacity` → the device has no room; check `kubectl get resourceslices -o wide` for `avail_*` attributes.
   - `NamespaceDeviceOccupied` → another pod in the same namespace already holds that device; see `internal/driver/claimbinder.go`'s occupancy map.
2. **Pod stuck ContainerCreating** — check driver DaemonSet logs (`kubectl -n kube-system logs ds/cxi-dra-driver`) around `PrepareResourceClaims` and the NRI `RunPodSandbox` hook; a missing namespace VNI annotation or a live fit-check failure in `internal/nri/plugin.go` are the two most common causes.
3. **Suspected leaked CXI service** — the reaper (`internal/driver/reaper.go`) should clean these up on its next tick; if it isn't, check that the pod UID is actually gone (`kubectl get pod -A | grep <uid>`) and that the reconciler (`internal/driver/reconciler.go`) ran recently in the logs.
4. **Real-hardware build issues** — CGO link errors mean you're not on an ARM64 Slingshot node with libcxi headers; use `make build-mock` for anything that doesn't need real hardware.
5. Cross-reference `docs/pod-lifecycle-flow.txt` and `ARCHITECTURE.md` for the full step-by-step flow and decision points before assuming a bug — many "stuck" states are expected backpressure (e.g. `Unschedulable=true` while waiting for capacity).
