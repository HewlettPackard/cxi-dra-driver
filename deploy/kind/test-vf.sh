#!/usr/bin/env bash
# Tests SR-IOV VF mode against a kind cluster running the driver on the mock VF
# topology (--mock-vfs). Two mock parents on PF cxi0: cxi0-svc4 with the VNI
# range 100-115 and cxi0-svc5 with the VNI list {200,202,204}; every VF carries
# its Ethernet child (see internal/cxi/mock.go).
#
#   T1  the ResourceSlice publishes the parents (vfs capacity, VNI forms, no LEs/TLEs)
#   T2  N pods (N = VFs per parent) in a namespace on parent A land on N different VFs
#   T3  pod N+1 stays Pending on vfs although parent B has free VFs
#   T4  a namespace whose VNI only parent B covers lands on B on the first allocation
#   T5  parent budget is shared across its VFs: pods stop fitting before VFs run out
#   T6  a namespace whose VNI no parent covers stays Pending, never allocated
#   Every claim must reach its parent with zero Unschedulable binding conditions.
#
# Prerequisites: MOCK_VFS=2x4 deploy/kind/setup.sh (or any <parents>x<vfs> with
# at least 2 parents and 3 VFs per parent).
#
# On a Helm install (policies from the chart) set DRIVER, DEVICE_CLASS, DS (namespace/name
# of the DaemonSet), IMAGE/PULL for the test pods, and APPLY_POLICIES=0. The driver pod
# needs /tmp/cxi-services as a hostPath (mock service files are bind-mounted into pods).
#
# Usage:
#   ./deploy/kind/test-vf.sh
#   KUBECONFIG=/path/to/kubeconfig ./deploy/kind/test-vf.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
export KUBECONFIG="${KUBECONFIG:-${REPO_ROOT}/.kube/config}"

DRIVER="${DRIVER:-cxi.slingshot.hpe.com}"
DEVICE_CLASS="${DEVICE_CLASS:-cxi-device}"
DS="${DS:-kube-system/cxi-dra-driver}"
IMAGE="${IMAGE:-busybox:stable}"
PULL="${PULL:-Never}"
APPLY_POLICIES="${APPLY_POLICIES:-1}"
# All pods go to one node so a parent's VFs and budget actually run out.
NODE="${NODE:-$(kubectl get resourceslices -o json | jq -r --arg d "${DRIVER}" '[.items[] | select(.spec.driver == $d and (.spec.devices | length) > 0) | .spec.nodeName] | sort | first // empty')}"
PARENT_A="cxi0-svc4" PARENT_B="cxi0-svc5"
VNI_A=100 VNI_A2=101 VNI_B=202 VNI_NONE=150
NS_A="test-vf-a" NS_B="test-vf-b" NS_BUDGET="test-vf-budget" NS_NONE="test-vf-none"
PENDING_WAIT="${PENDING_WAIT:-30}"

PASS=0
FAIL=0
pass() { echo "  [PASS] $*"; (( PASS++ )) || true; }
fail() { echo "  [FAIL] $*"; (( FAIL++ )) || true; }

wait_for() { # TIMEOUT COMMAND...
  local deadline=$(( $(date +%s) + $1 )); shift
  until "$@" &>/dev/null; do
    (( $(date +%s) >= deadline )) && return 1
    sleep 2
  done
}

