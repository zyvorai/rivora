---
sidebar_position: 3
title: API and web console
---

# API and web console

`rivorad` serves an HTTP API, an embedded web console and the health and metrics routes on one listener
(`apiListen`, default `127.0.0.1:9870`), and repeats `/healthz`, `/readyz` and `/metrics` on a second
one (`-metrics-listen`, default `:9871`) that is meant to be reachable from outside the node.

It is **plain HTTP and unauthenticated out of the box**. That is fine bound to loopback and not fine
anywhere else: read [Securing it](#securing-it) before changing `apiListen`.

## Endpoints

| Route | Method | Meaning |
| --- | --- | --- |
| `/api/v1/vips` | GET | Every VIP the node serves, with its backends and live counters. |
| `/api/v1/status` | GET | The node's VIP, when there is **exactly one**. On a node with none or several it answers `409` and points at `/api/v1/vips`: there is no single VIP to describe. |
| `/api/v1/backends` | GET | Every backend of every VIP, one row per (VIP, backend), each labelled with its `vip` (`addr:port:proto`, the form the `weight` route's `vip` takes). Any number of VIPs, including none (`[]`). |
| `/api/v1/backends/{id}/drain` | POST | Stop sending new flows to a backend. Established flows continue. |
| `/api/v1/backends/{id}/undrain` | POST | Undo an operator drain. |
| `/api/v1/backends/{id}/weight` | POST | Override a backend's Maglev weight. Body `{"weight": 5, "vip": "addr:port:proto"}`; `vip` optional, weight `0` clears the override, above 1000 is rejected. |
| `/healthz`, `/readyz`, `/metrics` | GET | Process liveness, BPF-map readiness (`503` when the maps cannot be read), Prometheus metrics. Never require a key. |
| `/` | GET | The web console. |

The POST routes need `Content-Type: application/json` and, when authentication is on, admin access.
Backend IDs are the `id` fields of `/api/v1/backends` and `/api/v1/vips`. A backend that serves several VIPs has one row per VIP, with that VIP's weight and its own (repeated) counters, so do not sum the counters across rows.

```sh
curl -s -H "Authorization: Bearer $RIVORA_API_KEY" http://127.0.0.1:9870/api/v1/vips
curl -s -X POST -H "Authorization: Bearer $RIVORA_API_KEY" -H 'Content-Type: application/json' \
     -d '{"weight": 5}' http://127.0.0.1:9870/api/v1/backends/12/weight
```

### A VIP as returned

```json
{
  "vipAddress": "10.0.0.100", "vipPort": 80, "protocol": "tcp", "mode": "nat",
  "interface": "eth0", "startedAt": "2026-09-19T12:10:56Z",
  "packets": 120345, "bytes": 98211334,
  "dropped": 0, "droppedRateLimited": 0, "droppedNoBackend": 0, "unserved": 0,
  "bgpCommunities": ["65001:7"], "bgpPeers": ["10.0.0.1"],
  "backends": [
    {"id": 0, "address": "10.0.1.11", "port": 8080, "weight": 3, "healthy": true,
     "state": "healthy", "adminDraining": false, "packets": 90111, "bytes": 71000000}
  ]
}
```

`vipPortEnd` appears only for a port-range VIP (`vipPort` is then its first port). `state` is `healthy`,
`draining` or `down`. `dropped` is node-wide; `droppedRateLimited`, `droppedNoBackend` and `unserved` are this
VIP's own: see [Why is traffic being dropped](runbook.md#why-is-traffic-being-dropped-or-not-load-balanced).

## Web console

The console at `/` is a read-only view: an overview, every VIP (a port range as `first-last`) and every
backend with its state (`healthy`, `draining`, `draining (operator)`, `down`), weight and counters. It has no
controls; drain and weight changes go through `rivoractl` or the API. It signs in with the same key as the
API (checked against `/api/v1/vips`, so it works for any number of VIPs), held in memory only, so reloading the
page signs out. With authentication off, an empty token signs in.

## Securing it

Everything below is opt-in and driven by environment variables (under systemd, put them in
`/etc/rivora/rivorad.env`; in the chart, see `rivorad.apiKey`, `rivorad.apiReadOnlyKey` and
`rivorad.tls`).

| Variable | Effect |
| --- | --- |
| `RIVORA_API_KEY` | Require this bearer token on every `/api/*` route and give it admin access. **Setting it is what turns authentication on.** |
| `RIVORA_API_READONLY_KEY` | A second key that may read but gets `403` on any change. Needs `RIVORA_API_KEY` too. |
| `RIVORA_TLS_CERT`, `RIVORA_TLS_KEY` | Serve HTTPS with these files. |
| `RIVORA_TLS_SELF_SIGNED` | Serve HTTPS with a certificate generated at start-up. |
| `RIVORA_TLS_CLIENT_CA` | Accept client certificates signed by this CA (mutual TLS). Needs TLS on. |
| `RIVORA_TLS_CLIENT_REQUIRED` | `1`: turn away callers with no verified client certificate at the TLS handshake. |
| `RIVORA_API_CERT_ADMIN_CNS` | Comma-separated certificate common names that get admin; any other verified certificate is read-only. |

### Two roles

The **admin** key has full access. The **read-only** key may read the API and console. Give dashboards and
anyone who only needs to look the read-only key, so a leaked screen-share cannot drain a backend.

`rivorad` **refuses to start**, before touching the kernel, when the key settings are unsafe: a read-only
key with no admin key (it would protect nothing), one key in both roles, or a setting that holds no usable
key (a stray `,` must not silently switch authentication off). A short key starts but warns; generate one
with `openssl rand -hex 24`. Keys are compared in constant time.

### Rotating a key with no outage

Either key variable takes a comma-separated list.

1. Generate the new key.
2. Set `RIVORA_API_KEY=<new>,<old>` and restart `rivorad`; both work.
3. Move every client to `<new>`.
4. Set `RIVORA_API_KEY=<new>` and restart; `<old>` now gets `401`.

Rotate the read-only key the same way. With the chart, escape the comma: `--set 'rivorad.apiKey=new\,old'`.

### Who did it: named keys and the audit trail

Write a key as `id:NAME=KEY` (`RIVORA_API_KEY=id:alice=...,id:bob=...`; NAME is letters, digits, `.`, `_` or
`-`, up to 64 characters). Every state-changing request (drain, undrain, weight) is then logged, whether it
succeeded or not:

```text
api change user=alice role=admin method=POST path=/api/v1/backends/12/drain status=200 remote=10.1.2.3:51234
```

and counted in `rivora_api_changes_total{user,code}`. A change refused because the caller's key or
certificate is read-only is always logged, naming them (`reason=forbidden user=viewer`), and never
throttled. A key without `id:` is named by its position (`key-1`). The audit line never contains a key, and
reusing a name is refused at start-up.

### Client certificates (mutual TLS)

Set `RIVORA_TLS_CLIENT_CA` to a PEM CA bundle (TLS must be on). A certificate signed by that CA then
authenticates its holder with no key: the common name is the identity (`cert:ops` in the audit trail),
names in `RIVORA_API_CERT_ADMIN_CNS` get admin, and any other verified certificate is read-only. By default a
certificate is optional and bearer keys keep working beside it; add `RIVORA_TLS_CLIENT_REQUIRED=1` to turn
away callers without one. With certificates on, authentication is on even if no key is set.

```sh
rivoractl --cert ops.pem --key ops.key --ca-file server.pem drain 3
```

A certificate is checked against the CA only: **revocation is not checked**, so keep certificates
short-lived or rotate the CA. Roles finer than admin and read-only are not implemented.

### Trusting the server's certificate

With `RIVORA_TLS_CERT`/`_KEY`, verify the certificate instead of skipping verification:
`rivoractl --ca-file cert.pem ...`. The self-signed certificate is new on every start, so it can only be
skipped (`--tls-insecure`), never pinned.

### Watching for probing

A publicly bound API attracts scanners. `rivora_api_auth_failures_total{reason}` counts `unauthenticated`
(`401`) and `forbidden` (`403`); alert on `rate(...{reason="unauthenticated"}[5m])`, since a steady rise
means someone is guessing keys. Rejections are also logged (about one line per 10 seconds, with a count of
those swallowed), naming the source address and path but never a key. Behind a proxy the address is the
proxy's.

### Under systemd

`scripts/install-systemd.sh` installs the unit and binds the API to `0.0.0.0:9870`, so it **refuses to run
without a key** (`RIVORA_API_KEY=$(openssl rand -hex 24) ./scripts/install-systemd.sh`, or a key already in
`/etc/rivora/rivorad.env`). `rivora-doctor` reports which of these settings are in force.
