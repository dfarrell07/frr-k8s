// SPDX-License-Identifier:Apache-2.0

package collector

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Synthetic show evpn es detail json in the shape of FRR 10.6.2's
// zebra_evpn_es_show_entry_detail: a local DF segment, a local non-DF segment
// and a segment known only from a remote VTEP.
const esCapture = `[
{"esi":"03:00:00:00:00:01:00:00:00:01","accessPort":"bond1","flags":["local","remote","df","readyForBgp","bridgePort","operUp","nexthopGroupActive"],"vniCount":2,"macCount":3,"dfPreference":50000,"nexthopGroup":536870913,"vteps":[{"vtep":"192.0.2.2","dfAlgorithm":"preference","dfPreference":32767,"nexthopId":268435458}]},
{"esi":"03:00:00:00:00:01:00:00:00:02","accessPort":"bond2","flags":["local","remote","nonDF","readyForBgp","bridgePort","operUp","nexthopGroupActive"],"vniCount":2,"macCount":0,"dfPreference":32767,"nexthopGroup":536870914,"vteps":[{"vtep":"192.0.2.2","dfAlgorithm":"preference","dfPreference":50000,"nexthopId":268435458}]},
{"esi":"03:00:00:00:00:02:00:00:00:01","flags":["remote"],"vniCount":0,"macCount":1,"dfPreference":0,"nexthopGroup":536870915,"vteps":[{"vtep":"192.0.2.3","dfAlgorithm":"preference","dfPreference":32767,"nexthopId":268435459}]}
]
`

const (
	esDFHeader = `# HELP frrk8s_evpn_es_df 1 if this VTEP is the designated forwarder for the local Ethernet segment, 0 if not; omitted while no election result applies
# TYPE frrk8s_evpn_es_df gauge
`
	esOperUpHeader = `# HELP frrk8s_evpn_es_oper_up Local Ethernet segment's access interface state (1 is up, 0 is down)
# TYPE frrk8s_evpn_es_oper_up gauge
`
	esFirst  = `"esi":"03:00:00:00:00:01:00:00:00:01"`
	esSecond = `"esi":"03:00:00:00:00:01:00:00:00:02"`
)

func TestES(t *testing.T) {
	checkES(t, esCapture, nil, esDFHeader+`frrk8s_evpn_es_df{esi="03:00:00:00:00:01:00:00:00:01"} 1
frrk8s_evpn_es_df{esi="03:00:00:00:00:01:00:00:00:02"} 0
`+esOperUpHeader+`frrk8s_evpn_es_oper_up{esi="03:00:00:00:00:01:00:00:00:01"} 1
frrk8s_evpn_es_oper_up{esi="03:00:00:00:00:01:00:00:00:02"} 1
`+scrapeSuccessText("es", 1))
}

// Zebra skips the DF election, so its df or nonDF flag means nothing, for a
// segment that is down, not ready for BGP, in LACP bypass or in its DF delay.
func TestESRoleOmitted(t *testing.T) {
	for _, tt := range []struct {
		name, flags, delay, df string
		operUp                 int
	}{
		{name: "elected", flags: `"local","df","readyForBgp","bridgePort","operUp"`, operUp: 1, df: "1"},
		{name: "oper down", flags: `"local","nonDF","readyForBgp","bridgePort"`},
		{name: "not ready for BGP", flags: `"local","df","bridgePort","operUp"`, operUp: 1},
		{name: "LACP bypass", flags: `"local","df","bypass","readyForBgp","bridgePort","operUp"`, operUp: 1},
		{name: "DF delay", flags: `"local","nonDF","readyForBgp","bridgePort","operUp"`, delay: `"dfDelayTimer":"00:00:02",`, operUp: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := `[{"esi":"03:00:00:00:00:01:00:00:00:01","accessPort":"bond1","flags":[` + tt.flags +
				`],"vniCount":2,"macCount":3,"dfPreference":50000,` + tt.delay + `"nexthopGroup":536870913}]`
			expected := fmt.Sprintf("%sfrrk8s_evpn_es_oper_up{esi=\"03:00:00:00:00:01:00:00:00:01\"} %d\n", esOperUpHeader, tt.operUp)
			if tt.df != "" {
				expected = esDFHeader + `frrk8s_evpn_es_df{esi="03:00:00:00:00:01:00:00:00:01"} ` + tt.df + "\n" + expected
			}
			checkES(t, out, nil, expected+scrapeSuccessText("es", 1))
		})
	}
}

func TestESEmptyAndFailures(t *testing.T) {
	for _, tt := range []struct {
		name, out string
		err       error
		success   int
	}{
		{name: "empty", out: "[]", success: 1},
		{name: "remote only", out: `[{"esi":"03:00:00:00:00:02:00:00:00:01","flags":["remote"]}]`, success: 1},
		{name: "null", out: "null"},
		{name: "malformed", out: "["},
		{name: "wrong shape", out: "{}"},
		{name: "command failed", out: esCapture, err: errors.New("zebra unavailable")},
		{name: "missing ESI", out: strings.Replace(esCapture, esFirst+",", "", 1)},
		{name: "invalid ESI", out: strings.Replace(esCapture, "00:01:00:00:00:01", "00:01:00:00:00:0g", 1)},
		{name: "short ESI", out: strings.Replace(esCapture, `"03:00:00:00:00:01:00:00:00:01"`, `"03:00:00:00:00:01:00:00:00"`, 1)},
		{name: "ESI with suffix", out: strings.Replace(esCapture, `"03:00:00:00:00:01:00:00:00:01"`, `"03:00:00:00:00:01:00:00:00:01:ff"`, 1)},
		{name: "duplicate ESI", out: strings.Replace(esCapture, esSecond, esFirst, 1)},
		{name: "no DF role", out: strings.Replace(esCapture, `"nonDF",`, "", 1)},
		{name: "both DF roles", out: strings.Replace(esCapture, `"df",`, `"df","nonDF",`, 1)},
		{name: "invalid flags", out: strings.Replace(esCapture, `"flags":["remote"]`, `"flags":"remote"`, 1)},
		{name: "invalid DF delay", out: strings.Replace(esCapture, `"dfPreference":50000,`, `"dfPreference":50000,"dfDelayTimer":3,`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.out == esCapture && tt.err == nil {
				t.Fatal("replacement did not apply")
			}
			checkES(t, tt.out, tt.err, scrapeSuccessText("es", tt.success))
		})
	}
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
