# Security Policy

## Supported versions

Rivora is pre-1.0 (currently v0.x). Security fixes land on `main` and are
released in the next tagged version; there is no long-term-support branch
yet.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Report privately via
[GitHub Security Advisories](https://github.com/zyvorai/rivora/security/advisories/new)
for this repository. If you can't use that, email
security@zyvor.dev with:

- A description of the vulnerability and its impact.
- Steps to reproduce, or a proof-of-concept if available.
- The Rivora version / commit and deployment mode (static-YAML,
  Kubernetes, BGP) involved.

We aim to acknowledge reports within 5 business days and to agree on a
disclosure timeline with the reporter once the issue is confirmed.

## Scope notes

Rivora runs privileged eBPF (XDP/TCX) programs and, in most deployments,
a cluster-scoped Kubernetes controller with RBAC to patch `Service`
status and watch `EndpointSlice`/`AddressPool` objects. Reports involving
any of the following are especially welcome:

- Packet parsing in `bpf/xdp_ingress.c` / `bpf/tc_nat.c` (attacker-facing
  surface: any packet reaching a VIP).
- `internal/config` YAML parsing and `api/v1alpha1` CRD handling
  (attacker-facing surface: anyone who can write a `Config`/`AddressPool`
  object or file).
- `internal/bgp` BGP UPDATE/OPEN handling (attacker-facing surface: any
  configured BGP peer, or — if peering is misconfigured — the network
  path to it).
- `internal/api`'s local HTTP API, particularly the bearer-token
  comparison, the read-only/admin role split, mutual-TLS client
  certificate handling and TLS setup (see README.md#securing-the-api and
  website/docs/operations/api.md).
- `internal/bgppeers` and the chart's RBAC for it: a `BGPPeer` password is
  read from a Secret in `rivorad`'s own namespace only, and the chart
  grants read access to Secrets in that namespace and nowhere else.
- The IPv6 extension-header walk and fragment tracking in
  `bpf/xdp_ingress.c` / `bpf/tc_nat.c` (bounded, but attacker-facing).

## Known posture gaps

Tracked openly rather than hidden: as of this writing there is no
automated fuzzing of the packet/config/BGP parsing paths above. Neither
blocks reporting — it means manual review is the only net catching
issues in that area today.

## Accepted vulnerabilities

CI runs `govulncheck` on every push (see `.github/workflows/ci.yml`) and
fails on any finding except the ones listed here, which are tracked
until an upstream fix exists:

- **GO-2026-4736** — GoBGP denial-of-service via the `NEXT_HOP` path
  attribute (`github.com/osrg/gobgp/v4`). No fixed version is published
  upstream as of this writing. Impact is scoped to `internal/bgp`, which
  only processes messages from explicitly configured BGP peers
  (`-bgp-peers`/`bgp.peers`/`BGPPeer` resources) — not exposed to arbitrary
  network input.
  Re-evaluate when gobgp ships a fix.
