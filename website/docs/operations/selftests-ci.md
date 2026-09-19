---
sidebar_position: 7
title: Selftests and CI
---

# Selftests and CI

Rivora's behaviour is checked at three levels: Go unit tests, **selftests** (a real `rivorad` and real
traffic in an isolated network namespace), and CI, which runs both on every push and pull request.

## Selftests

Each `scripts/selftest*.sh` builds an isolated topology of network namespaces, veth pairs and a bridge
(it never touches a host interface), runs `rivorad` (or a focused Go test) inside it as root, and drives
real traffic. They need root, `ip`, `curl`, `python3` and a Linux kernel with XDP; several need `ethtool`
or `tcpdump`. Run one with `sudo ./scripts/selftest-X.sh`, or `make selftest-X`.

| Target | Proves |
| --- | --- |
| `make selftest` | Single VIP, DSR and NAT, Maglev spread, health failover |
| `make selftest-multivip` | Two VIPs do not interfere; draining excludes new flows |
| `make selftest-weighted` | 9:1 Maglev skew |
| `make selftest-affinity` | `sessionAffinity: clientIP`: stickiness, spread across sources, reload, restart, failover |
| `make selftest-ratelimit` | Tight node-wide per-source limit drops a burst; disabled is a no-op |
| `make selftest-vipratelimit` | Per-VIP limits: own limit, isolation between VIPs, override of the node-wide limit, reload lifts it; IPv4 and IPv6 |
| `make selftest-drops` | Per-reason drop counters: `rate_limited`, `no_healthy_backend`, `unserved` |
| `make selftest-httpcheck` | HTTP probes: an open port with a failing app is marked down; reload; a bad config is rejected |
| `make selftest-portrange` | Port-range VIPs: TCP and UDP, NAT and DSR, IPv4 and IPv6, exact-port precedence, reload, adoption after restart |
| `make selftest-checksum` | Full-NAT leaves TCP and UDP checksums valid (IPv4 and IPv6), with checksum offload off so every packet is really verified |
| `make selftest-edgecases` | VLAN and QinQ, IP options, IPv4 fragments (NAT and DSR), ICMP path-MTU steering (IPv4 and IPv6) |
| `make selftest-l3dsr` | L3 DSR: IP-in-IP and GRE, IPv4 and IPv6, a routed backend, tunnel source configured and detected, the oversize-packet answer, native XDP |
| `make selftest-ipv6` | The dataplane basics over an all-IPv6 topology |
| `make selftest-ipv6-policy` | IPv6 clientIP affinity and IPv6 drop counters |
| `make selftest-ipv6-ext` | IPv6 Hop-by-Hop and Destination Options headers, and fragmented UDP, through NAT and DSR, checked by real reassembly at both ends, plus the chain-length boundary |
| `make selftest-ndp` | NS answered with NA for a NAT IPv6 VIP |
| `make selftest-bgp` | A real `rivorad` and a BGP peer two hops away across a router: multihop and TCP MD5 (match, mismatch, missing), route communities |
| `make selftest-restart` | A restart under steady load, default versus `-persist-datapath` |
| `make selftest-adopt` | Restart reconciliation: stale VIPs removed, shifted IDs kept, no cross-VIP forwarding |
| `make selftest-xdpmode` | Generic, native and auto attach; changing modes |
| `make selftest-apiauth` | API roles, key rotation, the failure metric, certificate pinning, mTLS, bad key configs, the audit trail |
| `make selftest-all` | All of the above |

## Go tests

`go test -race ./...` covers the control plane: config loading and validation, allocators, Maglev, the
Kubernetes and Gateway reconcilers (against fake clients), IPAM, BGP against in-process gobgp routers, the
API and its authentication, and every packet-independent decision the dataplane makes. A few tests need
root and Linux and skip themselves otherwise; CI runs them privileged:

- **TCP MD5** (`go test -run TestMD5 ./internal/bgp`) needs `CAP_NET_ADMIN`.
- **Datapath adoption** (`go test -run 'TestKubernetesModeAdopts|TestPruneUnclaimed' ./internal/dataplane`)
  creates real BPF maps (unpinned, so it never touches a running `rivorad`'s) and needs the compiled objects
  (`make bpf`) and root.

## How the tests are kept honest

- **Checksums are really checked.** `veth` marks packets as already verified, so a NAT that corrupts a
  checksum would pass every test. The checksum-sensitive selftests turn offload off on **both** ends of every
  pair (including the bridge side) so the receiving kernel actually verifies.
- **Mutation testing.** Each new behaviour was checked against deliberately broken variants of the code
  (a dropped `csum_replace`, an unrecorded fragment, an inverted deny list, a removed guard). A test that
  survives a mutation proves nothing, so the tests were strengthened until each mutant failed.
- **Fail before, pass after.** A regression selftest is run against the commit before the fix to show it
  fails there.
- **Assert on Rivora's own counters** where the host's connection tracking can make a bridged flow fail
  sporadically.
- **Isolation and cleanup.** Names are prefixed (`riv-*` namespaces, per-script veth prefixes, `rbr*`
  bridges), processes are started inside their namespace and killed by namespace before it is deleted (an exit trap
  does this even on failure). After a run, `ip netns list` and `ip -o link` should show none of them; on a
  shared host, only ever touch those.

## GitHub Actions

`.github/workflows/ci.yml` on every push and pull request:

| Job | Checks |
| --- | --- |
| `go` | `go mod tidy` clean, `go vet`, build every command, `go test -race ./...`, the privileged BGP MD5 and adoption tests, `gofmt` |
| `security` | `govulncheck`; fails only on findings not accepted in `SECURITY.md` |
| `helm` | Lint; template IPv4 pool, IPv6 pool with a BGP next hop, Gateway API, PDB and NetworkPolicy; the chart embedded in the `rivora` CLI matches `deploy/helm/rivora` |
| `crd` | Structurally validate the AddressPool CRD and the IPv6 sample |
| `cli` | Build the `rivora` CLI and run `install`, `status`, `upgrade` and `uninstall` against a kind cluster (its pods cannot pull unpublished images, so this checks the Helm objects, not traffic) |
| `web` | Typecheck and build the console; the committed bundle matches its source |
| `lint` | Every shell script parses; every example config validates; no hardcoded console credentials; workflows pass actionlint |
| `bpf` | `make bpf` with clang; uploads `bpf/*.o` |
| `integration` | Needs `go` and `bpf`; runs every selftest above as root |

`release.yml` builds, signs (cosign, keyless) and pushes the `rivorad` and `rivora-controller` images to
`ghcr.io/zyvorai/` with an SBOM on every `vX.Y.Z` tag, after a smoke test that the binary starts. `pages.yml`
builds and deploys this site when `website/` changes.

## Running everything

```sh
make bpf build           # bpf/*.o and bin/*
make selftest-all        # every selftest; needs root and a Linux host
make deploy-remote-verify H=<host> U=<user>   # the same, on a remote build host
```

On macOS use the remote path (`make deploy-remote H=<host> U=<user>`, then `deploy-remote-verify`): the
BPF programs only build and load on Linux.
