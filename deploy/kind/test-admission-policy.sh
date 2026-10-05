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
# Prerequisites:
#   A kind cluster is running.
#   The Slingshot network MutatingAdmissionPolicy and Binding are installed.
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