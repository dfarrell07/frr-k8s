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

func TestGRCIZebraCompletion(t *testing.T) {
	// Actual focused-CI worker2 output, with trailing whitespace removed, passed
	// the old terminal DONE gate, but
	// rendered 07:43:42 (zero-delay envelope 07:43:41..07:43:44), after the deadline.
	data, err := os.ReadFile("testdata/gr-zebra-client-worker2-timeout.txt")
	if err != nil {
		t.Fatal(err)
	}
	red := string(data)
	if err := checkGRCIZebraSync(red, 2); err != nil {
		t.Fatalf("red fixture must demonstrate the terminal-DONE false positive: %v", err)
	}
	started := time.Date(2026, 11, 3, 7, 41, 30, 817505217, time.UTC)
	const raw = "Tue Nov  3 07:43:42 2026"
	withTime := func(stamp string) string { return strings.ReplaceAll(red, raw, stamp) }
	for _, tc := range []struct {
		name, output string
		start        time.Time
		wantErr      bool
	}{
		{"actual worker2 timeout must fail", red, started, true},
		{"new cycle completes promptly", withTime("Tue Nov  3 07:41:44 2026"), started, false},
		{"whole envelope before fractional deadline", withTime("Tue Nov  3 07:43:28 2026"), started, false},
		{"envelope overlaps fractional deadline", withTime("Tue Nov  3 07:43:29 2026"), started, true},
		{"upper bound equals deadline", withTime("Tue Nov  3 07:43:29 2026"), started.Truncate(time.Second).Add(time.Second), false},
		{"missing completion timestamp", strings.ReplaceAll(red, "Route sync finished at "+raw+"\n", ""), started, true},
		{"malformed completion timestamp", withTime("not an ANSIC timestamp"), started, true},
		{"old prefault timestamp", withTime("Tue Nov  3 07:41:29 2026"), started, true},
		{"lower rounding bound overlaps cycle start", withTime("Tue Nov  3 07:41:31 2026"), started, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			completed, err := checkGRCIZebraCompletion(tc.output, tc.start, 0)
			if (err != nil) != tc.wantErr {
				t.Fatalf("completion=%s error=%v, wantErr=%v", completed, err, tc.wantErr)
			}
		})
	}
}

func TestGRCIZebraIntegerTimeRounding(t *testing.T) {
	// Derive FRR's rendering from independently timed actual events. Both old
	// false positives occur with a stable realtime/monotonic clock offset.
	offset := 1000*time.Second + 800*time.Millisecond
	for _, tc := range []struct {
		name                                   string
		eventMono, monotimeSample, timeSample  time.Duration
		started                                time.Time
		oldLowerAdjustment, oldUpperAdjustment time.Duration
	}{
		{"late event rounded before deadline", 100900 * time.Millisecond, 200100 * time.Millisecond, 200100 * time.Millisecond,
			time.Unix(981, 0).UTC(), 0, time.Second},
		{"prefault event hidden by inter-read pause", 100100 * time.Millisecond, 200300 * time.Millisecond, 202400 * time.Millisecond,
			time.Unix(1101, 500000000).UTC(), -time.Second, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actualEvent := time.Unix(0, int64(offset+tc.eventMono)).UTC()
			storedMonoSeconds := int64(tc.eventMono / time.Second)
			readRealSeconds := int64((offset + tc.timeSample) / time.Second)
			renderedSeconds := readRealSeconds - (int64(tc.monotimeSample/time.Second) - storedMonoSeconds)
			rendered := time.Unix(renderedSeconds, 0).UTC()
			deadline := tc.started.Add(2 * time.Minute)
			if !actualEvent.Before(tc.started) && !actualEvent.After(deadline) {
				t.Fatalf("actual event %s must be outside [%s,%s]", actualEvent, tc.started, deadline)
			}
			if rendered.Add(tc.oldLowerAdjustment).Before(tc.started) || rendered.Add(tc.oldUpperAdjustment).After(deadline) {
				t.Fatalf("counterexample no longer reproduces the old false acceptance: actual=%s rendered=%s", actualEvent, rendered)
			}
			output := "Client: bgp\nVRF : default\nCapabilities : Graceful Restart\n" +
				"AFI 2 enabled, route sync DONE\nRoute sync finished at " + rendered.Format(time.ANSIC) + "\n"
			if err := checkGRCIZebraSync(output, 2); err != nil {
				t.Fatal(err)
			}
			if _, err := checkGRCIZebraCompletion(output, tc.started, tc.timeSample-tc.monotimeSample); err == nil {
				t.Fatalf("outside actual event %s must not qualify from rendered value %s", actualEvent, rendered)
			}
		})
	}
}
