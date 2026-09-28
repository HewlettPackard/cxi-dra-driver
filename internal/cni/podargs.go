package cni

import (
	"fmt"
	"strings"
)

// podUIDArgKey is the CNI_ARGS key kubelet populates with the pod's UID on
// every CNI invocation (see docs/cnidra-cni-hint-contract.md) — no
// API-server lookup is needed to resolve it.
const podUIDArgKey = "K8S_POD_UID"

// PodUIDFromArgs parses the semicolon-delimited "key=value" CNI_ARGS string
// (passed through skel.CmdArgs.Args) and returns the K8S_POD_UID value
// kubelet sets on every invocation. This is the standard k8s CNI_ARGS
// convention, not part of the base CNI spec itself.
func PodUIDFromArgs(args string) (string, error) {
	for _, pair := range strings.Split(args, ";") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		if !found || key != podUIDArgKey {
			continue
		}
		if value == "" {
			return "", fmt.Errorf("CNI_ARGS %s is empty", podUIDArgKey)
		}
		return value, nil
	}
	return "", fmt.Errorf("CNI_ARGS missing required %s", podUIDArgKey)
}
