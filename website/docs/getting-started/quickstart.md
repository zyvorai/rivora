---
sidebar_position: 1
title: Quickstart
---

# Quickstart

Rivora is an eBPF/XDP load balancer: it needs a real Linux kernel, and the BPF programs only build and load
on Linux. From a Mac or a thin laptop use the [remote deploy](#remote-build-and-deploy) path. On Kubernetes,
skip to [Kubernetes](#kubernetes).

## Requirements

- **Linux with XDP and BPF**, and root (or `CAP_BPF`, `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_ADMIN`).
  Full-NAT also needs **TCX (Linux 6.6+)** for its return path; DSR-only nodes do not. The forwarding program
  uses helpers added in 5.18, and only 6.8 has been tested.
- **bpffs mounted** (`/sys/fs/bpf`): maps and links are pinned under `/sys/fs/bpf/rivora-lb`.
- To build: Go, `clang` and `llvm`, `libelf-dev`; `bpftool` helps with troubleshooting.

`rivora-doctor` checks all of it:

```sh
sudo ./bin/rivora-doctor            # --strict to fail on warnings, --interface eth0 for the driver
```

## Run one VIP on one node

```sh
make bpf build                      # bpf/*.o and bin/{rivorad,rivoractl,rivora-doctor,rivora,rivora-controller}
sudo ./bin/rivorad -config config/examples/single-vip.yaml -bpf-dir bpf
./bin/rivoractl status              # in another shell
./bin/rivoractl vips
```

A minimal config, full-NAT (no backend changes needed):

```yaml
interface: eth0
vips:
  - address: 10.0.0.100
    port: 80
    protocol: tcp
    mode: nat
    backends:
      - {address: 10.0.1.11, port: 8080}
      - {address: 10.0.1.12, port: 8080}
```

Check a file without a running daemon: `rivoractl validate config.yaml`. Every setting is in the
[configuration reference](../operations/configuration.md).

### What each mode needs from your backends

| Mode | Backends need |
| --- | --- |
| `nat` | Nothing, except that their route back to the client goes **through the Rivora node** (default gateway or a static route), so replies can be un-NATed. |
| `dsr` | The VIP bound locally (`ip addr add <vip>/32 dev lo`, no ARP for it), reachable at **L2** from the node, and their MAC in the config. |
| `dsr-ipip`, `dsr-gre` | A tunnel endpoint, the VIP on `lo` and reverse-path filtering off; they may be any number of routed hops away. |

Details: [Forwarding modes](../core-concepts/forwarding-modes.md).

### Example configs

All in [`config/examples/`](https://github.com/zyvorai/rivora/tree/main/config/examples) and validated by CI.

| File | Shows |
| --- | --- |
| `single-vip.yaml`, `single-vip-nat.yaml` | One VIP, DSR and full-NAT |
| `single-vip-ipv6-dsr.yaml`, `single-vip-ipv6-nat.yaml` | IPv6 |
| `multi-vip.yaml` | Several VIPs, mixing modes |
| `weighted-backends.yaml` | Unequal traffic shares |
| `session-affinity.yaml` | `sessionAffinity: clientIP` |
| `http-healthcheck.yaml` | HTTP probes |
| `rate-limited.yaml`, `vip-rate-limit.yaml` | Node-wide and per-VIP SYN limits |
| `port-ranges.yaml` | Port ranges and multiple ports |
| `l3-dsr.yaml` | IP-in-IP and GRE direct return |
| `bgp-ha.yaml`, `bgp-options.yaml` | BGP + BFD, and its options |
| `remote-api.yaml` | An API bound to a non-loopback address |

With more than one VIP, `rivoractl status` and `backends` answer with an error; use `rivoractl vips` (or
`/api/v1/vips`).

## Operate it

```sh
rivoractl vips                          # every VIP: mode, backends, packets
rivoractl drain 12                      # take backend 12 out of new-flow rotation (IDs from `vips --format json`)
rivoractl weight 12 5                   # canary: give it more or less traffic
sudo systemctl reload rivorad           # apply an edited config's VIP set (SIGHUP)
curl -s http://127.0.0.1:9871/metrics   # Prometheus
```

Install as a service with `scripts/install-systemd.sh` (it binds the API to `0.0.0.0` and so **refuses to run
without `RIVORA_API_KEY`**), or use `deploy/systemd/rivorad.service`. Add `-persist-datapath` to restart
without a traffic gap.

## Securing the API

By default the API listens on `127.0.0.1:9870`, **unauthenticated and plain HTTP**. Before binding it anywhere
else set `RIVORA_API_KEY` (generate one with `openssl rand -hex 24`), and use TLS. Read-only keys, key
rotation, named keys with an audit trail and client certificates are in [API and console](../operations/api.md).

## Kubernetes

```sh
rivora install --set rivorad.interface=eth0 \
  --set addressPools[0].name=default --set addressPools[0].addresses='{10.0.0.0/24}'
rivora status
kubectl expose deployment web --type=LoadBalancer --port=80     # gets an address from the pool
```

`rivora` drives the Helm chart with no `helm` binary; plain `helm install deploy/helm/rivora ...` takes the same
values. See [Kubernetes](../kubernetes/overview.md) and the [Helm chart](../kubernetes/helm.md).

## Remote build and deploy

```sh
make deploy-remote H=<host> U=<user>            # rsync, install build deps, build, install
make deploy-remote-quick H=<host> U=<user>      # skip the dependency install
make deploy-remote-verify H=<host> U=<user>     # run the selftests there
```

Selftest failures are warnings during a deploy and hard failures in CI.

## Next

- [Architecture](../core-concepts/architecture.md) and [Forwarding modes](../core-concepts/forwarding-modes.md)
- [Configuration reference](../operations/configuration.md) and [Commands and flags](../operations/cli.md)
- [Kubernetes](../kubernetes/overview.md), [BGP](../operations/bgp.md), [Runbook](../operations/runbook.md)
- [Limitations](../core-concepts/limitations.md): read this before production
