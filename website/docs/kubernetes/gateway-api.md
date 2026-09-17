---
sidebar_position: 2
title: Gateway API
---

# Gateway API

`rivorad -kubernetes -gateway-api` runs a second reconciler alongside
Service/EndpointSlice: `GatewayClass`, `Gateway`, and L4 experimental
`TCPRoute` / `UDPRoute`.

:::warning HTTPRoute is out of scope
Rivora's XDP dataplane has no L7 visibility. Claiming to enforce
HTTPRoute path/header matching would be a correctness hazard.
:::

```sh
rivorad -kubernetes -interface eth0 -gateway-api [-speaker=true]
rivora-controller -gateway-api
```

Address assignment uses the same `AddressPool`s as Services.
`backendRefs[].weight` maps to weighted Maglev. Same-namespace
`backendRefs` only in this version (no `ReferenceGrant` yet).

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
live. Full traffic-path scenarios pending an unrelated cluster networking
issue on the test host.
