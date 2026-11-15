// SPDX-License-Identifier:Apache-2.0

package collector

import (
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Synthetic show evpn es detail json in the shape of FRR 10.6.2's
// zebra_evpn_es_show_entry_detail: a local DF segment, a segment known only from
// a remote VTEP, a local non-DF segment and a local segment that is down.
const esCapture = `[
{"esi":"03:00:00:00:00:01:00:00:00:01","accessPort":"bond1","flags":["local","remote","df","readyForBgp","bridgePort","operUp","nexthopGroupActive"]},
{"esi":"03:00:00:00:00:02:00:00:00:01","flags":["remote"]},
{"esi":"03:00:00:00:00:01:00:00:00:02","accessPort":"bond2","flags":["local","remote","nonDF","readyForBgp","bridgePort","operUp","nexthopGroupActive"]},
{"esi":"03:aa:bb:cc:dd:ee:00:00:00:03","accessPort":"bond3","flags":["local","nonDF","readyForBgp","bridgePort"]}
]
`

const (
	esDFHeader = `# HELP frrk8s_evpn_es_df 1 if this VTEP is the designated forwarder for the local Ethernet segment, 0 if not; omitted while no election result applies
# TYPE frrk8s_evpn_es_df gauge
`
	esOperUpHeader = `# HELP frrk8s_evpn_es_oper_up Local Ethernet segment's access interface state (1 is up, 0 is down)
# TYPE frrk8s_evpn_es_oper_up gauge
`
)

func TestES(t *testing.T) {
	checkES(t, esCapture, nil, esDFHeader+`frrk8s_evpn_es_df{esi="03:00:00:00:00:01:00:00:00:01"} 1
frrk8s_evpn_es_df{esi="03:00:00:00:00:01:00:00:00:02"} 0
`+esOperUpHeader+`frrk8s_evpn_es_oper_up{esi="03:00:00:00:00:01:00:00:00:01"} 1
frrk8s_evpn_es_oper_up{esi="03:00:00:00:00:01:00:00:00:02"} 1
frrk8s_evpn_es_oper_up{esi="03:aa:bb:cc:dd:ee:00:00:00:03"} 0
`+scrapeSuccessText("es", 1))
}

func TestESNotReadyForBGP(t *testing.T) {
	out := `[{"esi":"03:00:00:00:00:01:00:00:00:01","accessPort":"bond1","flags":["local","df","bridgePort","operUp"]}]`
	checkES(t, out, nil, esOperUpHeader+`frrk8s_evpn_es_oper_up{esi="03:00:00:00:00:01:00:00:00:01"} 1
`+scrapeSuccessText("es", 1))
}

func TestESFailure(t *testing.T) {
	out := strings.Replace(esCapture, `"nonDF",`, "", 1)
	checkES(t, out, nil, scrapeSuccessText("es", 0))
}

func checkES(t *testing.T, out string, cliErr error, expected string) {
	t.Helper()
	calls := 0
	c := &es{Log: log.NewNopLogger(), frrCli: func(command string) (string, error) {
		calls++
		if command != "show evpn es detail json" {
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