cleanup() {
  echo ""
  echo "==> Cleaning up test namespaces"
  kubectl delete namespace "${NS_A}" "${NS_B}" "${NS_BUDGET}" "${NS_NONE}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

slice_json() {
  kubectl get resourceslices -o json |
    jq --arg d "${DRIVER}" --arg n "${NODE}" '[.items[] | select(.spec.driver == $d and .spec.nodeName == $n) | .spec.devices[]]'
}
# dev_attr DEVICE NAME -> attribute value (int or string), empty if absent
dev_attr() { slice_json | jq -r --arg n "$1" --arg a "$2" '.[] | select(.name == $n) | .attributes[$a] | (.int // .string // empty)'; }
dev_cap() { slice_json | jq -r --arg n "$1" --arg c "$2" '.[] | select(.name == $n) | .capacity[$c].value // empty'; }

make_namespace() { # NS VNI
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $1
  annotations:
    ${DRIVER}/vni: "$2"
EOF
}

# make_template NS TXQS: a CXI claim template requesting TXQS transmit queues
# (other resources at their published defaults, LEs/TLEs 0).
make_template() {
  kubectl apply -n "$1" -f - >/dev/null <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: vf
spec:
  spec:
    devices:
      requests:
        - name: cxi
          exactly:
            deviceClassName: ${DEVICE_CLASS}
            capacity:
              requests:
                txqs: "$2"
EOF
}

make_pod() { # NS NAME
  kubectl apply -n "$1" -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $2
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  resourceClaims:
    - name: cxi
      resourceClaimTemplateName: vf
  containers:
    - name: workload
      image: ${IMAGE}
      imagePullPolicy: ${PULL}
      command: ["sh", "-c", "sleep 3600"]
      resources:
        claims:
          - name: cxi
EOF
}

running() { [[ $(kubectl get pod -n "$1" "$2" -o jsonpath='{.status.phase}' 2>/dev/null) == Running ]]; }
pod_env() { kubectl exec -n "$1" "$2" -- printenv "$3" 2>/dev/null | tr -d '\r'; }
pod_claim() { kubectl get pod -n "$1" "$2" -o jsonpath='{.status.resourceClaimStatuses[0].resourceClaimName}' 2>/dev/null; }
claim_device() { kubectl get resourceclaim -n "$1" "$2" -o jsonpath='{.status.allocation.devices.results[0].device}' 2>/dev/null; }
# Unschedulable=True binding conditions on every claim of a namespace.
unschedulable_count() {
  kubectl get resourceclaims -n "$1" -o json |
    jq '[.items[].status.devices[]?.conditions[]? | select(.type == "Unschedulable" and .status == "True")] | length'
}
driver_logs() { kubectl -n "${DS%%/*}" logs "ds/${DS##*/}" --tail=-1 2>/dev/null; }
sched_message() {
  kubectl get events -n "$1" --field-selector "involvedObject.name=$2,reason=FailedScheduling" \
    -o jsonpath='{.items[-1:].message}' 2>/dev/null
}
# still_pending NS POD: Pending, unbound, and its claim has no allocation after PENDING_WAIT seconds.
still_pending() {
  sleep "${PENDING_WAIT}"
  [[ $(kubectl get pod -n "$1" "$2" -o jsonpath='{.status.phase}/{.spec.nodeName}') == "Pending/" ]] &&
    [[ -z $(claim_device "$1" "$(pod_claim "$1" "$2")") ]]
}

echo "========================================================"
echo " SR-IOV VF mode on the mock topology (node ${NODE})"
echo "========================================================"

[[ ${APPLY_POLICIES} == 1 ]] && kubectl apply -f "${REPO_ROOT}/deploy/kubernetes/admission-policy.yaml" >/dev/null
# The VNI selector takes effect asynchronously after the policy is created.
make_namespace "${NS_A}" "${VNI_A}"
selector_active() {
  kubectl create --dry-run=server -n "${NS_A}" -o jsonpath='{.spec.devices.requests[0].exactly.selectors[0].cel.expression}' -f - <<EOF | grep -q vni_set
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata: {name: probe}
spec:
  devices:
    requests:
      - name: cxi
        exactly: {deviceClassName: ${DEVICE_CLASS}}
EOF
}
wait_for 60 selector_active || { echo "VNI selector policy is not active; is admission-policy.yaml applied?"; exit 1; }

# ---------------------------------------------------------------------------
echo ""
echo "--- T1: ResourceSlice publishes the mock parents"
devices=$(slice_json | jq -r '.[].name' | sort | xargs)
VFS=$(dev_cap "${PARENT_A}" vfs)
if [[ ${devices} == "${PARENT_A} ${PARENT_B}" ]]; then pass "published devices: ${devices}"; else fail "published devices: '${devices}', want '${PARENT_A} ${PARENT_B}'"; fi
for p in "${PARENT_A}" "${PARENT_B}"; do
  [[ $(dev_attr "$p" function_type) == parent ]] && pass "$p function_type=parent" || fail "$p function_type=$(dev_attr "$p" function_type)"
  [[ $(dev_cap "$p" les) == 0 && $(dev_cap "$p" tles) == 0 ]] && pass "$p les=tles=0" || fail "$p les=$(dev_cap "$p" les) tles=$(dev_cap "$p" tles)"
done
[[ $(dev_attr "${PARENT_A}" vni_min)-$(dev_attr "${PARENT_A}" vni_max) == 100-115 && -z $(dev_attr "${PARENT_A}" vni_set) ]] &&
  pass "${PARENT_A} range form vni_min/vni_max 100-115" || fail "${PARENT_A} VNIs: min=$(dev_attr "${PARENT_A}" vni_min) max=$(dev_attr "${PARENT_A}" vni_max) set=$(dev_attr "${PARENT_A}" vni_set)"
[[ $(dev_attr "${PARENT_B}" vni_set) == ",200,202,204," && -z $(dev_attr "${PARENT_B}" vni_min) ]] &&
  pass "${PARENT_B} list form vni_set ,200,202,204," || fail "${PARENT_B} VNIs: set=$(dev_attr "${PARENT_B}" vni_set) min=$(dev_attr "${PARENT_B}" vni_min)"
if [[ ! ${VFS} =~ ^[0-9]+$ ]] || (( VFS < 3 )); then
  echo "need at least 3 VFs per parent (vfs capacity '${VFS}'); run setup.sh with MOCK_VFS=2x4"; exit 1
fi
pass "vfs capacity ${VFS} per parent"

# ---------------------------------------------------------------------------
echo ""
echo "--- T2: ${VFS} pods in ${NS_A} (VNI ${VNI_A}) land on ${VFS} different VFs of ${PARENT_A}"
make_template "${NS_A}" 2
for i in $(seq 1 "${VFS}"); do make_pod "${NS_A}" "a${i}"; done
vfs_used=()
for i in $(seq 1 "${VFS}"); do
  if wait_for 120 running "${NS_A}" "a${i}"; then
    vfs_used+=("$(pod_env "${NS_A}" "a${i}" SLINGSHOT_DEVICES)")
    dev=$(claim_device "${NS_A}" "$(pod_claim "${NS_A}" "a${i}")")
    [[ ${dev} == "${PARENT_A}" ]] || fail "a${i} allocated to '${dev}', want ${PARENT_A}"
  else
    fail "a${i} not Running"
  fi
done
distinct=$(printf '%s\n' "${vfs_used[@]}" | sort -u | grep -c . || true)
if (( distinct == VFS )); then pass "${VFS} pods on distinct VFs: ${vfs_used[*]}"; else fail "VFs: ${vfs_used[*]}"; fi
[[ $(unschedulable_count "${NS_A}") == 0 ]] && pass "no Unschedulable binding conditions in ${NS_A}" || fail "$(unschedulable_count "${NS_A}") Unschedulable conditions in ${NS_A}"

# ---------------------------------------------------------------------------
echo ""
echo "--- T3: pod $((VFS + 1)) in ${NS_A} stays Pending (${PARENT_A} out of VFs; ${PARENT_B} does not cover VNI ${VNI_A})"
make_pod "${NS_A}" extra
if still_pending "${NS_A}" extra; then
  pass "extra pod Pending and unallocated after ${PENDING_WAIT}s: $(sched_message "${NS_A}" extra | cut -c1-160)"
else
  fail "extra pod: phase $(kubectl get pod -n "${NS_A}" extra -o jsonpath='{.status.phase}') claim device '$(claim_device "${NS_A}" "$(pod_claim "${NS_A}" extra)")'"
fi
kubectl delete pod -n "${NS_A}" a1 --wait=true >/dev/null
if wait_for 120 running "${NS_A}" extra; then
  pass "extra pod Running on $(pod_env "${NS_A}" extra SLINGSHOT_DEVICES) once a VF was freed"
else
  fail "extra pod still $(kubectl get pod -n "${NS_A}" extra -o jsonpath='{.status.phase}') after a VF was freed"
fi

# ---------------------------------------------------------------------------
echo ""
echo "--- T4: ${NS_B} (VNI ${VNI_B}) lands on ${PARENT_B} on the first allocation"
make_namespace "${NS_B}" "${VNI_B}"
make_template "${NS_B}" 2
make_pod "${NS_B}" b1
if wait_for 120 running "${NS_B}" b1; then
  dev=$(claim_device "${NS_B}" "$(pod_claim "${NS_B}" b1)")
  vf=$(pod_env "${NS_B}" b1 SLINGSHOT_DEVICES)
  [[ ${dev} == "${PARENT_B}" ]] && pass "b1 allocated to ${PARENT_B} (VF ${vf}, VNI $(pod_env "${NS_B}" b1 SLINGSHOT_VNIS))" || fail "b1 allocated to ${dev}"
else
  fail "b1 not Running"
fi
[[ $(unschedulable_count "${NS_B}") == 0 ]] && pass "no Unschedulable binding conditions in ${NS_B}" || fail "$(unschedulable_count "${NS_B}") Unschedulable conditions in ${NS_B}"
kubectl delete namespace "${NS_A}" "${NS_B}" --wait=true --timeout=120s >/dev/null

# ---------------------------------------------------------------------------
CEIL=$(dev_cap "${PARENT_A}" txqs)
HALF=$(( CEIL / 2 ))
echo ""
echo "--- T5: ${PARENT_A} budget is shared by its VFs (txqs ceiling ${CEIL}, pods of ${HALF})"
make_namespace "${NS_BUDGET}" "${VNI_A2}"
make_template "${NS_BUDGET}" "${HALF}"
for i in 1 2 3; do make_pod "${NS_BUDGET}" "c${i}"; done
ok=0
for i in 1 2; do wait_for 120 running "${NS_BUDGET}" "c${i}" && (( ok++ )) || true; done
(( ok == 2 )) && pass "two pods of ${HALF} txqs Running on $(pod_env "${NS_BUDGET}" c1 SLINGSHOT_DEVICES) and $(pod_env "${NS_BUDGET}" c2 SLINGSHOT_DEVICES)" ||
  fail "${ok}/2 pods of ${HALF} txqs Running"
if still_pending "${NS_BUDGET}" c3; then
  pass "third pod Pending with $(( VFS - 2 )) VFs free: the parent's txqs are used up"
else
  fail "third pod: phase $(kubectl get pod -n "${NS_BUDGET}" c3 -o jsonpath='{.status.phase}') on $(claim_device "${NS_BUDGET}" "$(pod_claim "${NS_BUDGET}" c3)")"
fi
[[ $(unschedulable_count "${NS_BUDGET}") == 0 ]] && pass "no Unschedulable binding conditions in ${NS_BUDGET}" || fail "$(unschedulable_count "${NS_BUDGET}") Unschedulable conditions in ${NS_BUDGET}"

# ---------------------------------------------------------------------------
echo ""
echo "--- T6: ${NS_NONE} (VNI ${VNI_NONE}, in no parent's set) stays Pending"
make_namespace "${NS_NONE}" "${VNI_NONE}"
make_template "${NS_NONE}" 2
make_pod "${NS_NONE}" x1
if still_pending "${NS_NONE}" x1; then
  pass "x1 Pending and never allocated: $(sched_message "${NS_NONE}" x1 | cut -c1-160)"
else
  fail "x1: phase $(kubectl get pod -n "${NS_NONE}" x1 -o jsonpath='{.status.phase}') claim device '$(claim_device "${NS_NONE}" "$(pod_claim "${NS_NONE}" x1)")'"
fi
if driver_logs | grep -q "Unschedulable: claim=${NS_NONE}/"; then
  fail "driver bounced the claim: $(driver_logs | grep -m1 "Unschedulable: claim=${NS_NONE}/" | cut -c1-160)"
else
  pass "no ClaimBinder Unschedulable bounce for ${NS_NONE}"
fi

echo ""
echo "========================================================"
echo " Results: ${PASS} passed, ${FAIL} failed"
echo "========================================================"
(( FAIL == 0 ))
