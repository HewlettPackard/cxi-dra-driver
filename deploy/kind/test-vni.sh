#!/usr/bin/env bash
# Tests the namespace-scoped VNI allocator against a running kind cluster.
#
# The allocator is leader-elected: exactly one driver pod assigns a unique
# VNI annotation (cxi.slingshot.hpe.com/vni) to every namespace not on the
# skip-list (kube-system, kube-public, kube-node-lease). The driver's
# PrepareResourceClaims reads this annotation to populate the CXI service
# YAML for every pod scheduled into that namespace.
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
DRIVER="cxi.slingshot.hpe.com"
LEASE_NAME="cxi.slingshot.hpe.com-vni-allocator-leader"
DAEMONSET_NAME="cxi-dra-driver"
VNI_ANNOTATION="cxi.slingshot.hpe.com/vni"
# Must match --vni-range-start / --vni-range-end in daemonset-mock.yaml.
VNI_RANGE_START=1
VNI_RANGE_END=5
VNI_COUNT=$(( VNI_RANGE_END - VNI_RANGE_START + 1 ))
# Pre-existing VNI for T5; deliberately outside the configured range so the
# test proves the allocator respects any valid VNI (1–65535), not just ones
# in its own range.
PRESET_VNI=12345
# Out-of-range VNI used to verify ValidVNI rejection.
INVALID_VNI=99999

# Test namespaces — all created by this script, all cleaned up on exit.
TEST_NS_SINGLE="test-vni-ns-single"
TEST_NS_ALPHA="test-vni-ns-alpha"
TEST_NS_BETA="test-vni-ns-beta"
TEST_NS_PRESET="test-vni-ns-preset"
TEST_NS_POD="test-vni-ns-pod"
TEST_NS_EXHAUST_PREFIX="test-vni-ns-exhaust"

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

# Read the VNI annotation from a namespace, or empty string if absent.
ns_vni() {
  local ns="$1"
  kubectl get namespace "${ns}" \
    -o jsonpath="{.metadata.annotations.cxi\.slingshot\.hpe\.com/vni}" \
    2>/dev/null || true
}

# True if VNI is within the configured allocator range.
in_range() {
  local vni="$1"
  [[ "${vni}" =~ ^[0-9]+$ ]] && (( vni >= VNI_RANGE_START && vni <= VNI_RANGE_END ))
}

