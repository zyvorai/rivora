# Rivora

[![CI](https://github.com/zyvorai/rivora/actions/workflows/ci.yml/badge.svg)](https://github.com/zyvorai/rivora/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

![Rivora — eBPF-native load balancer for Kubernetes and bare metal](docs/social/rivora-share-card.png)

**eBPF-native load balancing for every environment.**

Rivora owns VIPs, backend selection, health checking and NAT/DSR — the
traffic-delivery layer the Zyvor platform doesn't yet have. It's
CNI-independent and doesn't require Cilium: it attaches its own XDP/TCX
programs and owns its own maps under `/sys/fs/bpf/rivora-lb`.

> **Status: v0.1 shipped, v0.2 (Kubernetes) implemented and verified
> end-to-end on a live cluster.** Single node, IPv4 TCP/UDP, DSR and
> full-NAT forwarding, Maglev backend selection with graceful draining,
> active TCP health checks. A node can run multiple VIPs, each
> independently reconciled and Maglev-partitioned — either from static
> YAML, or (v0.2, new) from Kubernetes `Service`/`EndpointSlice` objects
> via `rivorad -kubernetes`, with `rivora-controller` handling `AddressPool`
> IPAM and an in-`rivorad` L2/ARP speaker announcing assigned VIPs. BGP/BFD
> HA is implemented (opt-in, active/active ECMP) but not yet live-verified
> against a real router peer. IPv6 dataplane support (static-YAML mode
> only so far) is implemented and CI-verified — see [Roadmap](#roadmap).

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
- [IPv6 (v0.3, in progress)](#ipv6-v03-in-progress)
- [Building on the remote host](#building-on-the-remote-host)
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
  `--format json` for machine-readable output.
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
       |                                 cluster-wide Lease): ARP for the
       |                                 assigned VIP
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
internal/speaker/       L2/ARP responder for K8s-managed VIPs (mdlayher/arp)
bpf/                    XDP ingress + TCX egress programs (hand-rolled, no libbpf headers)
deploy/helm/rivora/     Helm chart: rivorad DaemonSet, rivora-controller Deployment, AddressPool CRD
deploy/systemd/         systemd unit for the static-YAML/non-Kubernetes deployment
scripts/                selftest.sh, selftest-multivip.sh, deploy-remote.sh
config/examples/        static-YAML config examples (DSR, full-NAT, multi-VIP)
```

## Securing the API

`rivorad`'s local API (`127.0.0.1:9870` by default) is plain HTTP and
unauthenticated out of the box — fine for a loopback-only listener, but both
are opt-in to lock down, same env-var-driven shape as netra's `netrad`:

| Env var                    | Effect                                              |
| --------------------------- | ---------------------------------------------------- |
| `RIVORA_API_KEY`            | Require this bearer token on every request           |
| `RIVORA_TLS_CERT` / `_KEY`  | Serve HTTPS with this certificate                     |
| `RIVORA_TLS_SELF_SIGNED`    | Serve HTTPS with an auto-generated self-signed cert (no cert files needed) |

`rivoractl` picks up `RIVORA_API_KEY` and `RIVORA_TLS_INSECURE` (accept a
self-signed cert) from its own environment, or via `--api-key`/
`--tls-insecure`. `rivora-doctor` reports which of these are set.

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
NAT), `multi-vip.yaml` (several VIPs on one node, mixing modes),
`weighted-backends.yaml` (unequal traffic shares within one VIP), and
`rate-limited.yaml` (opt-in per-source-IP SYN-flood protection) for what
each forwarding mode requires from your backends:

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
(see `api/v1alpha1` for the CRD shape). An in-`rivorad` L2/ARP speaker
(`internal/speaker`), behind its own single cluster-wide Lease, announces
the assigned VIP.

The [Helm chart](deploy/helm/rivora) installs both pieces plus the
`AddressPool` CRD:

```sh
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
```

See [`deploy/helm/rivora/README.md`](deploy/helm/rivora/README.md) for the
full chart reference, including the CRD's manual-upgrade caveat.

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
ARP speaker above (which answers for a VIP from exactly one node at a
time, behind a cluster-wide Lease), every node with `-bgp`/`bgp.enabled`
on **independently** advertises a `/32` host route for each VIP it
currently has at least one healthy backend for, and withdraws it the
instant that stops being true — no leader election, since BGP+ECMP's
whole point is multiple nodes advertising the same VIP simultaneously,
with upstream routers hashing traffic across next-hops. BFD is wired
per-peer (not a separate component) for sub-second peer-down detection,
feeding the same health-gated withdraw path. Unlike the Gateway API
feature above, this is entirely local to `rivorad` — `rivora-controller`
isn't involved, since BGP doesn't need cluster-wide IPAM coordination.

Static-YAML mode reads a `bgp:` section (see
[`config/examples/bgp-ha.yaml`](config/examples/bgp-ha.yaml)); Kubernetes
mode uses flags:

```sh
rivorad -kubernetes -interface eth0 \
  -bgp -bgp-asn 65001 -bgp-router-id 10.0.0.11 \
  -bgp-peers "10.0.0.1:65000:bfd,10.0.0.2:65000"
```

`-bgp-peers` is a comma-separated list of `addr:asn` or `addr:asn:bfd`
entries. The [Helm chart](deploy/helm/rivora) wires the equivalent
`bgp.*` values — see
[`deploy/helm/rivora/README.md#bgpbfd-ha`](deploy/helm/rivora/README.md#bgpbfd-ha).

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
cycle is verified end-to-end against a real BGP session — an integration
test peers rivorad's gobgp-backed speaker with a second, independent
in-process gobgp server over loopback, flips a fake backend's health, and
confirms the peer's own RIB actually gains and loses the route (see
`internal/bgp`). Not yet done: live verification against a real/
containerized BGP peer (FRRouting or BIRD) and BFD timing on the remote
test cluster — this needs an actual second router-like peer, which the
loopback integration test deliberately substitutes for correctness
coverage without needing one.

## IPv6 (v0.3, in progress)

The dataplane foundation is implemented: `rivorad` parses, forwards, and
checksum-rewrites IPv6 TCP/UDP traffic in both DSR and full-NAT mode,
in **static-YAML config mode only** — Kubernetes reconciliation, IPAM
(`AddressPool`), and an NDP responder (IPv6's equivalent of the L2/ARP
speaker) are separate, not-yet-implemented follow-on phases. A VIP's
backends must all share its own address family; mixing v4 and v6 behind
one VIP is rejected at config-validation time.

```yaml
interface: eth0
vips:
  - address: fd00:77::100
    port: 80
    protocol: tcp
    mode: nat
    backends:
      - address: fd00:77::11
        port: 8080
```

Under the hood, only the maps keyed or valued by a raw address
(`vip_map`, `backend_map`, `connection_affinity_map`, `nat_reverse_map`,
`rl_buckets_map`) have IPv6 siblings — `service_config_map`,
`maglev_table`, `backend_health_map`, `stats_map`, `iface_mac_map`, and
`rl_config_map` are address-family-agnostic and shared as-is between v4
and v6 VIPs/backends, including the same Maglev table and ID allocators.
Two IPv6-specific checksum differences from v4 are handled explicitly:
IPv6 has no IP-header checksum at all, and a UDP checksum is never
"unset" over IPv6 (unlike v4, where zero means unset) — both matter for
the full-NAT rewrite path's correctness.

Verified via `scripts/selftest-ipv6.sh` (DSR + full-NAT, Maglev spread,
failover) in CI on every push — BPF C can't be compiled or tested on
macOS, so this couldn't be verified locally during development the way
the Go-side changes were; CI (a real Linux runner) is the actual
verification. Not yet verified against a live cluster or real hardware.

## Building on the remote host

`scripts/deploy-remote.sh` (same shape as the sibling `guestkit` repo's
script) rsyncs the source, installs build deps, builds, and installs:

```sh
make deploy-remote H=<host> U=<user>          # full deploy
make deploy-remote-quick H=<host> U=<user>    # skip dependency install
make deploy-remote-verify H=<host> U=<user>   # re-run all selftest scripts
```

`scripts/selftest.sh`, `scripts/selftest-multivip.sh`,
`scripts/selftest-weighted.sh`, `scripts/selftest-ratelimit.sh`, and
`scripts/selftest-ipv6.sh` each build an isolated network-namespace/veth/
bridge topology (never touching a host's real interfaces) and run
`rivorad` against it: the first checks Maglev spread and health-check-
driven failover for a single VIP in both DSR and NAT mode; the second
checks that two independent VIPs on one node don't interfere with each
other and that a draining backend is excluded from new connections
without disrupting its established ones; the third checks that a
9:1-weighted VIP decisively skews traffic toward the heavier-weighted
backend; the fourth checks that a low, deliberately-tight per-source
rate limit drops most of a connection burst, recovers once the bucket
refills, and has zero effect when left disabled (the default); the fifth
repeats the first script's DSR/NAT/Maglev/failover checks over an
all-IPv6 topology.

## Roadmap

v0.1 was deliberately narrow: single VIP, single node, IPv4 only, static
config.

v0.2 (Kubernetes integration) is implemented and verified end-to-end
against a live cluster (IPAM allocation/release, Service/EndpointSlice
reconciliation, Maglev spread across scaling backends, ARP resolution via
the speaker) — see [Kubernetes (v0.2)](#kubernetes-v02) for usage. Still
ahead: publishing the container images the Helm chart's
`image.rivorad`/`image.controller` values reference (verification so far
used locally-built images, not a published registry).

v0.3 is underway. KubeVirt VMs and external/physical backends are done —
see [Backends beyond Pods](#backends-beyond-pods-kubevirt-vms-and-externalphysical-ips)
(verified against real KubeVirt VMIs and hand-authored EndpointSlices on
a live cluster; turned out to need zero dataplane/reconciler changes).
Gateway API is implemented — see [Gateway API](#gateway-api-v03) — with
reconciler startup/informer-sync/object-creation confirmed live twice;
the traffic-path scenarios are still pending a retry once an unrelated
cluster networking issue on the test host clears. BGP/BFD HA is
implemented and locally verified — see [BGP/BFD HA](#bgpbfd-ha-v03) — with
the health-gated advertise/withdraw cycle proven against a real BGP
session (a loopback-peered gobgp integration test); live verification
against a real/containerized router peer on the remote test cluster is a
separate follow-up, not yet done. IPv6's dataplane foundation is done —
see [IPv6](#ipv6-v03-in-progress) — with a new `scripts/selftest-ipv6.sh`
green in CI on every push (DSR + full-NAT, Maglev spread, failover);
still ahead for IPv6: the randomized-within-prefix IPAM allocator
redesign `internal/ipam`'s current eager-CIDR-expansion model can't
support (a real /64 has ~18 quintillion addresses), dual-stack
Kubernetes reconciliation, and an NDP responder (the IPv6 analog of the
L2/ARP speaker) — each its own follow-on phase, mirroring how the
dataplane foundation itself was scoped and landed as a first, focused
step rather than attempting all of IPv6 in one change.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
