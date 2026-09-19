---
sidebar_position: 2
title: Forwarding modes
---

# Forwarding modes

A VIP's `mode` decides how a packet reaches its backend and how the reply gets back. Rivora has three
families, and one node can serve VIPs in different modes side by side.

| | `nat` (full-NAT) | `dsr` (L2 direct return) | `dsr-ipip` / `dsr-gre` (L3 direct return) |
| --- | --- | --- | --- |
| Forward path | Destination address and port rewritten to the backend's, packet passed to the kernel to route | Destination **MAC** rewritten to the backend's, packet sent straight back out | Packet wrapped in an IP-in-IP or GRE tunnel to the backend, routed with the kernel's FIB |
| Return path | **Through the load balancer** (its egress program restores the VIP as the source) | **Straight from the backend to the client** | **Straight from the backend to the client** |
| Backend must be | Reachable, with the balancer as its route back to the client | On the balancer's **own L2 segment**, with the VIP on a local interface and its MAC configured | **Any number of routed hops away**, with a tunnel endpoint and the VIP on a local interface |
| Backend changes | None | Bind the VIP (loopback or dummy); no ARP for it | Tunnel device, VIP on `lo`, reverse-path filter off |
| Client address seen by backend | The client's (only the destination is rewritten) | The client's | The client's |
| Backend port | May differ from the VIP's | Same as the VIP's | Same as the VIP's |
| Kubernetes VIPs | **Yes (the only mode)** | Static config only | Static config only |
| Load balancer on the reply path | Yes: throughput and state | No | No |
| Path MTU | Unchanged | Unchanged | 20 to 44 bytes smaller: see [L3 DSR](../operations/runbook.md#l3-dsr-ip-in-ip-and-gre) |

## Full NAT (`mode: nat`)

The ingress program picks a backend, rewrites the destination address and port (and the checksums), and
returns `XDP_PASS`, so the kernel routes the packet on. The reply comes back to the balancer, where a TCX
egress program looks the flow up in `nat_reverse_map` and puts the VIP back as the source before the packet
leaves. The client never sees a backend's address.

- **Backends need no change**, except that their route back to the client must go **through the balancer**
  (their default gateway, or a static route for the client subnet). If replies take another path they reach
  the client from the wrong source address and the connection dies.
- Only the destination is translated, so a backend logs the **real client address**.
- The balancer carries both directions and keeps per-flow state (`nat_reverse_map`, a fixed-size LRU table), so it is the
  bottleneck and the state holder. In a multi-node ECMP setup the state lives on the
  node that first saw the flow; see the [BGP caveat](../operations/bgp.md#full-nat-and-ecmp).
- The egress program needs a kernel with **TCX (Linux 6.6+)**. A node started with only DSR VIPs does not
  load it.

## DSR (`mode: dsr`)

The ingress program rewrites only the destination **MAC** and sends the frame straight back out with
`XDP_TX`. The packet keeps the VIP as its destination, so the backend must own the VIP.

- Each backend needs the VIP on a local interface (`ip addr add <vip>/32 dev lo`), must **not** answer ARP or
  NDP for it (so the balancer stays the only owner on the wire), and must be on the balancer's L2 segment.
  Its MAC is required in the config: `mac:` on every backend.
- Replies never touch the balancer, so it only has to carry the request direction: the case for high
  throughput or asymmetric traffic such as downloads.
- Health checks still go to the backend's *own* address. On the balancer, give the VIP
  `preferred_lft 0` (or keep it off the interface the probes leave from), or a probe sourced from the VIP
  can never reach a backend that also owns it.
- Not available to Kubernetes-managed VIPs: binding the VIP into pods needs a CNI-specific story that has
  not been designed.
- Under **native** XDP, `XDP_TX` on a `veth` does not reliably cross a bridge; generic mode is the default for
  that reason. Test DSR under native mode on your own hardware first.

## L3 DSR (`mode: dsr-ipip`, `dsr-gre`)

The same direct return, without the L2 requirement. The packet is wrapped in a tunnel to the backend's
address and routed with `bpf_fib_lookup`. The backend unwraps it and answers the client from the VIP.
`dsr-ipip` uses IP-in-IP (or IPv6-in-IPv6); `dsr-gre` uses GRE. IPv4 VIPs get an IPv4 outer header and IPv6
VIPs an IPv6 one.

The full procedure (tunnel setup on the backend, the `tunnelSource` setting, MTU behaviour and the
kernel-fallback path) is in the [runbook](../operations/runbook.md#l3-dsr-ip-in-ip-and-gre). The main
trade-off is MTU: XDP cannot fragment, so a packet that fits the client's path but not the tunnel is answered
with "fragmentation needed" (IPv4, DF set) or "packet too big" (IPv6), and the client's path-MTU discovery
adapts.

## Choosing

- **Start with `nat`.** It needs nothing on the backends, works for Kubernetes, and its costs (reply
  traffic through the balancer, per-flow state) only matter at scale.
- **Use `dsr`** when the backends are on the balancer's own segment and reply traffic dwarfs request
  traffic, or you want the balancer out of the return path.
- **Use `dsr-ipip`/`dsr-gre`** for the same reason when the backends are routed hops away, and you can give
  the path to them a larger MTU.
- **Mixing** is fine: modes are per VIP, and a backend can serve several VIPs (with one MAC and one health
  probe across them).

## What is balanced, and what is not

Rivora balances **TCP and UDP**, IPv4 and IPv6, and handles the packet shapes around them:

| Traffic | Handling |
| --- | --- |
| VLAN (802.1Q) and QinQ tags | Stepped over. See the [runbook](../operations/runbook.md#vlans-fragments-ip-options-and-icmp). |
| IPv4 options | Balanced. |
| IPv4 and IPv6 fragments | The first fragment is balanced and the rest follow it, in every mode; replies are un-NATed likewise. A fragment that arrives *before* its first is not steered. |
| IPv6 Hop-by-Hop / Destination Options headers | Stepped over (up to four, 248 bytes). Routing, AH, ESP and longer chains are passed through untouched. |
| ICMP / ICMPv6 errors quoting a flow (path-MTU, time exceeded) | Sent to the backend that owns the flow. Echo (ping) to a VIP is untouched. |
| SCTP, ICMP echo and everything else | Not balanced: passed to the kernel, so it is answered (or not) as if Rivora were not there. |
| HTTP, gRPC and TLS routing | Not possible: an XDP load balancer has no L7 visibility. See [Limitations](limitations.md). |
