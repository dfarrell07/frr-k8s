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

const commandTimeout = 8 * time.Second

// Run bounds each CLI call, including calls made by the status exporter.
func Run(args string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	return run(ctx, "/usr/bin/vtysh", args)
}

func run(ctx context.Context, path, command string) (string, error) {
	args := []string{"-c", command}
	// An unrelated stalled daemon must not block BGP or BFD reads. Keep
	// all-daemon commands such as "show daemons" unqualified.
	switch {
	case strings.HasPrefix(command, "show bgp "):
		args = append([]string{"-d", "bgpd"}, args...)
	case strings.HasPrefix(command, "show bfd "):
		args = append([]string{"-d", "bfdd"}, args...)
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
		return string(out), fmt.Errorf("vtysh %q: %w: %s", command, err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
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
