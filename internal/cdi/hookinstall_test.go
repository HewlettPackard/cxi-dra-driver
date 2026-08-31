package cdi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLdcacheHookUsesDefaultPathWhenEmpty(t *testing.T) {
	hook := ldcacheHook("", "/usr/lib64")
	if hook.Path != DefaultHookPath {
		t.Errorf("Path = %q, want %q", hook.Path, DefaultHookPath)
	}
	if hook.HookName != "createContainer" {
		t.Errorf("HookName = %q, want createContainer", hook.HookName)
	}
}

func TestLdcacheHookUsesOverridePath(t *testing.T) {
	const override = "/opt/cxi/bin/cxi-cdi-hook"
	hook := ldcacheHook(override, "/usr/lib64", "/usr/lib")
	if hook.Path != override {
		t.Errorf("Path = %q, want %q", hook.Path, override)
	}
	want := []string{override, "update-ldcache", "--folder", "/usr/lib64", "--folder", "/usr/lib"}
	if len(hook.Args) != len(want) {
		t.Fatalf("Args = %v, want %v", hook.Args, want)
	}
	for i, a := range want {
		if hook.Args[i] != a {
			t.Errorf("Args[%d] = %q, want %q", i, hook.Args[i], a)
		}
	}
}

func TestInstallHook(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "cxi-cdi-hook")
	if err := os.WriteFile(src, []byte("fake binary"), 0o644); err != nil {
		t.Fatalf("writing fake source binary: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "bin", "cxi-cdi-hook")
	if err := InstallHook(src, dest); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading installed hook: %v", err)
	}
	if string(data) != "fake binary" {
		t.Errorf("installed content = %q, want %q", data, "fake binary")
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat installed hook: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed hook mode = %v, want executable", info.Mode())
	}
}

func TestInstallHookOverwritesExisting(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "cxi-cdi-hook")
	if err := os.WriteFile(src, []byte("v2"), 0o644); err != nil {
		t.Fatalf("writing fake source binary: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "cxi-cdi-hook")
	if err := os.WriteFile(dest, []byte("v1"), 0o755); err != nil {
		t.Fatalf("seeding existing hook: %v", err)
	}

	if err := InstallHook(src, dest); err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading installed hook: %v", err)
	}
	if string(data) != "v2" {
		t.Errorf("installed content = %q, want %q", data, "v2")
	}
}
