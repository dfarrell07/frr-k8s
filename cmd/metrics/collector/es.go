// SPDX-License-Identifier:Apache-2.0

package collector

import (
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/metallb/frr-k8s/cmd/metrics/vtysh"
	"github.com/metallb/frr-k8s/internal/frr"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	esOperUpDesc  = evpnDesc("es_oper_up", "Local Ethernet segment's access interface state (1 is up, 0 is down)", "esi")
	esDFDesc      = evpnDesc("es_df", "1 if this VTEP is the designated forwarder for the local Ethernet segment, 0 if not; omitted while no election result applies", "esi")
	esSuccessDesc = scrapeSuccessDesc("es")
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
	success := 0.0
	defer func() {
		ch <- prometheus.MustNewConstMetric(esSuccessDesc, prometheus.GaugeValue, success)
	}()

	segments, err := vtysh.GetEVPNES(c.frrCli)
	if err != nil {
		level.Error(c.Log).Log("error", err, "msg", "failed to collect EVPN Ethernet segments from FRR")
		return
	}

	updateESMetrics(ch, segments)
	success = 1
}

func updateESMetrics(ch chan<- prometheus.Metric, segments []frr.EVPNES) {
	for _, s := range segments {
		ch <- prometheus.MustNewConstMetric(esOperUpDesc, prometheus.GaugeValue, boolToFloat(s.OperUp), s.ESI)
		if s.DF != nil {
			ch <- prometheus.MustNewConstMetric(esDFDesc, prometheus.GaugeValue, boolToFloat(*s.DF), s.ESI)
		}
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
