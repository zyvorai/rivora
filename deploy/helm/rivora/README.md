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
