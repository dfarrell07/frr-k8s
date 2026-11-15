// SPDX-License-Identifier:Apache-2.0

package collector

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/metallb/frr-k8s/cmd/metrics/vtysh"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	esOperUpDesc  = evpnDesc("es_oper_up", "Local Ethernet segment's access interface state (1 is up, 0 is down)", "esi")
	esDFDesc      = evpnDesc("es_df", "1 if this VTEP is the designated forwarder for the local Ethernet segment, 0 if not; omitted while no election result applies", "esi")
	esSuccessDesc = scrapeSuccessDesc("es")

	// FRR's esi_to_str format.
	esiPattern = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){9}$`)
)

type es struct {
	Log    log.Logger
	frrCli vtysh.Cli
}

func NewES(l log.Logger) prometheus.Collector {
	return &es{Log: log.With(l, "collector", "es"), frrCli: vtysh.Run}
}

func (c *es) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{esOperUpDesc, esDFDesc, esSuccessDesc} {
		ch <- d
	}
}

func (c *es) Collect(ch chan<- prometheus.Metric) {
	out, err := c.frrCli("show evpn es detail json")
	var ms []prometheus.Metric
	if err == nil {
		ms, err = parseESMetrics(out)
	}
	success := 1.0
	if err != nil {
		success = 0
		level.Error(c.Log).Log("error", err, "msg", "failed to collect EVPN Ethernet segments from FRR")
	}
	for _, m := range ms {
		ch <- m
	}
	ch <- prometheus.MustNewConstMetric(esSuccessDesc, prometheus.GaugeValue, success)
}

type evpnES struct {
	ESI          string   `json:"esi"`
	Flags        []string `json:"flags"`
	DFDelayTimer *string  `json:"dfDelayTimer"`
}

// parseESMetrics reports zebra's local Ethernet segments; segments known only
// from remote VTEPs are skipped. Zebra's df or nonDF flag is not an election
// result while a segment is not ready for BGP, in LACP bypass, down or in its
// initial DF delay, so the role is exported only outside those states. An
// empty array is valid.
func parseESMetrics(out string) ([]prometheus.Metric, error) {
	var segments []evpnES
	if err := json.Unmarshal([]byte(out), &segments); err != nil {
		return nil, fmt.Errorf("parsing EVPN Ethernet segments: %w", err)
	}
	if segments == nil {
		return nil, fmt.Errorf("EVPN Ethernet segments must be an array, not null")
	}
	var ms []prometheus.Metric
	seen := make(map[string]bool)
	for _, s := range segments {
		if !esiPattern.MatchString(s.ESI) || seen[s.ESI] {
			return nil, fmt.Errorf("invalid or duplicate ESI %q", s.ESI)
		}
		seen[s.ESI] = true
		if !slices.Contains(s.Flags, "local") {
			continue
		}
		df, nonDF := slices.Contains(s.Flags, "df"), slices.Contains(s.Flags, "nonDF")
		if df == nonDF {
			return nil, fmt.Errorf("local Ethernet segment %s must be exactly one of df or nonDF", s.ESI)
		}
		operUp := slices.Contains(s.Flags, "operUp")
		ms = append(ms, prometheus.MustNewConstMetric(esOperUpDesc, prometheus.GaugeValue, boolToFloat(operUp), s.ESI))
		if operUp && slices.Contains(s.Flags, "readyForBgp") && !slices.Contains(s.Flags, "bypass") && s.DFDelayTimer == nil {
			ms = append(ms, prometheus.MustNewConstMetric(esDFDesc, prometheus.GaugeValue, boolToFloat(df), s.ESI))
		}
	}
	return ms, nil
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
