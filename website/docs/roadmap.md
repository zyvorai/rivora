---
sidebar_position: 9
title: Roadmap
---

# Roadmap

Where Rivora has been, what is still open, and what it will not do. For behaviours and limits in detail see
[Limitations and non-goals](core-concepts/limitations.md).

## Done

**Foundations (v0.1)**

- Single-node XDP load balancer from a static YAML file: IPv4 TCP and UDP, DSR and full-NAT, Maglev backend
  selection with weights and graceful draining, active TCP health checks, an opt-in per-source SYN limit.
- A local API, `rivoractl`, an embedded web console, `rivora-doctor`, a systemd unit.

**Kubernetes (v0.2)**

- `Service` / `EndpointSlice` reconciler; `rivora-controller` IPAM from `AddressPool`; L2 ARP and NDP
  speaker; Helm chart and the `rivora` CLI; signed, SBOM'd images on every version tag. Verified end to end
  on a live cluster, including KubeVirt VMs and external IPs as backends.

**Breadth (v0.3 and after)**

- **IPv6** everywhere: dataplane, sparse `/64` IPAM, dual-stack Services, NDP, BGP `/128`.
- **BGP and BFD**: health-gated `/32` and `/128` advertisement; MD5, multihop, graceful restart,
  communities, local-preference, aggregates; `BGPPeer` resources with password Secrets and node selectors;
  per-VIP and per-Service peer selection.
- **Gateway API** (`Gateway`, `TCPRoute`, `UDPRoute`): cross-namespace attachment with `allowedRoutes`,
  `ReferenceGrant`, and route and listener status.
- **Per-Service policy**: `ServicePolicy` (probe, per-source rate limit, endpoint weights, BGP tuning) and
  the static equivalents.
- **Service semantics**: `sessionAffinity: ClientIP`, `externalTrafficPolicy: Local` (with BGP).
- **Dataplane reach**: port-range and multi-port VIPs; L3 DSR (IP-in-IP and GRE, IPv4 and IPv6, with an
  oversize-packet answer); VLAN and QinQ; IPv4 options; IPv4 and IPv6 fragments; IPv6 extension headers;
  ICMP and ICMPv6 error steering; native XDP (`xdpMode`).
- **Operations**: live drain and weight, config reload, health-check types (TCP, HTTP), drop-reason, flow-table
  and BGP metrics, restart without a traffic gap (`-persist-datapath`), adoption of the pinned datapath on
  restart (static and Kubernetes), named API keys with an audit trail, and mutual TLS.
- **A full selftest suite** run in CI: see [Selftests and CI](operations/selftests-ci.md).

## Still open

These are known and worth doing; none is scheduled.

- **Live verification.** Run the Gateway API traffic path, `ServicePolicy`, `BGPPeer`, `externalTrafficPolicy:
  Local` and Kubernetes restart adoption on a real cluster; dual-stack Service traffic end to end; BGP
  (IPv4 and IPv6) against a real router such as FRR or BIRD, including BFD timing.
- **Performance measurement.** Throughput and latency numbers per mode, generic versus native XDP, and DSR
  under native XDP on real hardware.
- **Console and CLI on multi-VIP nodes.** `rivoractl status` and `backends`, `/api/v1/status` and
  `/api/v1/backends`, and the web console's sign-in check only work with exactly one VIP; on a node with
  several they should list them all.
- **Health checks.** HTTPS, gRPC and UDP probes; per-VIP (rather than global) timing; passive outlier
  detection.
- **Flow tables.** Configurable sizes and TCP-state-aware expiry (today: fixed-size LRU).
- **Wider kernel support.** A kernel compatibility matrix and CO-RE builds; a native-XDP support check in
  `rivora-doctor` (today it only reports the interface's driver).
- **Release engineering.** Multi-architecture images, binary and package artifacts, an OCI chart, a
  changelog and upgrade guide, fuzzing.
- **Mutual TLS niceties.** Certificate revocation checks and roles finer than admin and read-only.
- **Multiple interfaces per node**, and per-VIP L2 speaker leadership (today one Lease elects one node for
  every VIP).

## Not planned

- **L7 features.** `HTTPRoute`, `GRPCRoute`, `TLSRoute`, TLS termination, per-request balancing. An XDP L4
  balancer has no L7 visibility, and pretending otherwise would be a correctness hazard. Use a `TCPRoute` for
  TLS passthrough, or put an L7 proxy behind a Rivora VIP.
- **DSR for Kubernetes-managed VIPs.** It would need the VIP bound into pods, a CNI-specific design.
- **Sharing NAT state between nodes.** Use DSR where ECMP re-hash matters.
