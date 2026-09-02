// cxi-cdi-hook is a standalone, statically-linked OCI runtime hook installed
// on every node by cxi-dra-driver (see internal/cdi.InstallHook). It replaces
// the CDI env/nccl/aws-ofi-nccl specs' former dependency on NVIDIA's
// /usr/bin/nvidia-cdi-hook binary, which is only present on nodes with
// nvidia-container-toolkit installed and has no relationship to CXI hardware.
//
// Only the "update-ldcache" subcommand is implemented, matching the one
// operation cxi-dra-driver's CDI specs need: refreshing the ldconfig cache of
// a freshly created container's rootfs so libraries bind-mounted in by CDI
// are resolvable by the dynamic linker. It does not shell out to a bare
// "ldconfig" resolved from $PATH \u2014 that would just trade the nvidia-cdi-hook
// dependency for an equally external ldconfig one. Instead it execs the
// self-contained ldconfig cxi-dra-driver installs alongside this hook (see
// internal/cdi.DefaultLdconfigDir), via that ldconfig's own dynamic linker,
// so this never depends on anything pre-installed on the node.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/slingshot/cxi-dra-driver/internal/cdi"
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
	ldconfigDir := fs.String("ldconfig-dir", cdi.DefaultLdconfigDir,
		"Host directory containing the ldconfig binary installed alongside this hook, plus its dynamic linker and libc")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(1)
	}
	if len(folders) == 0 {
		fmt.Fprintln(os.Stderr, "cxi-cdi-hook: at least one --folder is required")
		os.Exit(1)
	}

	if err := updateLdcache(os.Stdin, *ldconfigDir, folders); err != nil {
		fmt.Fprintf(os.Stderr, "cxi-cdi-hook: %v\n", err)
		os.Exit(1)
	}
}

// updateLdcache reads the OCI runtime state from stdin, resolves the
// container's rootfs, and runs the ldconfig installed at ldconfigDir against
// it so it rebuilds /etc/ld.so.cache inside the container including the given
// folders. ldconfig is invoked through its own dynamic linker (also installed
// at ldconfigDir) with an explicit --library-path, so it resolves libc
// entirely from ldconfigDir rather than the node's own library layout \u2014 the
// same trick this repo's own Dockerfile uses to run cxi-dra-driver itself
// without depending on the runtime environment's glibc.
func updateLdcache(stdin *os.File, ldconfigDir string, folders []string) error {
	var state ociState
	if err := json.NewDecoder(stdin).Decode(&state); err != nil {
		return fmt.Errorf("decoding OCI state: %w", err)
	}

	rootPath, err := resolveRootfs(state.Bundle)
	if err != nil {
		return err
	}

	interp, ldconfigBin, err := resolveLdconfig(ldconfigDir)
	if err != nil {
		return err
	}

	args := append([]string{"--library-path", ldconfigDir, ldconfigBin, "-r", rootPath}, folders...)
	cmd := exec.Command(interp, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", interp, args, err)
	}
	return nil
}

// resolveLdconfig locates the dynamic linker and ldconfig binary that
// cxi-dra-driver installs at dir (see internal/cdi.InstallHook), alongside
// the libc.so.6 they both depend on. The driver controls the destination
// filenames itself, so unlike the ldconfig binary's own name they don't vary
// by architecture and no on-node introspection is needed to find them.
func resolveLdconfig(dir string) (interp, ldconfigBin string, err error) {
	interp = filepath.Join(dir, "ld.so")
	ldconfigBin = filepath.Join(dir, "ldconfig")
	if _, err := os.Stat(ldconfigBin); err != nil {
		return "", "", fmt.Errorf("ldconfig not found in %s (has cxi-dra-driver installed it on this node?): %w", dir, err)
	}
	return interp, ldconfigBin, nil
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
