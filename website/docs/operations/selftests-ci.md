---
sidebar_position: 2
title: Selftests and CI
---

# Selftests and CI

Each `scripts/selftest*.sh` builds an isolated netns/veth topology and
runs `rivorad` (or a focused Go test) as root.

| Target | Proves |
| --- | --- |
| `make selftest` | Single VIP, DSR + NAT, Maglev, health failover |
| `make selftest-multivip` | Two VIPs; draining excludes new flows |
| `make selftest-weighted` | 9:1 Maglev skew |
| `make selftest-ratelimit` | Tight per-source limit; disabled = no-op |
| `make selftest-vipratelimit` | Per-VIP limits: own limit, per-VIP buckets, override of the node-wide limit, reload lifts it; IPv4 + IPv6 |
| `make selftest-portrange` | Port-range VIPs: TCP/UDP, NAT/DSR, IPv4 + IPv6, exact-port precedence, reload, adoption after a restart |
| `make selftest-checksum` | Full-NAT leaves TCP/UDP checksums valid (IPv4 + IPv6), with checksum offload off so every packet is really verified |
| `make selftest-edgecases` | VLAN/QinQ, IP options, IPv4 fragments (NAT + DSR), ICMP path-MTU steering (IPv4 + IPv6) |
| `make selftest-l3dsr` | L3 DSR: IP-in-IP and GRE, IPv4 + IPv6, backend a routed hop away, tunnel source configured and auto-detected |
| `make selftest-bgp` | Real rivorad and a BGP peer two hops away: multihop, TCP MD5 (match/mismatch/missing), route communities |
| `make selftest-ipv6-ext` | IPv6 extension headers (Destination Options, Hop-by-Hop chained) and fragmented UDP through NAT and DSR, checked by real reassembly on both ends |
| `make selftest-ipv6-policy` | IPv6 clientIP affinity, and IPv6 drop counters (rate_limited, no_backend, unserved) |
| `make selftest-ipv6` | Same as first over all-IPv6 |
| `make selftest-ndp` | NS → NA for NAT IPv6 VIP |
| `make selftest-all` | All of the above |

```sh
make deploy-remote-verify H=<host> U=<user>
```

## GitHub Actions

| Job | Checks |
| --- | --- |
| `go` | `mod tidy`, `vet`, build, `go test -race ./…`, `gofmt` |
| `helm` | lint; IPv4/IPv6 pools; Gateway API template |
| `crd` | AddressPool CRD + IPv6 sample |
| `bpf` | `make bpf` |
| `integration` | Every selftest above |

Docs site: `.github/workflows/pages.yml` → this site (Docusaurus, same
shape as Netra).
