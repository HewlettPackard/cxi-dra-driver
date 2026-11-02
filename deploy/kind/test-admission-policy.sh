#!/usr/bin/env bash
# Tests the Slingshot network auto-attachment admission policy against a
# running kind cluster.
#
# The MutatingAdmissionPolicy intercepts Pod CREATE requests and checks
# spec.resourceClaims for CXI ResourceClaimTemplate names rendered by the
# Helm chart. CXI templates follow the <fullname>-<template> naming
# convention, for example cxi-slingshot-hpe-com-small.
#
# For each supported CXI ResourceClaimTemplate naming pattern, this test
# submits a Pod with --dry-run=server and verifies that the API server
# injects the Multus network-selection annotation:
#
#   k8s.v1.cni.cncf.io/networks: <namespace>/slingshot-hsn
#
# This validates the admission-time auto-attachment signal only. It does
# not require real CXI hardware and does not validate Multus or
# slingshot-cni runtime execution.
#
# It then submits ResourceClaims with --dry-run=server to check the two claim
# policies: the legacy opaque-config conversion (le_pools: 0 -> les "0", the only
# LE/TLE value valid on a VF-mode parent) and the VNI selector appended to CXI
# requests in a namespace carrying the cxi.slingshot.hpe.com/vni annotation
# (and only there; a selector the user supplied is kept).
#
# Prerequisites:
#   A kind cluster is running.
#   The Slingshot network MutatingAdmissionPolicy and Binding are installed,
#   as are the capacity-conversion and VNI-selector policies and bindings
#   (the Helm chart installs all of them). DRIVER and DEVICE_CLASS default to
#   the chart's names for the default driver name.
#   The kube-public namespace exists and is not annotated with a VNI.
#
# Usage:
#   ./deploy/kind/test-admission-policy.sh
#   KUBECONFIG=/path/to/kubeconfig ./deploy/kind/test-admission-policy.sh

set -euo pipefail

# All test Pods are created in the default namespace, so the expected
# Multus network reference is default/slingshot-hsn.
NAMESPACE="default"
EXPECTED_NETWORK="${NAMESPACE}/slingshot-hsn"

# Existing Helm-rendered CXI ResourceClaimTemplate naming patterns.
# The admission policy matches these via the <fullname>- prefix.
templates=(
  "cxi-slingshot-hpe-com-small"
  "cxi-slingshot-hpe-com-medium"
  "cxi-slingshot-hpe-com-large"
  "cxi-slingshot-hpe-com-whole"
  "cxi-slingshot-hpe-com-2-small"
  "cxi-slingshot-hpe-com-2-whole"
)

# Use a temporary manifest for each test case and remove it automatically
# when the script exits.
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

for template in "${templates[@]}"; do
  # Generate a unique Pod name from the template name.
  pod_name="test-admission-$(echo "$template" | tr '.' '-' | tr '_' '-')"

  # Build a minimal Pod that references the CXI ResourceClaimTemplate.
  # The Pod does not need to run; server-side dry-run is sufficient to
  # exercise the real Kubernetes admission pipeline.
  cat > "${tmpdir}/pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${NAMESPACE}
spec:
  containers:
    - name: test
      image: busybox
      command: ["sleep", "3600"]

  resourceClaims:
    - name: cxi
      resourceClaimTemplateName: ${template}
EOF

  # Submit the Pod to the API server without persisting it and read back
  # the Multus annotation added by the MutatingAdmissionPolicy.
  actual="$(
    kubectl create \
      --dry-run=server \
      -f "${tmpdir}/pod.yaml" \
      -o jsonpath='{.metadata.annotations.k8s\.v1\.cni\.cncf\.io/networks}'
  )"

  # Every supported CXI template must trigger the expected Slingshot
  # secondary-network attachment.
  if [[ "$actual" != "$EXPECTED_NETWORK" ]]; then
    echo "FAIL: ${template}"
    echo "  expected: ${EXPECTED_NETWORK}"
    echo "  actual:   ${actual}"
    exit 1
  fi

  echo "PASS: ${template} -> ${actual}"
done

echo
echo "PASS: all CXI ResourceClaimTemplate naming patterns triggered Slingshot network auto-attachment"

# ---------------------------------------------------------------------------
# Capacity conversion and VNI selector (ResourceClaim CREATE)
#
# Needs the Helm chart's DeviceClass name and driver name; override them for a
# differently named install. Claims are submitted with --dry-run=server, so the
# namespaces below are the only objects this section creates.
# ---------------------------------------------------------------------------

DRIVER="${DRIVER:-cxi.slingshot.hpe.com}"
DEVICE_CLASS="${DEVICE_CLASS:-cxi-slingshot-hpe-com-device}"
VNI_ANNOTATION="${DRIVER}/vni"
ANNOTATED_NS="test-admission-vni"
ANNOTATED_VNI=70
# kube-public is skipped by the VNI allocator, so it stays unannotated, and is
# not excluded by the policy binding (only kube-system is).
UNANNOTATED_NS="${UNANNOTATED_NS:-kube-public}"

