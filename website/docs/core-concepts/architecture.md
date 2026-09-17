---
sidebar_position: 1
title: Architecture
---

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
       |                                 Service + EndpointSlice reconciler
       |                                        |
       |                               internal/speaker (Lease): ARP+NDP
       |                                        |
       +---------------> internal/dataplane <---+
                               |
                    BPF maps @ /sys/fs/bpf/rivora-lb
                               |
                 xdp_ingress → DSR (XDP_TX) or NAT (XDP_PASS)
                               |
                    tc_nat (full-NAT reverse path only)
```

K8s-managed VIPs are **NAT-only** in v0.2 — DSR would need binding the VIP
into backend pods (CNI-specific; not designed yet).

## Components

| Component | Role |
| --- | --- |
| `bpf/xdp_ingress.c` | Match VIP, optional SYN rate-limit, Maglev / affinity, DSR or NAT |
| `bpf/tc_nat.c` | TCX egress: un-NAT replies using `nat_reverse_map` |
| `rivorad` | Load/pin BPF, apply config, health checks, local HTTP API |
| `rivoractl` | CLI over that API |
| `rivora-doctor` | Host readiness |
| `rivora-controller` | Cluster IPAM |
| `internal/speaker` | L2 ARP+NDP for NAT VIPs |
| `internal/bgp` | Optional gobgp speaker (`/32` / `/128`) |

## IPv6 map split

IPv6 siblings: `vip_map`, `backend_map`, `connection_affinity_map`,
`nat_reverse_map`, `rl_buckets_map`. Shared AF-agnostic maps:
`service_config_map`, `maglev_table`, `backend_health_map`, `stats_map`,
`iface_mac_map`, `rl_config_map`.

Checksum notes: IPv6 has no IP-header checksum; UDP over IPv6 never uses
an “unset” (zero) checksum.
