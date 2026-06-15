package cdi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// WriteContainerdCDIConfig writes a containerd config fragment that enables
// CDI and registers cdiSpecDirs as the CDI spec search paths. It is idempotent
// — calling it again with the same arguments overwrites the file in place.
//
// The file must be placed under containerd's conf.d drop-in directory
// (typically /etc/containerd/conf.d/) so containerd picks it up on restart.
func WriteContainerdCDIConfig(path string, cdiSpecDirs []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating containerd conf.d dir: %w", err)
	}

	quoted := make([]string, len(cdiSpecDirs))
	for i, d := range cdiSpecDirs {
		quoted[i] = fmt.Sprintf("%q", d)
	}

	content := fmt.Sprintf(`[plugins."io.containerd.grpc.v1.cri"]
  enable_cdi = true
  cdi_spec_dirs = [%s]
`, strings.Join(quoted, ", "))

	return os.WriteFile(path, []byte(content), 0o644)
}

// FindContainerdPID scans procRoot (normally "/proc") for a process whose comm
// name is "containerd" and returns its PID. Returns an error if not found.
func FindContainerdPID(procRoot string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", procRoot, err)
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // skip non-numeric entries like "self", "tty"
		}
		comm, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "comm"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(comm)) == "containerd" {
			return pid, nil
		}
	}
	return 0, errors.New("containerd process not found in " + procRoot)
}

// SignalContainerd sends SIGHUP to the process with the given PID, causing
// containerd to reload its configuration without restarting running containers.
func SignalContainerd(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", pid, err)
	}
	if err := p.Signal(syscall.SIGHUP); err != nil {
		return fmt.Errorf("sending SIGHUP to pid %d: %w", pid, err)
	}
	return nil
}
