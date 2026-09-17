---
sidebar_position: 1
title: Overview
---

# Kubernetes

`rivorad -kubernetes` replaces the static-YAML VIP set with a live
`Service` (type=LoadBalancer) / `EndpointSlice` reconciler. A node runs
**either** static YAML **or** Kubernetes-managed VIPs, not both.

```sh
rivorad -kubernetes -interface eth0 [-loadbalancer-class <class>] [-speaker=true]
```

## IPAM (`rivora-controller`)

Leader-elected, cluster-scoped Deployment. Watches `AddressPool` CRs and
patches `Service.Status.LoadBalancer.Ingress[].IP` (and Gateway addresses
when Gateway API is on). Pools may be IPv4, IPv6, or both — see
[IPv6](../core-concepts/ipv6.md).

## L2 speaker (ARP + NDP)

In-`rivorad` speaker behind one cluster-wide Lease announces NAT-mode
VIPs. Default on in Helm (`rivorad.speaker`). Soft-fails to ARP-only if
the iface has no IPv6 link-local.

## Backends beyond Pods

The reconciler only reads `EndpointSlice` addresses — no special casing:

- **KubeVirt VMs** — normal Service selector on the VMI / virt-launcher Pod.
- **External / physical IPs** — Service with no selector + hand-authored
  `EndpointSlice` labeled `kubernetes.io/service-name: <service>`.

Not yet: Multus secondary NIC targeting.

## Notes

- `rivorad.loadBalancerClass` and `controller.loadBalancerClass` must match.
- K8s VIPs are NAT-only today.
- Same-family backends only (v6 VIP → v6 endpoints).

See also [Helm](helm.md) and [Gateway API](gateway-api.md).
