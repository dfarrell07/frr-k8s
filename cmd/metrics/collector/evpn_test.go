// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

const (
	vniCmd = "show evpn vni json"
)

func TestEVPNFromRealFRR(t *testing.T) {
	ms, err := collectEVPN(context.Background(), fake(map[string]string{vniCmd: "@evpn_vni.json"}))
	if err != nil {
		t.Fatal(err)
	}
	got := series(t, ms)
	expect(t, got, map[string]float64{
		`frrk8s_evpn_vni_info{interface="vxlan10100",type="L2",vni="10100",vrf="default"}`: 1,
		`frrk8s_evpn_vni_info{interface="vxlan50001",type="L3",vni="50001",vrf="red"}`:     1,
		`frrk8s_evpn_macs{vni="10100",vrf="default"}`:                                      1,
		`frrk8s_evpn_neighbors{vni="10100",vrf="default"}`:                                 0,
		`frrk8s_evpn_remote_vteps{vni="10100",vrf="default"}`:                              1,
		`frrk8s_evpn_remote_vtep_present{vni="10100",vrf="default",vtep="10.78.0.3"}`:      1,
	})
	// For an L3 VNI the same fields mean router MACs and next hops.
	absent(t, got, `frrk8s_evpn_macs{vni="50001"`, `frrk8s_evpn_neighbors{vni="50001"`, `frrk8s_evpn_remote_vteps{vni="50001"`, `frrk8s_evpn_remote_vtep_present{vni="50001"`)
}

func TestEVPNMembershipScopedToVNIAndVRFFromRealFRR(t *testing.T) {
	ms, err := collectEVPN(context.Background(), fake(map[string]string{vniCmd: "@evpn_vni_membership.json"}))
	if err != nil {
		t.Fatal(err)
	}
	got := series(t, ms)
	expect(t, got, map[string]float64{
		`frrk8s_evpn_remote_vtep_present{vni="10100",vrf="default",vtep="10.77.0.3"}`: 1,
		`frrk8s_evpn_remote_vtep_present{vni="10100",vrf="default",vtep="10.77.0.4"}`: 1,
		`frrk8s_evpn_remote_vtep_present{vni="10200",vrf="red",vtep="10.77.0.3"}`:     1,
		`frrk8s_evpn_remote_vteps{vni="10100",vrf="default"}`:                         2,
		`frrk8s_evpn_remote_vteps{vni="10200",vrf="red"}`:                             1,
	})
	var memberships int
	for key := range got {
		if strings.HasPrefix(key, "frrk8s_evpn_remote_vtep_present{") {
			memberships++
		}
	}
	if memberships != 3 {
		t.Errorf("got %d memberships, want 3", memberships)
	}
}

func encodeJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func assertEVPNFailure(t *testing.T, replies map[string]string) {
	t.Helper()
	if ms, err := collectEVPN(context.Background(), fake(replies)); err == nil || len(ms) != 0 {
		t.Errorf("failed EVPN read: %d metrics, error %v", len(ms), err)
	}
	got := series(t, Gather(context.Background(), fake(replies), all[2:], discard))
	expect(t, got, map[string]float64{`frrk8s_scrape_collector_success{collector="evpn"}`: 0})
	absent(t, got, "frrk8s_evpn_")
}

