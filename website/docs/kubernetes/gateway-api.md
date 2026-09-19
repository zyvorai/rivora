---
sidebar_position: 2
title: Gateway API
---

# Gateway API

`rivorad -kubernetes -gateway-api` runs a second reconciler alongside
Service/EndpointSlice: `GatewayClass`, `Gateway`, and L4 experimental
`TCPRoute` / `UDPRoute`.

:::warning HTTPRoute, GRPCRoute and TLSRoute are out of scope
Rivora's XDP dataplane forwards packets and has no L7 visibility: it cannot match HTTP paths, headers or
methods (`HTTPRoute`, `GRPCRoute`) or read a TLS ClientHello for SNI routing (`TLSRoute`). Claiming to
enforce those rules would be a correctness hazard, so they are not implemented. For TLS passthrough to a
single backend set, use a `TCPRoute` on a `TCP` listener.
:::

```sh
rivorad -kubernetes -interface eth0 -gateway-api [-speaker=true]
rivora-controller -gateway-api
```

Address assignment uses the same `AddressPool`s as Services.
`backendRefs[].weight` maps to weighted Maglev.

## Which routes attach, and to what

- **Protocol.** A `TCPRoute` attaches only to a `TCP` listener and a `UDPRoute` only to a `UDP`
  one. A `sectionName` picks one listener; without one the route attaches to every listener it
  is allowed to.
- **`allowedRoutes.namespaces`.** By default (`from: Same`) only routes in the Gateway's own
  namespace attach. `from: All` admits any namespace; `from: Selector` admits namespaces whose
  labels match `selector` (a namespace that does not exist never matches). A route names a Gateway
  in another namespace with `parentRefs[].namespace`.
- **`allowedRoutes.kinds`** may narrow the kinds further; it cannot widen them beyond what the
  listener's protocol serves.
- **Cross-namespace `backendRefs`.** A `backendRef` to a Service in another namespace is used only
  if a `ReferenceGrant` **in the Service's namespace** allows it: `from` naming this route's group,
  kind (`TCPRoute`/`UDPRoute`) and namespace, and `to` naming kind `Service` (optionally one name).
  Without one the reference is not used and the route reports `ResolvedRefs: False` with reason
  `RefNotPermitted`. A grant for another route kind, another namespace or another Service does not
  help. Other backend kinds are `InvalidKind`; a missing Service is `BackendNotFound`.

```yaml
# In namespace "data": let TCPRoutes in "apps" reference the db Service.
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata: {name: apps-to-db, namespace: data}
spec:
  from: [{group: gateway.networking.k8s.io, kind: TCPRoute, namespace: apps}]
  to:   [{group: "", kind: Service, name: db}]
```

## Status

`rivora-controller` (the one leader-elected writer; enable `-gateway-api` on it too) reports the
outcome, using the same checks `rivorad` programs from, so what it says is what is programmed:

- On each **route**, one `status.parents` entry per `parentRef` naming a Rivora Gateway, with
  `Accepted` (`Accepted`, `NotAllowedByListeners` or `NoMatchingParent`) and `ResolvedRefs`
  (`ResolvedRefs`, `RefNotPermitted`, `InvalidKind` or `BackendNotFound`). Entries written by other
  controllers are left alone.
- On each **Gateway listener**, `attachedRoutes`, `supportedKinds` (`TCPRoute` for `TCP`,
  `UDPRoute` for `UDP`) and `Accepted`/`ResolvedRefs` conditions.

Status is only written when it changes.

## Still out of scope

`HTTPRoute`, `GRPCRoute` and `TLSRoute` (they need L7 or SNI routing; use a `TCPRoute` for TLS
passthrough), listener `hostname`s, and listener-level TLS. `ReferenceGrant`s and namespace labels
are watched cluster-wide (the chart grants the RBAC).

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

Reconciler start-up, informer sync and `GatewayClass` creation were confirmed on a live cluster; the
traffic-path scenarios (address assignment, real packets through a Gateway VIP, weighted split) were blocked
by an unrelated networking problem on that cluster and **have not been run live**. The attachment,
`ReferenceGrant` and status logic is covered by unit tests against fake clients (`internal/gatewayapi`), and
the VIPs a Gateway produces go through the same dataplane the Service path and the selftests exercise. It has
**not been run against a real cluster** since.

On restart, a Gateway's VIPs are adopted like a Service's and pruned only after both reconcilers' first pass
completes: see [Kubernetes overview](overview.md#restarts-and-upgrades).
