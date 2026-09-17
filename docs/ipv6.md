# IPv6

IPv6 is a first-class peer of IPv4 across the dataplane, IPAM, Kubernetes
reconciliation, L2 announcement, and BGP. A VIP's backends must all share
its address family; mixed v4/v6 behind one VIP is rejected.

## Dataplane

XDP and TCX parse, forward, and checksum-rewrite IPv6 TCP/UDP in **DSR**
and **full-NAT**, including Maglev, affinity, draining, and optional
SYN rate limiting. DSR health probes avoid sourcing from the VIP on
loopback (`preferred_lft 0` / omit LB VIP as preferred source).

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
    mode: nat
    backends:
      - address: fd00:77::11
        port: 8080
```

## IPAM and AddressPool

`AddressPool.spec.addresses` accepts IPv4 or IPv6 CIDRs, ranges, or
singles.

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

CI sample: `deploy/helm/rivora/ci/addresspool-ipv6.yaml`.

## Kubernetes dual-stack

- Status may carry IPv6 LoadBalancer / Gateway addresses from IPv6 pools.
- EndpointSlice backends filtered to the VIP's family.
- Dual-stack Services → one programmed VIP per family.

```sh
rivorad -kubernetes -interface eth0 -speaker=true [-gateway-api]
```

## NDP speaker

With ARP, behind the same Lease:

- Unsolicited Neighbor Advertisements for NAT IPv6 VIPs
- Join solicited-node multicast; answer Neighbor Solicitations
- Soft-fail to ARP-only without IPv6 link-local

DSR VIPs are not NDP-announced by the speaker (backend owns on-link VIP).

Coverage: `make selftest-ndp` → `TestNDPRespondsOnVeth`.

## BGP `/128`

See [BGP/BFD HA](bgp.md). Set `bgp.ipv6NextHop` / `-bgp-ipv6-next-hop` or
IPv6 VIPs are not advertised.

## Maps and checksums

IPv6 siblings: `vip_map`, `backend_map`, `connection_affinity_map`,
`nat_reverse_map`, `rl_buckets_map`. Shared AF-agnostic maps listed in
[Architecture](architecture.md).

1. No IPv6 IP-header checksum.
2. UDP checksum is never “unset” over IPv6.

## Verification matrix

| Layer | How |
| --- | --- |
| Dataplane DSR/NAT/Maglev/failover | `scripts/selftest-ipv6.sh` |
| NDP | `scripts/selftest-ndp.sh` |
| Sparse IPAM | `go test ./internal/ipam` |
| BGP `/128` | `go test ./internal/bgp` |
| Examples Load | `go test ./internal/config` |
| Helm IPv6 + `ipv6NextHop` | CI `helm` job |
| AddressPool sample | CI `crd` job |

Still ahead as **live-cluster** hardening: dual-stack Service traffic on
the remote test cluster; BGP IPv6 peering to a real router.
