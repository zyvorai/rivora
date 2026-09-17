---
sidebar_position: 3
title: Helm chart
---

# Helm chart

Chart path: [`deploy/helm/rivora`](https://github.com/zyvorai/rivora/tree/main/deploy/helm/rivora).

## Install

```sh
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
```

`rivorad.interface` is **required**.

### Dual-stack pools

```sh
helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}' \
  --set addressPools[1].name=v6 \
  --set addressPools[1].addresses='{2001:db8:1::/64}'
```

Large `/64`s allocate sparsely — see [IPv6](../core-concepts/ipv6.md).

## CRD lifecycle

The CRD lives at chart root `crds/`. Helm installs it on first
`helm install` but **never upgrades or deletes** it on upgrade/uninstall.

```sh
kubectl apply -f deploy/helm/rivora/crds/addresspool-crd.yaml
```

`AddressPool` ships a single version (`v1alpha1`) with no conversion
webhook — most schema changes (new optional fields, loosened validation)
apply in place with the command above, no migration needed. A
conversion webhook is only warranted for an actual breaking change; see
[Evolving the schema](https://github.com/zyvorai/rivora/blob/main/deploy/helm/rivora/README.md#evolving-the-schema)
for the checklist.

## Observability

Both workloads serve `/healthz`, `/readyz` and `/metrics` (Prometheus text
format) on a plain-HTTP, no-auth port `9871` — wired into the
DaemonSet/Deployment's liveness/readiness probes and annotated
`prometheus.io/scrape: "true"`. This is separate from `rivorad`'s main API
port (`9870`, loopback-only, hostNetwork) because a hostNetwork Pod's
127.0.0.1 isn't reachable from an in-cluster Prometheus. `rivora_*`
metrics cover per-VIP/backend packets/bytes/health from the BPF maps;
`rivora_controller_leader` reports which `rivora-controller` replica
holds the leader-election Lease. See the [production
runbook](../operations/runbook.md) for how to read these when something's
wrong.

## Availability

`controller.podDisruptionBudget` (enabled by default, `minAvailable: 1`)
keeps a `rivora-controller` replica up during voluntary node drains.
`controller.networkPolicy` (off by default) restricts ingress to the
controller's metrics port when a NetworkPolicy controller is installed.

## Values (IPv6-related)

| Path | Default | Notes |
| --- | --- | --- |
| `rivorad.interface` | `""` | Required |
| `rivorad.speaker` | `true` | ARP+NDP |
| `gatewayApi.enabled` | `false` | |
| `bgp.enabled` | `false` | |
| `bgp.ipv6NextHop` | `""` | Required for IPv6 `/128` ads |
| `addressPools[]` | `[]` | Seeded pools; IPv6 `/64` OK |
| `controller.podDisruptionBudget.enabled` | `true` | Keeps a replica up during drains |
| `controller.networkPolicy.enabled` | `false` | Restrict ingress to the metrics port |

Full comments: [`values.yaml`](https://github.com/zyvorai/rivora/blob/main/deploy/helm/rivora/values.yaml).
