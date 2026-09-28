package cni

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Hint is one claimed device's entry in a pod's CNI hint file, matching the
// JSON schema the driver writes (internal/driver.CNIHint). Kept as a
// separate type rather than importing internal/driver directly — the two
// sides of this contract are different processes on different lifecycles
// and should only agree on the JSON schema, not share a Go type.
type Hint struct {
	Device   string `json:"device"`
	HSNIface string `json:"hsn_iface"`
	VNI      int    `json:"vni"`
}

// ReadHint reads and decodes the hint file for podUID from hintsDir,
// retrying every interval until the file appears or timeout elapses. The
// retry is a safety net for the driver crashing between claim allocation and
// PrepareResourceClaims completing — see the ordering guarantee in
// docs/cnidra-cni-hint-contract.md, which this backstops rather than relies
// on for the common case.
func ReadHint(hintsDir, podUID string, interval, timeout time.Duration) ([]Hint, error) {
	path := filepath.Join(hintsDir, podUID+".json")
	deadline := time.Now().Add(timeout)

	var lastErr error
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			var hints []Hint
			if err := json.Unmarshal(data, &hints); err != nil {
				return nil, fmt.Errorf("parsing CNI hint file %s: %w", path, err)
			}
			return hints, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("hint file %s not found after %s: %w", path, timeout, lastErr)
		}
		time.Sleep(interval)
	}
}

// validateHints checks that every hint entry is well-formed. It does not
// check interface reachability or device availability — the driver already
// guarantees those.
func validateHints(hints []Hint) error {
	if len(hints) == 0 {
		return fmt.Errorf("hint file has no device entries")
	}
	for i, h := range hints {
		if h.Device == "" {
			return fmt.Errorf("hint entry %d: missing device", i)
		}
		if h.HSNIface == "" {
			return fmt.Errorf("hint entry %d: missing hsn_iface", i)
		}
		if h.VNI <= 0 {
			return fmt.Errorf("hint entry %d: invalid vni %d", i, h.VNI)
		}
	}
	return nil
}
