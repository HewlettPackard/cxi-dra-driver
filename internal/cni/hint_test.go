package cni

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
