# rivora

Kubernetes integration for [Rivora](https://github.com/zyvorai/rivora): a
per-node `rivorad` DaemonSet (eBPF dataplane + Service/EndpointSlice
reconciler + L2/ARP speaker) and the cluster-scoped `rivora-controller`
Deployment (IPAM).

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

## Gateway API

Setting `gatewayApi.enabled=true` turns on a second control loop, on both
`rivorad` and `rivora-controller`, that watches `Gateway`/`TCPRoute`/
`UDPRoute` (the Gateway API's L4 "experimental channel" resources) instead
of `Service`. `HTTPRoute` is deliberately out of scope — Rivora's XDP
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
an `AddressPool` the same way a `type: LoadBalancer` Service does;
`TCPRoute`/`UDPRoute` objects with `parentRefs` pointing at that `Gateway`
supply the backends (`backendRefs[].weight` maps onto Rivora's existing
weighted-Maglev backend selection).

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
