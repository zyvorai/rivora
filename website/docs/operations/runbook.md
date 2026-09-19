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

## Reloading the config without a restart

For a static-YAML node, edit `/etc/rivora/config.yaml`, check it, then reload:

```bash
rivoractl validate /etc/rivora/config.yaml   # same loader rivorad uses; no daemon needed
sudo systemctl reload rivorad                # sends SIGHUP; watch the log for the result
```

`rivorad` re-reads the file and makes the programmed VIPs match it: VIPs you
removed are torn down, new ones added, and changed ones updated. A VIP whose
definition is identical is not touched at all, so reloading an unchanged file
rewrites no BPF map and disturbs no flow. Health checking and BGP pick up the
new backends immediately. Operator drains and weight overrides (see above)
survive a reload.

What a reload does **not** do:

- **A file that fails validation is rejected whole.** The log says
  `config reload rejected; keeping the running config`, and nothing changes.
- **Startup-only settings are ignored, with a warning.** `interface`,
  `apiListen`, `healthCheck`, `rateLimit` and `bgp` are read once; if you changed
  any, the log says `NOT applied` and names them. Restart `rivorad` to apply.
- **The first NAT VIP needs a restart.** The `tc_nat` egress program (which
  un-NATs replies) is only loaded at startup if some VIP uses `mode: nat`. A
  reload that would introduce NAT to a node started DSR-only is rejected, since
  programming the VIP would break its return path.
- **Partial failure is reported, not hidden.** If one VIP can't be applied (for
  example the shared Maglev table is full) the others still are, the log says
  `config reload partly applied` with the error, and the next reload retries it.
- It is **not available with `-kubernetes`**, where the Service/EndpointSlice
  reconcilers own the VIP set and there is no file to re-read.

## Why is traffic being dropped, or not load-balanced?

Each VIP counts, on the XDP path, why a packet it matched did not reach a
backend. `rivora_vip_dropped_packets_total{vip,reason}` counts real drops:

| `reason` | What it means | First thing to check |
| --- | --- | --- |
| `rate_limited` | A SYN exceeded the per-source `rateLimit`. | Is it an attack, or is the limit set too low for legitimate clients? Compare with the source addresses in your flow logs. |
| `no_healthy_backend` | The VIP matched but every backend in its Maglev probe window is down or draining, so there was nowhere to send it. | `rivora_backend_healthy` for that VIP; the backends themselves, not `rivorad`. |

`rivora_vip_unserved_packets_total{vip}` is deliberately **not** a drop. It counts
packets that matched a VIP but could not be served (no service configuration or
backend entry), so they were passed to the kernel stack instead of being
load-balanced. A VIP that answers with a reset, or is reachable but skipping the
load balancer, will show here. It should always be zero; a nonzero, rising value
means the maps and the config disagree, so check `rivoractl vips` and restart or
reload `rivorad`.

The two drop reasons across all VIPs sum to the node-wide
`rivora_dropped_packets_total`, so a gap between them would itself be a bug.
Counters run from when the BPF maps were pinned, and a VIP that is removed and
added back starts from zero.

Suggested alerts:

```yaml
# A VIP is dropping because it has nothing to send to.
- alert: RivoraVIPNoHealthyBackend
  expr: sum by (vip) (rate(rivora_vip_dropped_packets_total{reason="no_healthy_backend"}[5m])) > 0
  for: 2m
# Traffic is matching a VIP but skipping the load balancer.
- alert: RivoraVIPUnserved
  expr: sum by (vip) (rate(rivora_vip_unserved_packets_total[5m])) > 0
  for: 2m
```

## BGP session and route health

With `bgp.enabled`, `rivorad` exports the speaker's state as `rivora_bgp_*`
(the series are absent, not zero, when BGP is off):

