# FRR metrics

Build with `go build ./cmd/metrics`. Kubernetes TokenReview/SubjectAccessReview
and the existing TLS flags remain the defaults. BGP and BFD are enabled by
default. Add `--collectors=bgp,bfd,evpn` where zebra supplies EVPN JSON; drop
collectors for daemons that are intentionally absent. Both serving modes use
the same collectors.

| Series | Labels | Meaning |
| --- | --- | --- |
| `frrk8s_bgp_session_up` | `peer,vrf` | 1 if Established, 0 in every other state (the state's name is not exported). For an unnumbered peer `peer` is the interface name |
| `frrk8s_bgp_announced_prefixes_total`, `frrk8s_bgp_received_prefixes_total` | `peer,vrf` | Gauges that **sum every address family**, as in frr-k8s |
| `frrk8s_bgp_family_announced_prefixes`, `frrk8s_bgp_family_received_prefixes` | `peer,vrf,afi,safi` | Per family: `ipv4/unicast`, `ipv6/unicast`, `ipv4/vpn`, `l2vpn/evpn`; any other family counts only in the `_total` gauges. A count FRR did not report is absent, never 0 |
| `frrk8s_bgp_{opens,notifications,updates,keepalives}_*`, `route_refresh_sent`, `total_*` | `peer,vrf` | Message counters, per TCP session |
| `frrk8s_bfd_session_up`, `_control_packet_*`, `_echo_packet_*`, `_session_{up,down}_events`, `_zebra_notifications` | `peer,vrf,local,interface,multihop` | frr-k8s's BFD series with ordinary single-hop/multihop session identity, so several sessions to one peer stay distinct. `peer` is the address, so it matches the BGP `peer` for numbered peers only |
| `frrk8s_evpn_vni_info` | `vni,vrf,type,interface` | 1 per VNI zebra knows: inventory, not health |
| `frrk8s_evpn_macs`, `_neighbors`, `_remote_vteps` | `vni,vrf` | L2 VNIs only: MACs and ARP/ND entries zebra holds, and known remote VTEPs. A known VTEP is not proof of reachability |
| `frrk8s_evpn_remote_vtep_present` | `vni,vrf,vtep` | Gauge 1 for each known remote VTEP of an L2 VNI. An absent list does not invent memberships or change the reported aggregate count |
| `frrk8s_evpn_es_oper_up` | `esi` | Gauge 1/0 for a local Ethernet segment's access-port operational state. Remote-only segments are omitted; this is not forwarding health |
| `frrk8s_evpn_es_df` | `esi` | Gauge 1 for local DF, 0 for local non-DF. Omitted while down, unready, bypassed, waiting for an election, or without an explicit role. Non-DF can be a healthy elected role |
| `frrk8s_scrape_collector_success`, `_duration_seconds` | `collector` | Per collector: 1 if the read and parse were complete. A failed collector emits no other series |

BFD intentionally adds `local,interface,multihop` to the existing `peer,vrf`
labels: migrate joins and expected-session inventory to all five labels.
A numbered BGP peer matches BFD's peer address; an unnumbered BGP peer uses its
interface name instead. BFD-only VRFs are included. IDs correlate the two bulk
reads and are never time-series labels. Missing/recreated sessions or duplicate
identities fail the entire BFD collector for that scrape. Named/SBFD sessions
(`bfd-name` present) also fail the collector: their mode is not represented by
the ordinary BFD labels.

Each collector builds a complete snapshot before publishing. Invalid JSON,
missing identity/state and invalid counts fail that collector atomically;
independent collectors continue. Missing measurements are omitted. The existing
BGP `_total` gauges still sum reported families. Alert on collector success and
absent expected series as well as `up`: FRR failure can return HTTP 200 with
collector success 0. Inconsistent output such as duplicate series returns 500.

Admitted handlers and concurrent collection share one `--scrape-timeout`
(default 8s), lowered to 90% of Prometheus's announced timeout. Each CLI command
connects only to its owning daemon and stdout is limited to 32 MiB; stderr only
adds error context. Eight requests may authenticate, gather or write; excess
requests return 503 before calling Kubernetes or FRR. Four scrapes may read FRR
concurrently; excess authenticated requests return 503 before reading FRR. A
slow reader releases its collection slot before writing. Read/write deadlines
bound clients; the liveness probe has a separate single slot and three-second
deadline.

Kubernetes's token cache may finish a lookup after its requesting handler times
out, using its existing ten-second API timeout. Eight Kubernetes API calls may
be active, including response bodies. Each review API retains client-go's
QPS/burst limits; overload fails instead of queuing detached lookups.

SIGINT/SIGTERM stops admission and drains requests for five seconds, then
cancels reads, closes connections and waits for handlers and their vtysh
processes to finish, then cancels and drains remaining Kubernetes API calls.
`/livez` retains the existing daemon-inventory check;
it does not establish routing or forwarding health.

VNI inventory, occupancy and remote VTEP membership are observations, not
expected inventory or proof of forwarding. Deleting a VNI removes its series.
ES operational state comes only from explicit local flags. DF is exported only
while ready, operational, not bypassed and without a pending election timer.
A healthy non-DF reports 0; an inapplicable or unknown role is absent. Remote
and implicit MAC-only segments do not acquire local state. The two EVPN bulk
reads form one collector: a failed ES read also discards VNI data.

Run `go test -race ./cmd/metrics/... ./internal/tlsconfig/...`. Parser tests use
[real captures](collector/testdata/README.md), including malformed variants,
BFD snapshot races and ES transitions. Scale benchmarks cover 2,000 BGP
neighbors and up to 16,384 VNI/VTEP memberships. The CLI output limit bounds
input, not an appliance capacity claim.
BFD RTT is omitted: the available echo-window fields cannot establish general
transport latency.

## Standalone authentication

For a host outside Kubernetes, require a trusted client certificate:

```sh
frr-metrics --disable-metrics-auth --collectors=bgp,bfd,evpn \
  --metrics-bind-address=0.0.0.0 \
  --tls-cert-file=/etc/frr-metrics/server.crt \
  --tls-private-key-file=/etc/frr-metrics/server.key \
  --tls-client-ca-file=/etc/frr-metrics/scraper-ca.crt
```

In another terminal, scrape with `curl --cacert server-ca.crt --cert scraper.crt
--key scraper.key https://HOST:9141/metrics`. The server certificate must have a
SAN matching HOST. Use a CA dedicated to scrapers: any client certificate from
that CA grants access. This mode requires an explicit server key pair and
cannot replace Kubernetes authentication unless `--disable-metrics-auth` is set.
Supplying only one server key-pair flag now fails at startup instead of
falling back to a generated certificate. Without the client CA flag, the opt-in
unauthenticated HTTPS mode remains
available for loopback/proxy deployments.

Existing TLS cipher, curve and minimum-version flags still apply (TLS 1.3 by
default). The existing certificate watcher reloads the server key pair; client
trust is re-read at TLS handshakes at most once a second. Invalid, empty or
missing CA replacements keep the last good trust and log an error. Every PEM
block must be a valid certificate; a partly damaged bundle is rejected as a
whole. Whitespace and `#` comments may separate certificates. New
connections, including resumed TLS sessions, use the new CA; already authenticated
keep-alive connections retain access. Restart to close existing connections when
removing trust. This does not implement certificate revocation lists.

## Container checks and downstream builds

Run `bash hack/metrics-smoke.sh` (podman, openssl, curl and Linux 802.3ad bonding).
`FRR_IMAGE=quay.io/frrouting/frr:10.6.2` selects another FRR release;
`METRICS_BINARY=/absolute/path` tests an existing static binary, including one
built downstream, and `SMOKE_DUMP=/path` saves a healthy scrape. To exercise an
image's packaged exporter, set `FRR_IMAGE=... METRICS_IMAGE_BINARY=/frr-metrics`
and omit `METRICS_BINARY`; no host binary is mounted. Two virtual PEs
exercise BGP/BFD, L2/L3 VNIs, membership, local ES/DF transitions, VNI deletion,
concurrent scrapers, required client certificates, daemon hangs/recovery and
SIGTERM during a hung read. These are local control-plane and process checks.

`openshift/frr` builds `cmd/metrics` with its own `go.mod`, vendor tree and
`Dockerfile.openshift` RHEL Go builder. To check a backport, apply `cmd/metrics`,
the `internal/tlsconfig` change and `hack/metrics-smoke.sh` there, then run `go test -mod=vendor
./cmd/metrics/... ./internal/tlsconfig/...` and `CGO_ENABLED=0 go build -mod=vendor
-o /tmp/frr-metrics ./cmd/metrics`. Test that binary with the smoke script before
building `podman build -f Dockerfile.openshift .`. Upstream's Dockerfile must
build the package too, so adjacent serving files are included.

Host Go 1.26 with the downstream vendor tree can check source compatibility;
only the actual RHEL image build establishes that build path. Appliance lifecycle,
SELinux, FIPS validation, declared scale, forwarding and shared-CE HA still need
qualification on the selected production image and platform.
