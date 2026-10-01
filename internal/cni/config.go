// Package cni implements the Slingshot CNI plugin's ADD/DEL/CHECK command
// logic, consumed by the thin cmd/slingshot-cni entrypoint (mirroring the
// cmd/cxi-cdi-hook + internal/cdi split already used in this repo for the
// CDI hook). It resolves the CXI DRA driver's per-pod hint file (see
// docs/cni/cnidra-cni-hint-contract.md), creates one L2 IPVLAN
// sub-interface per claimed device on that device's HSN parent, and
// delegates address assignment to the configured IPAM plugin (Whereabouts)
// per the standard CNI IPAM delegate contract. It owns L3 addressing only;
// the driver's internal/netsysfs synthetic-sysfs injection (which is what
// libfabric's HSN discovery depends on) runs independently alongside it.
package cni

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/containernetworking/cni/pkg/types"
)

// DefaultHintsDir is the default directory the CXI DRA driver writes per-pod
// CNI hint files to. Must match the driver's --cni-hints-dir default (see
// internal/driver/cnihints.go and docs/cni/cnidra-cni-hint-contract.md).
const DefaultHintsDir = "/var/run/cxi/net-hints"

// defaultRetryInterval/defaultRetryTimeout bound how long the plugin polls
// for a hint file that hasn't been written yet. This is a safety net for the
// driver crashing between claim allocation and PrepareResourceClaims
// completing, not the common case — see the ordering guarantee documented in
// docs/cni/cnidra-cni-hint-contract.md.
const (
	defaultRetryInterval = 100 * time.Millisecond
	defaultRetryTimeout  = 5 * time.Second
)

// NetConf is the plugin's stdin configuration, decoded from the network
// attachment definition's CNI config (delivered by containerd/Multus per the
// standard CNI ADD/DEL/CHECK contract). Only the fields this plugin needs
// beyond the common CNI header are defined here.
type NetConf struct {
	types.NetConf

	// HintsDir overrides the directory the driver writes per-pod hint files
	// to. Empty uses DefaultHintsDir.
	HintsDir string `json:"hintsDir,omitempty"`

	// RetryInterval/RetryTimeout override the polling interval and overall
	// deadline used while waiting for the hint file to appear, as Go
	// duration strings (e.g. "100ms", "5s"). Empty uses the package
	// defaults.
	RetryInterval string `json:"retryInterval,omitempty"`
	RetryTimeout  string `json:"retryTimeout,omitempty"`

	retryInterval time.Duration
	retryTimeout  time.Duration
}

// ParseNetConf decodes the plugin's stdin CNI configuration and resolves its
// retry/timeout overrides, failing fast if either is set to an unparsable
// duration rather than silently falling back to the default. ipam.type is
// required: the plugin has no built-in IPAM and no default delegate.
func ParseNetConf(data []byte) (*NetConf, error) {
	conf := &NetConf{}
	if err := json.Unmarshal(data, conf); err != nil {
		return nil, fmt.Errorf("parsing CNI network configuration: %w", err)
	}
	if conf.IPAM.Type == "" {
		return nil, fmt.Errorf(`network configuration is missing "ipam.type" (e.g. "whereabouts")`)
	}

	conf.retryInterval = defaultRetryInterval
	if conf.RetryInterval != "" {
		d, err := time.ParseDuration(conf.RetryInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid retryInterval %q: %w", conf.RetryInterval, err)
		}
		conf.retryInterval = d
	}

	conf.retryTimeout = defaultRetryTimeout
	if conf.RetryTimeout != "" {
		d, err := time.ParseDuration(conf.RetryTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid retryTimeout %q: %w", conf.RetryTimeout, err)
		}
		conf.retryTimeout = d
	}

	return conf, nil
}

// hintsDirOrDefault returns the configured hints directory, or
// DefaultHintsDir if unset.
func (c *NetConf) hintsDirOrDefault() string {
	if c.HintsDir == "" {
		return DefaultHintsDir
	}
	return c.HintsDir
}
