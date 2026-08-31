package cdi

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DefaultHookPath is the host path cxi-cdi-hook is installed at when no
// override is configured. It must resolve to the same path inside the driver
// container and on the host — see the cxi-state hostPath volume in
// deploy/kubernetes/daemonset.yaml, which mounts /var/run/cxi at an identical
// path on both sides.
const DefaultHookPath = "/var/run/cxi/bin/cxi-cdi-hook"

// InstallHook copies the cxi-cdi-hook binary from srcPath (baked into the
// driver's own container image) to destPath, a host path reachable by the
// container runtime. CDI createContainer hooks run in the runtime's (host)
// namespace, not the container's, so the hook binary must live on the host
// filesystem rather than inside any container image — the same constraint
// that requires nvidia-container-toolkit to install nvidia-cdi-hook onto the
// host instead of shipping it in application images.
//
// The copy is written to a temp file and renamed into place so a hook
// invocation racing an in-progress DaemonSet rollout never observes a
// partially written binary.
func InstallHook(srcPath, destPath string) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening hook binary %s: %w", srcPath, err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("creating hook install dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), filepath.Base(destPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for hook binary: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed into place

	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return fmt.Errorf("copying hook binary: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod hook binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp hook binary: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("installing hook binary at %s: %w", destPath, err)
	}
	return nil
}
