// SPDX-License-Identifier:Apache-2.0

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSmokeOnlyCleansResourcesItOwns(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go":      "exit 0",
		"openssl": "exit 0",
		"podman": `printf '%s\n' "$*" >> "$SMOKE_TEST_LOG"
if [ "$1" = network ] && [ "$2" = create ] && [ "$SMOKE_TEST_FAIL_AT" = network ]; then exit 1; fi
if [ "$1" = run ] || [ "$1" = rm ]; then exit 1; fi`,
	} {
		path := filepath.Join(dir, name)
		write(t, path, []byte("#!/bin/sh\n"+body+"\n"))
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var previous string
	for _, failAt := range []string{"network", "network", "run"} {
		log := filepath.Join(t.TempDir(), "podman.log")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, "bash", "../../hack/metrics-smoke.sh")
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "SMOKE_TEST_LOG="+log, "SMOKE_TEST_FAIL_AT="+failAt, "METRICS_BINARY=", "METRICS_IMAGE_BINARY=")
		out, err := cmd.CombinedOutput()
		cancel()
		if err == nil || ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("smoke: error %v, output %s", err, out)
		}
		calls, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
		if !strings.HasPrefix(lines[0], "network create ") {
			t.Fatalf("network creation not attempted: %s", calls)
		}
		if failAt == "network" && len(lines) != 1 {
			t.Fatalf("failed network creation attempted cleanup of unowned resources: %s", calls)
		}
		fields := strings.Fields(lines[0])
		name := fields[len(fields)-1]
		if !strings.HasPrefix(name, "egasmoke-") || name == previous {
			t.Fatalf("smoke reused a resource name: %q", name)
		}
		previous = name
		if failAt == "run" && (len(lines) != 4 || !strings.HasPrefix(lines[1], "run ") || lines[2] != "rm -f "+name+"-r1 "+name+"-r2" || lines[3] != "network rm -f "+name) {
			t.Fatalf("partial startup did not finish owned-resource cleanup: %s", calls)
		}
	}
}
