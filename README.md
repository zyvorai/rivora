# Rivora

[![CI](https://github.com/zyvorai/rivora/actions/workflows/ci.yml/badge.svg)](https://github.com/zyvorai/rivora/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**eBPF-native load balancing for every environment.**

Rivora owns VIPs, backend selection, health checking and NAT/DSR — the
traffic-delivery layer the Zyvor platform doesn't yet have. It's
CNI-independent and doesn't require Cilium: it attaches its own XDP/TCX
programs and owns its own maps under `/sys/fs/bpf/rivora-lb`.

> **Status: v0.1 shipped, v0.2 (Kubernetes) implemented, pending
> end-to-end cluster verification.** Single node, IPv4 TCP/UDP, DSR and
> full-NAT forwarding, Maglev backend selection with graceful draining,
> active TCP health checks. A node can run multiple VIPs, each
> independently reconciled and Maglev-partitioned — either from static
> YAML, or (v0.2, new) from Kubernetes `Service`/`EndpointSlice` objects
> via `rivorad -kubernetes`, with `rivora-controller` handling `AddressPool`
> IPAM and an in-`rivorad` L2/ARP speaker announcing assigned VIPs. No
> BGP/BFD HA yet — see [Roadmap](#roadmap).

## How it works

- **`bpf/xdp_ingress.c`** — XDP program: match the VIP, pick a backend
  (sticky per-flow via `connection_affinity_map`, otherwise Maglev
  consistent hashing over `maglev_table`), then either rewrite the
  destination MAC and `XDP_TX` (**DSR**) or rewrite the destination IP/port
  and `XDP_PASS` to normal routing (**full NAT**).
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
NAT), and `multi-vip.yaml` (several VIPs on one node, mixing modes) for what
each forwarding mode requires from your backends:

- **DSR** — backends need the VIP bound locally (loopback/dummy interface)
  and must be L2-reachable from the Rivora node; no backend MAC = no DSR.
- **Full NAT** — no backend changes needed, but backends must route return
  traffic back through the Rivora node (default gateway, or a static route
  for the client subnet).

With more than one VIP configured, `rivoractl status`/`/api/v1/status`
return an error pointing you at `rivoractl vips`/`/api/v1/vips` instead,
which lists every VIP.

## Building on the remote host

`scripts/deploy-remote.sh` (same shape as the sibling `guestkit` repo's
script) rsyncs the source, installs build deps, builds, and installs:

```sh
make deploy-remote H=<host> U=<user>          # full deploy
make deploy-remote-quick H=<host> U=<user>    # skip dependency install
make deploy-remote-verify H=<host> U=<user>   # re-run both selftest scripts
```

`scripts/selftest.sh` and `scripts/selftest-multivip.sh` each build an
isolated network-namespace/veth/bridge topology (never touching a host's
real interfaces) and run `rivorad` against it: the former checks Maglev
spread and health-check-driven failover for a single VIP in both DSR and
NAT mode; the latter checks that two independent VIPs on one node don't
interfere with each other and that a draining backend is excluded from new
connections without disrupting its established ones.

## Roadmap

v0.1 was deliberately narrow: single VIP, single node, IPv4 only, static
config.

v0.2 (Kubernetes integration) is implemented:
- Dataplane/BPF foundation for multiple VIPs and graceful draining
  (`scripts/selftest-multivip.sh`).
- `internal/ipam` + `AddressPool` CRD (`deploy/helm/rivora/crds`) for VIP
  address-pool allocation, reconciled by the leader-elected
  `rivora-controller` Deployment (`cmd/rivora-controller`), which patches
  `Service.Status.LoadBalancer.Ingress[].IP`.
- `internal/controller`: an in-`rivorad` `Service`/`EndpointSlice`
  reconciler (client-go informers, no leader election — every node mirrors
  the same cluster state into its own BPF maps independently), driving the
  same `Dataplane.UpsertVIP`/`RemoveVIP` the static-YAML path uses.
  K8s-managed VIPs are NAT-only. Enable with `rivorad -kubernetes
  -interface <iface> [-loadbalancer-class <class>]`.
- `internal/speaker`: an L2/ARP responder for K8s-managed VIPs
  (`mdlayher/arp`), behind a single cluster-wide leader-elected Lease.
- `deploy/helm/rivora`: Helm chart for both binaries plus the
  `AddressPool` CRD.

Still ahead before v0.2 is fully done: end-to-end verification against a
live cluster (a dedicated namespace on the shared remote test host's k3s
install), and the container images the Helm chart's `image.rivorad`/
`image.controller` values reference.

BGP/BFD HA, IPv6, KubeVirt/physical backends, and Gateway API come in later
milestones.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
