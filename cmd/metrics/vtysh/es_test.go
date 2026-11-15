// SPDX-License-Identifier:Apache-2.0

package vtysh

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/metallb/frr-k8s/internal/frr"
)

func TestGetEVPNES(t *testing.T) {
	cliErr := errors.New("zebra unavailable")
	df := true
	for _, tt := range []struct {
		name, out string
		err       error
		expected  []frr.EVPNES
		wantError bool
	}{
		{
			name:     "Ethernet segments",
			out:      `[{"esi":"03:00:00:00:00:01:00:00:00:01","flags":["local","df","readyForBgp","operUp"]}]`,
			expected: []frr.EVPNES{{ESI: "03:00:00:00:00:01:00:00:00:01", OperUp: true, DF: &df}},
		},
		{name: "command failed", out: "[]", err: cliErr, wantError: true},
		{name: "parse failed", out: "[", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			segments, err := GetEVPNES(func(command string) (string, error) {
				calls++
				if command != "show evpn es detail json" {
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
			if !cmp.Equal(tt.expected, segments) {
				t.Fatal("unexpected EVPN Ethernet segments (-want +got)\n", cmp.Diff(tt.expected, segments))
			}
		})
	}
}
