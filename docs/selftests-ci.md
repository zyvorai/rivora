# Selftests and CI

Each `scripts/selftest*.sh` builds an isolated netns/veth topology (never
touching the host's real interfaces) and runs `rivorad` or a focused Go
test as root.

| Target | Proves |
| --- | --- |
| `make selftest` | Single VIP, DSR + NAT, Maglev, health failover |
| `make selftest-multivip` | Two VIPs; draining excludes new flows |
| `make selftest-weighted` | 9:1 Maglev skew |
| `make selftest-ratelimit` | Tight per-source limit; disabled = no-op |
| `make selftest-ipv6` | Same as first over all-IPv6 |
| `make selftest-ndp` | NS → NA for NAT IPv6 VIP |
| `make selftest-all` | All of the above |

Remote verify:

```sh
make deploy-remote-verify H=<host> U=<user>
```

Runs IPv4 selftests plus IPv6 and NDP (warnings on deploy; hard fail in CI).

## GitHub Actions

Workflow: [`.github/workflows/ci.yml`](https://github.com/zyvorai/rivora/blob/main/.github/workflows/ci.yml)

| Job | Checks |
| --- | --- |
| `go` | `mod tidy`, `vet`, build all cmds, `go test -race ./…`, `gofmt` |
| `helm` | lint; IPv4 pool; IPv6 pool + `ipv6NextHop`; Gateway API template |
| `crd` | Structural validate AddressPool CRD + IPv6 sample |
| `bpf` | `make bpf`, upload objects |
| `integration` | Every selftest above (needs `go` + `bpf`) |

Docs site deploy: [`.github/workflows/docs.yml`](https://github.com/zyvorai/rivora/blob/main/.github/workflows/docs.yml)
→ [zyvorai.github.io/rivora](https://zyvorai.github.io/rivora/).

```sh
make docs-serve   # local MkDocs preview
make docs-build   # strict build into site/
```
