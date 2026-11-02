// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	evpnVNIInfo     = desc("evpn_vni_info", "Observed EVPN VNI inventory (always 1); not expected inventory or health", "vni", "vrf", "type", "interface")
	evpnMACs        = desc("evpn_macs", "MAC addresses zebra holds for the L2 VNI, local and remote", "vni", "vrf")
	evpnNeighbors   = desc("evpn_neighbors", "ARP and ND entries zebra holds for the L2 VNI", "vni", "vrf")
	evpnRemoteVTEPs = desc("evpn_remote_vteps", "Remote VTEPs known for the L2 VNI; a known VTEP is not proof of reachability", "vni", "vrf")
	evpnVTEPPresent = desc("evpn_remote_vtep_present", "Remote VTEP known for the L2 VNI (always 1); membership, not reachability", "vni", "vrf", "vtep")
)

type evpnVNI struct {
	VNI       uint32  `json:"vni"`
	Type      string  `json:"type"`
	VxlanIf   string  `json:"vxlanIf"`
	TenantVRF string  `json:"tenantVrf"`
	MACs      *uint64 `json:"numMacs"`
	ArpND     *uint64 `json:"numArpNd"`
	// A number for L2 VNIs and the string "n/a" for L3 VNIs.
	RemoteVTEPs json.RawMessage `json:"numRemoteVteps"`
	VTEPs       []string        `json:"remoteVteps"`
}

// collectEVPN reads zebra VNI inventory and L2 occupancy/membership atomically.
func collectEVPN(ctx context.Context, run Runner) ([]prometheus.Metric, error) {
	out, err := run(ctx, "show evpn vni json")
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	var vnis map[string]evpnVNI
	if err := json.Unmarshal(out, &vnis); err != nil {
		return nil, fmt.Errorf("parsing EVPN VNIs: %w", err)
	}
	if vnis == nil {
		return nil, fmt.Errorf("EVPN VNIs must be an object, not null")
	}
	var ms []prometheus.Metric
	for vni, v := range vnis {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if v.VNI == 0 || v.VNI > 16777215 || strconv.FormatUint(uint64(v.VNI), 10) != vni ||
			(v.Type != "L2" && v.Type != "L3") || v.TenantVRF == "" || v.VxlanIf == "" {
			return nil, fmt.Errorf("invalid EVPN VNI %q: identity, type, VRF and interface are required", vni)
		}
		ms = append(ms, prometheus.MustNewConstMetric(evpnVNIInfo, prometheus.GaugeValue, 1, vni, v.TenantVRF, v.Type, v.VxlanIf))
		if v.Type != "L2" {
			continue
		}
		if v.MACs != nil {
			ms = append(ms, prometheus.MustNewConstMetric(evpnMACs, prometheus.GaugeValue, float64(*v.MACs), vni, v.TenantVRF))
		}
		if v.ArpND != nil {
			ms = append(ms, prometheus.MustNewConstMetric(evpnNeighbors, prometheus.GaugeValue, float64(*v.ArpND), vni, v.TenantVRF))
		}
		var remote *uint64
		if len(v.RemoteVTEPs) > 0 {
			if err := json.Unmarshal(v.RemoteVTEPs, &remote); err != nil {
				return nil, fmt.Errorf("parsing remote VTEP count for L2 VNI %s: %w", vni, err)
			}
		}
		if remote != nil {
			ms = append(ms, prometheus.MustNewConstMetric(evpnRemoteVTEPs, prometheus.GaugeValue, float64(*remote), vni, v.TenantVRF))
		}
		if v.VTEPs != nil && remote != nil && uint64(len(v.VTEPs)) != *remote {
			return nil, fmt.Errorf("VNI %s remote VTEP list and count disagree", vni)
		}
		seen := make(map[netip.Addr]bool, len(v.VTEPs))
		for _, ip := range v.VTEPs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			addr, err := netip.ParseAddr(ip)
			if err != nil || addr.Zone() != "" || seen[addr] {
				return nil, fmt.Errorf("invalid or duplicate remote VTEP %q for VNI %s", ip, vni)
			}
			seen[addr] = true
			ms = append(ms, prometheus.MustNewConstMetric(evpnVTEPPresent, prometheus.GaugeValue, 1, vni, v.TenantVRF, addr.String()))
		}
	}
	return ms, nil
}
