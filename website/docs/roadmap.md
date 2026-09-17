---
sidebar_position: 5
title: Roadmap
---

# Roadmap

## Done

- **v0.1** — single VIP, single node, IPv4, static config.
- **v0.2** — Kubernetes LB path verified live. Still ahead: publish chart
  container images to registries referenced by Helm values.
- **KubeVirt / external backends** via normal EndpointSlices.
- **Gateway API** — implemented; traffic-path retry pending.
- **BGP/BFD** — `/32` and `/128` verified via gobgp tests; live FRR/BIRD
  follow-up.
- **IPv6 product surface** — dataplane, sparse IPAM, dual-stack,
  NDP, BGP `/128`, CI selftests.

## Still hardening

- End-to-end dual-stack Service traffic on the remote test cluster.
- BGP IPv6 peering against a real router.
- Gateway API live traffic + weighted-split scenarios.
- Published `ghcr.io` images for Helm `image.*` values.
