package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveRootfsDefaultsToRootfsSubdir(t *testing.T) {
	bundle := t.TempDir()
	writeConfig(t, bundle, ociConfig{})

	got, err := resolveRootfs(bundle)
	if err != nil {
		t.Fatalf("resolveRootfs: %v", err)
	}
	want := filepath.Join(bundle, "rootfs")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveRootfsRelativePath(t *testing.T) {
	bundle := t.TempDir()
	cfg := ociConfig{}
	cfg.Root.Path = "custom-root"
	writeConfig(t, bundle, cfg)

	got, err := resolveRootfs(bundle)
	if err != nil {
		t.Fatalf("resolveRootfs: %v", err)
	}
	want := filepath.Join(bundle, "custom-root")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveRootfsAbsolutePath(t *testing.T) {
	bundle := t.TempDir()
	cfg := ociConfig{}
	cfg.Root.Path = "/mnt/other-root"
	writeConfig(t, bundle, cfg)

	got, err := resolveRootfs(bundle)
	if err != nil {
		t.Fatalf("resolveRootfs: %v", err)
	}
	if got != "/mnt/other-root" {
		t.Errorf("got %q, want /mnt/other-root", got)
	}
}

func TestResolveRootfsEmptyBundle(t *testing.T) {
	if _, err := resolveRootfs(""); err == nil {
		t.Error("expected error for empty bundle, got nil")
	}
}

func TestResolveRootfsMissingConfig(t *testing.T) {
	bundle := t.TempDir()
	if _, err := resolveRootfs(bundle); err == nil {
		t.Error("expected error for missing config.json, got nil")
	}
}

func writeConfig(t *testing.T, bundle string, cfg ociConfig) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshalling config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644); err != nil {
		t.Fatalf("writing config.json: %v", err)
	}
}

func TestResolveLdconfigOK(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ldconfig"), []byte("fake ldconfig"), 0o755); err != nil {
		t.Fatalf("writing fake ldconfig: %v", err)
	}

	interp, ldconfigBin, err := resolveLdconfig(dir)
	if err != nil {
		t.Fatalf("resolveLdconfig: %v", err)
	}
	wantInterp := filepath.Join(dir, "ld.so")
	if interp != wantInterp {
		t.Errorf("interp = %q, want %q", interp, wantInterp)
	}
	wantLdconfig := filepath.Join(dir, "ldconfig")
	if ldconfigBin != wantLdconfig {
		t.Errorf("ldconfigBin = %q, want %q", ldconfigBin, wantLdconfig)
	}
}

func TestResolveLdconfigMissingBinary(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := resolveLdconfig(dir); err == nil {
		t.Error("expected error for missing ldconfig binary, got nil")
	}
}
