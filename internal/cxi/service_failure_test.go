package cxi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Compile the production C routine against fake libcxi calls so failures can
// be forced without hardware, libcxi headers, or production fault-injection hooks.
func TestServiceCreationFailureCleanup(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler unavailable; production C failure-path tests require cc")
	}
	source, err := os.ReadFile("service_cgo.go")
	if err != nil {
		t.Fatal(err)
	}
	extract := func(start, end string) string {
		t.Helper()
		_, rest, found := strings.Cut(string(source), start)
		if !found {
			t.Fatalf("production source marker missing: %q", start)
		}
		body, _, found := strings.Cut(rest, end)
		if !found {
			t.Fatalf("production source end marker missing: %q", end)
		}
		return start + body
	}
	types := extract("typedef struct {\n    int64_t txqs;", "static int device_index")
	create := extract("static int cxi_create_service(", "// cxi_destroy_service destroys")
	dir := t.TempDir()
	header := filepath.Join(dir, "production_create.h")
	if err := os.WriteFile(header, []byte(types+create), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "service-failure-test")
	build := exec.Command(cc, "-std=c99", "-Wall", "-Wextra", "-Werror", "-I", dir,
		"testdata/service_failure.c", "-o", binary)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile failure harness: %v\n%s", err, output)
	}
	for _, stage := range []string{"success", "open device", "allocate", "disable", "set netns", "enable"} {
		for _, cleanupFails := range []bool{false, true} {
			if cleanupFails && (stage == "success" || stage == "open device" || stage == "allocate") {
				continue
			}
			name := stage
			if cleanupFails {
				name += "/destroy failure"
			}
			t.Run(name, func(t *testing.T) {
				cleanup := "ok"
				if cleanupFails {
					cleanup = "fail"
				}
				for _, mode := range []string{"pf", "vf"} {
					output, err := exec.Command(binary, stage, cleanup, mode).CombinedOutput()
					if err != nil {
						t.Fatalf("%s: %v\n%s", mode, err, output)
					}
					if cleanupFails && (!strings.Contains(string(output), "service may be orphaned") ||
						!strings.Contains(string(output), "cxil_destroy_svc(7)")) {
						t.Fatalf("%s: missing cleanup-failure warning:\n%s", mode, output)
					}
				}
			})
		}
	}
}
