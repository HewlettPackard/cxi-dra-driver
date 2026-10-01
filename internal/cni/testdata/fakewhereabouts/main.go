// Command fakewhereabouts is a stand-in for the Whereabouts IPAM plugin, built
// on the fly by internal/cni's tests onto CNI_PATH so the real
// invoke.Delegate* exec/env/stdin/stdout contract is exercised without a
// Whereabouts install. It records every invocation (command, CNI_IFNAME,
// CNI_CONTAINERID) as one JSON line appended to the file named by
// FAKE_WHEREABOUTS_LOG, and on ADD prints a CNI 1.0.0 result. It ships in no
// binary.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type record struct {
	Command     string `json:"command"`
	IfName      string `json:"ifName"`
	ContainerID string `json:"containerID"`
	IPAMType    string `json:"ipamType"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	var conf struct {
		IPAM struct {
			Type       string `json:"type"`
			FailIfName string `json:"failIfName"`
		} `json:"ipam"`
	}
	if err := json.Unmarshal(stdin, &conf); err != nil {
		return fmt.Errorf("parsing stdin config: %w", err)
	}

	rec := record{
		Command:     os.Getenv("CNI_COMMAND"),
		IfName:      os.Getenv("CNI_IFNAME"),
		ContainerID: os.Getenv("CNI_CONTAINERID"),
		IPAMType:    conf.IPAM.Type,
	}
	if err := appendLog(rec); err != nil {
		return err
	}
	if conf.IPAM.FailIfName != "" && conf.IPAM.FailIfName == rec.IfName {
		return fmt.Errorf("forced failure for %s", rec.IfName)
	}

	switch rec.Command {
	case "ADD":
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"cniVersion": "1.0.0",
			"ips":        []map[string]any{{"address": "10.200.0.42/24", "gateway": "10.200.0.1"}},
			"routes":     []map[string]any{{"dst": "10.201.0.0/16"}},
		})
	case "DEL", "CHECK":
		return nil
	default:
		return fmt.Errorf("unsupported CNI_COMMAND %q", rec.Command)
	}
}

func appendLog(rec record) error {
	path := os.Getenv("FAKE_WHEREABOUTS_LOG")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rec)
}
