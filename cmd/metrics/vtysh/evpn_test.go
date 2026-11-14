// SPDX-License-Identifier:Apache-2.0

package vtysh

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/metallb/frr-k8s/internal/frr"
)

func TestGetEVPNVNIs(t *testing.T) {
	cliErr := errors.New("zebra unavailable")
	for _, tt := range []struct {
		name, out string
		err       error
		expected  []frr.EVPNVNI
		wantError bool
	}{
		{
			name:     "VNIs",
			out:      `{"100":{"vni":100,"type":"L3","vxlanIf":"vxlan100","tenantVrf":"red","numRemoteVteps":"n/a"}}`,
			expected: []frr.EVPNVNI{{VNI: 100, Type: "L3", Interface: "vxlan100", VRF: "red"}},
		},
		{name: "command failed", out: "{}", err: cliErr, wantError: true},
		{name: "parse failed", out: "{", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			vnis, err := GetEVPNVNIs(func(command string) (string, error) {
				calls++
				if command != "show evpn vni json" {
					t.Errorf("unexpected command %q", command)
				}
				return tt.out, tt.err
			})
			if calls != 1 {
				t.Errorf("got %d CLI calls, want 1", calls)
			}
			if (err != nil) != tt.wantError {
				t.Fatalf("error %v, want error %t", err, tt.wantError)
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Fatalf("expected CLI error %v, got %v", tt.err, err)
			}
			if !cmp.Equal(tt.expected, vnis) {
				t.Fatal("unexpected EVPN VNIs (-want +got)\n", cmp.Diff(tt.expected, vnis))
			}
		})
	}
}
