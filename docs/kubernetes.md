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
[IPv6 → IPAM](ipv6.md#ipam-and-addresspool).

## L2 speaker (ARP + NDP)

In-`rivorad` speaker behind one cluster-wide Lease announces NAT-mode
VIPs on the dataplane interface. Default on in Helm (`rivorad.speaker`).
Requires `CAP_NET_RAW`. Soft-fails to ARP-only if the iface has no IPv6
link-local.

## Install via Helm

```sh
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
```

Dual-stack pool example and chart caveats: [Helm chart](helm.md).

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
