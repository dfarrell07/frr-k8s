// SPDX-License-Identifier:Apache-2.0

package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestExistingMetricContract(t *testing.T) {
	// Keep names, types, HELP and labels stable. Distinct counter values in copies
	// of real captures also catch swapped JSON-to-metric mappings.
	contract := []struct {
		name, help, key, kind string
		value                 int
	}{
		{"bgp_announced_prefixes_total", "Number of prefixes currently being advertised on the BGP session", "", "gauge", 4},
		{"bgp_keepalives_received", "Number of BGP keepalive messages received", "keepalivesRecv", "counter", 11},
		{"bgp_keepalives_sent", "Number of BGP keepalive messages sent", "keepalivesSent", "counter", 12},
		{"bgp_notifications_sent", "Number of BGP notification messages sent", "notificationsSent", "counter", 13},
		{"bgp_opens_received", "Number of BGP open messages received", "opensRecv", "counter", 14},
		{"bgp_opens_sent", "Number of BGP open messages sent", "opensSent", "counter", 15},
		{"bgp_received_prefixes_total", "Number of prefixes currently being received on the BGP session", "", "gauge", 2},
		{"bgp_route_refresh_sent", "Number of BGP route refresh messages sent", "routeRefreshSent", "counter", 17},
		{"bgp_session_up", "BGP session state (1 is up, 0 is down)", "", "gauge", 1},
		{"bgp_total_received", "Number of total BGP messages received", "totalRecv", "counter", 19},
		{"bgp_total_sent", "Number of total BGP messages sent", "totalSent", "counter", 20},
		{"bgp_updates_total", "Number of BGP UPDATE messages sent", "updatesSent", "counter", 21},
		{"bgp_updates_total_received", "Number of BGP UPDATE messages received", "updatesRecv", "counter", 22},
		{"bfd_session_up", "BFD session state (1 is up, 0 is down)", "", "gauge", 1},
		{"bfd_control_packet_input", "Number of received BFD control packets", "control-packet-input", "counter", 24},
		{"bfd_control_packet_output", "Number of sent BFD control packets", "control-packet-output", "counter", 25},
		{"bfd_echo_packet_input", "Number of received BFD echo packets", "echo-packet-input", "counter", 26},
		{"bfd_echo_packet_output", "Number of sent BFD echo packets", "echo-packet-output", "counter", 27},
		{"bfd_session_down_events", "Number of BFD session down events", "session-down", "counter", 28},
		{"bfd_session_up_events", "Number of BFD session up events", "session-up", "counter", 29},
		{"bfd_zebra_notifications", "Number of BFD zebra notifications", "zebra-notifications", "counter", 30},
	}
	bgpRaw, err := os.ReadFile("testdata/bgp_neighbors.json")
	if err != nil {
		t.Fatal(err)
	}
	var bgp map[string]map[string]any
	if err := json.Unmarshal(bgpRaw, &bgp); err != nil {
		t.Fatal(err)
	}
	bfdRaw, err := os.ReadFile("testdata/bfd_counters.json")
	if err != nil {
		t.Fatal(err)
	}
	var bfd []map[string]any
	if err := json.Unmarshal(bfdRaw, &bfd); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	var names []string
	for _, m := range contract {
		labels := `peer="10.78.0.3",vrf="default"`
		counters := bgp["default"]["10.78.0.3"].(map[string]any)["messageStats"].(map[string]any)
		if strings.HasPrefix(m.name, "bfd_") {
			labels = `peer="10.78.0.3",vrf="default"`
			counters = bfd[0]
		}
		if m.key != "" {
			counters[m.key] = m.value
		}
		name := "frrk8s_" + m.name
		names = append(names, name)
		fmt.Fprintf(&want, "# HELP %s %s\n# TYPE %s %s\n%s{%s} %d\n", name, m.help, name, m.kind, name, labels, m.value)
	}
	bgpMetrics, err := collectBGP(context.Background(), fake(map[string]string{bgpCmd: encodeJSON(t, bgp)}))
	if err != nil {
		t.Fatal(err)
	}
	bfdMetrics, err := collectBFD(context.Background(), fake(map[string]string{peersCmd: "@bfd_peers.json", countersCmd: encodeJSON(t, bfd)}))
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.CollectAndCompare(Snapshot(append(bgpMetrics, bfdMetrics...)), &want, names...); err != nil {
		t.Fatal(err)
	}
}
