package cni

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/containernetworking/cni/pkg/skel"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// plugin bundles the two swappable dependencies behind ADD/DEL/CHECK so the
// orchestration can be unit-tested with fakes. newIPAM is a factory rather
// than an instance because the IPAM plugin type comes from the stdin config,
// which is not known until the command runs.
type plugin struct {
	links   linkManager
	newIPAM func(pluginType string) ipamDelegate
	stdout  io.Writer
}

func defaultPlugin() *plugin {
	return &plugin{links: newLinkManager(), newIPAM: newIPAMDelegate, stdout: os.Stdout}
}

// CmdAdd implements the CNI ADD command: one L2 IPVLAN sub-interface per
// claimed device, addressed via the configured IPAM plugin. See plugin.add.
func CmdAdd(args *skel.CmdArgs) error { return defaultPlugin().add(args) }

// CmdDel implements the CNI DEL command: releases each device's IPAM
// address and removes its interface, best-effort. See plugin.del.
func CmdDel(args *skel.CmdArgs) error { return defaultPlugin().del(args) }

// CmdCheck implements the CNI CHECK command: verifies each device's
// interface and IPAM state. See plugin.check.
func CmdCheck(args *skel.CmdArgs) error { return defaultPlugin().check(args) }

// add resolves the pod's hint file and, for every entry (a count>1 request
// yields several), creates the IPVLAN interface on that device's HSN parent,
// obtains an address from IPAM, applies it, and folds the outcome into one
// aggregated CNI result. Any failure unwinds everything this invocation has
// already done — CNI ADD must not leave partial state for kubelet's retry to
// trip over.
func (p *plugin) add(args *skel.CmdArgs) error {
	conf, hints, err := resolveHint(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ipam := p.newIPAM(conf.IPAM.Type)
	result := &types100.Result{CNIVersion: conf.CNIVersion}

	// Interfaces fully set up so far, unwound in reverse on failure.
	var done []string
	rollback := func() {
		for i := len(done) - 1; i >= 0; i-- {
			p.teardown(ctx, ipam, args, done[i])
		}
	}

	for _, hint := range hints {
		ifName, err := p.links.CreateIPVLAN(hint.HSNIface, args.ContainerID, args.Netns)
		if err != nil {
			rollback()
			return fmt.Errorf("device %s: %w", hint.Device, err)
		}

		ipamResult, err := ipam.Add(ctx, args.StdinData, ifName)
		if err != nil {
			_ = p.links.Delete(args.Netns, ifName)
			rollback()
			return fmt.Errorf("device %s: %w", hint.Device, err)
		}

		if err := p.links.ConfigureAddress(args.Netns, ifName, ipamResult); err != nil {
			p.teardown(ctx, ipam, args, ifName)
			rollback()
			return fmt.Errorf("device %s: %w", hint.Device, err)
		}
		done = append(done, ifName)

		result.Interfaces = append(result.Interfaces, &types100.Interface{Name: ifName, Sandbox: args.Netns})
		idx := len(result.Interfaces) - 1
		for _, ip := range ipamResult.IPs {
			ipc := ip.Copy()
			ipc.Interface = types100.Int(idx)
			result.IPs = append(result.IPs, ipc)
		}
		result.Routes = append(result.Routes, ipamResult.Routes...)
		log.Printf("slingshot-cni: ADD %s: device %s -> %s %v", args.ContainerID, hint.Device, ifName, ipamResult.IPs)
	}

	return result.PrintTo(p.stdout)
}

// del releases every device's IPAM address and removes its interface. It is
// best-effort and idempotent per the CNI spec: each entry is attempted even
// if an earlier one failed, an already-gone interface or netns is fine, and
// a missing hint file (driver crashed/restarted mid-teardown) is logged and
// treated as success — failing DEL would only wedge a pod that is going
// away regardless. A hint file that exists but is malformed is still an
// error, since that indicates a bug rather than a race.
func (p *plugin) del(args *skel.CmdArgs) error {
	conf, err := ParseNetConf(args.StdinData)
	if err != nil {
		return err
	}
	podUID, err := PodUIDFromArgs(args.Args)
	if err != nil {
		return err
	}
	hints, err := ReadHint(conf.hintsDirOrDefault(), podUID, conf.retryInterval, conf.retryTimeout)
	if err != nil {
		log.Printf("slingshot-cni: DEL %s: hint file unavailable, nothing to clean up: %v", podUID, err)
		return nil
	}
	if err := validateHints(hints); err != nil {
		return fmt.Errorf("validating CNI hint for pod %s: %w", podUID, err)
	}

	ctx := context.Background()
	ipam := p.newIPAM(conf.IPAM.Type)
	var errs []error
	for _, hint := range hints {
		// Recover the name ADD actually used so the IPAM release hits the
		// same (containerID, ifname) reservation. If the interface is already
		// gone, release under the preferred name — the most likely match.
		ifName, err := p.links.Find(args.Netns, hint.HSNIface)
		if err != nil {
			errs = append(errs, fmt.Errorf("device %s: %w", hint.Device, err))
		}
		if ifName == "" {
			ifName = hint.HSNIface
		}
		if err := ipam.Del(ctx, args.StdinData, ifName); err != nil {
			errs = append(errs, fmt.Errorf("device %s: %w", hint.Device, err))
		}
		if err := p.links.Delete(args.Netns, ifName); err != nil {
			errs = append(errs, fmt.Errorf("device %s: %w", hint.Device, err))
		}
	}
	return errors.Join(errs...)
}

// check verifies, for every device, that its interface exists and is up and
// that the IPAM plugin still considers its reservation valid.
func (p *plugin) check(args *skel.CmdArgs) error {
	conf, hints, err := resolveHint(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	ipam := p.newIPAM(conf.IPAM.Type)
	for _, hint := range hints {
		ifName, err := p.links.Find(args.Netns, hint.HSNIface)
		if err != nil {
			return fmt.Errorf("device %s: %w", hint.Device, err)
		}
		if ifName == "" {
			return fmt.Errorf("device %s: no interface found for %s (tried %v)", hint.Device, hint.HSNIface, ifaceNameCandidates(hint.HSNIface))
		}
		if err := p.links.Check(args.Netns, ifName); err != nil {
			return fmt.Errorf("device %s: %w", hint.Device, err)
		}
		if err := ipam.Check(ctx, args.StdinData, ifName); err != nil {
			return fmt.Errorf("device %s: %w", hint.Device, err)
		}
	}
	return nil
}

// teardown releases ifName's IPAM address and deletes the interface, logging
// rather than returning failures — it only runs on paths (ADD rollback) that
// are already reporting a more important error.
func (p *plugin) teardown(ctx context.Context, ipam ipamDelegate, args *skel.CmdArgs, ifName string) {
	if err := ipam.Del(ctx, args.StdinData, ifName); err != nil {
		log.Printf("slingshot-cni: rollback %s: %v", ifName, err)
	}
	if err := p.links.Delete(args.Netns, ifName); err != nil {
		log.Printf("slingshot-cni: rollback %s: %v", ifName, err)
	}
}

// resolveHint parses the plugin's stdin config and CNI_ARGS, reads and
// validates the pod's hint file (retrying per conf's bounded retry/timeout),
// and logs the resolved hint. Shared by ADD/CHECK; DEL has its own lenient
// variant because a missing hint file must not fail it. Logging goes to
// stderr (the log package's default) since CNI reserves stdout for the
// command's JSON result/error.
func resolveHint(args *skel.CmdArgs) (*NetConf, []Hint, error) {
	conf, err := ParseNetConf(args.StdinData)
	if err != nil {
		return nil, nil, err
	}

	podUID, err := PodUIDFromArgs(args.Args)
	if err != nil {
		return nil, nil, err
	}

	hints, err := ReadHint(conf.hintsDirOrDefault(), podUID, conf.retryInterval, conf.retryTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving CNI hint for pod %s: %w", podUID, err)
	}
	if err := validateHints(hints); err != nil {
		return nil, nil, fmt.Errorf("validating CNI hint for pod %s: %w", podUID, err)
	}

	log.Printf("slingshot-cni: resolved hint for pod %s: %+v", podUID, hints)
	return conf, hints, nil
}
