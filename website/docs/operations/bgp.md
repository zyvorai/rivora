---
sidebar_position: 1
title: BGP/BFD HA
---

# BGP/BFD HA

Opt-in **active/active ECMP** HA on `rivorad`. Every node with BGP enabled
independently advertises a host route for each VIP with at least one
healthy backend — and withdraws when that stops being true.

| Family | Prefix | Next-hop source |
| --- | --- | --- |
| IPv4 | `/32` | `bgp.routerId` / `-bgp-router-id` |
| IPv6 | `/128` | `bgp.ipv6NextHop` / `-bgp-ipv6-next-hop` (**required** for IPv6 ads) |

Sessions negotiate **both** unicast AFI/SAFIs. BFD is per-peer.
`rivora-controller` is not involved.

## Flags / Helm

```sh
rivorad -kubernetes -interface eth0 \
  -bgp -bgp-asn 65001 -bgp-router-id 10.0.0.11 \
  -bgp-ipv6-next-hop 2001:db8::11 \
  -bgp-peers "10.0.0.1:65000:bfd,10.0.0.2:65000"
```

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

Static YAML: [`config/examples/bgp-ha.yaml`](https://github.com/zyvorai/rivora/blob/main/config/examples/bgp-ha.yaml).

## Full-NAT / ECMP caveat

K8s VIPs are NAT-only. Connection state lives on the node that first
received the flow. ECMP rehash can disrupt in-flight NAT'd connections —
same tradeoff MetalLB documents. DSR (static YAML) avoids this.

## Verification

`go test ./internal/bgp` covers `/32` and `/128` advertise/withdraw
against a loopback gobgp peer. Live FRR/BIRD peering is a follow-up.
