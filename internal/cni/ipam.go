package cni

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/containernetworking/cni/pkg/invoke"
	types100 "github.com/containernetworking/cni/pkg/types/100"
	"github.com/containernetworking/cni/pkg/version"
)

// ipamDelegate is the IPAM-facing half of the plugin. The real
// implementation execs the configured IPAM plugin (Whereabouts) per the
// standard CNI IPAM delegate contract; tests substitute a fake so the
// ADD/DEL/CHECK orchestration can be exercised without a plugin binary.
type ipamDelegate interface {
	// Add asks the IPAM plugin for an address for ifName and returns its
	// result (guaranteed to carry at least one IP).
	Add(ctx context.Context, netconf []byte, ifName string) (*types100.Result, error)
	// Del releases whatever the IPAM plugin assigned to ifName.
	Del(ctx context.Context, netconf []byte, ifName string) error
	// Check asks the IPAM plugin to verify its state for ifName.
	Check(ctx context.Context, netconf []byte, ifName string) error
}

// execDelegate is the real ipamDelegate: it locates pluginType on CNI_PATH
// and execs it. netconf is forwarded verbatim — the IPAM plugin reads its
// own "ipam" block out of the same config slingshot-cni received, which is
// the standard delegate contract.
type execDelegate struct {
	pluginType string
}

func newIPAMDelegate(pluginType string) ipamDelegate {
	return execDelegate{pluginType: pluginType}
}

func (d execDelegate) Add(ctx context.Context, netconf []byte, ifName string) (*types100.Result, error) {
	raw, err := invoke.DelegateAdd(ctx, d.pluginType, netconf, newIfNameExec(ifName))
	if err != nil {
		return nil, fmt.Errorf("IPAM %s ADD for %s: %w", d.pluginType, ifName, err)
	}
	result, err := types100.GetResult(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing IPAM %s result for %s: %w", d.pluginType, ifName, err)
	}
	if len(result.IPs) == 0 {
		return nil, fmt.Errorf("IPAM %s returned no addresses for %s", d.pluginType, ifName)
	}
	return result, nil
}

func (d execDelegate) Del(ctx context.Context, netconf []byte, ifName string) error {
	if err := invoke.DelegateDel(ctx, d.pluginType, netconf, newIfNameExec(ifName)); err != nil {
		return fmt.Errorf("IPAM %s DEL for %s: %w", d.pluginType, ifName, err)
	}
	return nil
}

func (d execDelegate) Check(ctx context.Context, netconf []byte, ifName string) error {
	if err := invoke.DelegateCheck(ctx, d.pluginType, netconf, newIfNameExec(ifName)); err != nil {
		return fmt.Errorf("IPAM %s CHECK for %s: %w", d.pluginType, ifName, err)
	}
	return nil
}

// ifNameExec is invoke.DefaultExec with CNI_IFNAME overridden per call.
// invoke's delegate helpers otherwise pass this process's own environment
// through, i.e. the single CNI_IFNAME containerd/Multus invoked us with.
// That is wrong for a multi-device pod: Whereabouts keys each reservation on
// (containerID, ifname), so every device's interface must be presented under
// its own name or the reservations collide.
type ifNameExec struct {
	invoke.DefaultExec
	ifName string
}

func newIfNameExec(ifName string) invoke.Exec {
	return &ifNameExec{
		DefaultExec: invoke.DefaultExec{
			RawExec:       &invoke.RawExec{Stderr: os.Stderr},
			PluginDecoder: version.PluginDecoder{},
		},
		ifName: ifName,
	}
}

func (e *ifNameExec) ExecPlugin(ctx context.Context, pluginPath string, stdinData []byte, environ []string) ([]byte, error) {
	const key = "CNI_IFNAME="
	env := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, key) {
			env = append(env, kv)
		}
	}
	env = append(env, key+e.ifName)
	return e.DefaultExec.ExecPlugin(ctx, pluginPath, stdinData, env)
}
