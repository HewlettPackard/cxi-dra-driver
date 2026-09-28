package cni

import (
	"encoding/json"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
)

func testCmdArgs(t *testing.T, hintsDir, podUID string) *skel.CmdArgs {
	t.Helper()
	stdin, err := json.Marshal(map[string]any{
		"cniVersion":    "1.0.0",
		"name":          "slingshot-hsn",
		"type":          "slingshot-cni",
		"hintsDir":      hintsDir,
		"retryInterval": "5ms",
		"retryTimeout":  "200ms",
	})
	if err != nil {
		t.Fatalf("marshal stdin config: %v", err)
	}
	return &skel.CmdArgs{
		ContainerID: "container-1",
		Netns:       "/proc/1/ns/net",
		IfName:      "net1",
		Args:        "K8S_POD_UID=" + podUID,
		StdinData:   stdin,
	}
}

func TestCmdAdd_Success(t *testing.T) {
	dir := t.TempDir()
	if err := writeHintFile(dir, "pod-add", []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 137}}); err != nil {
		t.Fatalf("writeHintFile: %v", err)
	}

	if err := CmdAdd(testCmdArgs(t, dir, "pod-add")); err != nil {
		t.Fatalf("CmdAdd: %v", err)
	}
}

func TestCmdDel_Success(t *testing.T) {
	dir := t.TempDir()
	if err := writeHintFile(dir, "pod-del", []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 137}}); err != nil {
		t.Fatalf("writeHintFile: %v", err)
	}

	if err := CmdDel(testCmdArgs(t, dir, "pod-del")); err != nil {
		t.Fatalf("CmdDel: %v", err)
	}
}

func TestCmdCheck_Success(t *testing.T) {
	dir := t.TempDir()
	if err := writeHintFile(dir, "pod-check", []Hint{{Device: "cxi0", HSNIface: "hsn2", VNI: 137}}); err != nil {
		t.Fatalf("writeHintFile: %v", err)
	}

	if err := CmdCheck(testCmdArgs(t, dir, "pod-check")); err != nil {
		t.Fatalf("CmdCheck: %v", err)
	}
}

func TestCmdAdd_MissingHintFile(t *testing.T) {
	dir := t.TempDir()
	if err := CmdAdd(testCmdArgs(t, dir, "no-such-pod")); err == nil {
		t.Fatal("expected error for missing hint file, got nil")
	}
}

func TestCmdAdd_MissingPodUID(t *testing.T) {
	dir := t.TempDir()
	args := testCmdArgs(t, dir, "unused")
	args.Args = "IgnoreUnknown=1"

	if err := CmdAdd(args); err == nil {
		t.Fatal("expected error for missing K8S_POD_UID, got nil")
	}
}

func TestCmdDel_InvalidHint(t *testing.T) {
	dir := t.TempDir()
	if err := writeHintFile(dir, "pod-invalid", []Hint{{Device: "cxi0"}}); err != nil {
		t.Fatalf("writeHintFile: %v", err)
	}

	if err := CmdDel(testCmdArgs(t, dir, "pod-invalid")); err == nil {
		t.Fatal("expected validation error for incomplete hint, got nil")
	}
}
