---
sidebar_position: 4
title: CRD reference
---

# Custom resource reference

Group `rivora.zyvor.dev`, version `v1alpha1`. The manifests are in
[`deploy/helm/rivora/crds/`](https://github.com/zyvorai/rivora/tree/main/deploy/helm/rivora/crds). Helm
installs them on first install and never upgrades them: see [CRD lifecycle](helm.md#crd-lifecycle).

| Kind | Scope | Short name | Read by | Enabled by |
| --- | --- | --- | --- | --- |
| [`AddressPool`](#addresspool) | Cluster | | `rivora-controller` | always |
| [`ServicePolicy`](#servicepolicy) | Namespaced | `rsp` | `rivorad` | `servicePolicy.enabled` (default on) |
| [`BGPPeer`](#bgppeer) | Cluster | `rbp` | `rivorad` | `bgp.enabled` and `bgp.peerResources` (default on) |

## AddressPool

Where LoadBalancer addresses come from.

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: AddressPool
metadata: {name: default}
spec:
  addresses:
    - 10.0.0.0/24
    - 10.0.1.10-10.0.1.40
    - 2001:db8:1::/64
  autoAssign: true
  avoidBuggyIPs: true
```

| Field | Meaning |
| --- | --- |
| `spec.addresses` | **Required.** CIDRs, ranges (`a-b`) or single addresses, IPv4 and/or IPv6. Large IPv6 prefixes (over 16 host bits) are allocated sparsely, not enumerated. |
| `spec.autoAssign` | Whether Services that do not name a pool may use this one. Default `true`. A Service picks a specific pool with the `rivora.zyvor.dev/address-pool` annotation. |
| `spec.avoidBuggyIPs` | Skip an IPv4 CIDR's `.0` and `.255` (some old stacks mishandle them). Ignored for IPv6. |
| `spec.protocol` | Reserved. |
| `status.availableIPs`, `status.assignedIPs` | Coarse counts, refreshed periodically for observability. The allocator does not read them: an allocation is recorded on the Service itself. |

A Service may also ask for a specific address with `spec.loadBalancerIP`. Allocation state lives on the
Services (guarded by the `rivora.zyvor.dev/ipam` finalizer), so the pool object is never a point of contention.

## ServicePolicy

Per-Service tuning for what the Service API has no field for. Full guide, with the rules for conflicts and
invalid policies: [ServicePolicy](service-policy.md).

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: ServicePolicy
metadata: {name: web, namespace: shop}
spec:
  targetRef: {name: web}              # a Service in this namespace
  healthCheck: {type: http, path: /healthz, expectStatus: "200-299"}
  rateLimit: {perSourcePacketsPerSecond: 50, burst: 100}
  weights: {default: 1, nodes: {big-node-1: 4}}
  bgp: {communities: ["65001:7"], peers: [10.0.1.1]}
```

| Field | Meaning |
| --- | --- |
| `targetRef.name` | **Required.** The Service, in the policy's namespace. `targetRef.kind` may only be `Service`. |
| `healthCheck` | `type` (`tcp`, `http`), `port`, and for `http` `path`, `host`, `expectStatus`. Replaces the default TCP connect. |
| `rateLimit` | `perSourcePacketsPerSecond` and `burst`, both above 0. Per-source new-connection (SYN) limit that replaces the node-wide limit for this Service's VIPs. |
| `weights` | `default` and `nodes` (node name to weight), 1 to 1000. An endpoint gets its node's weight, else `default`, else 1. |
| `bgp.communities` | Communities added to this Service's route. |
| `bgp.peers` | Addresses of the BGP peers this Service's route is sent to. Unset means every peer. |

## BGPPeer

A BGP neighbour, declared in the cluster and added to the peers a node started with. Full guide:
[BGP: BGPPeer resources](../operations/bgp.md#bgppeer-resources-kubernetes).

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: BGPPeer
metadata: {name: rack1-tor}
spec:
  address: 10.0.1.1
  asn: 65000
  bfd: true
  multihop: 3
  gracefulRestart: {enabled: true, restartTime: 120}
  passwordSecretRef: {name: rack1-tor-bgp, key: password}   # Secret in rivorad's namespace
  nodeSelector: {matchLabels: {topology.kubernetes.io/rack: "1"}}
```

| Field | Meaning |
| --- | --- |
| `address` | **Required.** The neighbour's IPv4 or IPv6 address. |
| `asn` | **Required.** The neighbour's AS number. |
| `bfd` | Enable BFD on the session. |
| `multihop` | TTL (2 to 255) for an eBGP neighbour that is not directly connected. Refused for a neighbour in the speaker's own AS. |
| `gracefulRestart` | `enabled` and `restartTime` (1 to 4095 seconds, default 120). |
| `passwordSecretRef` | `name` and `key` (default `password`) of a Secret **in the namespace `rivorad` runs in**: the TCP MD5 password. A peer whose Secret cannot be read is not started. |
| `nodeSelector` | A label selector matched against the labels of the node `rivorad` runs on. Unset means every node. |

`BGPPeer` has no `status`: every node would write to the same object. Session state is in the
`rivora_bgp_*` [metrics](../operations/metrics.md#bgp-rivorad-with-bgp-on).
