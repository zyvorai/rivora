---
sidebar_position: 1
title: Configuration reference
---

# Static configuration reference

A node that is not run with `-kubernetes` reads one YAML file (`-config`, default
`/etc/rivora/config.yaml`). This page lists every setting. Check a file offline with
`rivoractl validate FILE`; it runs the same loader and validator `rivorad` starts with and needs no
running daemon. Edit and reload a running node with `systemctl reload rivorad` (SIGHUP): see
[Reloading the config](runbook.md#reloading-the-config-without-a-restart) for what a reload does and does
not apply.

Working examples for every feature live in
[`config/examples/`](https://github.com/zyvorai/rivora/tree/main/config/examples).

```yaml
interface: eth0                # required
xdpMode: generic               # generic | native | auto
apiListen: 127.0.0.1:9870
tunnelSource: 198.51.100.1     # only for dsr-ipip / dsr-gre
tunnelSource6: 2001:db8::1
healthCheck: {interval: 3s, timeout: 1s, failThreshold: 2, successThreshold: 2}
rateLimit: {enabled: false, perSourcePacketsPerSecond: 0, burst: 0}
bgp: {enabled: false}          # see the BGP page
vips: []                       # at least one
```

## Top level

| Key | Default | Meaning |
| --- | --- | --- |
| `interface` | **required** | The interface XDP (and, for NAT, TCX egress) attach to. One interface per node. |
| `xdpMode` | `generic` | `generic` works on any interface; `native` runs in the NIC driver and refuses to start if the driver cannot; `auto` tries native and falls back with a warning. See [XDP attach mode](runbook.md#xdp-attach-mode-generic-native-auto). Read once at start-up. |
| `apiListen` | `127.0.0.1:9870` | Address of the API, web console, `/healthz`, `/readyz` and `/metrics`. Anything but loopback needs an API key: see [API and console](api.md). |
| `tunnelSource` | the interface's IPv4 address | Outer-header source for IPv4 `dsr-ipip` / `dsr-gre` VIPs. A tunnel VIP with no source to use is refused. |
| `tunnelSource6` | the interface's global IPv6 address | The same for IPv6 tunnel VIPs. |
| `healthCheck` | see below | Timing of the active health checker, shared by every VIP. |
| `rateLimit` | off | Node-wide per-source SYN limit. |
| `bgp` | off | The BGP speaker; see [BGP](bgp.md). |
| `vips` | **required** | The VIPs this node serves. |

### `healthCheck` (timing)

| Key | Default | Meaning |
| --- | --- | --- |
| `interval` | `3s` | Time between probes of a backend. |
| `timeout` | `1s` | A probe that takes longer fails. |
| `failThreshold` | `2` | Consecutive failed probes before a backend is taken out of rotation. |
| `successThreshold` | `2` | Consecutive successful probes before it is restored. |

Timing is global. *What* is probed is per VIP (`vips[].healthCheck`, below). Read once at start-up.

### `rateLimit` (node-wide)

| Key | Meaning |
| --- | --- |
| `enabled` | Off by default. A disabled limit costs one array lookup. |
| `perSourcePacketsPerSecond` | Sustained new-connection (SYN) rate allowed per source address. |
| `burst` | Token-bucket depth. Both must be greater than 0 when enabled. |

Only TCP SYNs are limited; established connections and UDP are not. Each CPU keeps its own bucket, so
the configured rate is divided by the CPU count to approximate the intended global rate. A VIP's own
`rateLimit` **replaces** this for that VIP.

## `vips[]`

One entry per virtual IP, port and protocol.

```yaml
vips:
  - address: 10.0.0.100
    port: 80                     # or portRange: "30000-30100", or ports: [80, 443]
    protocol: tcp                # tcp | udp
    mode: nat                    # dsr | nat | dsr-ipip | dsr-gre
    sessionAffinity: clientIP    # none (default) | clientIP
    healthCheck: {type: http, path: /healthz, expectStatus: "200-299"}
    rateLimit: {perSourcePacketsPerSecond: 50, burst: 100}
    bgpCommunities: ["65001:7"]
    bgpPeers: [10.0.0.1]
    backends:
      - {address: 10.0.1.11, port: 8080, weight: 3}
      - {address: 10.0.1.12, port: 8080}
```

| Key | Meaning |
| --- | --- |
| `address` | The VIP. IPv4 or IPv6; every backend must be the same family. |
| `port` | The VIP's port, or the first port of a range when `portEnd` is set. |
| `portEnd` | Makes this a **range VIP** covering `port`..`portEnd`. Normally written as `portRange`. |
| `portRange` | `"30000-30100"`. Converted to `port`/`portEnd` on load. |
| `ports` | `[80, 443]`: shorthand for one VIP per port sharing the rest of the entry. |
| `protocol` | `tcp` or `udp`. |
| `mode` | `dsr`, `nat`, `dsr-ipip` or `dsr-gre`. See [Forwarding modes](../core-concepts/forwarding-modes.md). |
| `sessionAffinity` | `clientIP` sends every connection from one source address to one backend while the backend set is unchanged. Default `none` hashes the whole 5-tuple. |
| `healthCheck` | How this VIP's backends are probed. Unset means a TCP connect to the backend's service port. |
| `rateLimit` | This VIP's own per-source SYN limit, replacing the node-wide one for this VIP (whether or not that is enabled). |
| `bgpCommunities` | Communities added to this VIP's BGP route, on top of the speaker's own. |
| `bgpPeers` | Limits this VIP's BGP route to the peers with these addresses. Unset means every peer. |
| `backends` | At least one. |

### `backends[]`

| Key | Meaning |
| --- | --- |
| `address` | Backend IP, the same family as the VIP. |
| `port` | Backend port. **Omit for a range VIP**, which reaches the backend on the port the client used. |
| `mac` | Backend MAC. **Required for `mode: dsr`**, and rejected for every other mode (they route by address). |
| `weight` | Relative share of new connections; unset (0) means 1. A backend's share is its weight over the sum of the VIP's weights. Keep it modest: each unit costs Maglev table slots, and operator overrides and `ServicePolicy` weights are capped at 1000. |

### `vips[].healthCheck`

| Key | Meaning |
| --- | --- |
| `type` | `tcp` (default) or `http`. |
| `port` | Probe this port instead of the backend's service port. **Required for a range VIP**; the way to health-check a UDP VIP (which has no TCP port to connect to). |
| `path` | HTTP only. Default `/`, must start with `/`. |
| `host` | HTTP only. The `Host` header; default is the backend address. |
| `expectStatus` | HTTP only. `"200"` or a range; default `200-399`. Redirects are not followed. |

See [Health checks](runbook.md#health-checks-tcp-and-http).

## Validation rules

Load-time checks that are easy to trip over (each is reported with the VIP and backend it concerns):

- A VIP is identified by address, port (or range) and protocol; a duplicate is rejected.
- Backends must share the VIP's address family.
- `mac` is required in `dsr` mode and refused in every other mode.
- A backend has **one MAC** and is probed **once**, however many VIPs list it: VIPs sharing a backend
  address and port must agree on its MAC and on its `healthCheck`.
- A range VIP needs `1 <= port < portEnd`, a `healthCheck.port`, and backends with no `port`. Two ranges
  on one address and protocol must not overlap; an exact-port VIP inside a range is allowed and wins for
  its port.
- `rateLimit` needs both fields above 0.
- `tunnelSource` must be IPv4 and `tunnelSource6` IPv6.
- `bgpCommunities` are `"asn:value"` (both halves 0-65535) or `no-export`, `no-advertise`,
  `no-export-subconfed`. `bgpPeers` must be valid addresses and, when BGP is configured in the same file,
  addresses of configured peers.

## Settings a reload applies

`vips` (including a VIP's backends, weights, probes, affinity and limits) is applied live. `interface`,
`xdpMode`, `apiListen`, `healthCheck`, `rateLimit`, `tunnelSource(6)` and `bgp` are read once: a reload
that changes one logs `NOT applied` and names it, and a restart applies it. The first `mode: nat` VIP on a
node started without any also needs a restart, because the egress program is only loaded when a NAT VIP
exists at start-up.

## Capacity

Fixed at build time: 4096 VIPs, 8192 backends, a shared 65,537-slot Maglev table (each VIP takes a slice
sized for its backend count), and 16,384 entries in each fragment-tracking table. The affinity and NAT
flow tables are LRU maps; watch their occupancy with
[`rivora_conntrack_*`](metrics.md#flow-tables).
