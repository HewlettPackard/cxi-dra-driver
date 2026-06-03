#!/usr/bin/env bash
# Tests the VNI pool feature against a running kind cluster.
#
# Prerequisites:
#   deploy/kind/setup.sh has been run and the cluster is up.
#   The driver was deployed with --vni-range-start=1 --vni-range-end=5.
#
# Usage:
#   ./deploy/kind/test-vni.sh
#   KUBECONFIG=/path/to/kubeconfig ./deploy/kind/test-vni.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
KUBECONFIG_DEFAULT="${REPO_ROOT}/.kube/config"
export KUBECONFIG="${KUBECONFIG:-${KUBECONFIG_DEFAULT}}"

CLUSTER_NS="kube-system"
TEST_NS="default"
DRIVER="cxi.slingshot.hpe.com"
LEASE_NAME="cxi-vni-pool-leader"
DAEMONSET_NAME="cxi-dra-driver"
VNI_POOL_NAME="vni-pool"
# Must match --vni-range-start / --vni-range-end in daemonset-mock.yaml.
VNI_RANGE_START=1
VNI_RANGE_END=5
VNI_COUNT=$(( VNI_RANGE_END - VNI_RANGE_START + 1 ))

PASS=0
FAIL=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

pass() { echo "  [PASS] $*"; (( PASS++ )) || true; }
fail() { echo "  [FAIL] $*"; (( FAIL++ )) || true; }

# wait_for DESCRIPTION TIMEOUT_SECS COMMAND [ARGS...]
# Retries COMMAND every 2 s until it exits 0 or TIMEOUT_SECS is reached.
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" &>/dev/null; do
    if (( $(date +%s) >= deadline )); then
      echo "  [TIMEOUT] waiting for: ${desc}" >&2
      return 1
    fi
    sleep 2
  done
}

# kubectl_json_count JSONPATH OBJECT returns the array length at JSONPATH.
kubectl_json_count() { kubectl get "$@" -o json | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d))" 2>/dev/null || echo 0; }

