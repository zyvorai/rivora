# BGP/BFD HA

Opt-in **active/active ECMP** HA on `rivorad`. Unlike the L2 ARP/NDP
speaker (one elected node answers for a VIP), every node with BGP enabled
**independently** advertises a host route for each VIP that currently has
at least one healthy backend — and withdraws when that stops being true.

| Family | Prefix | Next-hop source |
| --- | --- | --- |
| IPv4 | `/32` (`RF_IPv4_UC`) | `bgp.routerId` / `-bgp-router-id` |
| IPv6 | `/128` (`RF_IPv6_UC`) | `bgp.ipv6NextHop` / `-bgp-ipv6-next-hop` (**required** to advertise IPv6 VIPs) |

Sessions negotiate **both** unicast AFI/SAFIs. BFD is per-peer for
sub-second peer-down → same health-gated withdraw path.
`rivora-controller` is not involved.

## Static YAML

See [`config/examples/bgp-ha.yaml`](https://github.com/zyvorai/rivora/blob/main/config/examples/bgp-ha.yaml):

```yaml
bgp:
  enabled: true
  asn: 65001
  routerId: 10.0.0.11
  # ipv6NextHop: 2001:db8::11   # uncomment for IPv6 /128 ads
  peers:
    - address: 10.0.0.1
      asn: 65000
      bfd: true
```

## Kubernetes flags

```sh
rivorad -kubernetes -interface eth0 \
  -bgp -bgp-asn 65001 -bgp-router-id 10.0.0.11 \
  -bgp-ipv6-next-hop 2001:db8::11 \
  -bgp-peers "10.0.0.1:65000:bfd,10.0.0.2:65000"
```

`-bgp-peers`: comma-separated `addr:asn` or `addr:asn:bfd`.

## Helm

```sh
helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set bgp.enabled=true \
  --set bgp.asn=65001 \
  --set bgp.routerId=10.0.0.11 \
  --set bgp.ipv6NextHop=2001:db8::11 \
  --set "bgp.peers[0].address=10.0.0.1" \
  --set "bgp.peers[0].asn=65000" \
  --set "bgp.peers[0].bfd=true"
```

## Full-NAT / ECMP caveat

K8s VIPs are NAT-only. Connection state lives on the node that first
received the flow. If the router's ECMP hash rebalances, in-flight NAT'd
connections on a node that lost the hash can break — same tradeoff MetalLB
documents for BGP + stateful NAT. DSR (static YAML) avoids this because
the backend owns the reply path.

## Verification

Health-gated advertise/withdraw for `/32` and `/128` is covered by
`go test ./internal/bgp` (loopback gobgp peer). Live FRR/BIRD peering and
BFD timing on the lab cluster are still a follow-up.
