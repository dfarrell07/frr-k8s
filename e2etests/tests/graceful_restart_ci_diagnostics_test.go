// SPDX-License-Identifier:Apache-2.0

package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGRCIZebraSync(t *testing.T) {
	// Full show zebra client output from the controlled fix-v6 observation,
	// including its active bgp, stale-client and bgp [1] notification sections.
	data, err := os.ReadFile("testdata/gr-zebra-client-done.txt")
	if err != nil {
		t.Fatal(err)
	}
	complete := string(data)
	ipv4Only := strings.ReplaceAll(complete, "AFI 2 enabled, route sync DONE\n", "")
	ipv4Only = strings.ReplaceAll(ipv4Only, "AFI 3 enabled, route sync DONE\n", "")
	for _, tc := range []struct {
		name, output string
		afi          int
		wantErr      bool
	}{
		{"complete IPv6", complete, 2, false},
		{"complete IPv4", complete, 1, false},
		{"AFI1 only cannot qualify IPv6", ipv4Only, 2, true},
		{"AFI1 only qualifies IPv4", ipv4Only, 1, false},
		{"expected AFI not done", strings.ReplaceAll(complete, "AFI 2 enabled, route sync DONE", "AFI 2 enabled, route sync NOT DONE"), 2, true},
		{"other enabled AFI not done", strings.ReplaceAll(complete, "AFI 3 enabled, route sync DONE", "AFI 3 enabled, route sync NOT DONE"), 2, true},
		{"missing default VRF", strings.ReplaceAll(complete, "VRF : default", "VRF : tenant"), 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkGRCIZebraSync(tc.output, tc.afi); (err != nil) != tc.wantErr {
				t.Fatalf("checkGRCIZebraSync(afi=%d) = %v, wantErr=%v", tc.afi, err, tc.wantErr)
			}
		})
	}
}

func TestGRCIRestartCoverage(t *testing.T) {
	started := time.Now()
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		wantErr bool
	}{
		{"within window", 134 * time.Second, false},
		{"at earliest boundary", grObserverDuration, false},
		{"restart after observers ended", 150 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &grCIObservers{dir: t.TempDir(), observers: []*grObserver{
				{started: started.Add(time.Second)}, {started: started},
			}}
			if err := group.checkRestartCoverage(started.Add(tc.elapsed)); (err != nil) != tc.wantErr {
				t.Fatalf("checkRestartCoverage(%s) = %v, wantErr=%v", tc.elapsed, err, tc.wantErr)
			}
			data, err := os.ReadFile(filepath.Join(group.dir, "restart-observer-coverage.json"))
			if err != nil {
				t.Fatal(err)
			}
			var status struct {
				Within bool `json:"within_window"`
			}
			if err := json.Unmarshal(data, &status); err != nil || status.Within == tc.wantErr {
				t.Fatalf("coverage artifact = %s, error=%v", data, err)
			}
		})
	}
}