| Metric | Meaning |
| --- | --- |
| `rivora_bgp_peer_up{peer,asn}` | `1` when the session is ESTABLISHED. `0` means that peer is not receiving this node's VIP routes. A configured peer gobgp fails to report shows as `0`, not as missing. |
| `rivora_bgp_peer_state_changes_total{peer}` | Session-state transitions since start. A rising value on a peer that is mostly up is a flapping session (or a BFD-detected drop). |
| `rivora_bgp_advertised_routes{family}` | VIP host routes advertised right now (`ipv4` `/32`, `ipv6` `/128`). A VIP is advertised only while it has a healthy backend, so this dropping is the health-gating working. |
| `rivora_bgp_route_update_errors_total{op}` | Failed `advertise`/`withdraw` attempts. A resync retries, so a value that stops rising was transient; one that keeps rising means routes are not reaching the peers. The usual cause is an IPv6 VIP with no `ipv6NextHop` configured. |

```yaml
- alert: RivoraBGPPeerDown
  expr: rivora_bgp_peer_up == 0
  for: 2m
- alert: RivoraBGPPeerFlapping
  expr: increase(rivora_bgp_peer_state_changes_total[10m]) > 4
- alert: RivoraBGPRouteUpdatesFailing
  expr: increase(rivora_bgp_route_update_errors_total[10m]) > 0
```

`peer_up == 0` for every peer while `rivora_backend_healthy` is `1` is a network
or peer-side problem; `peer_up == 1` with `advertised_routes == 0` means the VIPs
have no healthy backend.

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

### Restarting without a traffic gap (opt-in)

By default the XDP/TCX links belong to `rivorad`'s own file descriptors, so the
kernel detaches them when it exits and the VIP is unowned until the new process
has started, loaded and re-attached: on an idle test host that is a few dozen
milliseconds of refused connections, and longer on a busy or slow node.

Start `rivorad` with `-persist-datapath` to close that gap. It pins each link
under `/sys/fs/bpf/rivora-lb/links/`, so the attached program keeps forwarding
(against the pinned maps) while `rivorad` is down, and the next start swaps its
own program into the existing link in place instead of detaching and
re-attaching. The start-up log says which links were `hot_swapped`. With
systemd, put `RIVORAD_ARGS=-persist-datapath` in `/etc/rivora/rivorad.env`.

`scripts/selftest-restart.sh` measures this: it restarts `rivorad` under a
steady stream of connections and compares a control run (the default) against a
persisted one. On the reference host the control run drops a handful of
connections per restart and the persisted run drops none.

What you take on when you enable it:

- **It keeps forwarding after `rivorad` exits.** `systemctl stop rivorad` no
  longer takes the VIP down. To actually remove the datapath, run
  `rivorad -detach` (it removes the pinned links and leaves the maps).
- **Nothing is health-checking while `rivorad` is down.** Backend health is
  frozen at its last value, so a backend that dies during the restart window
  still receives new flows until the new process probes it.
- **State is whatever the maps held.** The new process re-applies its config on
  top of the pinned maps, exactly as a default restart does, and the same
  compatibility rule holds: a `rivorad` whose BPF maps changed shape won't load
  against maps from an older version. Read the release notes before upgrading
  across such a change.
- **Start-up reconciles against what the maps already hold (static-YAML
  mode).** `rivorad` reads the VIPs, Maglev extents and backends back from the
  pinned maps, so a VIP that stays in the config keeps the IDs it had, and one
  you removed while `rivorad` was down is torn down at start-up (the log says
  `removed VIPs left programmed by a previous config`). This matters more than
  it sounds: without it, the removed VIP's service ID is reused by the first VIP
  in the new config, and traffic still addressed to the removed VIP is answered
  by another VIP's backends. `scripts/selftest-adopt.sh` reproduces that on an
  older build and passes on this one. **Not done with `-kubernetes`**: there the
  reconcilers supply the VIPs after start-up, so there is nothing to reconcile
  against yet, and a VIP whose Service was deleted while `rivorad` was down stays
  programmed until its map entry is cleared.
- **Changing `interface` or dropping every `mode: nat` VIP leaves the old link
  attached.** The start-up log warns about persisted links this config no
  longer manages; run `rivorad -detach` to clear them.

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