trap 'rm -rf "$tmpdir"; kubectl delete namespace "${ANNOTATED_NS}" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT

kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${ANNOTATED_NS}
  annotations:
    ${VNI_ANNOTATION}: "${ANNOTATED_VNI}"
EOF

# submit_claim NAMESPACE JSONPATH [EXTRA_EXACTLY] [CONFIG] [SECOND_REQUEST]
# Server-side dry-runs a ResourceClaim with one CXI request and prints JSONPATH
# of the mutated object. EXTRA_EXACTLY is raw YAML under "exactly:" (10-space
# indent); CONFIG is a raw "config:" block (4-space indent); SECOND_REQUEST is
# a raw extra list item under "requests:" (6-space indent).
submit_claim() {
  local ns="$1" path="$2" extra="${3:-}" config="${4:-}" second="${5:-}"
  cat > "${tmpdir}/claim.yaml" <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: test-admission-claim
  namespace: ${ns}
spec:
  devices:
    requests:
      - name: cxi
        exactly:
          deviceClassName: ${DEVICE_CLASS}
${extra}
${second}
${config}
EOF
  kubectl create --dry-run=server -f "${tmpdir}/claim.yaml" -o jsonpath="${path}"
}

# expect_equal DESCRIPTION EXPECTED ACTUAL
expect_equal() {
  if [[ "$3" != "$2" ]]; then
    echo "FAIL: $1"
    echo "  expected: $2"
    echo "  actual:   $3"
    exit 1
  fi
  echo "PASS: $1"
}

# expect_contains DESCRIPTION SUBSTRING ACTUAL
expect_contains() {
  if [[ "$3" != *"$2"* ]]; then
    echo "FAIL: $1"
    echo "  expected to contain: $2"
    echo "  actual:              $3"
    exit 1
  fi
  echo "PASS: $1"
}

SELECTORS='{range .spec.devices.requests[0].exactly.selectors[*]}{.cel.expression}{"\n"}{end}'

echo
echo "==> Capacity conversion: le_pools: 0 / tle_pools: 0 (the only valid LE/TLE in VF mode)"
actual="$(submit_claim "${NAMESPACE}" \
  '{.spec.devices.requests[0].exactly.capacity.requests.les}/{.spec.devices.requests[0].exactly.capacity.requests.tles}' \
  "" \
  "    config:
      - requests: [cxi]
        opaque:
          driver: ${DRIVER}
          parameters:
            txqs: 4
            le_pools: 0
            tle_pools: 0")"
expect_equal "le_pools: 0 / tle_pools: 0 convert to les/tles \"0\"" "0/0" "${actual}"

echo
echo "==> VNI selector"
actual="$(submit_claim "${ANNOTATED_NS}" "${SELECTORS}")"
expect_equal "annotated namespace: exactly one selector appended" "1" "$(grep -c . <<<"${actual}")"
expect_contains "selector covers the range form" "vni_min <= ${ANNOTATED_VNI} && ${ANNOTATED_VNI} <= " "${actual}"
expect_contains "selector covers the list form" "vni_set.contains(\",${ANNOTATED_VNI},\")" "${actual}"
expect_contains "selector lets PF devices (no VNI attributes) through" \
  "!has(device.attributes[\"${DRIVER}\"].vni_min) && !has(device.attributes[\"${DRIVER}\"].vni_set)" "${actual}"

actual="$(submit_claim "${UNANNOTATED_NS}" "${SELECTORS}")"
expect_equal "unannotated namespace: no selector added" "" "${actual}"

USER_SELECTOR='device.attributes["'"${DRIVER}"'"].function_type == "parent"'
actual="$(submit_claim "${ANNOTATED_NS}" "${SELECTORS}" \
  "          selectors:
            - cel:
                expression: '${USER_SELECTOR}'")"
expect_equal "user selector keeps its place" "${USER_SELECTOR}" "$(sed -n 1p <<<"${actual}")"
expect_contains "user selector is followed by the VNI selector" "contains(\",${ANNOTATED_VNI},\")" "$(sed -n 2p <<<"${actual}")"
expect_equal "user selector + VNI selector: two selectors" "2" "$(grep -c . <<<"${actual}")"

actual="$(submit_claim "${ANNOTATED_NS}" \
  '{.spec.devices.requests[1].exactly.selectors[0].cel.expression}' \
  "" "" \
  "      - name: cxi2
        exactly:
          deviceClassName: ${DEVICE_CLASS}")"
expect_contains "second CXI request gets the selector too" "contains(\",${ANNOTATED_VNI},\")" "${actual}"

echo
echo "PASS: capacity conversion and VNI selector"