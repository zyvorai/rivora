---
sidebar_position: 2
title: IPv6
---

# IPv6

IPv6 is a first-class peer of IPv4 across the dataplane, IPAM, Kubernetes
reconciliation, L2 announcement, and BGP. A VIP's backends must all share
its address family; mixed v4/v6 behind one VIP is rejected.

## Dataplane

XDP and TCX parse, forward, and checksum-rewrite IPv6 TCP/UDP in **DSR**
and **full-NAT**, including Maglev, affinity, draining, and optional
SYN rate limiting.

```sh
sudo ./bin/rivorad -config config/examples/single-vip-ipv6-nat.yaml -bpf-dir bpf
sudo ./bin/rivorad -config config/examples/single-vip-ipv6-dsr.yaml -bpf-dir bpf
```

## IPAM and AddressPool

- **Small prefixes** (host bits ≤ 16) — expanded eagerly.
- **Large prefixes** (host bits > 16, e.g. `/64`) — **sparse**: randomize
  host bits inside the network; no 2^64 enumeration.

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: AddressPool
metadata:
  name: v6
spec:
  addresses:
    - 2001:db8:1::/64
  autoAssign: true
```

## Kubernetes dual-stack

- Status may carry IPv6 LoadBalancer / Gateway addresses from IPv6 pools.
- EndpointSlice backends filtered to the VIP's family.
- Dual-stack Services → one programmed VIP per family.

## NDP speaker

With ARP, behind the same Lease: unsolicited NAs, solicited-node join, and
NS → NA for NAT-mode IPv6 VIPs. Soft-fail to ARP-only without IPv6
link-local. Coverage: `make selftest-ndp`.

## BGP `/128`

Set `bgp.ipv6NextHop` / `-bgp-ipv6-next-hop` or IPv6 VIPs are not
advertised. See [BGP/BFD HA](../operations/bgp.md).

## Verification

| Layer | How |
| --- | --- |
| Dataplane | `scripts/selftest-ipv6.sh` |
| NDP | `scripts/selftest-ndp.sh` |
| Sparse IPAM | `go test ./internal/ipam` |
| BGP `/128` | `go test ./internal/bgp` |
| Helm IPv6 | CI `helm` job |
