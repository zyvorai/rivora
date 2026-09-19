---
sidebar_position: 5
---

# ServicePolicy: per-Service tuning

Kubernetes' Service API has no field for how a load balancer should probe its
backends, throttle clients or weight endpoints. A `ServicePolicy` fills that gap
for one Service, in the Service's namespace:

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: ServicePolicy
metadata:
  name: web
  namespace: shop
spec:
  targetRef:
    name: web                  # a Service in this namespace
  healthCheck:                 # replaces the default TCP connect
    type: http
    path: /healthz
    expectStatus: "200-299"
  rateLimit:                   # per source address, new TCP connections (SYNs)
    perSourcePacketsPerSecond: 50
    burst: 100
  weights:                     # relative shares of new connections
    default: 1
    nodes:
      big-node-1: 4
      big-node-2: 4
```

Every field is optional; a setting you leave out keeps its default, so a policy
that only sets `healthCheck` changes only that.

## What each setting does

- **`healthCheck`** is the same probe a static config's `healthCheck` takes (see
  [the runbook](../operations/runbook.md)): `tcp` (default) or `http`, an optional
  probe `port`, and for `http` a `path`, `host` and `expectStatus`.
- **`rateLimit`** caps how fast **one source address** may open new TCP
  connections (SYN packets) to this Service's VIPs. It **replaces** the node-wide
  `-rate-limit` for these VIPs, in either direction: a generous policy exempts a
  Service from a tight node-wide limit, and a Service without one follows the
  node-wide limit. Established connections and UDP are not limited. Like the
  node-wide limit, each CPU keeps its own bucket, so the configured rate is divided
  by the CPU count.
- **`weights`** gives an endpoint the weight of the **node it runs on**
  (`nodes[name]`), else `default`, else 1. Weights are relative (4 gets four times
  the new connections of 1) and range 1 to 1000. Pod IPs are ephemeral, so weights
  key on the stable thing an endpoint carries, its node. To take a node out of
  rotation, drain it rather than weighting it to 0.

## Rules worth knowing

- **One policy per Service.** If several target the same Service, the **oldest**
  applies (then the lowest name) and the rest are logged as ignored, so adding a
  second policy never changes what is in force.
- **An invalid policy applies nothing.** A typo in one field must not leave the
  others half-applied with the intended safeguard missing, so `rivorad` logs the
  problem (once per edit) and the Service keeps its defaults.
- **A backend shared by two Services is probed once.** If two Services select the
  same pods and their policies ask for different probes, the last one applied wins.
  Give both the same probe, or keep such Services on one policy.
- Only Services are supported as a target. Gateway API routes are not covered.

## Enabling it

The Helm chart installs the CRD and turns it on (`servicePolicy.enabled`, default
`true`), which adds the RBAC and passes `-service-policy=true` to `rivorad`.

**Helm installs a chart's CRDs only on first install, never on upgrade.** If you are upgrading
from a release that predates ServicePolicy, apply the CRD yourself first:

```sh
kubectl apply -f deploy/helm/rivora/crds/servicepolicy-crd.yaml
```

If you forget, `rivorad` checks at start-up, logs `ServicePolicy is disabled: the CRD is not
installed`, and carries on serving Services without policies (an informer on a CRD the cluster
does not serve would otherwise never sync and stall every Service). Restart it after applying the
CRD.

The same rate limit is available to static configs as a VIP's `rateLimit` block
(`config/examples/vip-rate-limit.yaml`).
