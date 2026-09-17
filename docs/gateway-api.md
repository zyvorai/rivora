# Gateway API

`rivorad -kubernetes -gateway-api` runs a second reconciler alongside
Service/EndpointSlice: `GatewayClass`, `Gateway`, and L4 experimental
`TCPRoute` / `UDPRoute` (`gateway.networking.k8s.io`).

!!! warning "HTTPRoute is out of scope"
    Rivora's XDP dataplane has no L7 visibility. Claiming to enforce
    HTTPRoute path/header matching would be a correctness hazard.

```sh
rivorad -kubernetes -interface eth0 -gateway-api [-speaker=true]
rivora-controller -gateway-api   # IPAM into Gateway.status.addresses
```

Address assignment uses the same `AddressPool`s as Services.
`backendRefs[].weight` maps to weighted Maglev (split evenly across ready
endpoints). Same-namespace `backendRefs` only in this version (no
`ReferenceGrant` yet).

## Helm

Install experimental Gateway API CRDs yourself first (not bundled):

```sh
kubectl kustomize \
  "https://github.com/kubernetes-sigs/gateway-api/config/crd/experimental?ref=v1.1.0" \
  | kubectl apply -f -

helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set gatewayApi.enabled=true \
  --set gatewayClass.create=true
```

## Verification status

Reconciler startup, informer sync, and `GatewayClass` creation confirmed
live. Full traffic-path scenarios (assignment + packets + weighted split)
were blocked by an unrelated cluster networking issue on the test host —
retry once that clears.
