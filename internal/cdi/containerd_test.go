package cdi

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWriteContainerdCDIConfig_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "50-cxi-cdi.toml")

	if _, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"}); err != nil {
		t.Fatalf("WriteContainerdCDIConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		`enable_cdi = true`,
		`"/var/run/cdi"`,
		`[plugins."io.containerd.grpc.v1.cri"]`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("config missing %q\ngot:\n%s", want, content)
		}
	}
}

func TestWriteContainerdCDIConfig_MultipleDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "50-cxi-cdi.toml")

	dirs := []string{"/var/run/cdi", "/etc/cdi"}
	if _, err := WriteContainerdCDIConfig(path, dirs); err != nil {
		t.Fatalf("WriteContainerdCDIConfig: %v", err)
	}

	data, _ := os.ReadFile(path)
	content := string(data)

	if !strings.Contains(content, `"/var/run/cdi"`) || !strings.Contains(content, `"/etc/cdi"`) {
		t.Errorf("config missing expected dirs\ngot:\n%s", content)
	}
}

func TestWriteContainerdCDIConfig_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf.d", "50-cxi-cdi.toml")

	if _, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"}); err != nil {
		t.Fatalf("WriteContainerdCDIConfig: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestWriteContainerdCDIConfig_IdempotentOnRerun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "50-cxi-cdi.toml")

	if _, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if _, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"}); err != nil {
		t.Fatalf("second write: %v", err)
	}
}

// TestWriteContainerdCDIConfig_ReportsChangedOnCreate ensures the first write
// to a fresh path reports changed=true so the caller knows it must reload
// containerd.
func TestWriteContainerdCDIConfig_ReportsChangedOnCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "50-cxi-cdi.toml")

	changed, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"})
	if err != nil {
		t.Fatalf("WriteContainerdCDIConfig: %v", err)
	}
	if !changed {
		t.Error("expected changed=true on first write, got false")
	}
}

// TestWriteContainerdCDIConfig_NoChangeOnIdenticalRerun is the regression test
// for the rollout-restart NotReady bug: when the drop-in already exists with
// identical content (the case on every image-only rollout, since the file
// lives on a persistent hostPath), the function must report changed=false and
// leave the file untouched so the caller skips the disruptive containerd SIGHUP.
func TestWriteContainerdCDIConfig_NoChangeOnIdenticalRerun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "50-cxi-cdi.toml")

	if _, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"}); err != nil {
		t.Fatalf("first write: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if changed {
		t.Error("expected changed=false when content is identical, got true")
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("file was rewritten despite identical content (mtime changed)")
	}
}

// TestWriteContainerdCDIConfig_ReportsChangedWhenContentDiffers ensures a
// genuine config change (different spec dirs) still triggers a reload.
func TestWriteContainerdCDIConfig_ReportsChangedWhenContentDiffers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "50-cxi-cdi.toml")

	if _, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi"}); err != nil {
		t.Fatalf("first write: %v", err)
	}

	changed, err := WriteContainerdCDIConfig(path, []string{"/var/run/cdi", "/etc/cdi"})
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when spec dirs differ, got false")
	}
}

func TestFindContainerdPID_FindsProcess(t *testing.T) {
	// Build a fake /proc tree with one "containerd" process at PID 1234.
	procRoot := t.TempDir()
	pidDir := filepath.Join(procRoot, "1234")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "comm"), []byte("containerd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pid, err := FindContainerdPID(procRoot)
	if err != nil {
		t.Fatalf("FindContainerdPID: %v", err)
	}
	if pid != 1234 {
		t.Errorf("got PID %d, want 1234", pid)
	}
}

func TestFindContainerdPID_NotFound(t *testing.T) {
	procRoot := t.TempDir()
	// Empty proc root — no containerd process.
	if _, err := FindContainerdPID(procRoot); err == nil {
		t.Fatal("expected error when containerd not found, got nil")
	}
}

func TestFindContainerdPID_IgnoresNonNumericDirs(t *testing.T) {
	procRoot := t.TempDir()
	// A non-PID directory (e.g. "self", "tty") must not cause a crash.
	for _, name := range []string{"self", "tty", "net"} {
		_ = os.MkdirAll(filepath.Join(procRoot, name), 0o755)
	}
	if _, err := FindContainerdPID(procRoot); err == nil {
		t.Fatal("expected not-found error, got nil")
	}
}

func TestSignalContainerd_SendsSIGHUP(t *testing.T) {
	// Start a process we can signal — use the current process as target.
	// We record receipt via a channel wired to a signal.Notify handler.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	if err := SignalContainerd(os.Getpid()); err != nil {
		t.Fatalf("SignalContainerd: %v", err)
	}

	select {
	case sig := <-sigCh:
		if sig != syscall.SIGHUP {
			t.Errorf("got signal %v, want SIGHUP", sig)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SIGHUP")
	}
}

func TestSignalContainerd_NoSuchProcess(t *testing.T) {
	// PID 0 is not a valid signal target and should return an error.
	if err := SignalContainerd(0); err == nil {
		t.Fatal("expected error for invalid PID, got nil")
	}
}
