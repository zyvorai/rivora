# Rivora

**eBPF-native load balancing for every environment.**

Rivora owns VIPs, backend selection, health checking, and NAT/DSR — CNI-independent,
with its own XDP/TCX programs and maps under `/sys/fs/bpf/rivora-lb`.

**Published site:** [zyvorai.github.io/rivora](https://zyvorai.github.io/rivora/) ·
**Source:** [github.com/zyvorai/rivora](https://github.com/zyvorai/rivora) ·
**Preview locally:** `make docs-serve`

!!! info "Status"
    **v0.1** shipped · **v0.2** Kubernetes verified on a live cluster · **v0.3**
    underway (Gateway API, BGP/BFD, full IPv6 product surface). IPv6 dataplane,
    NDP, sparse IPAM, and BGP `/128` are covered by CI selftests on every push.

## What you get

| Capability | Notes |
| --- | --- |
| IPv4 / IPv6 TCP/UDP | Same-family backends per VIP |
| DSR + full-NAT | Static YAML; K8s VIPs are NAT-only today |
| Maglev + draining | Optional weights; health-gated backends |
| Kubernetes LB | `Service` / `EndpointSlice` + `AddressPool` IPAM |
| L2 announce | ARP + NDP behind a cluster-wide Lease |
| BGP/BFD HA | Opt-in active/active ECMP (`/32` and `/128`) |
| Gateway API | `Gateway` + `TCPRoute` / `UDPRoute` (no `HTTPRoute`) |

## Documentation map

| Doc | Contents |
| --- | --- |
| [Getting started](getting-started.md) | Build, doctor, static YAML, remote deploy |
| [Architecture](architecture.md) | XDP/TCX path, components, map layout |
| [Kubernetes](kubernetes.md) | `-kubernetes`, IPAM, speaker, external backends |
| [Gateway API](gateway-api.md) | L4 routes, Helm enablement |
| [BGP/BFD HA](bgp.md) | Advertise/withdraw, `ipv6NextHop`, NAT caveat |
| [IPv6](ipv6.md) | Dataplane, sparse pools, NDP, dual-stack |
| [Helm chart](helm.md) | Install, dual-stack pools, values |
| [Selftests and CI](selftests-ci.md) | Makefile targets and GitHub Actions jobs |
| [Roadmap](roadmap.md) | What's done vs still hardening |

## Quick commands

```sh
make bpf build
sudo ./bin/rivora-doctor
sudo ./bin/rivorad -config config/examples/single-vip.yaml -bpf-dir bpf
./bin/rivoractl status
```

Helm (IPv4 pool):

```sh
helm install rivora deploy/helm/rivora \
  --namespace rivora-system --create-namespace \
  --set rivorad.interface=eth0 \
  --set addressPools[0].name=default \
  --set addressPools[0].addresses='{10.0.0.0/24}'
```

License: [Apache-2.0](https://github.com/zyvorai/rivora/blob/main/LICENSE).
