---
sidebar_position: 4
title: Limitations and non-goals
---

# Limitations and non-goals

What Rivora does not do, and what is not yet verified, in one place. The first two sections are properties
of the design; the last is about how much of it has been proven where.

## Non-goals: things an XDP L4 balancer cannot honestly do

- **No L7 routing.** Rivora forwards TCP and UDP packets; it never terminates or parses a connection. So no
  `HTTPRoute` or `GRPCRoute` (path, header and method matching), no TLS termination, no SNI routing (a
  `TLSRoute` needs the ClientHello), no per-request balancing or retries. Use a `TCPRoute` for TLS
  passthrough. Claiming to enforce L7 rules with no L7 visibility would be a correctness hazard, so these
  are refused, not approximated.
- **Only TCP and UDP.** SCTP and other IP protocols are not balanced (they pass to the kernel). ICMP errors
  that quote a TCP or UDP flow are steered; ping is not.
- **One interface per node** for XDP.
- **No connection-state sharing between nodes.** Full-NAT flow state lives on the node that first saw the
  flow. With BGP + ECMP a router re-hash can disrupt in-flight NAT'd connections on a node that traffic moves
  away from; DSR avoids this. Backends are chosen by consistent hashing, so nodes with the same backend set and
  health view choose the same backend for a *new* flow.
- **No SYN cookies, ACLs or allow/deny lists.** DDoS protection is the opt-in per-source SYN token bucket.

## Behaviours worth knowing

- **Fragments that arrive out of order.** A later fragment that reaches the balancer *before* its first
  fragment cannot be matched to a VIP or backend without state, so that datagram is lost. In-order
  fragments, in IPv4 and IPv6, follow their first fragment in every mode.
- **IPv6 extension headers.** Hop-by-Hop and Destination Options headers are stepped over (up to four, 248
  bytes). A Routing header, AH, ESP or a longer chain is passed through untouched, not balanced. ICMPv6
  errors are steered only when ICMPv6 directly follows the IPv6 header.
- **L3 DSR MTU.** XDP cannot fragment a packet that the tunnel makes too big. The balancer answers with
  "fragmentation needed" or "packet too big" for TCP-like traffic, but an IPv4 packet **without** DF, or a
  smaller link beyond the first hop, is not covered. Give the path to tunnelled backends a larger MTU.
- **Kubernetes VIPs are full-NAT only.** DSR needs the VIP bound inside backend pods, which needs a
  CNI-specific design.
- **`externalTrafficPolicy: Local`** is honoured only with BGP on and the L2 speaker off; elsewhere it is
  treated as `Cluster`, with a warning once per Service. Not implemented: `internalTrafficPolicy`,
  `trafficDistribution`, `healthCheckNodePort`.
- **`sessionAffinity: ClientIP`** has no timeout (stickiness is a function of the source address and the
  current backend set), and everyone behind one NAT is one client.
- **Health checks** are TCP or HTTP. No HTTPS, gRPC or UDP-protocol probes; the timing is global rather than
  per VIP.
- **Operator drains and weight overrides are not persisted**: a restart reverts to the configured state.
- **Flow tables are fixed size** (65,536 entries per affinity and NAT table) and LRU: at capacity a live flow
  can be evicted. Watch `rivora_conntrack_entries`.
- **Native XDP** is opt-in. It works for full-NAT on `veth` (tested), but DSR's `XDP_TX` on a `veth` does not
  reliably cross a bridge, and native mode has not been benchmarked here. Try it on your hardware first.
- **Changing some settings needs a restart**: interface, XDP mode, API address, health-check timing, rate
  limit, BGP, tunnel sources, and the first NAT VIP on a node started without one.
- **Revocation of API client certificates is not checked**, and API roles are only admin and read-only.
- **A `BGPPeer` has no status**, since every node would write to it; session state is in the `rivora_bgp_*`
  metrics.

## What has and has not been verified

The dataplane and control plane are covered by unit tests (`go test -race ./...`) and by the
[selftests](../operations/selftests-ci.md): about twenty isolated network-namespace scenarios run against a
real `rivorad` on a Linux host (the checksum-sensitive ones with offload switched off, so every rewritten
checksum is really verified), and each new behaviour has been checked against deliberately broken variants of the code (a check
that cannot fail proves nothing).

**Not verified:**

- **Anything on a real Kubernetes cluster since the early releases.** The Service and IPAM path, KubeVirt
  and external backends were verified on a live cluster; the Gateway API traffic path, `ServicePolicy`,
  `BGPPeer` with Secret RBAC, the attachment and status logic, `externalTrafficPolicy: Local` and Kubernetes-
  mode restart adoption are covered by tests against fake clients and by the selftests, **not by a real
  cluster**.
- **BGP against a router other than gobgp.** Sessions, multihop, TCP MD5, communities and route limits are
  tested against gobgp instances (in-process, and two hops away across a router in `selftest-bgp.sh`). FRR or
  BIRD peering and BFD timing on real hardware are not.
- **Dual-stack Service traffic** end to end on a cluster, and BGP IPv6 peering against a real router.
- **Performance.** There are no throughput numbers; the design (in-kernel forwarding, per-CPU counters, no
  allocation per packet) is meant to be fast but is unmeasured.
- **L3 DSR with IPv6 extension headers or fragments**, which take the same code but are not selftested
  together, and kernels other than the one the tests ran on (Linux 6.8).
- **Kernel range.** The forwarding program uses helpers added in Linux 5.18, and full-NAT needs TCX (6.6).
  Only 6.8 is exercised; run `rivora-doctor` on a new host.
