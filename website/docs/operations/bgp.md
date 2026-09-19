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

## Peer options

Each entry under `bgp.peers` accepts more than an address and an AS
([`config/examples/bgp-options.yaml`](https://github.com/zyvorai/rivora/blob/main/config/examples/bgp-options.yaml)):

| Option | What it does |
| --- | --- |
| `password` / `passwordFile` | **TCP MD5** authentication (RFC 2385). The peer must use the same password. Prefer `passwordFile` (a mounted Secret) so it stays out of committed configs. Max 80 bytes. A session with a wrong or missing password never establishes. |
| `multihop` | TTL (2-255) for an **eBGP** peer that is not directly connected. Without it an eBGP session does not cross a router (the default TTL is too small). Refused on an iBGP peer. |
| `gracefulRestart` | `{enabled, restartTime}`: negotiates graceful restart (RFC 4724, default 120 s) so the peer keeps this node's routes for `restartTime` seconds if the session drops, rather than withdrawing them at once. |
| `nodes` | Node names this peer applies to. Unset means every node, so one shared config can give each rack its own router. Needs `-node-name` (the chart sets it) or the hostname. |
| `bfd` | as before |

## Route attributes and aggregation

- **`bgp.communities`** are attached to every route this node originates, and a VIP's own
  **`bgpCommunities`** (or a Kubernetes [`ServicePolicy`](../kubernetes/service-policy.md)'s
  `spec.bgp.communities`) are added for that VIP's route: `"65001:100"` (both halves 0-65535) or
  `no-export`, `no-advertise`, `no-export-subconfed`. Changing them re-advertises the route.
- **`bgp.localPref`** sets LOCAL_PREF on originated routes. It is only carried to **iBGP** peers
  (your own AS); an eBGP peer never receives it.
- **`bgp.aggregates`** advertise a covering prefix (say a /24) while at least one VIP inside it has
  a healthy backend, and withdraw it when none does. `suppressSpecifics: true` stops the covered
  VIPs' host routes being advertised while the aggregate is; otherwise both go out. Give an
  aggregate its own `communities`.

## Kubernetes: `-bgp-config`

`-bgp-peers` only says address, AS and BFD. For anything above, put a `bgp:` section (the same schema
as the static config) in a Secret and point the chart at it:

```sh
kubectl -n rivora-system create secret generic rivora-bgp --from-file=bgp.yaml=./bgp.yaml
helm upgrade rivora deploy/helm/rivora --reuse-values --set bgp.enabled=true --set bgp.configSecret=rivora-bgp
```

`bgp.configSecret` **replaces** `bgp.asn`, `routerId`, `ipv6NextHop` and `peers`; rivorad gets
`-bgp-config=/etc/rivora/bgp/bgp.yaml`. Passwords then live only in the Secret, not on a command line or
in `helm get values`.

## Not implemented

A **peer CRD** (peers still come from the static config, `-bgp-peers` or `-bgp-config`, and need a restart
to change), **per-service peer selection** (a VIP is advertised to every peer), MD5 for BFD, GTSM, and
BGP-level local-pref/MED per VIP.

## Full-NAT / ECMP caveat

K8s VIPs are NAT-only. Connection state lives on the node that first
received the flow. ECMP rehash can disrupt in-flight NAT'd connections —
same tradeoff MetalLB documents. DSR (static YAML) avoids this.

## Verification

`go test ./internal/bgp` covers `/32` and `/128` advertise/withdraw, communities, local-pref,
aggregates and graceful-restart negotiation against a loopback gobgp peer; run as root on Linux it also
covers TCP MD5 (match, mismatch and password from a file). `scripts/selftest-bgp.sh` runs a real
`rivorad` against a peer **two hops away, across a router**: multihop and MD5 establish the session and
the VIP route arrives with its communities, while a missing multihop, a wrong password and a missing
password each keep it down. Live FRR/BIRD peering is still a follow-up.
