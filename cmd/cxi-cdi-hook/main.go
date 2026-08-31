// cxi-cdi-hook is a standalone, statically-linked OCI runtime hook installed
// on every node by cxi-dra-driver (see internal/cdi.InstallHook). It replaces
// the CDI env/nccl/aws-ofi-nccl specs' former dependency on NVIDIA's
// /usr/bin/nvidia-cdi-hook binary, which is only present on nodes with
// nvidia-container-toolkit installed and has no relationship to CXI hardware.
//
// Only the "update-ldcache" subcommand is implemented, matching the one
// operation cxi-dra-driver's CDI specs need: refreshing the ldconfig cache of
// a freshly created container's rootfs so libraries bind-mounted in by CDI
// are resolvable by the dynamic linker.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ociState is the subset of the OCI runtime state JSON (delivered on stdin to
// createContainer hooks, per the OCI runtime spec) needed to locate the
// container's bundle directory.
type ociState struct {
	Bundle string `json:"bundle"`
}

// ociConfig is the subset of the bundle's config.json needed to locate the
// container's rootfs, whose path is not guaranteed to be "rootfs" even
// though that is the near-universal convention.
type ociConfig struct {
	Root struct {
		Path string `json:"path"`
	} `json:"root"`
}

func main() {
	if len(os.Args) < 2 || os.Args[1] != "update-ldcache" {
		fmt.Fprintln(os.Stderr, "usage: cxi-cdi-hook update-ldcache --folder DIR [--folder DIR...]")
		os.Exit(1)
	}

	var folders folderFlag
	fs := flag.NewFlagSet("update-ldcache", flag.ExitOnError)
	fs.Var(&folders, "folder", "directory to add to the container's ldconfig cache (repeatable)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(1)
	}
	if len(folders) == 0 {
		fmt.Fprintln(os.Stderr, "cxi-cdi-hook: at least one --folder is required")
		os.Exit(1)
	}

	if err := updateLdcache(os.Stdin, folders); err != nil {
		fmt.Fprintf(os.Stderr, "cxi-cdi-hook: %v\n", err)
		os.Exit(1)
	}
}

// updateLdcache reads the OCI runtime state from stdin, resolves the
// container's rootfs, and runs the host's ldconfig against it so it rebuilds
// /etc/ld.so.cache inside the container including the given folders.
func updateLdcache(stdin *os.File, folders []string) error {
	var state ociState
	if err := json.NewDecoder(stdin).Decode(&state); err != nil {
		return fmt.Errorf("decoding OCI state: %w", err)
	}

	rootPath, err := resolveRootfs(state.Bundle)
	if err != nil {
		return err
	}

	args := append([]string{"-r", rootPath}, folders...)
	cmd := exec.Command("ldconfig", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ldconfig %v: %w", args, err)
	}
	return nil
}

// resolveRootfs reads bundle/config.json to find the container's rootfs path,
// which is conventionally "rootfs" but per the OCI runtime spec may be any
// value, absolute or relative to bundle.
func resolveRootfs(bundle string) (string, error) {
	if bundle == "" {
		return "", fmt.Errorf("OCI state has no bundle path")
	}

	configData, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return "", fmt.Errorf("reading bundle config.json: %w", err)
	}
	var config ociConfig
	if err := json.Unmarshal(configData, &config); err != nil {
		return "", fmt.Errorf("parsing bundle config.json: %w", err)
	}
	rootPath := config.Root.Path
	if rootPath == "" {
		rootPath = "rootfs"
	}
	if !filepath.IsAbs(rootPath) {
		rootPath = filepath.Join(bundle, rootPath)
	}
	return rootPath, nil
}

type folderFlag []string

func (f *folderFlag) String() string { return fmt.Sprint([]string(*f)) }

func (f *folderFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