cleanup() {
  echo ""
  echo "==> Cleaning up test namespaces"
  local ns_to_delete=(
    "${TEST_NS_SINGLE}"
    "${TEST_NS_ALPHA}"
    "${TEST_NS_BETA}"
    "${TEST_NS_PRESET}"
    "${TEST_NS_POD}"
  )
  # Add any exhaust namespaces that may have been created.
  for i in $(seq 1 $((VNI_COUNT + 2))); do
    ns_to_delete+=("${TEST_NS_EXHAUST_PREFIX}-${i}")
  done
  # Background each delete so a stuck namespace doesn't block the others.
  for ns in "${ns_to_delete[@]}"; do
    kubectl delete namespace "${ns}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

# Create a namespace (idempotent — apply not create, so re-runs work).
make_namespace() {
  local ns="$1"
  kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: ${ns}
EOF
}

# Create a namespace with a pre-existing VNI annotation.
make_namespace_with_vni() {
  local ns="$1" vni="$2"
  kubectl apply -f - <<EOF >/dev/null
apiVersion: v1
kind: Namespace
metadata:
  name: ${ns}
  annotations:
    ${VNI_ANNOTATION}: "${vni}"
EOF
}

# Deploy a CXI ResourceClaimTemplate + Pod into NS using the cxi-small shape.
make_cxi_pod() {
  local ns="$1" pod="$2" claim_template="cxi-small"
  kubectl apply -n "${ns}" -f - <<EOF >/dev/null
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: ${claim_template}
  namespace: ${ns}
spec:
  spec:
    devices:
      requests:
        - name: cxi
          exactly:
            deviceClassName: cxi-device
      config:
        - requests: ["cxi"]
          opaque:
            driver: ${DRIVER}
            parameters:
              txqs: 4
              tgqs: 2
              eqs: 8
              cts: 4
              ptes: 4
              acs: 2
              le_pools: 1
              tle_pools: 1
---
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
spec:
  restartPolicy: Never
  resourceClaims:
    - name: cxi
      resourceClaimTemplateName: ${claim_template}
  containers:
    - name: workload
      image: busybox:stable
      imagePullPolicy: Never
      command: ["sh", "-c", "sleep 3600"]
      resources:
        claims:
          - name: cxi
EOF
}

# ---------------------------------------------------------------------------
echo "========================================================"
echo " CXI DRA Driver — Namespace VNI Allocator Tests"
echo " VNI range: ${VNI_RANGE_START}–${VNI_RANGE_END} (${VNI_COUNT} VNIs)"
echo " Annotation key: ${VNI_ANNOTATION}"
echo "========================================================"
echo ""

# ---------------------------------------------------------------------------
# T1: Allocator lease exists and is held
# ---------------------------------------------------------------------------
echo "--- T1: Allocator lease created and held"

LEASE_HOLDER=$(kubectl get lease "${LEASE_NAME}" -n "${CLUSTER_NS}" \
  -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)

if [[ -n "${LEASE_HOLDER}" ]]; then
  pass "Lease '${LEASE_NAME}' held by '${LEASE_HOLDER}'"
else
  fail "Lease '${LEASE_NAME}' not found or has no holder in namespace '${CLUSTER_NS}'"
  echo "     Hint: check driver logs: kubectl logs -n ${CLUSTER_NS} ds/${DAEMONSET_NAME}"
fi

# ---------------------------------------------------------------------------
# T2: Skip-list namespaces have no VNI annotation
# ---------------------------------------------------------------------------
echo "--- T2: Skip-list namespaces are not annotated"

SKIP_OK=true
for skip_ns in kube-system kube-public kube-node-lease; do
  vni=$(ns_vni "${skip_ns}")
  if [[ -n "${vni}" ]]; then
    fail "Skip-list namespace '${skip_ns}' was annotated with VNI ${vni} (should have no annotation)"
    SKIP_OK=false
  fi
done
if [[ "${SKIP_OK}" == "true" ]]; then
  pass "kube-system, kube-public, kube-node-lease have no VNI annotation"
fi

# ---------------------------------------------------------------------------
# T3: A newly created namespace gets a unique VNI within the configured range
# ---------------------------------------------------------------------------
echo "--- T3: New namespace gets a VNI within range"

make_namespace "${TEST_NS_SINGLE}"

if wait_for "namespace '${TEST_NS_SINGLE}' to receive a VNI annotation" 60 \
    bash -c "[[ -n \"\$(kubectl get namespace ${TEST_NS_SINGLE} -o jsonpath='{.metadata.annotations.cxi\.slingshot\.hpe\.com/vni}' 2>/dev/null)\" ]]"; then
  VNI_SINGLE=$(ns_vni "${TEST_NS_SINGLE}")
  if in_range "${VNI_SINGLE}"; then
    pass "Namespace '${TEST_NS_SINGLE}' assigned VNI ${VNI_SINGLE} (within range ${VNI_RANGE_START}–${VNI_RANGE_END})"
  else
    fail "Namespace '${TEST_NS_SINGLE}' assigned VNI ${VNI_SINGLE} (out of range ${VNI_RANGE_START}–${VNI_RANGE_END})"
  fi
else
  fail "Namespace '${TEST_NS_SINGLE}' was not annotated within 60s"
fi

# ---------------------------------------------------------------------------
# T4: Two new namespaces get different VNIs
# ---------------------------------------------------------------------------
echo "--- T4: Two new namespaces get distinct VNIs"

make_namespace "${TEST_NS_ALPHA}"
make_namespace "${TEST_NS_BETA}"

BOTH_ANNOTATED=false
if wait_for "both alpha/beta namespaces to be annotated" 60 \
    bash -c "
      [[ -n \"\$(kubectl get namespace ${TEST_NS_ALPHA} -o jsonpath='{.metadata.annotations.cxi\.slingshot\.hpe\.com/vni}' 2>/dev/null)\" ]] &&
      [[ -n \"\$(kubectl get namespace ${TEST_NS_BETA}  -o jsonpath='{.metadata.annotations.cxi\.slingshot\.hpe\.com/vni}' 2>/dev/null)\" ]]
    "; then
  BOTH_ANNOTATED=true
fi

if [[ "${BOTH_ANNOTATED}" == "true" ]]; then
  VNI_ALPHA=$(ns_vni "${TEST_NS_ALPHA}")
  VNI_BETA=$(ns_vni "${TEST_NS_BETA}")
  if [[ "${VNI_ALPHA}" != "${VNI_BETA}" ]] && in_range "${VNI_ALPHA}" && in_range "${VNI_BETA}"; then
    pass "Alpha=${VNI_ALPHA}, beta=${VNI_BETA} — distinct and both in range"
  elif [[ "${VNI_ALPHA}" == "${VNI_BETA}" ]]; then
    fail "Both namespaces got the same VNI ${VNI_ALPHA} — uniqueness violated"
  else
    fail "Alpha=${VNI_ALPHA}, beta=${VNI_BETA} — one or both out of range"
  fi
else
  fail "One or both namespaces were not annotated within 60s"
fi

# ---------------------------------------------------------------------------
# T5: A pre-existing valid VNI annotation is respected (not overwritten)
# ---------------------------------------------------------------------------
echo "--- T5: Pre-existing valid VNI annotation is respected"

make_namespace_with_vni "${TEST_NS_PRESET}" "${PRESET_VNI}"

# Give the allocator time to observe the Add event and decide to do nothing.
sleep 5

VNI_PRESET=$(ns_vni "${TEST_NS_PRESET}")
if [[ "${VNI_PRESET}" == "${PRESET_VNI}" ]]; then
  pass "Pre-existing VNI ${PRESET_VNI} preserved on '${TEST_NS_PRESET}'"
else
  fail "Pre-existing VNI was overwritten: expected ${PRESET_VNI}, got '${VNI_PRESET}'"
fi

# ---------------------------------------------------------------------------
# T6: Pod with a CXI claim in an annotated namespace reaches Running
#     (validates that PrepareResourceClaims reads the namespace annotation)
# ---------------------------------------------------------------------------
echo "--- T6: Pod prepare reads namespace VNI annotation"

# Prerequisite: at least one node-scoped CXI ResourceSlice must exist so the
# scheduler can allocate a device. The driver publishes these after
# kubeletplugin.Start() succeeds.
if ! wait_for "CXI device ResourceSlice to be published" 120 \
    bash -c "kubectl get resourceslices -o jsonpath=\"{range .items[?(@.spec.driver=='${DRIVER}')]}{.metadata.name}{'\n'}{end}\" 2>/dev/null | grep -q ."; then
  fail "T6: no CXI device ResourceSlice appeared within 120s — skipping pod test"
else
  make_namespace "${TEST_NS_POD}"

  # Wait for the namespace to be annotated before deploying the pod, so we can
  # confirm exactly which VNI prepare should read.
  if wait_for "namespace '${TEST_NS_POD}' to receive a VNI annotation" 60 \
      bash -c "[[ -n \"\$(kubectl get namespace ${TEST_NS_POD} -o jsonpath='{.metadata.annotations.cxi\.slingshot\.hpe\.com/vni}' 2>/dev/null)\" ]]"; then
    VNI_POD=$(ns_vni "${TEST_NS_POD}")

    make_cxi_pod "${TEST_NS_POD}" "test-vni-cxi-pod"

    if wait_for "pod 'test-vni-cxi-pod' to be Running" 120 \
        bash -c "[[ \"\$(kubectl get pod test-vni-cxi-pod -n ${TEST_NS_POD} -o jsonpath='{.status.phase}' 2>/dev/null)\" == 'Running' ]]"; then
      pass "Pod 'test-vni-cxi-pod' is Running — prepare succeeded reading VNI ${VNI_POD} from namespace"
    else
      fail "Pod 'test-vni-cxi-pod' did not reach Running within 120s"
      kubectl describe pod test-vni-cxi-pod -n "${TEST_NS_POD}" 2>/dev/null | tail -30 >&2 || true
    fi
  else
    fail "Namespace '${TEST_NS_POD}' was not annotated within 60s — cannot test prepare"
  fi
fi

# ---------------------------------------------------------------------------
# T7: VNI exhaustion — namespaces beyond pool capacity stay un-annotated
# ---------------------------------------------------------------------------
echo "--- T7: Exhausted pool leaves new namespaces un-annotated"

# Count VNIs currently consumed from the allocator range. Skip-list namespaces
# and the preset namespace (out of range) do not count against the budget.
USED_IN_RANGE=$(kubectl get namespaces -o json | python3 -c "
import json, sys
data = json.load(sys.stdin)
start, end = ${VNI_RANGE_START}, ${VNI_RANGE_END}
count = 0
for n in data['items']:
    v = n.get('metadata', {}).get('annotations', {}).get('${VNI_ANNOTATION}')
    if v is None:
        continue
    try:
        vi = int(v)
    except ValueError:
        continue
    if start <= vi <= end:
        count += 1
print(count)
")

REMAINING=$(( VNI_COUNT - USED_IN_RANGE ))
echo "     ${USED_IN_RANGE} VNIs in [${VNI_RANGE_START},${VNI_RANGE_END}] already assigned; ${REMAINING} free"

if (( REMAINING < 0 )); then
  fail "Allocator over-assigned: ${USED_IN_RANGE} > ${VNI_COUNT} VNIs"
else
  # Fill the remaining VNIs, then create 2 extra namespaces that must stay
  # un-annotated.
  CREATED=0
  for i in $(seq 1 $((REMAINING + 2))); do
    make_namespace "${TEST_NS_EXHAUST_PREFIX}-${i}"
    CREATED=$(( CREATED + 1 ))
  done

  # Give the allocator time to process all of them (it serializes reconciles
  # through a single worker so each requeue is a few seconds).
  sleep 15

  ANNOTATED_COUNT=0
  UNANNOTATED_COUNT=0
  for i in $(seq 1 "${CREATED}"); do
    v=$(ns_vni "${TEST_NS_EXHAUST_PREFIX}-${i}")
    if [[ -n "${v}" ]]; then
      ANNOTATED_COUNT=$(( ANNOTATED_COUNT + 1 ))
    else
      UNANNOTATED_COUNT=$(( UNANNOTATED_COUNT + 1 ))
    fi
  done

  # All REMAINING free VNIs should be consumed; the 2 extras should fail.
  if (( ANNOTATED_COUNT == REMAINING )) && (( UNANNOTATED_COUNT == 2 )); then
    pass "Pool exhaustion confirmed: ${ANNOTATED_COUNT} new namespaces annotated, ${UNANNOTATED_COUNT} left un-annotated"
  else
    fail "Pool exhaustion mismatch: expected ${REMAINING} annotated + 2 un-annotated, got ${ANNOTATED_COUNT} + ${UNANNOTATED_COUNT}"
  fi
fi

# ---------------------------------------------------------------------------
# T8: Leader failover — deleting the leader pod re-elects a new leader and
#     the allocator continues annotating new namespaces.
# ---------------------------------------------------------------------------
echo "--- T8: Leader failover"

OLD_HOLDER=$(kubectl get lease "${LEASE_NAME}" -n "${CLUSTER_NS}" \
  -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)

if [[ -z "${OLD_HOLDER}" ]]; then
  fail "Cannot determine current lease holder — skipping failover test"
else
  # Identity is <nodeName>/<podUID>; split out the node and find the pod on it.
  LEADER_NODE="${OLD_HOLDER%%/*}"
  LEADER_POD=$(kubectl get pods -n "${CLUSTER_NS}" -l "app=${DAEMONSET_NAME}" \
    -o jsonpath="{range .items[*]}{.metadata.name}{' '}{.spec.nodeName}{'\n'}{end}" \
    | awk -v node="${LEADER_NODE}" '$2 == node {print $1; exit}')

  if [[ -z "${LEADER_POD}" ]]; then
    fail "Could not find DaemonSet pod on leader node '${LEADER_NODE}' — skipping failover test"
  else
    echo "     Old lease holder: '${OLD_HOLDER}' (pod '${LEADER_POD}')"
    echo "     Deleting leader pod"
    kubectl delete pod "${LEADER_POD}" -n "${CLUSTER_NS}" --wait=false --grace-period=0 --force >/dev/null 2>&1

    # Wait for the DaemonSet to spin a replacement and a NEW holderIdentity
    # to appear on the lease. The new pod has a fresh UID, so identity must
    # change even on a single-worker cluster.
    if wait_for "a new lease holder to be elected" 90 \
        bash -c "
          NEW=\$(kubectl get lease ${LEASE_NAME} -n ${CLUSTER_NS} -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)
          [[ -n \"\${NEW}\" && \"\${NEW}\" != \"${OLD_HOLDER}\" ]]
        "; then
      NEW_HOLDER=$(kubectl get lease "${LEASE_NAME}" -n "${CLUSTER_NS}" \
        -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)
      pass "New leader elected: '${NEW_HOLDER}' (was '${OLD_HOLDER}')"

      # Verify the new leader is functional by freeing a VNI and creating a
      # fresh namespace; it should be annotated by the new leader.
      # --wait=true blocks until the namespace is fully gone so its VNI is
      # truly released (assignedVNIs lists Terminating namespaces too).
      kubectl delete namespace "${TEST_NS_EXHAUST_PREFIX}-1" \
        --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
      make_namespace "test-vni-ns-postfailover"

      if wait_for "post-failover namespace to be annotated" 60 \
          bash -c "[[ -n \"\$(kubectl get namespace test-vni-ns-postfailover -o jsonpath='{.metadata.annotations.cxi\.slingshot\.hpe\.com/vni}' 2>/dev/null)\" ]]"; then
        VNI_POST=$(ns_vni "test-vni-ns-postfailover")
        if in_range "${VNI_POST}"; then
          pass "Post-failover namespace assigned VNI ${VNI_POST} — new leader is functional"
        else
          fail "Post-failover namespace assigned out-of-range VNI ${VNI_POST}"
        fi
      else
        fail "New leader did not annotate post-failover namespace within 60s"
      fi
      # Ensure cleanup picks this up too.
      kubectl delete namespace "test-vni-ns-postfailover" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    else
      fail "No new lease holder appeared within 90s after pod deletion"
    fi
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
