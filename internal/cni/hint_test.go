package cni

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadHint_SchemaCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name, data, wantName string
	}{
		{"legacy", `[{"device":"cxi4","hsn_iface":"hsn0","vni":137}]`, "hsn0"},
		{"version 1", `[{"schema_version":1,"device":"cxi4","hsn_iface":"hsn0","vni":137}]`, "hsn0"},
		{"custom name", `[{"schema_version":1,"device":"cxi4","hsn_iface":"enp197s0f1","ifname":"cxi4","vni":137}]`, "cxi4"},
		{"optional name", `[{"schema_version":1,"device":"cxi4","hsn_iface":"hsn0","vni":137}]`, "hsn0"},
		{"empty name", `[{"device":"cxi4","hsn_iface":"hsn0","ifname":"","vni":137}]`, "hsn0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "pod.json"), []byte(tt.data), 0o644); err != nil {
				t.Fatal(err)
			}
			hints, err := ReadHint(dir, "pod", time.Millisecond, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateHints(hints); err != nil {
				t.Fatal(err)
			}
			if got := hints[0].effectiveName(); got != tt.wantName {
				t.Errorf("effectiveName = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestReadHint_NewerSchemaIsLoggedAndRejected(t *testing.T) {
	dir := t.TempDir()
	if err := writeHintFile(dir, "pod", []Hint{{SchemaVersion: supportedHintSchemaVersion + 1}}); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })
	_, err := ReadHint(dir, "pod", time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "unsupported schema_version 2") {
		t.Fatalf("ReadHint error = %v, want unsupported schema", err)
	}
	if !strings.Contains(logs.String(), "upgrade slingshot-cni") {
		t.Errorf("missing actionable schema error log: %s", logs.String())
	}
}

func writeHintFile(dir, podUID string, hints []Hint) error {
	data, err := json.Marshal(hints)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, podUID+".json"), data, 0o644)
}

func TestReadHint_Immediate(t *testing.T) {
	dir := t.TempDir()
	want := []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 137}}
	if err := writeHintFile(dir, "pod-1", want); err != nil {
		t.Fatalf("writeHintFile: %v", err)
	}

	got, err := ReadHint(dir, "pod-1", 10*time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("ReadHint: %v", err)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("ReadHint = %+v, want %+v", got, want)
	}
}

func TestReadHint_RetriesUntilFileAppears(t *testing.T) {
	dir := t.TempDir()
	want := []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 137}}

	errCh := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		errCh <- writeHintFile(dir, "pod-2", want)
	}()

	got, err := ReadHint(dir, "pod-2", 10*time.Millisecond, 2*time.Second)
	if err != nil {
		t.Fatalf("ReadHint: %v", err)
	}
	if writeErr := <-errCh; writeErr != nil {
		t.Fatalf("writeHintFile (background): %v", writeErr)
	}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("ReadHint = %+v, want %+v", got, want)
	}
}

func TestReadHint_TimesOut(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()

	_, err := ReadHint(dir, "missing-pod", 10*time.Millisecond, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned before timeout elapsed: %s", elapsed)
	}
}

func TestReadHint_MalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pod-3.json"), []byte("not json"), 0o644); err != nil {
		t.Fatalf("writing malformed hint file: %v", err)
	}

	if _, err := ReadHint(dir, "pod-3", 10*time.Millisecond, time.Second); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestValidateHints(t *testing.T) {
	tests := []struct {
		name    string
		hints   []Hint
		wantErr bool
	}{
		{name: "valid", hints: []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 137}}},
		{name: "custom name on long host link", hints: []Hint{{Device: "cxi4", HSNIface: "enp197s0f1np0", IfName: "cxi4", VNI: 137}}},
		{name: "maximum base name", hints: []Hint{{Device: "cxi0", HSNIface: "1234567890", VNI: 137}}},
		{name: "maximum custom name", hints: []Hint{{Device: "cxi0", HSNIface: "hsn0", IfName: "1234567890", VNI: 137}}},
		{name: "long legacy name", hints: []Hint{{Device: "cxi0", HSNIface: "12345678901", VNI: 137}}, wantErr: true},
		{name: "long custom name", hints: []Hint{{Device: "cxi0", HSNIface: "hsn0", IfName: "12345678901", VNI: 137}}, wantErr: true},
		{name: "negative schema", hints: []Hint{{SchemaVersion: -1, Device: "cxi0", HSNIface: "hsn0", VNI: 137}}, wantErr: true},
		{name: "newer schema", hints: []Hint{{SchemaVersion: supportedHintSchemaVersion + 1, Device: "cxi0", HSNIface: "hsn0", VNI: 137}}, wantErr: true},
		{name: "empty", hints: nil, wantErr: true},
		{name: "missing device", hints: []Hint{{HSNIface: "hsn2", VNI: 137}}, wantErr: true},
		{name: "missing hsn_iface", hints: []Hint{{Device: "cxi0", VNI: 137}}, wantErr: true},
		{name: "invalid vni", hints: []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 0}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateHints(tt.hints)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateHints(%+v) error = %v, wantErr %v", tt.hints, err, tt.wantErr)
			}
		})
	}
}
