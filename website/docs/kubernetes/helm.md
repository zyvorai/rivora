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

## Values (IPv6-related)

| Path | Default | Notes |
| --- | --- | --- |
| `rivorad.interface` | `""` | Required |
| `rivorad.speaker` | `true` | ARP+NDP |
| `gatewayApi.enabled` | `false` | |
| `bgp.enabled` | `false` | |
| `bgp.ipv6NextHop` | `""` | Required for IPv6 `/128` ads |
| `addressPools[]` | `[]` | Seeded pools; IPv6 `/64` OK |

Full comments: [`values.yaml`](https://github.com/zyvorai/rivora/blob/main/deploy/helm/rivora/values.yaml).
