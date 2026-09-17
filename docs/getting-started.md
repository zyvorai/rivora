# Getting started

Rivora needs a real Linux kernel (XDP/eBPF). From macOS or a thin laptop,
use the remote deploy path below.

## Local Linux build

```sh
make bpf build                     # bpf/*.o + bin/{rivorad,rivoractl,rivora-doctor,rivora-controller}
sudo ./bin/rivora-doctor           # host readiness
sudo ./bin/rivorad -config config/examples/single-vip.yaml -bpf-dir bpf
./bin/rivoractl status
```

### Example configs

| File | Mode |
| --- | --- |
| `config/examples/single-vip.yaml` | DSR |
| `config/examples/single-vip-nat.yaml` | full-NAT |
| `config/examples/single-vip-ipv6-nat.yaml` | IPv6 NAT |
| `config/examples/single-vip-ipv6-dsr.yaml` | IPv6 DSR |
| `config/examples/multi-vip.yaml` | several VIPs |
| `config/examples/weighted-backends.yaml` | Maglev weights |
| `config/examples/rate-limited.yaml` | per-source SYN limit |
| `config/examples/bgp-ha.yaml` | BGP+BFD |

### Forwarding modes

- **DSR** — backends need the VIP on loopback/dummy and must be L2-reachable;
  backends supply a MAC. For IPv6 DSR labs, bind the VIP on `lo` with
  `preferred_lft 0` and install a permanent neigh to the LB MAC.
- **Full NAT** — no backend VIP binding; backends must return traffic through
  the Rivora node so TCX can un-NAT replies.

With more than one VIP, use `rivoractl vips` / `/api/v1/vips` instead of
`status`.

## Securing the local API

Default listen: `127.0.0.1:9870` (plain HTTP, unauthenticated).

| Env var | Effect |
| --- | --- |
| `RIVORA_API_KEY` | Bearer token required |
| `RIVORA_TLS_CERT` / `_KEY` | HTTPS with files |
| `RIVORA_TLS_SELF_SIGNED` | Auto self-signed HTTPS |

`rivoractl` honors `RIVORA_API_KEY` and `RIVORA_TLS_INSECURE` (or
`--api-key` / `--tls-insecure`).

## Remote deploy

```sh
make deploy-remote H=<host> U=<user>          # full: sync, deps, build, verify
make deploy-remote-quick H=<host> U=<user>    # skip dependency install
make deploy-remote-verify H=<host> U=<user>   # selftests only (incl. IPv6 + NDP)
```

`scripts/deploy-remote.sh` rsyncs sources, builds on the host, and runs
verify. Selftest failures are warnings for deploy; they are hard failures
in CI.

## Kubernetes path

See [Kubernetes](kubernetes.md) and [Helm chart](helm.md).
