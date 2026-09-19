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

## API keys: read-only access, rotation, and probing

The API has two roles. `RIVORA_API_KEY` is the **admin** key (full access,
including the mutating calls below); `RIVORA_API_READONLY_KEY` may read but gets a
`403` on anything that changes state. Hand the read-only key to dashboards and to
anyone who only needs to look, so a leaked screen-share can't drain a backend.

**Rotating a key with no outage** (either variable accepts a comma-separated list):

1. Generate the new key: `openssl rand -hex 24`.
2. Set `RIVORA_API_KEY=<new>,<old>` (in `/etc/rivora/rivorad.env`, or the chart's
   `rivorad.apiKey`, escaping the comma with `--set`) and restart `rivorad`. Both
   keys now work.
3. Move every client to `<new>`.
4. Set `RIVORA_API_KEY=<new>` and restart. `<old>` now gets `401`.

Rotate a read-only key the same way with `RIVORA_API_READONLY_KEY`.

**Watching for probing.** A publicly bound API attracts scanners. Alert on
`rate(rivora_api_auth_failures_total{reason="unauthenticated"}[5m])`; a steady
rise means someone is guessing keys (`forbidden` means a read-only key was used to
try to change something). Rejections are also logged, throttled to about one line
per 10 seconds with a count of how many were swallowed, showing the source
address and path but never a key. Behind a proxy the address is the proxy's.

**Trusting the certificate.** With `RIVORA_TLS_CERT`/`RIVORA_TLS_KEY`, verify it
rather than skipping verification: `rivoractl --ca-file cert.pem ...`. The
auto-generated self-signed certificate changes on every start, so it can only be
skipped with `--tls-insecure`.

`rivorad` refuses to start, before touching the kernel, if the key settings are
unsafe: a read-only key with no admin key, a key in both roles, or a setting with
no usable key in it.

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

## XDP attach mode (generic, native, auto)

The XDP program can attach in two ways, chosen with `xdpMode` (or `-xdp-mode`
under `-kubernetes`, and `rivorad.xdpMode` in the Helm chart):

| Mode | What it is | When |
| --- | --- | --- |
| `generic` (default) | Runs in the kernel's network stack after the driver has already built an skb. Works on any interface, including veth, VLANs and bonds. | Anything without native support, and the safe default. |
| `native` | Runs inside the NIC driver, before an skb exists. Much faster, and the reason to use XDP on real hardware. Needs driver support. | Physical NICs and SR-IOV VFs whose driver supports XDP. |
| `auto` | Tries native, falls back to generic and logs a warning. | When you want native where available without a per-host setting. |

`generic` stays the default so nothing changes unless you opt in. `native` is
strict on purpose: if the driver can't do it, `rivorad` refuses to start and
says so, rather than quietly running at generic speed. Use `auto` if you want the
fallback.

**Check what is really attached** rather than trusting the config: the mode flag
on the interface's first `ip link` line (`xdp` is native, `xdpgeneric` is
generic), or `bpftool net list` (`driver` or `generic`). The start-up log also
reports `xdp_mode=`, and warns when `auto` fell back. Note the `prog/xdp` line
under it appears for both modes and doesn't tell them apart.

Things to know:

- **Forwarding modes still matter.** DSR forwards with `XDP_TX`; the loader's
  own notes record that native `XDP_TX` on veth does not reliably cross a bridge
  (which is why generic is the default). Full-NAT forwards with `XDP_PASS` and
  is exercised natively on veth by `scripts/selftest-xdpmode.sh`. DSR under
  native mode has not been tested here: try it on your own hardware first.
- **Changing the mode is a one-off gap.** The kernel allows only one mode per
  interface, so switching replaces the link. With `-persist-datapath` the old
  pin is dropped and the new mode's link pinned (exactly one XDP pin remains);
  restarting in the *same* mode afterwards hot-swaps as usual. `xdpMode` is read
  at start-up, so a reload (SIGHUP) that changes it logs `NOT applied`.
- **Multiple interfaces** are still not supported: one `interface` per node.

## Kubernetes Service semantics: session affinity and `externalTrafficPolicy`

For `type: LoadBalancer` Services, `rivorad` reads two Service fields.

**`sessionAffinity: ClientIP` is honoured always.** All connections from one client
address go to the same backend while the backend set is unchanged, and when it
changes Maglev moves only a small share of clients. The same behaviour is available
to static configs as a VIP's `sessionAffinity: clientIP`
(`config/examples/session-affinity.yaml`). Two differences from Kubernetes to know
about:

