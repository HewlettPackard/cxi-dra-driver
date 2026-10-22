---
description: "Walks through triaging a stuck CXI DRA claim, pod, or leaked service — the driver's most common failure modes."
agent: 'agent'
---

# Debug a driver failure

Use this when a pod is stuck `Pending`/`ContainerCreating`, a `ResourceClaim` never reaches `Ready`, or a CXI service appears leaked.

1. **Claim stuck / pod Pending** — check `kubectl describe resourceclaim <name>` for the `Unschedulable` reason:
   - `DeviceAtCapacity` → live hardware capacity cannot fit the request (usually a CXI service created outside Kubernetes); compare the device's `consumedCapacity` across claims with `cxi_service list` on the node. The scheduler may re-pick the same device until that consumption goes away.
   - `Ready=False` (`InsufficientCapacity`) → in-flight pending grants on the device are holding the claim; it is re-evaluated every 2s, see `internal/driver/claimbinder.go`.
   - Claim never allocated at all → the scheduler's consumable-capacity accounting rejected it; check `kubectl get resourceslices -o yaml` for the device's `capacity` and that `DRAConsumableCapacity` is enabled (Kubernetes 1.36+).
2. **Pod stuck ContainerCreating** — check driver DaemonSet logs (`kubectl -n kube-system logs ds/cxi-dra-driver`) around `PrepareResourceClaims` and the NRI `RunPodSandbox` hook; a missing namespace VNI annotation or a live fit-check failure in `internal/nri/plugin.go` are the two most common causes.
3. **Suspected leaked CXI service** — the reaper (`internal/driver/reaper.go`) should clean these up on its next tick; if it isn't, check that the pod UID is actually gone (`kubectl get pod -A | grep <uid>`) that the reaper's grace period (3 consecutive `--resource-refresh-interval` cycles) has elapsed — it never reaps blessed (`--system-svc-ids`), kernel-system, or NRI-tracked live services, nor any whose pod netns is still live.
4. **Real-hardware build issues** — CGO link errors mean the build host lacks libcxi headers; use `make build-mock` for anything that doesn't need real hardware, or `make image` (builds libcxi in-container).
5. **Pod has no HSN IP interface / CNI `ADD` fails** (only when `slingshot-cni` is attached) — check the per-pod hint file `/var/run/cxi/net-hints/<podUID>.json` exists on the node (written by `PrepareResourceClaims`), that `whereabouts` and `slingshot-cni` are on the node's `CNI_PATH`, and the kubelet/containerd logs for the `ADD` error; see `docs/cni/cni-plugin.md`.
6. Cross-reference `docs/pod-lifecycle-flow.txt` and `ARCHITECTURE.md` for the full step-by-step flow and decision points before assuming a bug — many "stuck" states are expected backpressure (e.g. `Unschedulable=true` while waiting for capacity).
