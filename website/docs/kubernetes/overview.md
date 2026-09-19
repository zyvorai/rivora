---
sidebar_position: 1
title: Overview
---

# Kubernetes

Rivora is a `LoadBalancer` implementation for Kubernetes and bare metal. Install it with the
[Helm chart](helm.md) (or `rivora install`), hand it an `AddressPool`, and any Service of `type:
LoadBalancer` gets an address, is programmed into the dataplane on every node, and is announced to your
network by ARP/NDP or BGP.

```sh
rivora install --set rivorad.interface=eth0 \
  --set addressPools[0].name=default --set addressPools[0].addresses='{10.0.0.0/24}'
kubectl create deployment web --image=nginx && kubectl expose deployment web --type=LoadBalancer --port=80
kubectl get service web        # EXTERNAL-IP is assigned from the pool
```

## The pieces

| Piece | Runs as | Does |
| --- | --- | --- |
| **`rivora-controller`** | Deployment, 2 replicas, one Lease-elected leader | IPAM: assigns an address from an `AddressPool` to each `LoadBalancer` Service (and Gateway), releases it on delete (a finalizer stops the address leaking). The only writer of Gateway route and listener status. |
| **`rivorad -kubernetes`** | DaemonSet, privileged, host network | On every node: attaches XDP/TCX to `rivorad.interface`, turns each Service and its EndpointSlices into a programmed VIP, health-checks backends, and announces VIPs. |
| **L2 speaker** | inside `rivorad`, behind one cluster-wide Lease | One elected node answers ARP and NDP for the VIPs. On by default (`rivorad.speaker`). |
| **BGP speaker** | inside `rivorad`, opt-in | Every node advertises a `/32` or `/128` for each VIP it has a healthy backend for; upstream routers ECMP across nodes. An alternative to the L2 speaker: see [BGP](../operations/bgp.md). |

A node runs Kubernetes-managed VIPs **or** static-YAML VIPs, not both.

## What a Service gets

`rivorad` builds one VIP per `(address, port, protocol)` of the Service:

- **Backends** are the Service's *ready* EndpointSlice endpoints, of the **same address family as the
  VIP** (a v6 VIP programs only v6 endpoints; a dual-stack Service gets one VIP per family). A terminating
  endpoint stays in as *draining*: it takes no new flows but its established ones finish.
- **Mode** is full-NAT. DSR would need the VIP bound into pods, which needs a CNI-specific design.
- **Health** is an active TCP connect to each endpoint (or whatever the Service's
  [`ServicePolicy`](service-policy.md) asks for), on top of Kubernetes readiness.
- **Session affinity**: `sessionAffinity: ClientIP` is honoured (no timeout: see the
  [runbook](../operations/runbook.md#kubernetes-service-semantics-session-affinity-and-externaltrafficpolicy)).
- **`externalTrafficPolicy: Local`** is honoured only with BGP on and `rivorad.speaker: false`; otherwise it
  is treated as `Cluster` with a once-per-Service warning. The client address is preserved either way.
- **Per-Service tuning** (probe, per-source rate limit, endpoint weights, BGP communities and peers) comes
  from a `ServicePolicy` beside the Service.
- Ports: a Service already yields one VIP per entry in `spec.ports`; Kubernetes has no port-range Service.
  SCTP ports are skipped.

To pick a pool, or ask for an address, use the standard fields:

```yaml
metadata:
  annotations:
    rivora.zyvor.dev/address-pool: v6      # a specific pool (default: any pool with autoAssign)
spec:
  type: LoadBalancer
  loadBalancerIP: 10.0.0.42                # a specific address (must be free and in a pool)
```

`spec.loadBalancerClass` scopes which Services Rivora manages: `rivorad.loadBalancerClass` and
`controller.loadBalancerClass` must be equal. Empty manages every `LoadBalancer` Service with no class set.

## Custom resources

Rivora defines three (group `rivora.zyvor.dev`, version `v1alpha1`), described in the
[CRD reference](crds.md): **`AddressPool`** (cluster-scoped: where addresses come from),
**`ServicePolicy`** (namespaced: per-Service tuning) and **`BGPPeer`** (cluster-scoped: a BGP neighbour).
Gateway API objects are supported separately: see [Gateway API](gateway-api.md).

## Backends beyond Pods

The reconciler only reads EndpointSlice addresses, conditions and ports, so these work with no special
casing:

- **KubeVirt VMs**: a normal Service selecting the VMI's labels (KubeVirt propagates them to the
  virt-launcher pod) yields an ordinary EndpointSlice.
- **External or physical IPs**: a Service with no `spec.selector` plus a hand-written EndpointSlice labelled
  `kubernetes.io/service-name: <service>`. Kubernetes leaves manually created slices alone when the Service
  has no selector.

Both were verified against a live cluster. Not supported: a VM's *secondary* (Multus) interface.

## Restarts and upgrades

`rivorad` adopts what the pinned BPF maps hold when it starts, so a restarted node keeps forwarding its VIPs
and reclaims them as the reconcilers catch up; VIPs whose Service was deleted while it was down are removed
once the first pass completes. With `-persist-datapath` there is no traffic gap at all. A DaemonSet rolling
update goes one node at a time. See [Restarting rivorad](../operations/runbook.md#restarting-rivorad) and
[Helm: upgrading](helm.md#upgrading).

## Read next

[Helm chart](helm.md) · [CRD reference](crds.md) · [ServicePolicy](service-policy.md) ·
[Gateway API](gateway-api.md) · [BGP](../operations/bgp.md) · [Runbook](../operations/runbook.md)
