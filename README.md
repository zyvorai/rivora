# Rivora

[![CI](https://github.com/zyvorai/rivora/actions/workflows/ci.yml/badge.svg)](https://github.com/zyvorai/rivora/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-zyvorai.github.io%2Frivora-blue)](https://zyvorai.github.io/rivora/)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

![Rivora — eBPF-native load balancer for Kubernetes and bare metal](docs/social/rivora-share-card.png)

**eBPF-native load balancing for every environment.**

Rivora owns VIPs, backend selection, health checking and NAT/DSR — the
traffic-delivery layer the Zyvor platform doesn't yet have. It's
CNI-independent and doesn't require Cilium: it attaches its own XDP/TCX
programs and owns its own maps under `/sys/fs/bpf/rivora-lb`.

> **Status: v0.1 shipped, v0.2 (Kubernetes) implemented and verified
> end-to-end on a live cluster.** Single node, IPv4/IPv6 TCP/UDP, DSR and
> full-NAT forwarding, Maglev backend selection with graceful draining,
> active TCP health checks. A node can run multiple VIPs, each
> independently reconciled and Maglev-partitioned — either from static
> YAML, or from Kubernetes `Service`/`EndpointSlice` objects via
> `rivorad -kubernetes`, with `rivora-controller` handling `AddressPool`
> IPAM (IPv4 and IPv6, including sparse `/64` allocation) and an
> in-`rivorad` L2 ARP+NDP speaker announcing assigned VIPs. BGP/BFD HA is
> implemented (opt-in, active/active ECMP, `/32` and `/128`) but not yet
> live-verified against a real router peer. IPv6 dataplane, NDP, sparse
> IPAM, and BGP `/128` are covered by CI selftests on every push — see
> [IPv6](#ipv6-v03) and [Selftests and CI](#selftests-and-ci).

## Contents

- [How it works](#how-it-works)
- [Architecture](#architecture)
- [Repository](#repository)
- [Securing the API](#securing-the-api)
- [Quickstart](#quickstart)
- [Kubernetes (v0.2)](#kubernetes-v02)
  - [Backends beyond Pods: KubeVirt VMs and external/physical IPs](#backends-beyond-pods-kubevirt-vms-and-externalphysical-ips)
  - [Gateway API (v0.3)](#gateway-api-v03)
  - [BGP/BFD HA (v0.3)](#bgpbfd-ha-v03)
- [IPv6 (v0.3)](#ipv6-v03)
  - [Dataplane](#dataplane)
  - [Static YAML](#static-yaml)
  - [IPAM and AddressPool](#ipam-and-addresspool)
  - [Kubernetes dual-stack](#kubernetes-dual-stack)
  - [NDP speaker](#ndp-speaker)
  - [BGP IPv6 `/128`](#bgp-ipv6-128)
  - [Maps and checksums](#maps-and-checksums)
  - [Verification](#verification)
- [Building on the remote host](#building-on-the-remote-host)
- [Selftests and CI](#selftests-and-ci)
- [Operator docs (GitHub Pages)](#operator-docs-github-pages)
- [Roadmap](#roadmap)
- [License](#license)

## How it works

- **`bpf/xdp_ingress.c`** — XDP program: match the VIP, optionally
  rate-limit new TCP connections per source IP (opt-in SYN-flood
  protection, a per-CPU token bucket — see
  `config/examples/rate-limited.yaml`; a no-op, single-array-lookup cost
  when unconfigured), pick a backend (sticky per-flow via
  `connection_affinity_map`, otherwise Maglev consistent hashing over
  `maglev_table`, optionally **weighted** — backends can carry unequal
  traffic shares for canary/capacity-based balancing, see
  `config/examples/weighted-backends.yaml`), then either rewrite the
  destination MAC and `XDP_TX` (**DSR**) or rewrite the destination
  IP/port and `XDP_PASS` to normal routing (**full NAT**).
- **`bpf/tc_nat.c`** — TCX egress program, full-NAT mode only: un-NATs a
  backend's reply (source IP/port back to VIP:port) before it leaves, using
  the reverse mapping `xdp_ingress` wrote to `nat_reverse_map`.
- **`rivorad`** (`cmd/rivorad`) — the daemon: loads/pins the BPF objects,
  applies the YAML config to the maps, builds the Maglev table, runs active
  TCP health checks, and serves a local HTTP API on `127.0.0.1:9870`.
- **`rivoractl`** (`cmd/rivoractl`) — CLI, talks to `rivorad` over that API.
  `rivoractl status`, `rivoractl vips`, `rivoractl backends` — add
  `--format json` for machine-readable output. Live operations:
  `rivoractl drain|undrain ID` and `rivoractl weight ID N` (see the
  [runbook](website/docs/operations/runbook.md)); `rivoractl validate FILE`
  checks a static config offline, and `systemctl reload rivorad` (SIGHUP)
  applies an edited config's VIP set live. `sessionAffinity: clientIP` pins a client
  to one backend (and maps a Service's `sessionAffinity: ClientIP`);
  `externalTrafficPolicy: Local` is honoured with BGP and the L2 speaker off (see the
  [runbook](website/docs/operations/runbook.md)). A VIP's `healthCheck: {type: http, ...}`
  probes an HTTP endpoint and judges its status instead of only a TCP connect
  (see `config/examples/http-healthcheck.yaml`). `xdpMode: native` (or `auto`)
  attaches XDP in the NIC driver instead of the default generic mode.
  `rivorad -persist-datapath` keeps
  the datapath attached across restarts (no traffic gap; see the runbook).
  `rivorad` and `rivora-controller` take `-log-level` and `-log-format text|json`.
- **`rivora-doctor`** (`cmd/rivora-doctor`) — standalone host-readiness
  checker: bpffs mounted, kernel new enough for TCX, build tools present.
  `--json`, `--strict`, exit 0/2 — same shape as netra's doctor tool.

## Architecture

Two ways to get a VIP into the shared dataplane — static YAML on one node,
or Kubernetes objects across a cluster — both converge on the same
`internal/dataplane` → BPF maps path:

```text
  static-YAML path                    Kubernetes path (v0.2)
  -----------------                    ----------------------
  rivorad -config                      rivora-controller (leader-elected)
       |                                 watches Service + AddressPool
       v                                 allocates VIP, patches
  internal/config                       Service.Status.LoadBalancer.Ingress
       |                                        |
       |                               rivorad -kubernetes (every node)
       |                                 internal/controller: watches
       |                                 Service + EndpointSlice, reconciles
       |                                        |
       |                               internal/speaker (leader-elected
       |                                 cluster-wide Lease): ARP+NDP for
       |                                 the assigned VIP
       |                                        |
       +---------------> internal/dataplane <---+
                     UpsertVIP / RemoveVIP
                  backend-ID + Maglev-extent
                       allocators, health
                               |
                               v
                    +-----------------------+
                    |       BPF maps        |
                    |  /sys/fs/bpf/rivora-lb |
                    +-----------+-----------+
                               |
                 xdp_ingress (match VIP, pick backend,
                    DSR: rewrite MAC + XDP_TX
                    NAT: rewrite IP/port + XDP_PASS)
                               |
                    tc_nat (full-NAT reverse path only)
                               |
                               v
                           backends
```

K8s-managed VIPs are NAT-only in v0.2 — DSR would need binding the VIP
into backend pods, which needs a CNI-specific story not yet designed.

## Repository

```text
cmd/rivorad/            per-node daemon: BPF load/attach, static-YAML apply, K8s reconciler + speaker, local API
cmd/rivora/             cluster-lifecycle CLI: install/upgrade/uninstall/status against a live cluster
cmd/rivoractl/          CLI for rivorad's local API
cmd/rivora-doctor/      standalone host-readiness checker
cmd/rivora-controller/  leader-elected cluster-scoped IPAM Deployment
api/v1alpha1/           AddressPool CRD Go types (dynamic-client based, no codegen)
internal/dataplane/     BPF map writer: VIP/backend/Maglev allocators, health, UpsertVIP/RemoveVIP
internal/bpfmaps/       Go ABI mirrors of the BPF maps' C structs
internal/config/        static-YAML config loading/validation
internal/healthcheck/   active TCP health probing
internal/maglev/        Maglev consistent-hashing table generation
internal/api/           rivorad's local HTTP API (bearer-token auth, optional TLS)
internal/apiclient/     rivoractl's client for that API
internal/loader/        BPF object loading/pinning/attachment (cilium/ebpf)
internal/tlsutil/       self-signed cert generation for the local API
internal/doctor/        host-readiness checks used by rivora-doctor
internal/ipam/          address-pool CIDR/range expansion + allocator (pure Go, no K8s dependency)
internal/ipamctrl/      rivora-controller's reconcile logic: AddressPool + Service watch, IPAM
internal/controller/    rivorad's in-process Service/EndpointSlice reconciler
internal/k8s/           shared client-go bootstrap (in-cluster/kubeconfig, typed + dynamic clients)
internal/speaker/       L2 ARP+NDP responder for K8s-managed VIPs (mdlayher/arp + ndp)
internal/bgp/           gobgp-backed BGP+BFD speaker (/32 IPv4, /128 IPv6, dual AFI/SAFI)
internal/gatewayapi/    Gateway / TCPRoute / UDPRoute reconciler
internal/installer/     rivora CLI's install/upgrade/uninstall/status logic (Helm SDK + embedded chart)
bpf/                    XDP ingress + TCX egress programs (hand-rolled, no libbpf headers)
deploy/helm/rivora/     Helm chart: rivorad DaemonSet, rivora-controller Deployment, AddressPool CRD
deploy/systemd/         systemd unit for the static-YAML/non-Kubernetes deployment
scripts/                selftest*.sh (v4/v6 dataplane, NDP, weighted, rate-limit), deploy-remote.sh
config/examples/        static-YAML examples (DSR/NAT, IPv6, multi-VIP, weighted, rate-limit, BGP)
```

## Securing the API

`rivorad`'s local API (`127.0.0.1:9870` by default) is plain HTTP and
unauthenticated out of the box — fine for a loopback-only listener, but all of
this is opt-in, same env-var-driven shape as netra's `netrad`:

| Env var                    | Effect                                              |
| --------------------------- | ---------------------------------------------------- |
| `RIVORA_API_KEY`            | Admin key: require this bearer token on every request; full access (including `drain`/`weight`). Setting it is what turns authentication on. |
| `RIVORA_API_READONLY_KEY`   | Read-only key: may read the API and console, gets `403` on anything that changes state. Needs `RIVORA_API_KEY` too. |
| `RIVORA_TLS_CERT` / `_KEY`  | Serve HTTPS with this certificate                     |
| `RIVORA_TLS_SELF_SIGNED`    | Serve HTTPS with an auto-generated self-signed cert (no cert files needed) |

- **Rotation with no outage:** either key variable takes a comma-separated list.
  Set `RIVORA_API_KEY=<new>,<old>`, move clients to `<new>`, then drop `<old>` and
  restart. Give dashboards and anyone who only needs to look the read-only key.
- **`rivorad` refuses to start** with a read-only key but no admin key (it would
  protect nothing), a key in both roles, or a key setting that holds no usable
  key (a stray `,` must not silently switch auth off). Short keys start but warn;
  generate one with `openssl rand -hex 24`.
- **Failed attempts are visible:** `rivora_api_auth_failures_total{reason}`
  counts `unauthenticated` (401) and `forbidden` (403), and rejections are logged
  (throttled, source address only, never a key).
- **Trust the certificate instead of skipping verification:** start `rivorad`
  with `RIVORA_TLS_CERT`/`_KEY` and pass that certificate (or its CA) to
  `rivoractl --ca-file cert.pem` (or `RIVORA_CA_FILE`). The auto-generated
  self-signed certificate is new on every start, so it can only be skipped
  (`--tls-insecure`), not pinned.

`rivoractl` picks up `RIVORA_API_KEY`, `RIVORA_CA_FILE` and
`RIVORA_TLS_INSECURE` from its own environment, or via `--api-key`/`--ca-file`/
`--tls-insecure`; either TLS flag makes a bare `host:port` mean `https`.
`rivora-doctor` reports which of these are set.

## Quickstart

Rivora needs a real Linux kernel (XDP/eBPF) — see
[Building on the remote host](#building-on-the-remote-host) if you're
working from a Mac or a host without a build toolchain.

```sh
make bpf build                     # produces bpf/*.o and bin/{rivorad,rivoractl,rivora-doctor}
sudo ./bin/rivora-doctor           # confirm the host is ready
sudo ./bin/rivorad -config config/examples/single-vip.yaml -bpf-dir bpf
./bin/rivoractl status
```

See `config/examples/single-vip.yaml` (DSR), `single-vip-nat.yaml` (full
NAT), `single-vip-ipv6-nat.yaml` / `single-vip-ipv6-dsr.yaml` (IPv6),
`multi-vip.yaml` (several VIPs on one node, mixing modes),
`weighted-backends.yaml` (unequal traffic shares within one VIP),
`rate-limited.yaml` (opt-in per-source-IP SYN-flood protection), and
`bgp-ha.yaml` (BGP+BFD advertise of healthy VIPs) for what each
forwarding mode requires from your backends:

- **DSR** — backends need the VIP bound locally (loopback/dummy interface)
  and must be L2-reachable from the Rivora node; no backend MAC = no DSR.
- **Full NAT** — no backend changes needed, but backends must route return
  traffic back through the Rivora node (default gateway, or a static route
  for the client subnet).

With more than one VIP configured, `rivoractl status`/`/api/v1/status`
return an error pointing you at `rivoractl vips`/`/api/v1/vips` instead,
which lists every VIP.

## Kubernetes (v0.2)

`rivorad -kubernetes` replaces the static-YAML VIP set with a live
`Service`(type=LoadBalancer)/`EndpointSlice` reconciler — a node runs
either static-YAML VIPs or Kubernetes-managed VIPs, not both:

```sh
rivorad -kubernetes -interface eth0 [-loadbalancer-class <class>] [-speaker=true]
```

Address assignment is handled separately by `rivora-controller`, a
leader-elected, cluster-scoped Deployment that watches `AddressPool`
custom resources and patches `Service.Status.LoadBalancer.Ingress[].IP`
(see `api/v1alpha1` for the CRD shape). Pools may be IPv4, IPv6, or both
(separate pools or mixed `addresses` entries) — see
[IPAM and AddressPool](#ipam-and-addresspool). An in-`rivorad` L2
ARP+NDP speaker (`internal/speaker`), behind its own single cluster-wide
Lease, announces the assigned VIP on the node's dataplane interface.

The [Helm chart](deploy/helm/rivora) installs both pieces plus the
`AddressPool` CRD — either directly with `helm`, or with the `rivora` CLI
(`cmd/rivora`), which drives the same chart via the Helm SDK with no
`helm` binary required:

```sh
# rivora CLI — install a prebuilt binary (see Releases), or build it:
#   make build-cli && sudo install -m755 bin/rivora /usr/local/bin/rivora
curl -fsSL https://raw.githubusercontent.com/zyvorai/rivora/main/scripts/install-cli.sh | bash

rivora install --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
rivora status       # cluster-wide rollout status
rivora upgrade --set addressPools[1].name=v6 --set addressPools[1].addresses='{2001:db8:1::/64}'
rivora uninstall

# Equivalent with plain helm:
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'

# Dual-stack: add an IPv6 pool (large /64s allocate sparsely — see IPv6)
helm upgrade rivora deploy/helm/rivora \
  --namespace rivora-system --reuse-values \
  --set addressPools[1].name=v6 \
  --set addressPools[1].addresses='{2001:db8:1::/64}'
```

`rivora`'s chart is embedded at build time from `deploy/helm/rivora` (kept
in sync via `make sync-chart`/`make check-chart-sync`) — each CLI release
installs exactly the chart version it shipped with; there's no
multi-version chart registry to manage.

See [`deploy/helm/rivora/README.md`](deploy/helm/rivora/README.md) for the
full chart reference, including IPv6 pools, BGP `ipv6NextHop`, and the
CRD's manual-upgrade caveat.

### Backends beyond Pods: KubeVirt VMs and external/physical IPs

The Kubernetes reconciler (`internal/controller`) only ever reads
`EndpointSlice` addresses/conditions/ports — it never looks at what kind
of object backs an endpoint, so two cases work today with **no Rivora
code path treating them specially**, each verified against a live
cluster:

- **KubeVirt VMs** — a `VirtualMachineInstance` fronted by a normal
  `Service` (matching labels on the VMI, which KubeVirt propagates to its
  virt-launcher Pod) produces a completely ordinary `EndpointSlice` —
  same `addresses`/`conditions` shape as any Pod-backed Service. Nothing
  to configure beyond a normal Service selector.
- **External / physical / bare-metal IPs** — a `Service` with no
  `spec.selector`, paired with a hand-authored `EndpointSlice` (labeled
  `kubernetes.io/service-name: <service-name>`) pointing at any IP —
  Kubernetes' own EndpointSlice controller leaves manually-created slices
  alone as long as the owning Service has no selector, and Rivora's
  reconciler needs no `TargetRef`/Pod reference at all. This is the same
  mechanism `rivora-controller`'s `AddressPool` IPAM/`rivorad`'s VIP
  assignment already work with — the Service still needs
  `type: LoadBalancer` to get a VIP the normal way.

Not yet supported: routing to a KubeVirt VM's *secondary* (Multus)
network interface specifically — only the primary interface IP that
Service/EndpointSlice already expose.

### Gateway API (v0.3)

`rivorad -kubernetes -gateway-api` runs a second, parallel reconciler
alongside the `Service`/`EndpointSlice` one: it watches `GatewayClass`,
`Gateway`, and the Gateway API's L4 "experimental channel" `TCPRoute`/
`UDPRoute` resources (group `gateway.networking.k8s.io`). `HTTPRoute` is
deliberately out of scope — Rivora's XDP dataplane has no L7 visibility,
so it can't enforce HTTPRoute's path/header matching rules; pretending to
would be a correctness hazard, not a feature. A node can run Service- and
Gateway-sourced VIPs side by side, both funneling into the same dataplane.

```sh
rivorad -kubernetes -interface eth0 -gateway-api [-speaker=true]
rivora-controller -gateway-api   # also needs the matching flag, for IPAM
```

Address assignment works exactly like a `Service`: `rivora-controller`
allocates from the same `AddressPool`s and patches `Gateway.status.addresses`
instead of `Service.Status.LoadBalancer.Ingress[].IP`. A `TCPRoute`/
`UDPRoute`'s `spec.rules[].backendRefs` resolve to a Service's
`EndpointSlice`s the same way the Service reconciler does; `backendRef.weight`
(Gateway API's native traffic-split field) maps onto the existing weighted-
Maglev backend selection, divided evenly across that backend's ready
endpoints. Same-namespace `backendRefs` only in this version — cross-
namespace references (which Gateway API gates behind a `ReferenceGrant`)
aren't implemented yet.

The [Helm chart](deploy/helm/rivora) wires both flags behind a
`gatewayApi.enabled` value and can optionally create a matching
`GatewayClass` — see
[`deploy/helm/rivora/README.md#gateway-api`](deploy/helm/rivora/README.md#gateway-api)
for the install command (the Gateway API CRDs themselves aren't bundled;
install them separately, same as any other vendor's CRDs) and full
reference.

**Verification status:** confirmed twice against a live cluster that both
reconcilers (`rivorad`'s and `rivora-controller`'s) start cleanly with
`-gateway-api` on, sync their informer caches, and that a `GatewayClass`
gets created with the correct `controllerName` — no crashes across two
independent fresh-image builds. The actual traffic path (address
assignment, real packets through the VIP, and the weighted-split
scenario) is **not yet verified live**: both attempts were blocked by a
pre-existing, intermittent new-pod-to-ClusterIP networking issue on the
test cluster, confirmed unrelated to Rivora (a plain `curl` debug pod
with zero Rivora involvement failed identically). Retry once that
cluster-level issue is resolved.

### BGP/BFD HA (v0.3)

`rivorad`'s BGP+BFD speaker is opt-in active/active ECMP HA: unlike the L2
ARP/NDP speaker above (which answers for a VIP from exactly one node at a
time, behind a cluster-wide Lease), every node with `-bgp`/`bgp.enabled`
on **independently** advertises a host route for each VIP it currently
has at least one healthy backend for — `/32` (`RF_IPv4_UC`) for IPv4 and
`/128` (`RF_IPv6_UC`) for IPv6 — and withdraws it the instant that stops
being true. Sessions negotiate both unicast families (dual AFI/SAFI) so
one peer can carry v4 and v6 VIP ads together. No leader election: BGP+
ECMP's whole point is multiple nodes advertising the same VIP
simultaneously, with upstream routers hashing traffic across next-hops.
BFD is wired per-peer (not a separate component) for sub-second peer-down
detection, feeding the same health-gated withdraw path. Unlike the
Gateway API feature above, this is entirely local to `rivorad` —
`rivora-controller` isn't involved, since BGP doesn't need cluster-wide
IPAM coordination.

Static-YAML mode reads a `bgp:` section (see
[`config/examples/bgp-ha.yaml`](config/examples/bgp-ha.yaml)); Kubernetes
mode uses flags:

```sh
rivorad -kubernetes -interface eth0 \
  -bgp -bgp-asn 65001 -bgp-router-id 10.0.0.11 \
  -bgp-ipv6-next-hop 2001:db8::11 \
  -bgp-peers "10.0.0.1:65000:bfd,10.0.0.2:65000"
```

`-bgp-peers` is a comma-separated list of `addr:asn` or `addr:asn:bfd`
entries. `routerId` is the BGP identifier and the IPv4 next-hop for
advertised `/32`s. IPv6 VIP ads need `-bgp-ipv6-next-hop` / `bgp.ipv6NextHop`
(the node's IPv6 address used as next-hop in `MP_REACH_NLRI`) — without
it, IPv4 ads still work and IPv6 VIPs are skipped with an error log.
The [Helm chart](deploy/helm/rivora) wires the equivalent `bgp.*` values —
see [`deploy/helm/rivora/README.md#bgpbfd-ha`](deploy/helm/rivora/README.md#bgpbfd-ha).

**A real caveat, not hidden:** in full-NAT mode — which is the **only**
mode K8s-managed VIPs currently run in (see [Kubernetes (v0.2)](#kubernetes-v02))
— a flow's connection state lives only on the node that first received
it. If the router's ECMP hash rebalances (a peer flaps, a node's
advertised route flaps, a node joins or leaves the ECMP set), in-flight
NAT'd connections on a node that's rebalanced away from can be disrupted,
since there's no cluster-shared connection table. DSR mode (available in
static-YAML deployments) doesn't have this problem — the backend itself
owns the reply path, so the load balancer is only ever in the forward
path. This is the same tradeoff MetalLB's BGP mode documents; it's an
inherent property of stateful NAT + ECMP, not a Rivora-specific gap.

**Verification status:** the health-gated advertise/withdraw/re-advertise
cycle is verified end-to-end against a real BGP session for both families
— an integration test peers rivorad's gobgp-backed speaker with a second,
independent in-process gobgp server over loopback, flips a fake backend's
health, and confirms the peer's RIB gains and loses the `/32` or `/128`
(see `internal/bgp`, including `TestSpeakerAdvertisesIPv6HostRoute`). Not
yet done: live verification against a real/containerized BGP peer
(FRRouting or BIRD) and BFD timing on the remote test cluster — this needs
an actual second router-like peer, which the loopback integration test
deliberately substitutes for correctness coverage without needing one.

## IPv6 (v0.3)

IPv6 is a first-class peer of IPv4 across the dataplane, IPAM, Kubernetes
reconciliation, L2 announcement, and BGP. A VIP's backends must all share
its address family; mixing v4 and v6 behind one VIP is rejected at
config-validation / reconcile time.

### Dataplane

XDP (`bpf/xdp_ingress.c`) and TCX (`bpf/tc_nat.c`) parse, forward, and
checksum-rewrite IPv6 TCP/UDP in both **DSR** and **full-NAT**, including
Maglev selection, connection affinity, graceful draining, and opt-in
per-source SYN rate limiting. Behavior mirrors IPv4 except where the
protocols differ (see [Maps and checksums](#maps-and-checksums)).

DSR health checks that would otherwise source from the VIP on loopback
use `preferred_lft 0` / omit the LB VIP from the node's preferred source
selection so probes still originate from a real node address.

### Static YAML

```sh
sudo ./bin/rivorad -config config/examples/single-vip-ipv6-nat.yaml -bpf-dir bpf
# or DSR:
sudo ./bin/rivorad -config config/examples/single-vip-ipv6-dsr.yaml -bpf-dir bpf
```

```yaml
interface: eth0
vips:
  - address: fd00:77::100
    port: 80
    protocol: tcp
    mode: nat   # or dsr (backends need VIP on lo + L2 MAC)
    backends:
      - address: fd00:77::11
        port: 8080
```

Shipped examples:

| File | Mode | Notes |
| --- | --- | --- |
| [`config/examples/single-vip-ipv6-nat.yaml`](config/examples/single-vip-ipv6-nat.yaml) | full-NAT | Backends route return traffic via the LB |
| [`config/examples/single-vip-ipv6-dsr.yaml`](config/examples/single-vip-ipv6-dsr.yaml) | DSR | Bind VIP on backend `lo` with `preferred_lft 0`; permanent neigh to LB MAC in labs |

### IPAM and AddressPool

`AddressPool.spec.addresses` accepts IPv4 or IPv6 CIDRs, ranges
(`a-b`), or single addresses — same field, same CRD.

- **Small prefixes** (host bits ≤ 16, e.g. `/112` or tighter) are expanded
  eagerly into concrete addresses, same as IPv4 `/24`.
- **Large prefixes** (host bits > 16, typically a `/64`) are kept as
  **sparse** prefixes: `rivora-controller` allocates by randomizing host
  bits inside the network prefix instead of enumerating 2^64 entries.
  Capacity accounting uses a capped virtual size so status stays useful.

```yaml
apiVersion: rivora.zyvor.dev/v1alpha1
kind: AddressPool
metadata:
  name: v6
spec:
  addresses:
    - 2001:db8:1::/64   # sparse
  autoAssign: true
```

Helm: `--set addressPools[0].addresses='{2001:db8:1::/64}'`. Sample used
by CI: [`deploy/helm/rivora/ci/addresspool-ipv6.yaml`](deploy/helm/rivora/ci/addresspool-ipv6.yaml).

### Kubernetes dual-stack

With `-kubernetes` (and optional `-gateway-api`):

- `Service` / `Gateway` status may carry IPv6 LoadBalancer / Gateway
  addresses allocated from IPv6 (or dual) pools.
- `EndpointSlice` backends are filtered to the **same address family** as
  the VIP — a v6 VIP only programs v6 endpoints.
- Dual-stack Services (both families in status) produce one VIP program
  path per family, each Maglev-partitioned independently.

```sh
rivorad -kubernetes -interface eth0 -speaker=true [-gateway-api]
rivora-controller [-gateway-api]   # same AddressPool IPAM for Gateway.status.addresses
```

### NDP speaker

The L2 speaker (`internal/speaker`, `-speaker` / `rivorad.speaker`, on by
default in Helm) is **ARP + NDP** behind one cluster-wide Lease:

- **IPv4** — gratuitous ARP + reply to ARP requests for NAT-mode VIPs.
- **IPv6** — unsolicited Neighbor Advertisements, join each VIP's
  solicited-node multicast, and reply to Neighbor Solicitations for
  NAT-mode IPv6 VIPs (`mdlayher/ndp`). Soft-fails to ARP-only if the
  interface has no IPv6 link-local (v4-only lab NICs).

Only the elected speaker answers; other nodes keep the dataplane warm
but do not claim the VIP on-link. DSR VIPs are not announced via NDP/ARP
by the speaker (the backend owns the VIP on-link).

Functional coverage: `scripts/selftest-ndp.sh` →
`TestNDPRespondsOnVeth` (root + veth).

### BGP IPv6 `/128`

See [BGP/BFD HA](#bgpbfd-ha-v03). Summary for IPv6:

| Setting | Role |
| --- | --- |
| `bgp.routerId` / `-bgp-router-id` | BGP ID + IPv4 next-hop for `/32` |
| `bgp.ipv6NextHop` / `-bgp-ipv6-next-hop` | IPv6 next-hop for `/128` (required to advertise IPv6 VIPs) |
| Peer AFI/SAFI | Both `AFI_IP` and `AFI_IP6` unicast negotiated on each peer |

```yaml
bgp:
  enabled: true
  asn: 65001
  routerId: 10.0.0.11
  ipv6NextHop: 2001:db8::11
  peers:
    - address: 10.0.0.1
      asn: 65000
      bfd: true
```

### Maps and checksums

Only maps keyed or valued by a raw address have IPv6 siblings:
`vip_map`, `backend_map`, `connection_affinity_map`, `nat_reverse_map`,
`rl_buckets_map`. Address-family-agnostic maps are shared as-is:
`service_config_map`, `maglev_table`, `backend_health_map`, `stats_map`,
`iface_mac_map`, `rl_config_map`.

IPv6-specific checksum handling:

1. IPv6 has **no** IP-header checksum (unlike IPv4).
2. A UDP checksum is **never** "unset" over IPv6 (IPv4 uses zero to mean
   unset); the dataplane always maintains a correct UDP checksum for v6.

### Verification

| Layer | How |
| --- | --- |
| Dataplane DSR + NAT, Maglev, failover | `scripts/selftest-ipv6.sh` (CI every push) |
| NDP solicit → advertise | `scripts/selftest-ndp.sh` (CI every push) |
| Sparse IPAM | `go test ./internal/ipam` (`TestParsePoolIPv6SparsePrefix`, …) |
| BGP `/128` advertise/withdraw | `go test ./internal/bgp` (`TestSpeakerAdvertisesIPv6HostRoute`) |
| Example configs still Load | `go test ./internal/config` (`TestExamplesLoadAndValidate`) |
| Helm IPv6 pool + `ipv6NextHop` | CI `helm` job |
| AddressPool CRD + IPv6 sample | CI `crd` job |

Still ahead as **live-cluster** hardening (not blocking the product
surface): end-to-end dual-stack Service traffic on the remote test
cluster, and BGP IPv6 peering against a real router (FRR/BIRD).

## Building on the remote host

`scripts/deploy-remote.sh` (same shape as the sibling `guestkit` repo's
script) rsyncs the source, installs build deps, builds, and installs:

```sh
make deploy-remote H=<host> U=<user>          # full deploy
make deploy-remote-quick H=<host> U=<user>    # skip dependency install
make deploy-remote-verify H=<host> U=<user>   # re-run all selftest scripts
make bpf build                                # local Linux build → bpf/*.o + bin/*
make selftest-all                             # every selftest (needs root + bpftool)
```

`deploy-remote-verify` runs IPv4 selftests plus
`scripts/selftest-ipv6.sh` and `scripts/selftest-ndp.sh` (failures are
warnings for deploy, hard failures in CI).

## Selftests and CI

Each `scripts/selftest*.sh` builds an isolated netns/veth/bridge topology
(never touching the host's real interfaces) and runs `rivorad` (or a
focused Go test) against it:

| Target / script | What it proves |
| --- | --- |
| `make selftest` / `selftest.sh` | Single VIP, DSR + NAT, Maglev spread, health failover |
| `make selftest-multivip` | Two VIPs don't interfere; draining excludes new flows |
| `make selftest-weighted` | 9:1 weighted Maglev skew |
| `make selftest-ratelimit` | Tight per-source limit drops burst; disabled = no-op |
| `make selftest-ipv6` | Same as first script over an all-IPv6 topology |
| `make selftest-ndp` | Speaker answers NS with NA for a NAT IPv6 VIP |
| `make selftest-all` | All of the above |

GitHub Actions (`.github/workflows/ci.yml`) on every push/PR:

| Job | Checks |
| --- | --- |
| `go` | `go mod tidy`, `vet`, build all cmds, `go test -race ./…`, `gofmt` |
| `helm` | lint; template IPv4 pool; IPv6 pool + BGP `ipv6NextHop`; Gateway API |
| `crd` | Structural validate AddressPool CRD + IPv6 sample YAML |
| `bpf` | `make bpf` (clang), upload `bpf/*.o` |
| `integration` | Needs `go`+`bpf`; runs every selftest above as root |

## Operator docs (GitHub Pages)

Published Docusaurus site (same shape as Netra):
**[zyvorai.github.io/rivora](https://zyvorai.github.io/rivora/)**.
Sources live under `website/`; preview with `make docs-serve`, production
build with `make docs-build`. Deploys on every push that touches
`website/` or `docs/social/` via `.github/workflows/pages.yml`.

## Roadmap

v0.1 was deliberately narrow: single VIP, single node, IPv4 only, static
config.

v0.2 (Kubernetes integration) is implemented and verified end-to-end
against a live cluster (IPAM allocation/release, Service/EndpointSlice
reconciliation, Maglev spread across scaling backends, ARP resolution via
the speaker) — see [Kubernetes (v0.2)](#kubernetes-v02) for usage. The
container images the Helm chart's `image.rivorad`/`image.controller`
values reference are now published to `ghcr.io/zyvorai/` on every version
tag (cosign-signed, with an SBOM) by `.github/workflows/release.yml`; the
original live-cluster verification predates that and used locally-built
images.

v0.3 is underway. KubeVirt VMs and external/physical backends are done —
see [Backends beyond Pods](#backends-beyond-pods-kubevirt-vms-and-externalphysical-ips)
(verified against real KubeVirt VMIs and hand-authored EndpointSlices on
a live cluster; turned out to need zero dataplane/reconciler changes).
Gateway API is implemented — see [Gateway API](#gateway-api-v03) — with
reconciler startup/informer-sync/object-creation confirmed live twice;
the traffic-path scenarios are still pending a retry once an unrelated
cluster networking issue on the test host clears. BGP/BFD HA is
implemented and locally verified for IPv4 `/32` and IPv6 `/128` — see
[BGP/BFD HA](#bgpbfd-ha-v03) — with the health-gated advertise/withdraw
cycle proven against a real BGP session (loopback-peered gobgp); live
verification against a real/containerized router peer on the remote test
cluster is a separate follow-up. IPv6 is done end-to-end for the product
surface (dataplane, sparse IPAM, dual-stack Service/Gateway
reconciliation, NDP speaker, BGP `/128`, CI selftests) — see
[IPv6](#ipv6-v03). Still ahead as live-cluster hardening: end-to-end
dual-stack Service traffic on the remote test cluster and BGP IPv6
peering against a real router.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
