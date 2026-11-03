// SPDX-License-Identifier:Apache-2.0

package vtysh

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/metallb/frr-k8s/internal/frr"
)

type Cli func(args string) (string, error)

var vtyshPath = "/usr/bin/vtysh"

// Keep below Prometheus's default 10s scrape_timeout so one stuck read fails its
// collector instead of stalling the scrape. Several sequential reads can still
// exceed the scrape timeout (see cmd/metrics/README.md).
var commandTimeout = 8 * time.Second

// Run bounds each CLI call, including calls made by the status exporter.
func Run(args string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	return run(ctx, vtyshPath, args)
}

func run(ctx context.Context, path, command string) (string, error) {
	args := []string{"-c", command}
	// An unrelated stalled daemon must not block BGP, BFD or EVPN reads.
	// Keep all-daemon commands such as "show daemons" unqualified.
	switch {
	case strings.HasPrefix(command, "show bgp "):
		args = append([]string{"-d", "bgpd"}, args...)
	case strings.HasPrefix(command, "show bfd "):
		args = append([]string{"-d", "bfdd"}, args...)
	case strings.HasPrefix(command, "show evpn "):
		args = append([]string{"-d", "zebra"}, args...)
	}
	cmd := exec.CommandContext(ctx, path, args...) //#nosec G204 -- production path and commands are fixed by the callers
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return string(out), fmt.Errorf("vtysh %q: %w%s", command, err, diagnostic(stderr.String(), string(out)))
	}
	return string(out), nil
}

// diagnostic returns the message for a failed vtysh command. Many vtysh
// errors, such as "% Unknown command", go to stdout rather than stderr.
func diagnostic(stderr, stdout string) string {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = strings.TrimSpace(stdout)
	}
	if msg == "" {
		return ""
	}
	const limit = 256
	if len(msg) > limit {
		msg = msg[:limit] + "..."
	}
	return ": " + msg
}

var _ Cli = Run

func VRFs(frrCli Cli) ([]string, error) {
	vrfs, err := frrCli("show bgp vrf all json")
	if err != nil {
		return nil, err
	}
	parsedVRFs, err := frr.ParseVRFs(vrfs)
	if err != nil {
		return nil, err
	}
	return parsedVRFs, nil
}
