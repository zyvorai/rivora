---
sidebar_position: 4
title: Metrics and alerts
---

# Metrics and alerts

`rivorad` and `rivora-controller` serve Prometheus metrics on `/metrics` of the metrics listener
(`-metrics-listen`, default `:9871`, plain HTTP, no authentication; the Helm chart annotates the pods with
`prometheus.io/scrape`). `rivorad` serves the same route on its API address too. `rivorad`'s dataplane
metrics are computed **on every scrape** from the BPF maps rather than kept as separate counters, so they
are always what the kernel holds. Both programs also export the standard Go runtime and process metrics.

Counters that come from the BPF maps run from when the maps were pinned, so they survive a `rivorad`
restart and reset only if the maps are recreated. A VIP that is removed and added again starts from zero.

Label sets: **VIP metrics** carry `vip` (`address:port`, or `address:first-last` for a range), `protocol`
and `mode`. **Backend metrics** carry `vip` and `backend` (`address:port`).

## Dataplane (`rivorad`)

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `rivora_vip_info` | gauge | vip, protocol, mode | Always 1; describes a configured VIP. |
| `rivora_vip_backends` | gauge | vip, protocol, mode | Backends currently configured behind the VIP. |
| `rivora_vip_packets_total` | counter | vip, protocol, mode | Packets forwarded for the VIP. |
| `rivora_vip_bytes_total` | counter | vip, protocol, mode | Bytes forwarded for the VIP. |
| `rivora_vip_dropped_packets_total` | counter | vip, protocol, mode, `reason` | Packets dropped for the VIP: `rate_limited` (a SYN over the per-source limit) or `no_healthy_backend`. |
| `rivora_vip_unserved_packets_total` | counter | vip, protocol, mode | Packets that matched the VIP but could not be served, so passed to the kernel stack. **Not a drop; should always be 0.** |
| `rivora_dropped_packets_total` | counter | | Node-wide drops. Equals the sum of every VIP's `dropped_packets_total`. |
| `rivora_backend_packets_total` | counter | vip, backend | Packets forwarded to the backend. |
| `rivora_backend_bytes_total` | counter | vip, backend | Bytes forwarded to the backend. |
| `rivora_backend_healthy` | gauge | vip, backend | 1 when the active health checker considers it healthy. |
| `rivora_backend_draining` | gauge | vip, backend | 1 while it takes no new flows: an operator drain or a terminating Kubernetes endpoint. |
| `rivora_backend_weight` | gauge | vip, backend | Its Maglev weight. |
| `rivora_dataplane_scrape_errors_total` | gauge | | 1 when the last scrape could not read the BPF maps, else 0 (despite the name). |

### Flow tables

| Metric | Labels | Meaning |
| --- | --- | --- |
| `rivora_conntrack_entries` | `table` | Live entries, sampled at most every 10 s. `table` is `affinity`, `affinity6`, `nat_reverse` or `nat_reverse6`. |
| `rivora_conntrack_capacity` | `table` | The table's maximum. These are LRU maps: at capacity the kernel evicts the least recently used flow, which may re-hash a live connection onto another backend (affinity) or lose its return-path mapping (NAT). |

The fragment-tracking tables are LRU too but are not exported: an entry only matters for the moments
between a datagram's first and last fragment.

## BGP (`rivorad` with BGP on)

The series are absent, not zero, when BGP is off.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `rivora_bgp_peer_up` | peer, asn | 1 when the session is ESTABLISHED. A configured peer gobgp fails to report shows as 0, not as missing. |
| `rivora_bgp_peer_state_changes_total` | peer | Session-state transitions since start. Rising on a mostly-up peer means a flapping session or a BFD-detected drop. |
| `rivora_bgp_advertised_routes` | family | VIP host routes advertised now (`ipv4` `/32`, `ipv6` `/128`). |
| `rivora_bgp_route_update_errors_total` | op | Failed `advertise` or `withdraw` attempts, including failures to update a peer's route limits. A resync retries; a value that keeps rising means routes are not reaching peers. |

## API (`rivorad`)

| Metric | Labels | Meaning |
| --- | --- | --- |
| `rivora_api_auth_failures_total` | reason | Rejected requests: `unauthenticated` (401) or `forbidden` (403). |
| `rivora_api_changes_total` | user, code | State-changing requests (drain, undrain, weight), by caller and HTTP status: the audit trail as a counter. |

## Controller (`rivora-controller`)

| Metric | Meaning |
| --- | --- |
| `rivora_controller_leader` | 1 on the replica holding the leader-election Lease. Exactly one replica should report 1; none for longer than the lease means IPAM and status writing have stalled. |

## Suggested alerts

```yaml
groups:
- name: rivora
  rules:
  # A VIP is dropping because it has nothing to send to.
  - alert: RivoraVIPNoHealthyBackend
    expr: sum by (vip) (rate(rivora_vip_dropped_packets_total{reason="no_healthy_backend"}[5m])) > 0
    for: 2m
  # Traffic matches a VIP but skips the load balancer: the maps and the config disagree.
  - alert: RivoraVIPUnserved
    expr: sum by (vip) (rate(rivora_vip_unserved_packets_total[5m])) > 0
    for: 2m
  # A backend has been unhealthy for a while.
  - alert: RivoraBackendDown
    expr: rivora_backend_healthy == 0 and rivora_backend_draining == 0
    for: 5m
  # A flow table is close to evicting live flows.
  - alert: RivoraFlowTableNearlyFull
    expr: rivora_conntrack_entries / rivora_conntrack_capacity > 0.8
    for: 10m
  # rivorad cannot read its BPF maps.
  - alert: RivoraMapsUnreadable
    expr: rivora_dataplane_scrape_errors_total == 1
    for: 1m
  # BGP.
  - alert: RivoraBGPPeerDown
    expr: rivora_bgp_peer_up == 0
    for: 2m
  - alert: RivoraBGPPeerFlapping
    expr: increase(rivora_bgp_peer_state_changes_total[10m]) > 4
  - alert: RivoraBGPRouteUpdatesFailing
    expr: increase(rivora_bgp_route_update_errors_total[10m]) > 0
  # Someone is guessing API keys.
  - alert: RivoraAPIKeyProbing
    expr: rate(rivora_api_auth_failures_total{reason="unauthenticated"}[5m]) > 0.5
    for: 5m
  # No controller leader: IPAM and status writing have stalled.
  - alert: RivoraNoControllerLeader
    expr: sum(rivora_controller_leader) == 0
    for: 1m
```

What each one means and what to check first is in the [runbook](runbook.md).
