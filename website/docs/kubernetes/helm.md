---
sidebar_position: 3
title: Helm chart
---

# Helm chart

Chart path: [`deploy/helm/rivora`](https://github.com/zyvorai/rivora/tree/main/deploy/helm/rivora). The
`rivora` CLI installs the same chart (embedded in each release), so `rivora install --set ...` and
`helm install ... --set ...` take identical values.

It installs:

- **`rivorad`**, a privileged host-network DaemonSet (the dataplane and per-node reconcilers);
- **`rivora-controller`**, a two-replica Deployment (IPAM and status, Lease-elected);
- the **CRDs** (`AddressPool`, `ServicePolicy`, `BGPPeer`), RBAC and service accounts, a
  PodDisruptionBudget for the controller, and, if asked, seeded `AddressPool`s and a `GatewayClass`.

## Install

```sh
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
```

`rivorad.interface` is **required**: the interface every node attaches XDP and TCX to. A cluster whose nodes
name their interface differently is not supported by one release; pin the DaemonSet to matching nodes with
`rivorad.nodeSelector`. Run `rivora-doctor --interface <if>` on a node first.

### Dual-stack pools

```sh
helm upgrade rivora deploy/helm/rivora --namespace rivora-system --reuse-values \
  --set addressPools[1].name=v6 --set addressPools[1].addresses='{2001:db8:1::/64}'
```

Large `/64`s allocate sparsely: see [IPv6](../core-concepts/ipv6.md).

## Values

The main ones (all documented in
[`values.yaml`](https://github.com/zyvorai/rivora/blob/main/deploy/helm/rivora/values.yaml)):

| Path | Default | Meaning |
| --- | --- | --- |
| `image.rivorad.*`, `image.controller.*` | `ghcr.io/zyvorai/rivora-*`, tag = chart appVersion | Images (cosign-signed, with an SBOM). |
| `rivorad.interface` | `""` | **Required.** Interface XDP/TCX attach to. |
| `rivorad.xdpMode` | `generic` | `generic`, `native` or `auto`. See [XDP attach mode](../operations/runbook.md#xdp-attach-mode-generic-native-auto). |
| `rivorad.bpfDir` | `/usr/local/share/rivora/bpf` | Where the BPF objects are in the image. |
| `rivorad.loadBalancerClass` | `""` | Must equal `controller.loadBalancerClass`. |
| `rivorad.workers` | `2` | Reconcile workers. |
| `rivorad.speaker` | `true` | The L2 ARP + NDP speaker. Set `false` to use BGP instead and to honour `externalTrafficPolicy: Local`. |
| `rivorad.apiKey`, `rivorad.apiReadOnlyKey` | `""` | Admin and read-only API keys (in a Secret). See [API](../operations/api.md). |
| `rivorad.tls.selfSigned`, `.cert`, `.key` | off | HTTPS for the pod-local API. |
| `rivorad.securityContext.privileged` | `true` | Needed to create pins under `/sys/fs/bpf`; the same posture as other eBPF DaemonSets. |
| `rivorad.resources`, `.nodeSelector`, `.tolerations`, `.affinity` | small requests, tolerate all | Scheduling. |
| `servicePolicy.enabled` | `true` | Honour `ServicePolicy`. Adds the RBAC and `-service-policy`. |
| `gatewayApi.enabled` | `false` | Also run the Gateway API reconcilers. Needs the Gateway API CRDs installed first. |
| `gatewayClass.create`, `.name` | `false`, `rivora` | Create a `GatewayClass` for Rivora. |
| `bgp.enabled` | `false` | The BGP speaker. |
| `bgp.asn`, `bgp.routerId`, `bgp.ipv6NextHop` | | Speaker identity and next hops. |
| `bgp.peers[]` | `[]` | `address`, `asn`, `bfd`. For more, use `bgp.configSecret` or `BGPPeer` resources. |
| `bgp.configSecret` | `""` | A Secret whose `bgp.yaml` holds a `bgp:` section; **replaces** `asn`, `routerId`, `ipv6NextHop` and `peers`. |
| `bgp.peerResources` | `true` | Also read `BGPPeer` resources; grants RBAC to read them, the node, and Secrets in the release namespace. |
| `addressPools[]` | `[]` | Seeded `AddressPool`s (`name`, `addresses`, `autoAssign`, `avoidBuggyIPs`). |
| `controller.replicaCount` | `2` | One works, one is a hot standby. |
| `controller.podDisruptionBudget.enabled`, `.minAvailable` | `true`, `1` | Keeps a replica up during drains. `minAvailable` must be below `replicaCount`. |
| `controller.networkPolicy.enabled` | `false` | Restrict ingress to the controller's metrics port. |
| `rbac.create`, `serviceAccount.*`, `imagePullSecrets` | | Standard. |

## CRD lifecycle

The CRDs live in the chart's `crds/` directory. Helm installs them on the first `helm install` but **never
upgrades or deletes them**: not on `helm upgrade`, not on `rivora upgrade`, not on uninstall. When a release
adds or changes a CRD, apply it yourself **before** upgrading:

```sh
kubectl apply -f deploy/helm/rivora/crds/
```

This matters most when upgrading from a release that predates a CRD:

- **`ServicePolicy` or `BGPPeer` missing:** `rivorad` probes for the CRD at start-up, logs that the feature
  is ignored, and carries on serving Services (an informer on a CRD the cluster does not serve would never
  sync and would stall everything). Apply the CRD and restart the DaemonSet.
- **`AddressPool` schema changed:** a chart that assumes a newer schema than the installed one surfaces as
  `rivora-controller` reconcile errors reading `AddressPool`, not as a Helm failure. Diff
  `crds/addresspool-crd.yaml` against `kubectl get crd addresspools.rivora.zyvor.dev -o yaml` first.

All three CRDs ship one version (`v1alpha1`) and no conversion webhook, so additive schema changes apply in
place. See the chart README's
[Evolving the schema](https://github.com/zyvorai/rivora/blob/main/deploy/helm/rivora/README.md#evolving-the-schema)
checklist for a breaking one.

## Upgrading

1. Read the release notes and apply any changed CRDs (above).
2. `helm upgrade` (or `rivora upgrade`). The DaemonSet rolls **one node at a time**; watch
   `kubectl rollout status daemonset/rivora-rivorad -n rivora-system`. Surviving nodes keep serving every VIP.
3. A restarted `rivorad` adopts the datapath the maps still hold and reclaims its VIPs as the reconcilers
   catch up, so the disruption is the brief re-attach of the XDP program unless `-persist-datapath` is on.
   A release that changes the shape of a BPF map will not load against maps from an older version; the
   release notes say so.

## Permissions

`rivorad` gets a ClusterRole to read Services, EndpointSlices, Leases (read and write, for the speaker),
Events, and, only when the feature is on, `ServicePolicy`; `BGPPeer` and Nodes (get); Gateway API objects,
`ReferenceGrant`s and Namespaces; plus a **namespaced Role to read Secrets in the release namespace only**
(for `BGPPeer` passwords). `rivora-controller` additionally writes Service and Gateway status and patches
finalizers. The chart's RBAC is the authoritative list.

## Observability

Both workloads serve `/healthz`, `/readyz` and `/metrics` on port `9871` (plain HTTP, no authentication),
wired into the liveness and readiness probes and annotated `prometheus.io/scrape: "true"`. This is separate
from `rivorad`'s API port (`9870`, loopback) because a host-network pod's `127.0.0.1` is not reachable from an
in-cluster Prometheus. The metrics, alerts and how to read them: [Metrics](../operations/metrics.md) and the
[runbook](../operations/runbook.md).
