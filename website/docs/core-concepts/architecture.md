---
sidebar_position: 1
title: Architecture
---

# Architecture

Rivora is two things: a **dataplane** of BPF programs that forwards packets in the kernel, and a
**control plane** (Go) that decides what those programs know. There is no proxy process in the packet
path: once a VIP is programmed, packets never leave the kernel.

## The two ways to define VIPs

A VIP gets into the dataplane one of two ways, and both converge on the same `internal/dataplane` code:

```text
  static-YAML path                     Kubernetes path
  ----------------                     ---------------
  rivorad -config                      rivora-controller (leader-elected)
       |                                 AddressPool IPAM: assigns the VIP,
       v                                 writes Service / Gateway status
  internal/config                               |
       |                               rivorad -kubernetes (every node)
       |                                 Service + EndpointSlice reconciler
       |                                 Gateway / TCPRoute / UDPRoute reconciler
       |                                 ServicePolicy, BGPPeer readers
       |                                        |
       |                               internal/speaker (Lease): ARP + NDP
       |                               internal/bgp: /32 and /128 routes
       |                                        |
       +--------------> internal/dataplane <----+
                     UpsertVIP / RemoveVIP
               ID allocators, health, adoption
                               |
                               v
                     BPF maps (pinned under /sys/fs/bpf/rivora-lb)
                               |
               xdp_ingress  --->  tc_nat (full-NAT replies)
                               |
                            backends
```

A node runs **either** static-YAML VIPs **or** Kubernetes-managed VIPs, not both.

## Components

| Component | Role |
| --- | --- |
| `bpf/xdp_ingress.c` | The forwarding program: match a VIP, rate-limit, pick a backend, then rewrite (NAT), redirect (DSR) or tunnel (L3 DSR). Also steers ICMP errors and fragments. |
| `bpf/tc_nat.c` | TCX egress program, full-NAT only: turns a backend's reply back into the VIP's. |
| `rivorad` | Per-node daemon: loads and pins the BPF objects, programs the maps, runs health checks, serves the API, and (Kubernetes) the reconcilers, speaker and BGP speaker. |
| `rivoractl` | CLI over rivorad's API. |
| `rivora-controller` | Cluster IPAM (`AddressPool`) and the one writer of Service, Gateway and route status. Two replicas, one leader. |
| `rivora` | Cluster lifecycle CLI (install, upgrade, status, uninstall) over the embedded Helm chart. |
| `rivora-doctor` | Host readiness checker. |
| `internal/dataplane` | The map writer: allocators for service IDs, backend IDs and Maglev slots; `UpsertVIP`/`RemoveVIP`; health state; restart adoption. |
| `internal/controller` | Service and EndpointSlice reconciler (also applies `ServicePolicy`). |
| `internal/gatewayapi` | Gateway, TCPRoute and UDPRoute reconciler, attachment rules and status. |
| `internal/speaker` | L2 ARP + NDP responder behind one cluster-wide Lease. |
| `internal/bgp`, `internal/bgppeers` | gobgp speaker; `BGPPeer` resources. |
| `internal/healthcheck` | Active TCP and HTTP probes. |
| `internal/maglev` | Weighted Maglev table generation. |
| `internal/initsync` | Tells `rivorad` when each reconciler has finished its first pass, so leftovers can be pruned safely. |

## The packet path

For each packet arriving on the attached interface, `xdp_ingress` does, in order:

1. **Parse.** Ethernet, up to two VLAN tags, then IPv4 (with options) or IPv6 (stepping over Hop-by-Hop,
   Destination Options and Fragment headers). Anything else is passed to the kernel untouched.
2. **Handle the special shapes.** ICMP errors are steered to the backend that owns the connection they
   quote; a later fragment (which has no ports) follows the backend its first fragment chose.
3. **Match a VIP.** An exact `(address, port, protocol)` lookup in `vip_map`/`vip_map6`, then, if none, a
   longest-prefix lookup in the port-range tries. No match means `XDP_PASS`.
4. **Rate-limit.** For TCP SYNs, a per-source token bucket (per VIP if it has its own limit, else the
   node-wide one, if enabled). Over the limit is a drop, counted per VIP.
5. **Pick a backend.** If the flow is in `connection_affinity_map` and its backend is healthy, use it.
   Otherwise hash (the 5-tuple, or the source address for `sessionAffinity: clientIP`) into the VIP's slice
   of the **Maglev table**, skipping unhealthy and draining backends within a bounded probe window, and
   record the choice in the affinity map. Nothing healthy is a `no_healthy_backend` drop.
