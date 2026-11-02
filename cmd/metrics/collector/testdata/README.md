# FRR captures

These public lab fixtures were ported from the concurrent reference exporter
working tree on 2026-11-02 (published baseline `3f88c3a` plus local corrections).
All captures are FRR 10.4.3. They contain no production configuration.
`bgp_neighbors.json`, `bfd_{peers,counters}.json`, and `evpn_vni.json` use the
two-router topology in `hack/metrics-smoke.sh`. Other BGP captures include VRFs,
IPv6, unnumbered and down peers; the multihop BFD captures add a multihop session.

`bfd_identity_{peers,counters}.json` were captured on 2026-11-02 from FRR 10.4.3
(`quay.io/frrouting/frr@sha256:9c7e397a43877e28029d4b49b7d9eba0383940c0db36382fa712504532219397`)
with the two BFD commands above. The lab configured three default-VRF sessions to
`192.0.2.1`: single-hop on `dummy0` from `.10`, and multihop from `.11` and `.12`;
a fourth to that peer from `.20` on `dummy-blue` in kernel VRF `blue` (table 1002),
with no BGP instance; and a peer `192.0.2.2` with no explicit local address or interface.
All five were down. Invalid-input tests corrupt these and the other real captures.

`bgp_reserved_interfaces.json` captures actual unnumbered peers named `vrfId` and `vrfName`:
FRR places neighbor objects under the same keys it otherwise uses for scalar bookkeeping.
`bgp_unknown_vrf.json` captures a configured BGP VRF without a kernel VRF, whose bookkeeping
ID is the legitimate sentinel `-1`.
`evpn_es_mac_only.json` captures an implicit ES from a Type-2 MAC route with nonzero ESI and
RT `65001:10100`, without Type-1/Type-4 routes. These were captured on FRR 10.4.3 with the
same image digest above; the MAC-only ES legitimately has no flags or access port.

`evpn_es_*.json` were captured from the same 10.4.3 image with `show evpn es detail json`.
The setup matches `hack/smoke.sh`: two LACP PE/virtual-CE bond pairs on VNI 10100, ESI
`03:44:38:39:ff:ff:01:00:00:01`, and DF preferences 50000/40000. Captures cover elected
DF and non-DF, `bond0` down, `evpn mh bypass`, removing the local `es-id` (remote-only),
re-adding it during the DF delay, and deleting `vxlan10100` (unready). The empty array
was captured before configuring any ES. The unready and bypass captures actually contain
`df`; the down and pending captures contain `nonDF`. Tests must omit the role in all four cases.

`evpn_vni_membership.json` adds a third FRR 10.4.3 peer at `10.77.0.4` (AS 65003) advertising
VNI 10100 to r1, plus VNI 10200 on r1/r2 with its bridge/SVI in VRF `red`. r1 observes two
default-VRF VTEPs and one red-VRF VTEP, alongside the L3 VNI. This checks that membership
is scoped by VNI and VRF.
Tests corrupt copies of these captures to exercise invalid and missing fields.