func TestEVPNVTEPMembershipOmissionsAndInvalidData(t *testing.T) {
	raw, err := os.ReadFile("testdata/evpn_vni.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		change            func(map[string]any)
		fail              bool
		count, membership bool
	}{
		"absent list":      {func(v map[string]any) { delete(v, "remoteVteps") }, false, true, false},
		"null list":        {func(v map[string]any) { v["remoteVteps"] = nil }, false, true, false},
		"absent count":     {func(v map[string]any) { delete(v, "numRemoteVteps") }, false, false, true},
		"null count":       {func(v map[string]any) { v["numRemoteVteps"] = nil }, false, false, true},
		"empty list":       {func(v map[string]any) { v["remoteVteps"], v["numRemoteVteps"] = []string{}, 0 }, false, true, false},
		"invalid ip":       {func(v map[string]any) { v["remoteVteps"] = []string{"not-an-ip"} }, true, false, false},
		"scoped ip":        {func(v map[string]any) { v["remoteVteps"] = []string{"fe80::1%eth0"} }, true, false, false},
		"null ip":          {func(v map[string]any) { v["remoteVteps"] = []any{nil} }, true, false, false},
		"wrong list type":  {func(v map[string]any) { v["remoteVteps"] = "10.78.0.3" }, true, false, false},
		"wrong entry type": {func(v map[string]any) { v["remoteVteps"] = []int{1} }, true, false, false},
		"count mismatch":   {func(v map[string]any) { v["numRemoteVteps"] = 2 }, true, false, false},
		"list mismatch":    {func(v map[string]any) { v["remoteVteps"] = []string{} }, true, false, false},
		"duplicate ip":     {func(v map[string]any) { v["remoteVteps"], v["numRemoteVteps"] = []string{"10.78.0.3", "10.78.0.3"}, 2 }, true, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			var vnis map[string]map[string]any
			if err := json.Unmarshal(raw, &vnis); err != nil {
				t.Fatal(err)
			}
			tc.change(vnis["10100"])
			replies := map[string]string{vniCmd: encodeJSON(t, vnis)}
			if tc.fail {
				assertEVPNFailure(t, replies)
				return
			}
			ms, err := collectEVPN(context.Background(), fake(replies))
			if err != nil {
				t.Fatal(err)
			}
			got := series(t, ms)
			for key, want := range map[string]bool{
				`frrk8s_evpn_remote_vteps{vni="10100",vrf="default"}`:                         tc.count,
				`frrk8s_evpn_remote_vtep_present{vni="10100",vrf="default",vtep="10.78.0.3"}`: tc.membership,
			} {
				if _, present := got[key]; present != want {
					t.Errorf("%s present = %v, want %v", key, present, want)
				}
			}
		})
	}
}

func TestEVPNEmptyAndPartial(t *testing.T) {
	ms, err := collectEVPN(context.Background(), fake(map[string]string{vniCmd: "{}"}))
	if err != nil || len(ms) != 0 {
		t.Errorf("no VNIs: %d series, err %v", len(ms), err)
	}
	ms, err = collectEVPN(context.Background(), fake(map[string]string{vniCmd: `{"7":{"vni":7,"type":"L2","vxlanIf":"v7","tenantVrf":"default"}}`}))
	if err != nil {
		t.Fatal(err)
	}
	// Counts FRR did not report are absent, not zero.
	if got := series(t, ms); len(got) != 1 {
		t.Errorf("want only the inventory series, got %v", got)
	}
}

// Hold VNI count fixed while increasing advertised VTEPs. BGP peer count is
// not a bound on membership cardinality when a relay advertises many VTEPs.
func BenchmarkCollectEVPN(b *testing.B) {
	raw, err := os.ReadFile("testdata/evpn_vni_membership.json")
	if err != nil {
		b.Fatal(err)
	}
	var fixture map[string]evpnVNI
	if err := json.Unmarshal(raw, &fixture); err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{1, 256} {
		b.Run(fmt.Sprintf("64_VNIs_%d_VTEPs", count), func(b *testing.B) {
			vteps := make([]string, count)
			for i := range vteps {
				vteps[i] = fmt.Sprintf("10.200.%d.%d", i/254, i%254+1)
			}
			vnis := make(map[string]evpnVNI, 64)
			for i := 0; i < 64; i++ {
				v := fixture["10100"]
				v.VNI, v.VxlanIf = uint32(10001+i), fmt.Sprintf("vxlan%d", 10001+i)
				v.VTEPs, v.RemoteVTEPs = vteps, json.RawMessage(fmt.Sprint(count))
				vnis[fmt.Sprint(v.VNI)] = v
			}
			payload, err := json.Marshal(vnis)
			if err != nil {
				b.Fatal(err)
			}
			run := func(_ context.Context, command string) ([]byte, error) {
				if command == vniCmd {
					return payload, nil
				}
				return nil, fmt.Errorf("unexpected command %s", command)
			}
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := collectEVPN(context.Background(), run); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
