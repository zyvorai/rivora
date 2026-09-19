---
sidebar_position: 3
title: IPv6
---

# IPv6

IPv6 is a first-class peer of IPv4 across the dataplane, IPAM, Kubernetes reconciliation, L2 announcement
and BGP. A VIP's backends must all share its address family; mixing v4 and v6 behind one VIP is rejected.

## Dataplane

XDP and TCX parse, forward and checksum-rewrite IPv6 TCP and UDP in **DSR**, **full-NAT** and **L3 DSR**,
including Maglev, session affinity, draining, port ranges, per-VIP rate limiting and ICMPv6 error steering.
Behaviour mirrors IPv4 except where the protocols differ:

- **No IP-header checksum.** Only the L4 checksum is updated, and it covers the 128-bit addresses.
- **UDP checksum is never "unset".** IPv4 uses zero to mean "no checksum"; over IPv6 that is forbidden, so
  the checksum is always maintained.
- **Extension headers.** Hop-by-Hop and Destination Options headers are stepped over to reach the TCP or UDP
  header (up to four, 248 bytes in total) and stay in the packet. A Routing header, AH, ESP or a longer
  chain is passed through untouched.
- **Fragments.** The first fragment carries the ports and is balanced; its backend is remembered (keyed by
  source, destination, the Fragment header's identification and protocol) and later fragments follow it. In
  NAT the backend's fragmented replies are un-NATed on the way out. A fragment that arrives before its first
  is not steered.
- **ICMPv6 errors** ("packet too big", "time exceeded", "parameter problem") quoting a flow are sent to the
  backend that owns it, so path-MTU discovery keeps working behind a VIP. Only when ICMPv6 directly follows
  the IPv6 header.
- **L3 DSR** wraps an IPv6 VIP's packets in IPv6-in-IPv6 or GRE over IPv6 (`tunnelSource6`), and answers an
  oversize packet with "packet too big".

Details and limits: [the runbook](../operations/runbook.md#vlans-fragments-ip-options-and-icmp).

```sh
sudo ./bin/rivorad -config config/examples/single-vip-ipv6-nat.yaml -bpf-dir bpf
sudo ./bin/rivorad -config config/examples/single-vip-ipv6-dsr.yaml -bpf-dir bpf
```

```yaml
interface: eth0
vips:
  - address: fd00:77::100
    port: 80
    protocol: tcp
    mode: nat            # or dsr (backends need the VIP on lo and a MAC)
    backends:
      - {address: fd00:77::11, port: 8080}
```

For DSR labs, bind the VIP on the backend's `lo` with `preferred_lft 0`, and give the *balancer* its VIP with
`preferred_lft 0` too, so its own health probes are never sourced from an address the backend also owns.

## IPAM and AddressPool

`AddressPool.spec.addresses` takes IPv4 or IPv6 CIDRs, ranges (`a-b`) or single addresses.

- **Small prefixes** (host bits of 16 or fewer, such as a `/112` or tighter) are expanded into concrete
  addresses, like an IPv4 `/24`.
- **Large prefixes** (more than 16 host bits, typically a `/64`) are kept **sparse**: the controller
  allocates by randomising host bits inside the prefix instead of enumerating 2^64 addresses. Capacity
  accounting uses a capped virtual size so the status stays useful.

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: AddressPool
metadata: {name: v6}
spec:
  addresses: ["2001:db8:1::/64"]   # sparse
  autoAssign: true
```

## Kubernetes dual-stack

- A Service or Gateway may carry IPv6 (or both) addresses allocated from IPv6 (or dual) pools.
- EndpointSlice backends are filtered to the VIP's address family: a v6 VIP programs only v6 endpoints.
- A dual-stack Service yields one programmed VIP per family, each Maglev-partitioned independently.

## NDP speaker

The L2 speaker answers ARP and NDP behind one cluster-wide Lease: gratuitous ARP and replies for IPv4 VIPs;
unsolicited Neighbor Advertisements, solicited-node multicast membership and replies to Neighbor
Solicitations for IPv6 VIPs. It falls back to ARP only if the interface has no IPv6 link-local address (a
v4-only lab NIC). Only NAT-mode VIPs are announced: a DSR backend owns its VIP on the wire.

## BGP `/128`

Each healthy IPv6 VIP is advertised as a `/128` (`RF_IPv6_UC`), with `bgp.ipv6NextHop` as the next hop: set
it, or IPv6 VIPs are skipped with an error. Sessions negotiate both IPv4 and IPv6 unicast, so one peer
carries both. See [BGP](../operations/bgp.md).

## Maps

Only maps keyed or valued by a raw address have IPv6 siblings (`vip_map`, `backend_map`,
`connection_affinity_map`, `nat_reverse_map`, `rl_buckets_map`, the fragment tables). The rest are shared.
See [Architecture](architecture.md#maps).

## Verification

| Layer | How |
| --- | --- |
| Dataplane DSR and NAT, Maglev, failover | `scripts/selftest-ipv6.sh` |
| Session affinity and drop counters over IPv6 | `scripts/selftest-ipv6-policy.sh` |
| Extension headers and fragmented UDP, NAT and DSR | `scripts/selftest-ipv6-ext.sh` |
| L3 DSR and its oversize-packet answer | `scripts/selftest-l3dsr.sh` |
| ICMPv6 path-MTU steering, VLAN | `scripts/selftest-edgecases.sh` |
| Checksums (offload off, so every packet is verified) | `scripts/selftest-checksum.sh` |
| NDP solicit to advertise | `scripts/selftest-ndp.sh` |
| Sparse IPAM | `go test ./internal/ipam` |
| BGP `/128` | `go test ./internal/bgp` |
| Helm IPv6 pool and next hop | CI `helm` job |

Not yet verified live: end-to-end dual-stack Service traffic on a cluster, and BGP IPv6 peering against a
real router.