- **There is no timeout.** Stickiness is a pure function of the source address and the
  current backend set, so the Service's `sessionAffinityConfig.clientIP.timeoutSeconds`
  is ignored.
- **A client behind a NAT or proxy is one client.** Everyone behind one address hashes
  identically, so a large NAT'd population lands on a single backend. That is what
  source-address affinity means, but it is easy to trip over with a corporate proxy.

**`externalTrafficPolicy: Local` is honoured only where it is safe**, and otherwise
treated as `Cluster` (which is what it always was here). Both preserve the client
address, since the NAT rewrites only the destination, so what you lose without `Local`
is only the locality of delivering on the node that has the pod.

| Configuration | `Local` |
| --- | --- |
| `-bgp` on, `-speaker=false`, node name known | **Honoured**: a node uses only its own endpoints, and a node with none programs no VIP, so it withdraws the route rather than attracting traffic it would have to forward elsewhere. |
| L2 speaker on (`-speaker=true`, the default) | Treated as `Cluster`. The speaker elects **one** node to answer ARP/NDP for **every** VIP, whether or not it runs a Service's pods; honouring `Local` there would blackhole a Service whenever the elected node has none of its pods. |
| BGP and the L2 speaker both on | Treated as `Cluster`; run with `-speaker=false` to honour `Local`. |
| Neither BGP nor L2 | Treated as `Cluster`: nothing stops a node without the pods from receiving the traffic. |
| BGP on, speaker off, but no node name | Treated as `Cluster`; set `-node-name` (the Helm chart sets `NODE_NAME` from `spec.nodeName`). |

Whenever a Service asks for `Local` and it isn't honoured, `rivorad` logs a warning
**once per Service** saying why (`externalTrafficPolicy: Local is being treated as
Cluster for this Service`), and its start-up log states whether `Local` is honoured on
that node. Endpoints with no node (external addresses, hand-authored slices) are not
assumed local; a local terminating endpoint stays in as draining; node names must
match exactly.

Not implemented: topology-aware routing (`trafficDistribution`), `internalTrafficPolicy`
(this is a LoadBalancer implementation), and the Service's `healthCheckNodePort`
(that exists for external load balancers to probe a node; here `rivorad` is the load
balancer).

## Health checks: TCP and HTTP

By default each backend is probed with a TCP connect to its service port. That
proves the port accepts connections and nothing more: a backend whose app is
wedged, returning 500s or still warming up stays "healthy" and keeps receiving
traffic. Give a VIP an HTTP probe to judge the app itself:

```yaml
vips:
  - address: 10.0.0.100
    port: 80
    protocol: tcp
    mode: nat
    healthCheck:
      type: http
      path: /healthz          # default "/"
      expectStatus: 200-299   # "200" or a range; default 200-399
      port: 8081              # optional: a separate health port
      host: app.internal      # optional Host header
    backends: [{address: 10.0.1.11, port: 8080}]
```

- **What passes:** a `GET` that answers within the probe `timeout` with a status in
  `expectStatus`. Redirects are **not followed**, only counted, so a `302` passes
  the default `200-399` and fails `expectStatus: 200`. Anything else fails: a
  connect error, a timeout, a backend that accepts and never replies, a service
  that isn't HTTP.
- **Every probe is a fresh connection**, so a backend that has stopped accepting
  new connections is noticed rather than masked by a kept-alive one. Proxy
  environment variables are ignored: the probe reaches the backend itself.
- **`port`** probes another port than the service port. Use it when health is
  served separately, and for a UDP VIP (there is no TCP port to connect to).
  It applies to `tcp` probes too.
- **Timing is still global** (`healthCheck.interval`, `timeout`, `failThreshold`,
  `successThreshold`), so one failed probe doesn't flap a backend out.
- **A backend is probed once, however many VIPs list it**, so VIPs that share a
  backend address and port must agree on its probe. `rivorad` and
  `rivoractl validate` refuse a config where they don't, naming the backend,
  instead of letting one VIP silently win.
- **Editing only a VIP's `healthCheck` and reloading (SIGHUP) takes effect
  immediately**; the backend keeps its current health state until the new probe
  says otherwise. Removing `healthCheck` returns it to a TCP connect.
- **Not covered yet:** `https` (there is no TLS probe), gRPC, and per-backend or
  per-Service probe settings. VIPs created from Kubernetes Services or Gateways
  have no probe setting and always use the TCP connect.

To see it working, `rivora_backend_healthy` drops to `0` for a backend whose
health endpoint fails while a plain TCP connect to its service port still
succeeds, and `rivora_vip_dropped_packets_total{reason="no_healthy_backend"}`
starts counting if that was the VIP's only backend.

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
