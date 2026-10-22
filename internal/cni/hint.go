package cni

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const supportedHintSchemaVersion = 1

// Hint is one claimed device's entry in a pod's CNI hint file, matching the
// JSON schema the driver writes (internal/driver.CNIHint). Kept as a
// separate type rather than importing internal/driver directly — the two
// sides of this contract are different processes on different lifecycles
// and should only agree on the JSON schema, not share a Go type.
type Hint struct {
	SchemaVersion int    `json:"schema_version,omitempty"`
	Device        string `json:"device"`
	HSNIface      string `json:"hsn_iface"`
	IfName        string `json:"ifname,omitempty"`
	VNI           int    `json:"vni"`
}

func (h Hint) effectiveName() string {
	if h.IfName != "" {
		return h.IfName
	}
	return h.HSNIface
}

func validateHintVersions(hints []Hint) error {
	for i, h := range hints {
		if h.SchemaVersion < 0 || h.SchemaVersion > supportedHintSchemaVersion {
			return fmt.Errorf("hint entry %d: unsupported schema_version %d (maximum supported: %d); upgrade slingshot-cni for newer schemas", i, h.SchemaVersion, supportedHintSchemaVersion)
		}
	}
	return nil
}

// ReadHint reads and decodes the hint file for podUID from hintsDir,
// retrying every interval until the file appears or timeout elapses. The
// retry is a safety net for the driver crashing between claim allocation and
// PrepareResourceClaims completing — see the ordering guarantee in
// docs/cni/cnidra-cni-hint-contract.md, which this backstops rather than relies
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
			if err := validateHintVersions(hints); err != nil {
				log.Printf("slingshot-cni: hint file %s: %v", path, err)
				return nil, fmt.Errorf("reading CNI hint file %s: %w", path, err)
			}
			return hints, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("reading CNI hint file %s: %w", path, err)
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
	if err := validateHintVersions(hints); err != nil {
		return err
	}
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
		if len(h.effectiveName())+len(inetSuffix) > maxIfaceNameLen {
			return fmt.Errorf("hint entry %d: effective interface name %q is too long: name plus %q fallback must fit in %d bytes (base name at most %d bytes); set a shorter ifname", i, h.effectiveName(), inetSuffix, maxIfaceNameLen, maxIfaceNameLen-len(inetSuffix))
		}
		if h.VNI <= 0 {
			return fmt.Errorf("hint entry %d: invalid vni %d", i, h.VNI)
		}
	}
	return nil
}
