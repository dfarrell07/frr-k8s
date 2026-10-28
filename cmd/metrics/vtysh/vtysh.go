// SPDX-License-Identifier:Apache-2.0

package vtysh

import (
	"bytes"
	"fmt"
	"os/exec"

	"github.com/metallb/frr-k8s/internal/frr"
)

type Cli func(args string) (string, error)

// Run returns the stdout of a vtysh command. The vtysh binary can warn on
// stderr and still succeed, so stderr appears only in the error on failure,
// falling back to stdout, where vtysh reports errors such as unknown commands.
func Run(args string) (string, error) {
	return run("/usr/bin/vtysh", args)
}

func run(path, args string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(path, "-c", args) //#nosec G204 -- path is fixed outside tests and args are vtysh commands built in this package
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := bytes.TrimSpace(stderr.Bytes())
		if len(detail) == 0 {
			detail = bytes.TrimSpace(out)
		}
		if len(detail) > 0 {
			err = fmt.Errorf("%w: %s", err, detail)
		}
		err = fmt.Errorf("vtysh -c %q: %w", args, err)
	}
	return string(out), err
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
