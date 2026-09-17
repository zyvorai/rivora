# Roadmap

## Done

- **v0.1** — single VIP, single node, IPv4, static config.
- **v0.2** — Kubernetes LB path verified live (IPAM, Service/EndpointSlice,
  Maglev scale, ARP speaker). Still ahead: publish chart container images
  to the registries referenced by Helm values.
- **KubeVirt / external backends** — work via normal EndpointSlices; no
  special Rivora code path.
- **Gateway API** — implemented; startup/informer/`GatewayClass` confirmed
  live; traffic-path retry pending unrelated cluster net issue.
- **BGP/BFD** — `/32` and `/128` health-gated ads verified via gobgp
  loopback tests; live FRR/BIRD peering follow-up.
- **IPv6 product surface** — dataplane, sparse IPAM, dual-stack
  Service/Gateway, NDP, BGP `/128`, CI selftests (see [IPv6](ipv6.md)).

## Still hardening

- End-to-end dual-stack Service traffic on the remote test cluster.
- BGP IPv6 peering against a real/containerized router.
- Gateway API live traffic + weighted-split scenarios.
- Published `ghcr.io` images for Helm `image.*` values.
