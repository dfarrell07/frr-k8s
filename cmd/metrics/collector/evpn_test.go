// SPDX-License-Identifier:Apache-2.0

package collector

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Adapted from FRR 10.4.3's show evpn vni json; distinct counts catch field swaps.
const evpnCapture = `{
"10100":{"vni":10100,"type":"L2","vxlanIf":"vxlan10100","numMacs":2,"numArpNd":0,"numRemoteVteps":1,"tenantVrf":"default","remoteVteps":["10.78.0.3"]},
"50001":{"vni":50001,"vxlanIf":"vxlan50001","numMacs":0,"numArpNd":0,"numRemoteVteps":"n/a","type":"L3","tenantVrf":"red"}}
`

func TestEVPN(t *testing.T) {
	expected := `# HELP frrk8s_evpn_l2_macs Host MAC addresses zebra holds for the L2 VNI
# TYPE frrk8s_evpn_l2_macs gauge
frrk8s_evpn_l2_macs{vni="10100",vrf="default"} 2
# HELP frrk8s_evpn_l2_neighbors ARP/ND entries zebra holds for the L2 VNI
# TYPE frrk8s_evpn_l2_neighbors gauge
frrk8s_evpn_l2_neighbors{vni="10100",vrf="default"} 0
# HELP frrk8s_evpn_l2_remote_vteps Remote VTEPs known for the L2 VNI
# TYPE frrk8s_evpn_l2_remote_vteps gauge
frrk8s_evpn_l2_remote_vteps{vni="10100",vrf="default"} 1
# HELP frrk8s_evpn_l3_router_macs Router MACs zebra holds for the L3 VNI
# TYPE frrk8s_evpn_l3_router_macs gauge
frrk8s_evpn_l3_router_macs{vni="50001",vrf="red"} 0
# HELP frrk8s_evpn_l3_nexthops Next hops zebra holds for the L3 VNI
# TYPE frrk8s_evpn_l3_nexthops gauge
frrk8s_evpn_l3_nexthops{vni="50001",vrf="red"} 0
# HELP frrk8s_evpn_vni_info EVPN VNI inventory; constant 1
# TYPE frrk8s_evpn_vni_info gauge
frrk8s_evpn_vni_info{interface="vxlan10100",type="L2",vni="10100",vrf="default"} 1
frrk8s_evpn_vni_info{interface="vxlan50001",type="L3",vni="50001",vrf="red"} 1
` + scrapeSuccessText("evpn", 1)
	checkEVPN(t, evpnCapture, nil, expected)
	out := strings.Replace(evpnCapture, `"numMacs":0,"numArpNd":0`, `"numMacs":3,"numArpNd":4`, 1)
	expected = strings.Replace(expected, `frrk8s_evpn_l3_router_macs{vni="50001",vrf="red"} 0`, `frrk8s_evpn_l3_router_macs{vni="50001",vrf="red"} 3`, 1)
	expected = strings.Replace(expected, `frrk8s_evpn_l3_nexthops{vni="50001",vrf="red"} 0`, `frrk8s_evpn_l3_nexthops{vni="50001",vrf="red"} 4`, 1)
	checkEVPN(t, out, nil, expected)
}

func TestEVPNEmptyAndFailures(t *testing.T) {
	for _, tt := range []struct {
		name, out string
		err       error
		success   int
	}{
		{name: "empty", out: "{}", success: 1},
		{name: "command failed", out: evpnCapture, err: errors.New("zebra unavailable")},
		{name: "parse failed", out: strings.Replace(evpnCapture, `"numRemoteVteps":1`, `"numRemoteVteps":"n/a"`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checkEVPN(t, tt.out, tt.err, scrapeSuccessText("evpn", tt.success))
		})
	}
}

func TestEVPNMissingCounts(t *testing.T) {
	for _, vniType := range []string{"L2", "L3"} {
		for _, counts := range []string{"", `,"numMacs":null,"numArpNd":null,"numRemoteVteps":null`} {
			out := `{"100":{"vni":100,"type":"` + vniType + `","vxlanIf":"vxlan100","tenantVrf":"default"` + counts + `}}`
			expected := `# HELP frrk8s_evpn_vni_info EVPN VNI inventory; constant 1
# TYPE frrk8s_evpn_vni_info gauge
frrk8s_evpn_vni_info{interface="vxlan100",type="` + vniType + `",vni="100",vrf="default"} 1
` + scrapeSuccessText("evpn", 1)
			checkEVPN(t, out, nil, expected)
		}
	}
}

func TestEVPNPartialCounts(t *testing.T) {
	out := `{"100":{"vni":100,"type":"L2","vxlanIf":"vxlan100","tenantVrf":"default","numMacs":2,"numArpNd":null,"numRemoteVteps":0}}`
	expected := `# HELP frrk8s_evpn_l2_macs Host MAC addresses zebra holds for the L2 VNI
# TYPE frrk8s_evpn_l2_macs gauge
frrk8s_evpn_l2_macs{vni="100",vrf="default"} 2
# HELP frrk8s_evpn_l2_remote_vteps Remote VTEPs known for the L2 VNI
# TYPE frrk8s_evpn_l2_remote_vteps gauge
frrk8s_evpn_l2_remote_vteps{vni="100",vrf="default"} 0
# HELP frrk8s_evpn_vni_info EVPN VNI inventory; constant 1
# TYPE frrk8s_evpn_vni_info gauge
frrk8s_evpn_vni_info{interface="vxlan100",type="L2",vni="100",vrf="default"} 1
` + scrapeSuccessText("evpn", 1)
	checkEVPN(t, out, nil, expected)
}

func checkEVPN(t *testing.T, out string, cliErr error, expected string) {
	t.Helper()
	calls := 0
	c := &evpn{Log: log.NewNopLogger(), frrCli: func(command string) (string, error) {
		calls++
		if command != "show evpn vni json" {
			t.Errorf("unexpected command %q", command)
		}
		return out, cliErr
	}}
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("got %d CLI calls, want 1", calls)
	}
}
