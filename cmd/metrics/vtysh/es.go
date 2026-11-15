// SPDX-License-Identifier:Apache-2.0

package vtysh

import (
	"github.com/metallb/frr-k8s/internal/frr"
)

func GetEVPNES(frrCli Cli) ([]frr.EVPNES, error) {
	res, err := frrCli("show evpn es detail json")
	if err != nil {
		return nil, err
	}

	segments, err := frr.ParseEVPNES(res)
	if err != nil {
		return nil, err
	}
	return segments, nil
}
