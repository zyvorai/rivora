---
sidebar_position: 3
title: Production runbook
---

# Production runbook

Operational procedures for a live `rivorad`/`rivora-controller` deployment
— what to check first, and how to read the signals covered in
[Observability](../kubernetes/helm.md#observability).

## Health and metrics endpoints

Both binaries serve the same three routes on port `9871` (plain HTTP, no
auth — see [Helm chart: Observability](../kubernetes/helm.md#observability)):

| Route | Meaning |
| --- | --- |
| `/healthz` | Process is alive and answering HTTP. |
| `/readyz` | `rivorad` only: the BPF maps are readable. `rivora-controller`: always ready once started. |
| `/metrics` | Prometheus text exposition. |

`rivorad` additionally serves `/api/v1/status`, `/api/v1/vips`,
`/api/v1/backends` on its main API port (`9870`, loopback-only) — see
[Securing the API](https://github.com/zyvorai/rivora#securing-the-api).
`rivoractl status` / `rivoractl vips` wrap the same data for human
reading.

## A VIP stopped responding

1. **Which node owns it?** For a static-YAML VIP, it's whichever node has
   that VIP in its `-config`. For a K8s-managed VIP, check
   `kubectl get service <svc> -o jsonpath='{.status.loadBalancer}'` for the
   assigned address, then check which node's `rivorad` currently has a
   healthy backend for it — `rivora_vip_backends` and
   `rivora_backend_healthy` (Prometheus) or `rivoractl vips --format json`
   (per-node) both show this.
2. **Are there healthy backends?** `rivora_backend_healthy == 0` for every
   backend behind the VIP means the dataplane is correctly withdrawing
   traffic from a VIP with nothing to send it to — the active health
   checker (`internal/healthcheck`) found every backend unreachable. Check
   the backends themselves, not rivorad, first.
3. **Is the VIP announced?**
   - DSR/static-YAML with no HA: the VIP must be reachable at L2 from
     clients — confirm ARP resolves to the Rivora node's MAC.
   - K8s-managed (NAT-only): the ARP+NDP speaker
     (`internal/speaker`) announces it — confirm exactly one node holds
     the speaker's Lease (`kubectl get lease -n rivora-system`) and that
     node is the one with healthy backends.
   - BGP/BFD HA: confirm the VIP's `/32` or `/128` is actually in your
     router's RIB (`show ip bgp` / `show bgp ipv6 unicast` on the peer) —
     see [BGP/BFD HA](./bgp.md). A withdrawn route with all backends
     healthy usually means a BFD/BGP session flap, not a Rivora bug.
4. **Check `rivora_dataplane_scrape_errors_total`.** A `1` means the last
   `/metrics` scrape failed to read the BPF maps at all — proceed to
   "BPF maps became unreadable" below rather than chasing VIP-specific
   causes.

## BPF maps became unreadable

`rivora_dataplane_scrape_errors_total == 1` or `/readyz` returning 503
both mean `rivorad` can no longer read `/sys/fs/bpf/rivora-lb`. This is
not expected in normal operation — the maps are pinned for the daemon's
whole lifetime (see [Restarting rivorad](#restarting-rivorad)). Likely
causes: something outside Rivora removed the pin directory (a manual
`rm -rf /sys/fs/bpf/rivora-lb`, or a node's `/sys/fs/bpf` got remounted),
or the node ran out of the kernel's locked-memory limit for BPF maps
under heavy VIP/backend churn. Check `dmesg` for BPF/memory-related
errors, then restart `rivorad` — pinned maps still on disk are reused; if
they were actually removed, a restart rebuilds them from the current
config/K8s state (some in-flight NAT connection affinity is lost in that
case, existing DSR flows are not since the backend owns the reply path).

## Draining a backend or shifting weight (live)

Use these for maintenance and canary shifts without editing config or
restarting `rivorad`. Backend IDs come from `rivoractl backends`.

```bash
rivoractl backends                 # ID, address, weight, STATE, counters
rivoractl drain 12                 # no NEW flows; established flows keep flowing
rivoractl undrain 12
rivoractl weight 12 5              # override Maglev weight to 5 on every VIP using backend 12
rivoractl weight 12 5 --vip 10.0.0.1:80:tcp   # only that VIP
rivoractl weight 12 0              # clear the override; configured weight applies again
```

- The same operations are `POST /api/v1/backends/{id}/drain`, `/undrain` and
  `/weight` (body `{"weight": 5, "vip": "addr:port:proto"}`, `vip` optional).
  They require `Content-Type: application/json` and, when `RIVORA_API_KEY` is
  set, the bearer token.
- An operator drain is tracked separately from a Kubernetes-driven one
  (terminating endpoint), so neither silently undoes the other. `STATE` shows
  `draining (operator)` for yours, and `rivora_backend_draining` is `1` for either.
- A failed health probe still wins over a drain: a backend that is down stays
  down.
- Weight overrides survive Kubernetes reconciles and static-config re-syncs, and
  are dropped if the backend leaves the VIP. Weights above 1000 are rejected.
- **Not persisted.** Drains and overrides live in `rivorad`'s memory: a restart
  (or pod recreation) reverts to the configured state. Put lasting changes in
  the config or the Service.
- Check a config file before deploying it with `rivoractl validate FILE`; it
  runs the same loader `rivorad` starts with and needs no running daemon.

## The flow tables are filling up

`connection_affinity_map` (sticky per-flow backend choice) and `nat_reverse_map`
(full-NAT return path) are fixed-size LRU tables, with separate IPv6 siblings.
When one is full the kernel evicts the least-recently-used flow, so a live
connection can be re-hashed onto a different backend (affinity) or lose its
un-NAT mapping (nat_reverse) and reset.

Watch `rivora_conntrack_entries / rivora_conntrack_capacity` per `table`. A
sustained ratio above ~0.8 means you are close to evicting live flows; the
usual causes are a SYN flood (see the rate-limit example config) or many
long-lived idle connections. Entry counts are sampled at most every 10 seconds.
The table sizes are compile-time today, so the remedies are rate limiting and
spreading VIPs across nodes.

## Restarting rivorad

Safe under normal conditions: `internal/loader` pins BPF maps under
`/sys/fs/bpf/rivora-lb` and reuses them on the next `Load()` rather than
recreating them, so connection affinity, Maglev table state, and
packet/byte counters all survive a restart. What does *not* survive: any
in-flight full-NAT connection whose `nat_reverse_map` entry needs a
kernel-side conntrack-like return path that depended on the *old*
process's XDP/TCX attachment briefly detaching during the restart window
— expect a short blip for existing full-NAT flows, not for DSR (backend
owns the reply path) or for new connections after the daemon comes back.

A `helm upgrade`/pod restart on the DaemonSet follows the same path — one
node at a time (`kubectl rollout status daemonset/<name>-rivorad`), never
all nodes simultaneously, so surviving nodes keep serving each VIP
throughout.

## rivora-controller failover

`rivora-controller` is Lease-elected (`leaderelection.NewLeaderElector`,
15s lease/10s renew/2s retry — see `cmd/rivora-controller/main.go`); the
`rivora_controller_leader` gauge is `1` on exactly one replica at a time.
If no replica reports `1` for longer than the lease duration, IPAM
allocation and `Service.Status`/`Gateway.status` patching have stalled —
check `kubectl get lease -n rivora-system rivora-controller` for the
current holder and its renew time, and check that replica's logs for
Kubernetes API errors (RBAC, API server unavailability) rather than
assuming a code-level bug.

## CRD schema changes

`helm upgrade`/`helm uninstall` never touch the `AddressPool` CRD once
installed — see [CRD lifecycle](../kubernetes/helm.md#crd-lifecycle).
Before upgrading to a chart version whose CRD schema changed, diff
`deploy/helm/rivora/crds/addresspool-crd.yaml` against what's installed
(`kubectl get crd addresspools.rivora.zyvor.dev -o yaml`) and
`kubectl apply` the new version yourself first — a chart upgrade that
assumes a newer schema than what's installed will surface as `rivora-controller`
reconcile errors reading `AddressPool` objects, not as a Helm failure.
