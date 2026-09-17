# rivora

Kubernetes integration for [Rivora](https://github.com/zyvorai/rivora): a
per-node `rivorad` DaemonSet (eBPF dataplane + Service/EndpointSlice
reconciler + L2 ARP+NDP speaker + optional BGP) and the cluster-scoped
`rivora-controller` Deployment (IPAM for Services and optional Gateway
API).

## Install

```sh
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
```

`rivorad.interface` is required — the host network interface every node
attaches XDP/TCX to (see `values.yaml`).

### Dual-stack (IPv4 + IPv6 pools)

Large IPv6 prefixes (e.g. `/64`) allocate **sparsely** — the controller
does not expand 2^64 addresses into memory. See the top-level
[IPv6](../../../README.md#ipv6-v03) docs.

```sh
helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}' \
  --set addressPools[1].name=v6 \
  --set addressPools[1].addresses='{2001:db8:1::/64}'
```

Equivalent values file:

```yaml
addressPools:
  - name: default
    addresses: ["10.0.0.0/24"]
    autoAssign: true
  - name: v6
    addresses: ["2001:db8:1::/64"]
    autoAssign: true
```

CI sample: [`ci/addresspool-ipv6.yaml`](ci/addresspool-ipv6.yaml).

## The CRD is not managed by upgrade/uninstall

`crds/addresspool-crd.yaml` lives at the chart root (not
`templates/crds/`), which is Helm's documented convention for CRDs: Helm
installs it on the first `helm install`, but **never upgrades or deletes
it** on subsequent `helm upgrade` or `helm uninstall` — this is intentional
upstream behavior, not a bug in this chart. If a future chart version
changes the `AddressPool` schema, apply the updated CRD yourself:

```sh
kubectl apply -f deploy/helm/rivora/crds/addresspool-crd.yaml
```

And `helm uninstall` leaves both the CRD and any `AddressPool` objects in
place — delete them explicitly if you want them gone:

```sh
kubectl delete addresspools.rivora.zyvor.dev --all
kubectl delete -f deploy/helm/rivora/crds/addresspool-crd.yaml
```

The OpenAPI description on `spec.addresses` documents IPv6 CIDRs/ranges
and sparse `/64` allocation; keep that text in sync when changing IPAM.

## L2 speaker (ARP + NDP)

`rivorad.speaker` defaults to `true`. The elected speaker (cluster-wide
Lease) answers ARP for IPv4 NAT VIPs and NDP (Neighbor Solicitation →
Advertisement, plus unsolicited NAs) for IPv6 NAT VIPs on
`rivorad.interface`. Disable only if something else on the cluster already
owns ARP/NDP for these addresses.

```sh
--set rivorad.speaker=false
```

## Gateway API

Setting `gatewayApi.enabled=true` turns on a second control loop, on both
`rivorad` and `rivora-controller`, that watches `Gateway`/`TCPRoute`/
`UDPRoute` (the Gateway API's L4 "experimental channel" resources) instead
of only `Service`. `HTTPRoute` is deliberately out of scope — Rivora's XDP
dataplane has no L7 visibility, so it can't enforce HTTPRoute's path/header
matching rules.

This chart does **not** bundle the Gateway API CRDs — like any other
vendor's CRDs, install them yourself first:

```sh
kubectl kustomize "https://github.com/kubernetes-sigs/gateway-api/config/crd/experimental?ref=v1.1.0" | kubectl apply -f -
```

Then enable the feature, optionally letting the chart create a
`GatewayClass` for you:

```sh
helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set gatewayApi.enabled=true \
  --set gatewayClass.create=true
```

A `Gateway` referencing this `GatewayClass` gets an address assigned from
an `AddressPool` the same way a `type: LoadBalancer` Service does
(IPv4 and/or IPv6 depending on pools and Gateway listeners);
`TCPRoute`/`UDPRoute` objects with `parentRefs` pointing at that `Gateway`
supply the backends (`backendRefs[].weight` maps onto Rivora's existing
weighted-Maglev backend selection). Same-family backends only per VIP.

## BGP/BFD HA

Setting `bgp.enabled=true` turns on rivorad's BGP+BFD speaker: active/
active ECMP HA where every node independently advertises a `/32` (IPv4)
or `/128` (IPv6) host route for each VIP it currently has a healthy
backend for, and withdraws it the instant that stops being true. Peers
negotiate both unicast families. Unlike Gateway API, this is `rivorad`-
only — `rivora-controller` isn't involved, since BGP doesn't need
cluster-wide IPAM coordination.

```sh
# IPv4 ads (routerId is BGP ID + IPv4 next-hop)
helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set bgp.enabled=true \
  --set bgp.asn=65001 \
  --set bgp.routerId=10.0.0.11 \
  --set "bgp.peers[0].address=10.0.0.1" \
  --set "bgp.peers[0].asn=65000" \
  --set "bgp.peers[0].bfd=true"

# Also advertise IPv6 VIP /128s — ipv6NextHop is required
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

| Value | Meaning |
| --- | --- |
| `bgp.asn` | Local AS |
| `bgp.routerId` | BGP identifier + IPv4 next-hop for `/32` |
| `bgp.ipv6NextHop` | IPv6 next-hop for `/128` (omit → IPv6 VIPs not advertised) |
| `bgp.peers[].address` / `.asn` / `.bfd` | Peer list; `bfd: true` enables BFD on that peer |

Read the top-level README's [BGP/BFD HA](../../../README.md#bgpbfd-ha-v03)
section before enabling this — it covers the health-gated
advertise/withdraw model and a real caveat: in full-NAT mode (the
**only** mode K8s-managed VIPs currently run in, per the note below), a
router-side ECMP rehash can disrupt in-flight connections on a node that's
rebalanced away from, since connection state isn't shared across nodes.

## Observability

Both `rivorad` and `rivora-controller` serve `/healthz`, `/readyz` and
`/metrics` (Prometheus text format) on a plain-HTTP, no-auth port `9871` —
wired into the DaemonSet/Deployment's liveness/readiness probes and
annotated `prometheus.io/scrape: "true"` for annotation-based scrape
configs. `rivorad` runs `hostNetwork: true`, which is why this is a
*separate* port from its main API (`9870`, loopback-only, optionally
TLS'd/authenticated per [Securing the API](../../../README.md#securing-the-api)) —
127.0.0.1 on a hostNetwork Pod is the node's own loopback, unreachable to
an in-cluster Prometheus. `rivora_*` metrics cover per-VIP/backend
packets/bytes/health from the BPF maps; `rivora_controller_leader` reports
which `rivora-controller` replica currently holds the leader-election
Lease.

If you run the Prometheus Operator instead of annotation-based scraping,
add your own `PodMonitor` targeting port `9871` on both workloads — this
chart doesn't bundle `monitoring.coreos.com` CRDs, the same policy it
applies to the Gateway API CRDs above.

## Values reference (IPv6-related)

| Path | Default | Notes |
| --- | --- | --- |
| `rivorad.interface` | `""` (required) | Host iface for XDP/TCX + speaker |
| `rivorad.speaker` | `true` | ARP+NDP Lease speaker |
| `gatewayApi.enabled` | `false` | Service + Gateway control loops |
| `gatewayClass.create` / `.name` | `false` / `rivora` | Optional GatewayClass |
| `bgp.enabled` | `false` | Per-node BGP speaker |
| `bgp.ipv6NextHop` | `""` | Required for IPv6 `/128` ads |
| `addressPools[]` | `[]` | Seeded `AddressPool` CRs; IPv6 `/64` OK |
| `controller.podDisruptionBudget.enabled` / `.minAvailable` | `true` / `1` | Keeps a controller replica up during node drains |
| `controller.networkPolicy.enabled` | `false` | Restrict ingress to the controller's metrics port |

Full defaults and comments: [`values.yaml`](values.yaml).

## Notes

- `rivorad.loadBalancerClass` and `controller.loadBalancerClass` must
  match — both sides need to agree on which `Service` objects they manage.
  Leave both `""` to manage every `type: LoadBalancer` Service with no
  `spec.loadBalancerClass` set (the common single-LB-controller case).
- K8s-managed VIPs are NAT-only in v0.2 — DSR isn't available through this
  path yet.
- A node running this DaemonSet manages only K8s-sourced VIPs; the
  static-YAML `-config` path (see the top-level README) is a separate,
  non-Kubernetes deployment mode and isn't used here.
- Dual-stack Services get one programmed VIP per family; backends are
  filtered to matching EndpointSlice address types.
