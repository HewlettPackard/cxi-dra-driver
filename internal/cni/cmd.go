package cni

import (
	"fmt"
	"log"

	"github.com/containernetworking/cni/pkg/skel"
	types100 "github.com/containernetworking/cni/pkg/types/100"
)

// CmdAdd implements the CNI ADD command. At this stage it only resolves and
// validates the pod's hint (device/hsn_iface/vni) and returns success.
func CmdAdd(args *skel.CmdArgs) error {
	conf, _, err := resolveHint(args)
	if err != nil {
		return err
	}
	// ADD must emit a CNI result even though no interface exists yet.
	result := &types100.Result{CNIVersion: conf.CNIVersion}
	return result.Print()
}

// CmdDel implements the CNI DEL command. Like CmdAdd, this scaffold only
// resolves/validates the hint and returns success.
func CmdDel(args *skel.CmdArgs) error {
	_, _, err := resolveHint(args)
	return err
}

// CmdCheck implements the CNI CHECK command, with the same scaffold scope as
// CmdAdd/CmdDel.
func CmdCheck(args *skel.CmdArgs) error {
	_, _, err := resolveHint(args)
	return err
}

// resolveHint parses the plugin's stdin config and CNI_ARGS, reads and
// validates the pod's hint file (retrying per conf's bounded retry/timeout),
// and logs the resolved hint. Shared by ADD/DEL/CHECK, which at this stage
// all do the same read-only resolution and nothing else. Logging goes to
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
