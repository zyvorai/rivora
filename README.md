# Rivora

[![CI](https://github.com/zyvorai/rivora/actions/workflows/ci.yml/badge.svg)](https://github.com/zyvorai/rivora/actions/workflows/ci.yml)
[![License: Apache 2.0](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

**eBPF-native load balancing for every environment.**

Rivora owns VIPs, backend selection, health checking and NAT/DSR — the
traffic-delivery layer the Zyvor platform doesn't yet have. It's
CNI-independent and doesn't require Cilium: it attaches its own XDP/TCX
programs and owns its own maps under `/sys/fs/bpf/rivora-lb`.

> **Status: v0.1.** Single node, one VIP, IPv4 TCP/UDP, static YAML config,
> DSR and full-NAT forwarding, Maglev backend selection, active TCP health
> checks. No Kubernetes controller, BGP, or multi-VIP yet — see
> [Roadmap](#roadmap).

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

See `config/examples/single-vip.yaml` (DSR) and
`config/examples/single-vip-nat.yaml` (full NAT) for the two forwarding
modes and what each requires from your backends:

- **DSR** — backends need the VIP bound locally (loopback/dummy interface)
  and must be L2-reachable from the Rivora node; no backend MAC = no DSR.
- **Full NAT** — no backend changes needed, but backends must route return
  traffic back through the Rivora node (default gateway, or a static route
  for the client subnet).

## Building on the remote host

`scripts/deploy-remote.sh` (same shape as the sibling `guestkit` repo's
script) rsyncs the source, installs build deps, builds, and installs:

```sh
make deploy-remote H=<host> U=<user>          # full deploy
make deploy-remote-quick H=<host> U=<user>    # skip dependency install
make deploy-remote-verify H=<host> U=<user>   # just re-run scripts/selftest.sh
```

`scripts/selftest.sh` builds an isolated network-namespace/veth/bridge
topology (never touches a host's real interfaces), runs `rivorad` against
it, and checks that Maglev spreads traffic across backends and that killing
a backend takes it out of rotation within the health-check interval.

## Roadmap

v0.1 is deliberately narrow: single VIP, single node, IPv4 only, static
config. Kubernetes `LoadBalancer` Services, multi-VIP, BGP/BFD HA, IPv6,
KubeVirt/physical backends, and Gateway API come in later milestones — see
the design notes for the full v0.2–v0.4 plan.

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
