# FRR metrics

`frr-metrics` exports local FRR state through `vtysh` in Kubernetes or on a
standalone host. Build with `go build -o frr-metrics ./cmd/metrics`. Kubernetes
TokenReview/SubjectAccessReview and TLS remain the defaults. Both deployment
modes use the same collectors: BGP and BFD by default; add
`--collectors=bgp,bfd,evpn` where zebra supplies EVPN JSON. Select collectors for
the daemons you run; selection does not change `/livez` requirements below.

| Series | Labels | Meaning |
| --- | --- | --- |
| `frrk8s_bgp_session_up` | `peer,vrf` | 1 if Established, 0 in every other state (the state's name is not exported). For an unnumbered peer `peer` is the interface name |
| `frrk8s_bgp_announced_prefixes_total`, `frrk8s_bgp_received_prefixes_total` | `peer,vrf` | Gauges summing reported counts across all address families; 0 when none are reported |
| `frrk8s_bgp_family_announced_prefixes`, `frrk8s_bgp_family_received_prefixes` | `peer,vrf,afi,safi` | Per-family gauges: `ipv4/unicast`, `ipv6/unicast`, `ipv4/vpn`, `l2vpn/evpn`; any other family counts only in the `_total` gauges. Missing per-family counts are omitted; explicit zeros are exported |
| `frrk8s_bgp_{opens,keepalives,total}_{sent,received}`, `frrk8s_bgp_notifications_sent`, `frrk8s_bgp_route_refresh_sent`, `frrk8s_bgp_updates_total` (sent), `frrk8s_bgp_updates_total_received` | `peer,vrf` | FRR message counters shared across address families; missing counters are omitted |
| `frrk8s_bfd_session_up`, `_control_packet_*`, `_echo_packet_*`, `_session_{up,down}_events`, `_zebra_notifications` | `peer,vrf,local,interface,multihop` | Session up is 1 for up, 0 otherwise; other series are FRR counters. Full identity distinguishes sessions sharing an address. `peer` matches BGP for numbered peers only |
| `frrk8s_evpn_vni_info` | `vni,vrf,type,interface` | 1 per VNI zebra knows: inventory, not health |
| `frrk8s_evpn_macs`, `_neighbors`, `_remote_vteps` | `vni,vrf` | L2 VNIs only: MACs and ARP/ND entries zebra holds, and known remote VTEPs. A known VTEP is not proof of reachability |
| `frrk8s_evpn_remote_vtep_present` | `vni,vrf,vtep` | Gauge 1 for each known remote VTEP of an L2 VNI. An absent list does not invent memberships or change the reported aggregate count |
| `frrk8s_evpn_es_oper_up` | `esi` | Gauge 1/0 for a local Ethernet segment's access-port operational state. Remote-only segments are omitted; this is not forwarding health |
| `frrk8s_evpn_es_df` | `esi` | Gauge 1 for local DF, 0 for local non-DF. Omitted while down, unready, bypassed, waiting for an election, or without an explicit role. Non-DF can be a healthy elected role |
| `frrk8s_scrape_collector_success`, `_duration_seconds` | `collector` | Gauges: success is 1 for a complete read and parse, otherwise 0; duration is elapsed seconds. Both remain present on failure |

BFD intentionally adds `local,interface,multihop` to the existing `peer,vrf`
labels: migrate BFD-to-BFD joins and expected-session inventory to all five.
BFD-only VRFs are included. IDs correlate the two bulk
reads and are never time-series labels. Missing/recreated sessions or duplicate
identities fail the entire BFD collector for that scrape. Named/SBFD sessions
(`bfd-name` present) also fail the collector: their mode is not represented by
the ordinary BFD labels. BFD RTT is omitted; echo windows do not establish
transport latency.

Each collector builds a complete snapshot before publishing. Invalid JSON,
missing identity/state and invalid counts fail that collector atomically;
independent collectors continue. Data series are omitted on failure; otherwise,
missing measurements are omitted except for the legacy BGP totals above. Alert on
collector success and absent expected series as well as `up`: FRR failure can
return HTTP 200 with collector success 0. Inconsistent output such as duplicate series returns 500.

Authentication and concurrent FRR collection share `--scrape-timeout` (default
8s), lowered to 90% of Prometheus's announced timeout. Eight requests may
authenticate, collect or write; excess requests return 503 before Kubernetes or
FRR calls. Four scrapes may read FRR concurrently; excess authenticated requests
return 503 before reading FRR. Slow responses release collection slots but
retain request slots until writing and connection cleanup finish. Server
read/write deadlines bound clients. CLI output is limited to 32 MiB per command.

Cached TokenReview lookups can outlive a scrape, under a ten-second timeout.
Kubernetes API work is bounded; review saturation or timeouts return 500.
SIGINT/SIGTERM drains requests for five seconds, then cancels remaining reads, closes connections and waits for handlers,
vtysh cleanup and remaining Kubernetes API calls.

`/livez` checks for `bfdd`, `bgpd`, `staticd`, `watchfrr` and `zebra` regardless of
collector selection, with a separate three-second deadline. It does not establish
routing or forwarding health.

Deleting a VNI removes its series. Remote-only and implicit MAC-only Ethernet
segments do not acquire local state. VNI and ES reads form one EVPN collector:
a failed ES read also discards VNI data.

## Standalone authentication

For a host outside Kubernetes, require a trusted client certificate:

```sh
./frr-metrics --disable-metrics-auth --collectors=bgp,bfd,evpn \
  --metrics-bind-address=0.0.0.0 \
  --tls-cert-file=/etc/frr-metrics/server.crt \
  --tls-private-key-file=/etc/frr-metrics/server.key \
  --tls-client-ca-file=/etc/frr-metrics/scraper-ca.crt
```

In another terminal, scrape with `curl --cacert server-ca.crt --cert scraper.crt
--key scraper.key https://HOST:9141/metrics`. The server certificate must have a
SAN matching HOST. Use a CA dedicated to scrapers: any client certificate from
that CA grants access. Client-CA mode requires `--disable-metrics-auth` and both server key-pair flags.
A partial server key pair fails startup. Without the client CA flag, opt-in
unauthenticated HTTPS remains available for loopback/proxy deployments.

Existing TLS cipher, curve and minimum-version flags still apply (TLS 1.3 by
default). Server certificates reload automatically; client trust is re-read at
TLS handshakes at most once a second. Invalid initial CA trust prevents startup.
Invalid, empty or missing CA replacements keep the last good trust and log an
error. Every PEM block must be a valid certificate; a partly damaged bundle is
rejected as a whole. Whitespace and `#` comments may separate certificates.
New connections, including resumed TLS sessions, use the new CA; authenticated
keep-alive connections retain access. Restart to close existing connections when
removing trust. This does not implement certificate revocation lists.

## Container checks and downstream builds

Run `go test -race ./cmd/metrics/... ./internal/tlsconfig/...`; parser tests use
[real captures](collector/testdata/README.md) and malformed variants.

Run `bash hack/metrics-smoke.sh` (podman, openssl, curl, Linux
802.3ad bonding, and Go unless using an existing binary).
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
building `podman build -f Dockerfile.openshift .`.

Host Go 1.26 with the downstream vendor tree can check source compatibility;
only the actual RHEL image build establishes that build path. Appliance lifecycle,
SELinux, FIPS validation, declared scale, forwarding and shared-CE HA still need
qualification on the selected production image and platform.