6. **Forward**, by the VIP's mode:
   - **NAT:** rewrite the destination address and port and the checksums, record the flow in
     `nat_reverse_map`, `XDP_PASS`.
   - **DSR:** rewrite the destination MAC, `XDP_TX`.
   - **L3 DSR:** add the outer header, look the route up with `bpf_fib_lookup`, redirect out of the egress
     interface (or hand the packet to the kernel if the lookup cannot answer).

On the way out, `tc_nat` matches a NAT VIP's reply in `nat_reverse_map` and restores the VIP as its source.

### Why Maglev

Maglev consistent hashing spreads flows evenly and, when a backend is added or removed, moves only a small
share of them. Weights are honoured by giving a backend proportionally more table slots. Each VIP owns a
contiguous slice of one shared 65,537-slot table, sized to its backend count and rebuilt atomically: a lookup
sees the old table or the new one, never a mix.

### Health and draining

`backend_health_map` holds one of three states per backend: healthy, draining or down. The active health
checker flips healthy and down; draining comes from a terminating Kubernetes endpoint or an operator
(`rivoractl drain`). A **draining** backend takes no new flows but its established ones continue (they are
found in the affinity map); a **down** backend takes nothing, and a failed probe always wins over a drain.

## Maps

All maps are pinned under `/sys/fs/bpf/rivora-lb` and outlive `rivorad`.

| Map | Holds |
| --- | --- |
| `vip_map`, `vip_map6` | `(address, port, protocol)` to service ID. |
| `vip_range_map`, `vip_range_map6` | LPM tries for port-range VIPs. |
| `service_config_map` | Per service ID: mode, affinity, backend count, Maglev slice, flags. |
| `maglev_table` | The shared Maglev table. |
| `backend_map`, `backend_map6` | Backend ID to address, port and MAC. |
| `backend_health_map` | Backend ID to healthy, draining or down. |
| `connection_affinity_map`, `..6` | Sticky flow to backend (LRU). |
| `nat_reverse_map`, `..6` | NAT return-path mappings (LRU), shared between the two programs. |
| `frag_map`, `frag_rev_map`, `..6` | Fragment tracking for the request and reply directions (LRU). |
| `stats_map`, `drop_stats_map` | Per-CPU packet, byte and per-reason drop counters. |
| `rl_config_map`, `rl_buckets_map`, `..6`, `svc_rl_config_map` | Node-wide and per-VIP SYN rate limits and their buckets. |
| `iface_mac_map`, `tunnel_config_map` | The interface's MAC; the outer-header sources for tunnel modes. |

Only maps keyed by a raw address have IPv6 siblings; the rest are address-family agnostic. Fixed
capacities: 4096 VIPs, 8192 backends, 65,536 entries in each affinity and NAT table.

## State, restarts and adoption

The maps are pinned, but `rivorad`'s allocators and bookkeeping are in memory. On start the daemon
**adopts** what the maps hold: it reads back each VIP's service ID, Maglev slice and backends, so a VIP
keeps its IDs and none is handed to a new VIP by mistake.

- **Static config:** the file is the whole desired set, so leftovers the file no longer lists are removed at
  once.
- **Kubernetes:** adopted VIPs keep forwarding and are marked unclaimed. Each is claimed as its Service or
  Gateway is reconciled; when both reconcilers finish their first full pass (`internal/initsync`), whatever
  nothing claimed is removed. If a Service keeps failing, nothing is removed.

With `-persist-datapath` the XDP and TCX links are pinned too, so the datapath keeps forwarding while
`rivorad` is down and the next start hot-swaps the program: no traffic gap. See
[Restarting rivorad](../operations/runbook.md#restarting-rivorad).

## The Kubernetes control loop

- **`rivora-controller`** (Lease-elected) assigns an address from an `AddressPool` to each `LoadBalancer`
  Service and Gateway, writes `Service.status.loadBalancer` and `Gateway.status.addresses`, and is the only
  writer of route and listener status.
- **`rivorad`** on every node builds VIPs from the Service and its EndpointSlices (ready endpoints, same
  address family, `externalTrafficPolicy` and `ServicePolicy` applied) and from Gateways and their routes,
  and programs them. Endpoint changes reach the dataplane without a restart.
- **The L2 speaker** makes one elected node answer ARP and NDP for the VIPs; **BGP** instead has every node
  advertise the VIPs it has healthy backends for. They are alternatives, chosen per deployment.
