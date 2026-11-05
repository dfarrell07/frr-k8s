// SPDX-License-Identifier:Apache-2.0

package main

import (
	"net"
	"testing"
)

func TestMetricsAddress(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"127.0.0.1", "127.0.0.1:9141"},
		{"0.0.0.0", "0.0.0.0:9141"},
		{"localhost", "localhost:9141"},
		{"", ":9141"},
		{"::1", "[::1]:9141"},
		{"::", "[::]:9141"},
		{"[::1]", "[::1]:9141"},
		{"fe80::1%eth0", "[fe80::1%eth0]:9141"},
		{"[fe80::1%eth0]", "[fe80::1%eth0]:9141"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if got := metricsAddress(tc.host, 9141); got != tc.want {
				t.Fatalf("metricsAddress(%q, 9141) = %q, want %q", tc.host, got, tc.want)
			}
		})
	}
}

func TestMetricsAddressIPv6Listener(t *testing.T) {
	control, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"::1", "[::1]"} {
		t.Run(host, func(t *testing.T) {
			listener, err := net.Listen("tcp6", metricsAddress(host, 0))
			if err != nil {
				t.Fatal(err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
