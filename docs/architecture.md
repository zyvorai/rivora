# Architecture

Two ways to get a VIP into the shared dataplane — static YAML on one node,
or Kubernetes objects across a cluster — both converge on
`internal/dataplane` → BPF maps.

```text
  static-YAML path                    Kubernetes path (v0.2)
  -----------------                    ----------------------
  rivorad -config                      rivora-controller (leader-elected)
       |                                 watches Service + AddressPool
       v                                 allocates VIP, patches
  internal/config                       Service.Status.LoadBalancer.Ingress
       |                                        |
       |                               rivorad -kubernetes (every node)
       |                                 internal/controller: watches
       |                                 Service + EndpointSlice, reconciles
       |                                        |
       |                               internal/speaker (leader-elected
       |                                 cluster-wide Lease): ARP+NDP for
       |                                 the assigned VIP
       |                                        |
       +---------------> internal/dataplane <---+
                     UpsertVIP / RemoveVIP
                               |
                               v
                    BPF maps @ /sys/fs/bpf/rivora-lb
                               |
                 xdp_ingress (match VIP, pick backend,
                    DSR: rewrite MAC + XDP_TX
                    NAT: rewrite IP/port + XDP_PASS)
                               |
                    tc_nat (full-NAT reverse path only)
                               |
                           backends
```

K8s-managed VIPs are **NAT-only** in v0.2 — DSR would need binding the VIP
into backend pods (CNI-specific; not designed yet).

## Dataplane pieces

| Component | Role |
| --- | --- |
| `bpf/xdp_ingress.c` | Match VIP, optional SYN rate-limit, Maglev / affinity, DSR or NAT |
| `bpf/tc_nat.c` | TCX egress: un-NAT replies using `nat_reverse_map` |
| `rivorad` | Load/pin BPF, apply config, health checks, local HTTP API |
| `rivoractl` | CLI over that API |
| `rivora-doctor` | Host readiness (bpffs, TCX kernel, build tools) |
| `rivora-controller` | Cluster IPAM (`AddressPool` → Service/Gateway status) |
| `internal/speaker` | L2 ARP+NDP for NAT VIPs (Lease-elected) |
| `internal/bgp` | Optional gobgp speaker (`/32` / `/128`) |

## Repository layout (high level)

```text
cmd/rivorad|rivoractl|rivora-doctor|rivora-controller
api/v1alpha1/           AddressPool CRD types
internal/dataplane/     VIP/backend/Maglev/health writers
internal/bpfmaps/       Go ABI for C map structs
internal/ipam/          CIDR/range expand + sparse IPv6 alloc
internal/speaker/       ARP + NDP
internal/bgp/           BGP+BFD
internal/gatewayapi/    Gateway / TCPRoute / UDPRoute
bpf/                    XDP + TCX (hand-rolled)
deploy/helm/rivora/     Helm chart + CRD
scripts/selftest*.sh    Isolated netns verification
config/examples/        Static YAML samples
```

## IPv6 map split

Only maps keyed/valued by a raw address have IPv6 siblings:
`vip_map`, `backend_map`, `connection_affinity_map`, `nat_reverse_map`,
`rl_buckets_map`. Shared AF-agnostic maps:
`service_config_map`, `maglev_table`, `backend_health_map`, `stats_map`,
`iface_mac_map`, `rl_config_map`.

Checksum notes: IPv6 has no IP-header checksum; UDP over IPv6 never uses
an “unset” (zero) checksum.
