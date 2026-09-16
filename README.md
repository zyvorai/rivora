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
> IPAM and an in-`rivorad` L2/ARP speaker announcing assigned VIPs. No
> BGP/BFD HA yet — see [Roadmap](#roadmap).

## Contents

- [How it works](#how-it-works)
- [Architecture](#architecture)
- [Repository](#repository)
- [Securing the API](#securing-the-api)
- [Quickstart](#quickstart)
- [Kubernetes (v0.2)](#kubernetes-v02)
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

## Building on the remote host

`scripts/deploy-remote.sh` (same shape as the sibling `guestkit` repo's
script) rsyncs the source, installs build deps, builds, and installs:

```sh
make deploy-remote H=<host> U=<user>          # full deploy
make deploy-remote-quick H=<host> U=<user>    # skip dependency install
make deploy-remote-verify H=<host> U=<user>   # re-run all selftest scripts
```

`scripts/selftest.sh`, `scripts/selftest-multivip.sh`,
`scripts/selftest-weighted.sh`, and `scripts/selftest-ratelimit.sh` each
build an isolated network-namespace/veth/bridge topology (never touching
a host's real interfaces) and run `rivorad` against it: the first checks
Maglev spread and health-check-driven failover for a single VIP in both
DSR and NAT mode; the second checks that two independent VIPs on one node
don't interfere with each other and that a draining backend is excluded
from new connections without disrupting its established ones; the third
checks that a 9:1-weighted VIP decisively skews traffic toward the
heavier-weighted backend; the fourth checks that a low, deliberately-tight
per-source rate limit drops most of a connection burst, recovers once the
bucket refills, and has zero effect when left disabled (the default).

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

BGP/BFD HA, IPv6, KubeVirt/physical backends, and Gateway API come in later
milestones.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