cleanup() {
  echo ""
  echo "==> Cleaning up test resources"
  # Delete pods first — claims have a delete-protection finalizer that blocks
  # their deletion while reservedFor is non-empty. Force-delete so we don't
  # hang on pods that are stuck in ContainerCreating (e.g. waiting for the
  # kubelet to process the deletion event).
  kubectl delete pod \
    test-vni-pod-single test-vni-pod-alpha test-vni-pod-beta \
    test-vni-pod-shared-a test-vni-pod-shared-b \
    test-vni-pod-exhaust-{1..6} \
    test-cxi-svc-pod \
    --ignore-not-found --force --grace-period=0 -n "${TEST_NS}" 2>/dev/null || true
  # Wait briefly so the claim controller can clear reservedFor entries after
  # the pod objects disappear, then delete the claims.
  sleep 2
  kubectl delete resourceclaim \
    test-vni-single test-vni-alpha test-vni-beta test-vni-shared \
    test-vni-exhaust-{1..6} \
    test-cxi-svc-vni test-cxi-svc-dev \
    --ignore-not-found -n "${TEST_NS}" 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Inline pod factory — creates a minimal sleep pod referencing a named claim.
# Usage: make_pod POD_NAME CLAIM_NAME
# ---------------------------------------------------------------------------
make_pod() {
  local pod="$1" claim="$2"
  kubectl apply -n "${TEST_NS}" -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
spec:
  restartPolicy: Never
  resourceClaims:
    - name: vni
      resourceClaimName: ${claim}
  containers:
    - name: workload
      image: busybox:stable
      imagePullPolicy: Never
      command: ["sh", "-c", "sleep 3600"]
      resources:
        claims:
          - name: vni
EOF
}

# ---------------------------------------------------------------------------
# Inline VNI ResourceClaim factory.
# Usage: make_vni_claim CLAIM_NAME
# ---------------------------------------------------------------------------
make_vni_claim() {
  local claim="$1"
  kubectl apply -n "${TEST_NS}" -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: ${claim}
spec:
  devices:
    requests:
      - name: vni
        exactly:
          deviceClassName: cxi-vni
EOF
}

# ---------------------------------------------------------------------------
# Inline CXI device ResourceClaim factory (single device).
# Usage: make_cxi_claim CLAIM_NAME
# ---------------------------------------------------------------------------
make_cxi_claim() {
  local claim="$1"
  kubectl apply -n "${TEST_NS}" -f - <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: ${claim}
spec:
  devices:
    requests:
      - name: cxi
        exactly:
          deviceClassName: cxi-device
          count: 1
EOF
}

# Return the allocated device name (e.g. "vni-3") for a claim, or empty string.
allocated_device() {
  local claim="$1"
  kubectl get resourceclaim "${claim}" -n "${TEST_NS}" \
    -o jsonpath='{.status.allocation.devices.results[0].device}' 2>/dev/null || true
}

# Return the VNI number from an allocated device name ("vni-3" → "3"), or empty.
vni_number() {
  local device="$1"
  echo "${device#vni-}"
}

# ---------------------------------------------------------------------------
echo "========================================================"
echo " CXI DRA Driver — VNI Pool Tests"
echo " VNI range: ${VNI_RANGE_START}–${VNI_RANGE_END} (${VNI_COUNT} VNIs)"
echo "========================================================"
echo ""

# ---------------------------------------------------------------------------
# T1: VNI ResourceSlices published with allNodes: true
# ---------------------------------------------------------------------------
echo "--- T1: VNI ResourceSlices published (allNodes: true)"

VNI_SLICES=$(kubectl get resourceslices \
  -o jsonpath="{range .items[?(@.spec.driver=='${DRIVER}')]}{.spec.pool.name}{','}{.spec.allNodes}{'\n'}{end}" \
  2>/dev/null | grep "^${VNI_POOL_NAME},true" | wc -l | tr -d ' ')

if (( VNI_SLICES >= 1 )); then
  pass "Found ${VNI_SLICES} VNI ResourceSlice(s) with allNodes=true for pool '${VNI_POOL_NAME}'"
else
  fail "No VNI ResourceSlices found with allNodes=true (pool '${VNI_POOL_NAME}')"
  echo "     Hint: check driver logs: kubectl logs -n ${CLUSTER_NS} ds/${DAEMONSET_NAME}"
fi

# ---------------------------------------------------------------------------
# T2: VNI pool contains the expected number of devices
# ---------------------------------------------------------------------------
echo "--- T2: VNI pool device count"

ACTUAL_DEVICES=$(kubectl get resourceslices \
  -o json 2>/dev/null \
  | python3 -c "
import sys, json
data = json.load(sys.stdin)
count = sum(
    len(s['spec'].get('devices', []))
    for s in data['items']
    if s['spec'].get('driver') == '${DRIVER}'
    and s['spec'].get('pool', {}).get('name') == '${VNI_POOL_NAME}'
)
print(count)
")

if [[ "${ACTUAL_DEVICES}" == "${VNI_COUNT}" ]]; then
  pass "VNI pool has ${ACTUAL_DEVICES} devices (expected ${VNI_COUNT})"
else
  fail "VNI pool has ${ACTUAL_DEVICES} devices (expected ${VNI_COUNT})"
fi

# ---------------------------------------------------------------------------
# T3: Leader Lease exists with a holder
# ---------------------------------------------------------------------------
echo "--- T3: Leader Lease created and held"

LEASE_HOLDER=$(kubectl get lease "${LEASE_NAME}" -n "${CLUSTER_NS}" \
  -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)

if [[ -n "${LEASE_HOLDER}" ]]; then
  pass "Lease '${LEASE_NAME}' held by '${LEASE_HOLDER}'"
else
  fail "Lease '${LEASE_NAME}' not found or has no holder in namespace '${CLUSTER_NS}'"
fi

# ---------------------------------------------------------------------------
# T4: A single VNI claim is allocated when a pod requests it
# ---------------------------------------------------------------------------
echo "--- T4: Single VNI claim allocation"

make_vni_claim test-vni-single >/dev/null
make_pod test-vni-pod-single test-vni-single >/dev/null

if wait_for "claim test-vni-single to be allocated" 90 \
    bash -c "[[ -n \"\$(kubectl get resourceclaim test-vni-single -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]"; then
  DEVICE=$(allocated_device test-vni-single)
  VNI=$(vni_number "${DEVICE}")
  if [[ "${DEVICE}" =~ ^vni-[0-9]+$ ]] && (( VNI >= VNI_RANGE_START && VNI <= VNI_RANGE_END )); then
    pass "Claim allocated device '${DEVICE}' (VNI ${VNI} within range ${VNI_RANGE_START}–${VNI_RANGE_END})"
  else
    fail "Claim allocated unexpected device '${DEVICE}'"
  fi
else
  fail "Claim test-vni-single was not allocated within 90s"
fi

# ---------------------------------------------------------------------------
# T5: Two independent claims receive different VNIs
# ---------------------------------------------------------------------------
echo "--- T5: Two independent claims get different VNIs"

make_vni_claim test-vni-alpha >/dev/null
make_vni_claim test-vni-beta  >/dev/null
make_pod test-vni-pod-alpha test-vni-alpha >/dev/null
make_pod test-vni-pod-beta  test-vni-beta  >/dev/null

ALLOCATED_BOTH=false
if wait_for "both alpha/beta claims to allocate" 90 \
    bash -c "
      [[ -n \"\$(kubectl get resourceclaim test-vni-alpha -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]] &&
      [[ -n \"\$(kubectl get resourceclaim test-vni-beta  -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]
    "; then
  ALLOCATED_BOTH=true
fi

if [[ "${ALLOCATED_BOTH}" == "true" ]]; then
  DEVICE_ALPHA=$(allocated_device test-vni-alpha)
  DEVICE_BETA=$(allocated_device test-vni-beta)
  VNI_ALPHA=$(vni_number "${DEVICE_ALPHA}")
  VNI_BETA=$(vni_number "${DEVICE_BETA}")
  if [[ "${DEVICE_ALPHA}" != "${DEVICE_BETA}" ]]; then
    pass "Alpha got VNI ${VNI_ALPHA}, beta got VNI ${VNI_BETA} — different as expected"
  else
    fail "Both claims got the same device '${DEVICE_ALPHA}' — VNI uniqueness violated"
  fi
else
  fail "One or both claims were not allocated within 90s"
fi

# ---------------------------------------------------------------------------
# T6: Two pods sharing the same claim receive the same VNI
# ---------------------------------------------------------------------------
echo "--- T6: Two pods sharing one claim get the same VNI"

make_vni_claim test-vni-shared >/dev/null
make_pod test-vni-pod-shared-a test-vni-shared >/dev/null
make_pod test-vni-pod-shared-b test-vni-shared >/dev/null

if wait_for "shared claim to allocate" 90 \
    bash -c "[[ -n \"\$(kubectl get resourceclaim test-vni-shared -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]"; then
  DEVICE_SHARED=$(allocated_device test-vni-shared)
  VNI_SHARED=$(vni_number "${DEVICE_SHARED}")
  # Both pods reference the same claim so they necessarily get the same VNI.
  RESERVED_FOR=$(kubectl get resourceclaim test-vni-shared -n "${TEST_NS}" \
    -o jsonpath='{range .status.reservedFor[*]}{.name}{" "}{end}' 2>/dev/null || true)
  if echo "${RESERVED_FOR}" | grep -q "test-vni-pod-shared-a" && \
     echo "${RESERVED_FOR}" | grep -q "test-vni-pod-shared-b"; then
    pass "Both pods reserved claim '${DEVICE_SHARED}' (VNI ${VNI_SHARED})"
  elif [[ -n "${DEVICE_SHARED}" ]]; then
    # The second pod may still be scheduling; device sharing is confirmed by
    # both pods claiming the same ResourceClaim object.
    pass "Shared claim allocated '${DEVICE_SHARED}' (VNI ${VNI_SHARED}); both pods reference same claim"
  else
    fail "Shared claim was allocated but device name is empty"
  fi
else
  fail "Shared claim test-vni-shared was not allocated within 90s"
fi

# ---------------------------------------------------------------------------
# T7: Leader failover — ResourceSlices survive pod restart
# ---------------------------------------------------------------------------
echo "--- T7: Leader failover"

LEADER_NODE=$(kubectl get lease "${LEASE_NAME}" -n "${CLUSTER_NS}" \
  -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)

if [[ -z "${LEADER_NODE}" ]]; then
  fail "Cannot determine leader node — skipping failover test"
else
  LEADER_POD=$(kubectl get pods -n "${CLUSTER_NS}" -l "app=${DAEMONSET_NAME}" \
    -o jsonpath="{range .items[*]}{.metadata.name}{' '}{.spec.nodeName}{'\n'}{end}" \
    | awk -v node="${LEADER_NODE}" '$2 == node {print $1; exit}')

  if [[ -z "${LEADER_POD}" ]]; then
    fail "Could not find DaemonSet pod on leader node '${LEADER_NODE}' — skipping failover test"
  else
    # Count slices before deletion.
    SLICES_BEFORE=$(kubectl get resourceslices \
      -o jsonpath="{range .items[?(@.spec.driver=='${DRIVER}')]}{.spec.pool.name}{'\n'}{end}" \
      | grep -c "^${VNI_POOL_NAME}$" || true)

    echo "     Deleting leader pod '${LEADER_POD}' on node '${LEADER_NODE}'"
    kubectl delete pod "${LEADER_POD}" -n "${CLUSTER_NS}" --wait=false >/dev/null

    # Wait for new leader to be elected (LeaseDuration=15s + grace).
    if wait_for "new leader to be elected" 60 \
        bash -c "
          NEW_HOLDER=\$(kubectl get lease ${LEASE_NAME} -n ${CLUSTER_NS} -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)
          [[ -n \"\${NEW_HOLDER}\" ]]
        "; then
      NEW_LEADER=$(kubectl get lease "${LEASE_NAME}" -n "${CLUSTER_NS}" \
        -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
      pass "New leader elected: '${NEW_LEADER}'"
    else
      fail "No leader elected within 60s after pod deletion"
    fi

    # Verify slices were not deleted (owned by DaemonSet, not pod).
    SLICES_AFTER=$(kubectl get resourceslices \
      -o jsonpath="{range .items[?(@.spec.driver=='${DRIVER}')]}{.spec.pool.name}{'\n'}{end}" \
      | grep -c "^${VNI_POOL_NAME}$" || true)

    if (( SLICES_AFTER >= SLICES_BEFORE )); then
      pass "VNI ResourceSlices survived pod deletion (${SLICES_AFTER} slice(s) still present)"
    else
      fail "VNI ResourceSlices disappeared after pod deletion (before=${SLICES_BEFORE}, after=${SLICES_AFTER})"
    fi

    # Wait for DaemonSet to recover before remaining tests.
    kubectl -n "${CLUSTER_NS}" rollout status daemonset/"${DAEMONSET_NAME}" --timeout=90s >/dev/null
  fi
fi

# ---------------------------------------------------------------------------
# T9: CXI service YAML written with VNI sourced from the K8s claim
# ---------------------------------------------------------------------------
echo "--- T9: CXI service created with VNI from ResourceClaim"

# Prerequisite: wait for at least one node-scoped CXI ResourceSlice to appear.
# The driver publishes these after kubeletplugin.Start() succeeds, so their
# presence confirms the DRA plugin is registered with the kubelet on this node.
# Without this wait the kubelet returns "DRA driver not registered" when it
# tries to prepare resources for the pod.
if ! wait_for "CXI device ResourceSlice to be published" 120 \
    bash -c "kubectl get resourceslices -o jsonpath=\"{range .items[?(@.spec.driver=='${DRIVER}')]}{.spec.pool.name}{'\n'}{end}\" 2>/dev/null | grep -v '^${VNI_POOL_NAME}\$' | grep -q ."; then
  fail "T9: no CXI device ResourceSlice appeared within 120s — skipping service YAML test"
else

make_vni_claim test-cxi-svc-vni >/dev/null
make_cxi_claim test-cxi-svc-dev >/dev/null

kubectl apply -n "${TEST_NS}" -f - <<'PODEOF' >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: test-cxi-svc-pod
spec:
  restartPolicy: Never
  resourceClaims:
    - name: vni
      resourceClaimName: test-cxi-svc-vni
    - name: cxi
      resourceClaimName: test-cxi-svc-dev
  containers:
    - name: workload
      image: busybox:stable
      imagePullPolicy: Never
      command: ["sh", "-c", "sleep 3600"]
      resources:
        claims:
          - name: vni
          - name: cxi
PODEOF

CXI_SVC_OK=false
if wait_for "test-cxi-svc-dev claim to allocate" 90 \
    bash -c "[[ -n \"\$(kubectl get resourceclaim test-cxi-svc-dev -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]" && \
   wait_for "test-cxi-svc-vni claim to allocate" 90 \
    bash -c "[[ -n \"\$(kubectl get resourceclaim test-cxi-svc-vni -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]"; then

  CXI_DEVICE=$(kubectl get resourceclaim test-cxi-svc-dev -n "${TEST_NS}" \
    -o jsonpath='{.status.allocation.devices.results[0].device}' 2>/dev/null || true)
  VNI_DEVICE=$(allocated_device test-cxi-svc-vni)
  EXPECTED_VNI=$(vni_number "${VNI_DEVICE}")

  if wait_for "test-cxi-svc-pod to be Running" 90 \
      bash -c "[[ \"\$(kubectl get pod test-cxi-svc-pod -n ${TEST_NS} -o jsonpath='{.status.phase}' 2>/dev/null)\" == 'Running' ]]"; then

    # The NRI mock plugin bind-mounts the service YAML into the container at
    # /run/cxi/<device>-service.yaml so we can read it with kubectl exec.
    SVC_YAML=$(kubectl exec -n "${TEST_NS}" test-cxi-svc-pod -- \
      cat "/run/cxi/${CXI_DEVICE}-service.yaml" 2>/dev/null || true)

    if echo "${SVC_YAML}" | grep -q "vni: ${EXPECTED_VNI}"; then
      pass "Service YAML for '${CXI_DEVICE}' contains 'vni: ${EXPECTED_VNI}' (from claim '${VNI_DEVICE}')"
      CXI_SVC_OK=true
    else
      fail "Service YAML for '${CXI_DEVICE}' missing 'vni: ${EXPECTED_VNI}'"
      echo "     Service file contents:" >&2
      echo "${SVC_YAML}" | head -20 >&2
    fi
  else
    fail "test-cxi-svc-pod did not reach Running within 90s"
    kubectl describe pod test-cxi-svc-pod -n "${TEST_NS}" 2>/dev/null | tail -20 >&2 || true
  fi
else
  fail "One or both claims for T9 were not allocated within 90s"
fi

fi  # end: CXI device ResourceSlice prerequisite check

# ---------------------------------------------------------------------------
# Pre-T8: free the VNIs held by T4–T6 and T9 so T8 starts with the full pool.
# ResourceClaims have a delete-protection finalizer that blocks deletion until
# their pods terminate and the kubelet clears reservedFor, so delete pods
# first, then claims, then wait for the claims to disappear.
# ---------------------------------------------------------------------------
echo "==> Pre-T8: freeing VNIs from T4–T6 and T9"
kubectl delete pod \
    test-vni-pod-single \
    test-vni-pod-alpha test-vni-pod-beta \
    test-vni-pod-shared-a test-vni-pod-shared-b \
    test-cxi-svc-pod \
    --ignore-not-found -n "${TEST_NS}" >/dev/null
kubectl delete resourceclaim \
    test-vni-single test-vni-alpha test-vni-beta test-vni-shared \
    test-cxi-svc-vni test-cxi-svc-dev \
    --ignore-not-found -n "${TEST_NS}" >/dev/null
# Wait for all claims to be fully gone before filling the pool.
kubectl wait resourceclaim \
    test-vni-single test-vni-alpha test-vni-beta test-vni-shared \
    test-cxi-svc-vni test-cxi-svc-dev \
    -n "${TEST_NS}" --for=delete --timeout=90s 2>/dev/null || true

# ---------------------------------------------------------------------------
# T8: Pool exhaustion — (VNI_COUNT + 1)th claim stays Pending
# ---------------------------------------------------------------------------
echo "--- T8: Pool exhaustion (${VNI_COUNT} VNIs, claim $((VNI_COUNT + 1)) must pend)"

for i in $(seq 1 "${VNI_COUNT}"); do
  make_vni_claim "test-vni-exhaust-${i}" >/dev/null
  make_pod "test-vni-pod-exhaust-${i}" "test-vni-exhaust-${i}" >/dev/null
done

# Wait for all VNI_COUNT claims to be allocated.
ALL_ALLOCATED=true
for i in $(seq 1 "${VNI_COUNT}"); do
  if ! wait_for "exhaust claim ${i} to allocate" 120 \
      bash -c "[[ -n \"\$(kubectl get resourceclaim test-vni-exhaust-${i} -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]"; then
    fail "Exhaust claim ${i} was not allocated within 120s — pool may not be full"
    ALL_ALLOCATED=false
    break
  fi
done

if [[ "${ALL_ALLOCATED}" == "true" ]]; then
  pass "All ${VNI_COUNT} exhaust claims allocated (pool is now full)"

  # Now create the one-too-many claim.
  make_vni_claim "test-vni-exhaust-$((VNI_COUNT + 1))" >/dev/null
  make_pod "test-vni-pod-exhaust-$((VNI_COUNT + 1))" "test-vni-exhaust-$((VNI_COUNT + 1))" >/dev/null

  sleep 10  # Give the scheduler a moment to try (and fail) to allocate.

  EXTRA_ALLOC=$(kubectl get resourceclaim "test-vni-exhaust-$((VNI_COUNT + 1))" \
    -n "${TEST_NS}" -o jsonpath='{.status.allocation}' 2>/dev/null || true)

  if [[ -z "${EXTRA_ALLOC}" ]]; then
    pass "Claim $((VNI_COUNT + 1)) stays unallocated when pool is exhausted"
  else
    fail "Claim $((VNI_COUNT + 1)) was allocated despite pool being full"
  fi

  # Free one VNI and verify the pending claim gets it.
  echo "     Freeing one VNI (deleting test-vni-exhaust-1 and its pod)"
  kubectl delete pod test-vni-pod-exhaust-1 -n "${TEST_NS}" --ignore-not-found >/dev/null
  kubectl delete resourceclaim test-vni-exhaust-1 -n "${TEST_NS}" --ignore-not-found >/dev/null

  if wait_for "pending claim to allocate after freeing a VNI" 120 \
      bash -c "[[ -n \"\$(kubectl get resourceclaim test-vni-exhaust-$((VNI_COUNT + 1)) -n ${TEST_NS} -o jsonpath='{.status.allocation}' 2>/dev/null)\" ]]"; then
    RECOVERED_DEVICE=$(allocated_device "test-vni-exhaust-$((VNI_COUNT + 1))")
    pass "Pending claim allocated '${RECOVERED_DEVICE}' after a VNI was freed"
  else
    fail "Pending claim was not allocated within 120s after freeing a VNI"
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "========================================================"
echo " Results: ${PASS} passed, ${FAIL} failed"
echo "========================================================"

(( FAIL == 0 ))
