// SPDX-License-Identifier:Apache-2.0

package vtysh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func cliScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vtysh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunSelectsOwningDaemon(t *testing.T) {
	path := cliScript(t, `printf '%s\n' "$@"`)
	for _, tc := range []struct {
		command string
		daemon  string
	}{
		{"show bgp vrf all json", "bgpd"},
		{"show bgp vrf blue neighbors json", "bgpd"},
		{"show bfd vrf default peers json", "bfdd"},
		{"show bfd vrf blue peers counters json", "bfdd"},
		{"show daemons", ""},
	} {
		t.Run(tc.command, func(t *testing.T) {
			want := "-c\n" + tc.command + "\n"
			if tc.daemon != "" {
				want = "-d\n" + tc.daemon + "\n" + want
			}
			out, err := run(t.Context(), path, tc.command)
			if err != nil || out != want {
				t.Fatalf("output %q, error %v; want %q", out, err, want)
			}
		})
	}
}

func TestRunKeepsDiagnosticsOutOfJSON(t *testing.T) {
	path := cliScript(t, `printf '{"peers": []}\n'; printf 'warning\n' >&2`)
	out, err := run(t.Context(), path, "show bgp vrf all json")
	if err != nil || out != "{\"peers\": []}\n" {
		t.Fatalf("output %q, error %v", out, err)
	}
}

func TestRunReportsCommandFailure(t *testing.T) {
	path := cliScript(t, `printf 'failed to read daemon\n' >&2; exit 7`)
	_, err := run(t.Context(), path, "show bfd vrf default peers json")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 || !strings.Contains(err.Error(), "failed to read daemon") {
		t.Fatalf("missing exit status or diagnostic: %v", err)
	}
}

func TestRunDeadlineKillsAndReapsCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	path := cliScript(t, fmt.Sprintf("echo $$ > %q\nexec sleep 2", pidFile))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := run(ctx, path, "show bgp vrf all json")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscan(string(data), &pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("command %d remains after cancellation: %v", pid, err)
	}
}
