---
sidebar_position: 5
title: BGP/BFD HA
---

# BGP/BFD HA

Opt-in **active/active ECMP** HA on `rivorad`. Every node with BGP enabled **independently** advertises a
host route for each VIP it currently has at least one healthy backend for, and withdraws it the moment that
stops being true. Upstream routers then spread traffic across every node advertising the VIP (ECMP), and a
node whose backends all die simply stops attracting traffic. There is no leader election: multiple nodes
advertising the same VIP at once is the point.

| Family | Prefix | Next-hop source |
| --- | --- | --- |
| IPv4 | `/32` | `bgp.routerId` / `-bgp-router-id` |
| IPv6 | `/128` | `bgp.ipv6NextHop` / `-bgp-ipv6-next-hop` (**required** for IPv6 ads; without it IPv4 still works and IPv6 VIPs are skipped with an error) |

Sessions negotiate **both** unicast families, so one peer carries IPv4 and IPv6 VIPs together. BFD is
per-peer and, when enabled, gives sub-second peer-down detection that feeds the same withdraw path.
`rivora-controller` is not involved: BGP needs no cluster-wide coordination.

## BGP or the L2 speaker?

Both announce a VIP; pick one per deployment.

| | L2 speaker (ARP + NDP) | BGP |
| --- | --- | --- |
| Where VIPs must live | On the nodes' own L2 segment | Anywhere your routers can route to |
| Which node carries a VIP | **One** elected node at a time (a Lease) | **Every** node with a healthy backend (ECMP) |
| Failover | Lease handover, then a gratuitous ARP / NA | Route withdrawal (sub-second with BFD) |
| Needs | Nothing on the network | A BGP-speaking router or top-of-rack switch |
| `externalTrafficPolicy: Local` | Not honoured | Honoured (with `rivorad.speaker: false`) |

## On the router side

Accept `/32` (and `/128`) routes from the nodes and enable ECMP so it uses more than one next hop. The
nodes' next hop is their `routerId` (IPv4) or `ipv6NextHop`, so those must be reachable from the router.
Configure BFD on the router too if a peer has `bfd: true`. Use `multihop` when the router is more than one hop
from the node, and TCP MD5 (`password`) if the session crosses a network you do not control.

## Full-NAT and ECMP

A full-NAT flow's state lives on the node that first received it. If the router re-hashes (a peer flaps, a node
joins or leaves the ECMP set), an in-flight NAT'd connection can land on a node with no state for it and reset.
DSR has no such problem, since the load balancer is only ever on the forward path; that makes it the better fit
for BGP where you can use it. This is the same trade-off other BGP-mode load balancers document, and inherent
to stateful NAT with ECMP.

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
| `bfd` | Bidirectional Forwarding Detection on the session, for sub-second peer-down detection. The router must enable it too. |

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

## BGPPeer resources (Kubernetes)

Peers can also be declared in the cluster, and change without a restart. With `bgp.peerResources`
(on by default with `bgp.enabled`; the flag is `-bgp-peer-resources`) `rivorad` watches cluster-scoped
`BGPPeer` objects and adds each one to the peers the node started with:

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: BGPPeer
metadata:
  name: rack1-tor
spec:
  address: 10.0.1.1
  asn: 65000
  bfd: true
  gracefulRestart: {enabled: true, restartTime: 120}
  passwordSecretRef: {name: rack1-tor-bgp}      # key "password", in the namespace rivorad runs in
  nodeSelector: {matchLabels: {topology.kubernetes.io/rack: "1"}}
```

- **`nodeSelector`** is matched against the labels of the node `rivorad` runs on (it needs
  `NODE_NAME`, which the chart sets). Unset means every node. A peer for another rack is skipped
  without comment.
- **`passwordSecretRef`** reads the TCP MD5 password from a Secret in `rivorad`'s own namespace only
  (that is all the chart grants it), and a rotated Secret takes effect within a minute by restarting
  that session. A peer whose Secret cannot be read is **not started**, and the log says why: it
  would never establish, and starting it without the password would be worse.
- Adding, editing or deleting a `BGPPeer` starts, restarts or stops only that session. Changing a peer
  drops and re-establishes its session; untouched peers are not disturbed.
- A `BGPPeer` for the address of a peer the node started with (flags or `-bgp-config`) is ignored, so it
  cannot quietly replace that peer's settings; two `BGPPeer`s for one address keep the older one. Each
  is reported once in the log. The speaker's own AS, router-id and next-hops still come from the flags or
  `-bgp-config`; with `peerResources` on, the static `peers` list may be empty.
- The CRD is in `crds/`, which Helm does not upgrade: on an existing install apply
  `deploy/helm/rivora/crds/bgppeer-crd.yaml` yourself. Without it `rivorad` logs that BGPPeer
  resources are ignored and carries on.

## Sending a route to only some peers

By default every route goes to every peer. A VIP can name the peers its route is for:

- static config: `bgpPeers: [10.0.0.1]` on the VIP (and `peers: [...]` on an aggregate);
- Kubernetes: `spec.bgp.peers` of the Service's [`ServicePolicy`](../kubernetes/service-policy.md).

Peers are named by address. A peer not named never receives that route (it is not advertised and then
filtered later: it is never exported to that neighbour), so this decides which router, rack or upstream
carries a VIP's traffic. In a static config a name that is not one of the configured peers is rejected;
with `BGPPeer` resources the peers are not known ahead, so the list is not checked, and naming no
running peer means the route goes to nobody (the log says which peers it was for).

When several VIPs share one address (different ports) the route goes to every peer any of them names,
or to all peers as soon as one of them names none. Changing which peers a route is for withdraws and
re-advertises it, so the peers that keep it see it flap once.

**How it works.** gobgp cannot point a path at one neighbour, and its per-neighbour export policy
exists only for route-server clients (a different session behaviour). So the speaker adds, to gobgp's
global export policy, a rule per peer, "when exporting to this neighbour, reject these prefixes", and
keeps that list equal to the routes the peer must not have, updating it before any route is advertised
or changed. A new peer's rule is in place before its session is created, so it never sees a route it
should not. If a list cannot be updated, limited routes are held back (and withdrawals still go out)
until it can.

## Not implemented

MD5 for BFD, GTSM, and BGP-level local-pref/MED per VIP or per peer. A `BGPPeer` has no status
(session state is in the `rivora_bgp_*` metrics and `rivoractl`), since every node would be writing to
the same object.

## Verification

`go test ./internal/bgp` covers `/32` and `/128` advertise/withdraw, communities, local-pref,
aggregates and graceful-restart negotiation against a loopback gobgp peer; run as root on Linux it also
covers TCP MD5 (match, mismatch and password from a file). Peers changing at run time and routes limited
to some peers are tested over real sessions to two loopback routers (one over IPv4, one over IPv6): a
limited route reaches only its routers, through moving it, lifting the limit, and adding a peer later.
`go test ./internal/bgppeers` covers which `BGPPeer`s apply to a node. `scripts/selftest-bgp.sh` runs a real
`rivorad` against a peer **two hops away, across a router**: multihop and MD5 establish the session and
the VIP route arrives with its communities, while a missing multihop, a wrong password and a missing
password each keep it down. Live FRR/BIRD peering is still a follow-up.
