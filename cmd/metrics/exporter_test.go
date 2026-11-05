// SPDX-License-Identifier:Apache-2.0

package main

import "testing"

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
