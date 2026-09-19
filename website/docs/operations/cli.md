---
sidebar_position: 2
title: Commands, flags and environment
---

# Commands, flags and environment

Rivora ships five programs. This page lists every flag and environment variable they read. Options
that only apply in one mode say so.

| Program | Runs where | What it is |
| --- | --- | --- |
| `rivorad` | every node | The per-node daemon: loads and attaches the BPF programs, programs the maps, health-checks backends, serves the API. |
| `rivoractl` | anywhere that can reach a `rivorad` | Command-line client for `rivorad`'s API. |
| `rivora-controller` | cluster (2 replicas, one leader) | Kubernetes IPAM and status writer. |
| `rivora` | your workstation | Installs, upgrades and inspects Rivora in a cluster. |
| `rivora-doctor` | every node, before installing | Host readiness checker. |

## `rivorad`

```sh
rivorad -config /etc/rivora/config.yaml -bpf-dir /usr/local/share/rivora/bpf        # static YAML
rivorad -kubernetes -interface eth0 -bpf-dir /usr/local/share/rivora/bpf             # Kubernetes
```

### Both modes

| Flag | Default | Meaning |
| --- | --- | --- |
| `-bpf-dir` | `/usr/local/share/rivora/bpf` | Directory holding `xdp_ingress.o` and `tc_nat.o`. |
| `-persist-datapath` | off | Pin the XDP/TCX links so the datapath keeps forwarding while `rivorad` is down, and hot-swap the program on the next start. It stays attached after `rivorad` exits: remove it with `-detach`. See [Restarting without a traffic gap](runbook.md#restarting-without-a-traffic-gap-opt-in). |
| `-detach` | off | Remove datapath links left by a `-persist-datapath` run, keep the maps, and exit. |
| `-metrics-listen` | `:9871` | Address of `/healthz`, `/readyz` and `/metrics`, meant to be reachable from outside the node. |
| `-log-level` | `info` | `debug`, `info`, `warn` or `error`. |
| `-log-format` | `text` | `text` or `json`. |
| `-version` | | Print the version and exit. |

### Static-YAML mode

| Flag | Default | Meaning |
| --- | --- | --- |
| `-config` | `/etc/rivora/config.yaml` | The [configuration file](configuration.md). Interface, API address, health-check timing, rate limit, BGP and the XDP mode all come from it. `SIGHUP` reloads its VIPs. |

### `-kubernetes` mode

The file's settings become flags, and the VIPs come from Services (and Gateways) instead of a file.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-kubernetes` | off | Run the Service/EndpointSlice reconciler and L2 speaker instead of loading `-config`. |
| `-interface` | | **Required.** The interface to attach to. |
| `-kubeconfig` | in-cluster, then `$KUBECONFIG` | Path to a kubeconfig. |
| `-api-listen` | `127.0.0.1:9870` | API address. |
| `-xdp-mode` | `generic` | `generic`, `native` or `auto`. |
| `-loadbalancer-class` | `""` | Manage only Services whose `spec.loadBalancerClass` matches; empty manages Services with none set. Must match the controller's. |
| `-namespace` | `$POD_NAMESPACE`, else `rivora-system` | Namespace of the speaker's Lease and of BGP password Secrets. |
| `-node-name` | `$NODE_NAME` | This node's name. Needed for `externalTrafficPolicy: Local` and for `BGPPeer` node selectors. |
| `-workers` | `2` | Concurrent Service reconcile workers. |
| `-speaker` | `true` | Run the L2 ARP + NDP speaker (needs `CAP_NET_RAW`). |
| `-service-policy` | `false` | Honour `ServicePolicy` objects. Needs the CRD. |
| `-gateway-api` | `false` | Also reconcile `Gateway`, `TCPRoute` and `UDPRoute`. Needs the Gateway API CRDs. |
| `-health-interval`, `-health-timeout`, `-health-fail-threshold`, `-health-success-threshold` | `3s`, `1s`, `2`, `2` | Health-check timing. |
| `-rate-limit`, `-rate-limit-pps`, `-rate-limit-burst` | off, 0, 0 | Node-wide per-source SYN limit. |
| `-bgp`, `-bgp-asn`, `-bgp-router-id`, `-bgp-ipv6-next-hop`, `-bgp-peers` | off | Simple BGP setup: `-bgp-peers "10.0.0.1:65000:bfd,10.0.0.2:65001"`. |
| `-bgp-config` | | A YAML file with a `bgp:` section, for everything the flags cannot say (passwords, multihop, graceful restart, communities, local-pref, aggregates, per-node peers). **Replaces** the other `-bgp*` flags. |
| `-bgp-peer-resources` | `false` | Also read cluster-scoped `BGPPeer` resources. Needs BGP and the CRD. |

## `rivora-controller`

| Flag | Default | Meaning |
| --- | --- | --- |
| `-kubeconfig` | in-cluster | Path to a kubeconfig. |
| `-namespace` | `$POD_NAMESPACE`, else `rivora-system` | Namespace of its leader-election Lease. |
| `-loadbalancer-class` | `""` | Must match `rivorad`'s. |
| `-workers` | `2` | Concurrent reconcile workers. |
| `-gateway-api` | `false` | Also assign addresses to Gateways and write route and listener status. |
| `-metrics-listen` | `:9871` | Address of `/healthz`, `/readyz` and `/metrics`. |
| `-log-level`, `-log-format`, `-version` | | As for `rivorad`. |

## `rivoractl`

```sh
rivoractl status                     # one VIP: its detail; several: a line per VIP
rivoractl vips                       # every VIP: mode, backends, packets
rivoractl backends                   # every backend of every VIP: VIP, ID, address, weight, state, counters
rivoractl drain 12                   # no new flows to backend 12
rivoractl undrain 12
rivoractl weight 12 5 [--vip 10.0.0.1:80:tcp]    # override a Maglev weight; 0 clears it
rivoractl validate /etc/rivora/config.yaml       # offline; needs no daemon
rivoractl version
```

`status`, `vips` and `backends` accept `--format json`. `status` describes the node's VIP in detail when there is
one and prints a line per VIP when there are several. `backends` has one row per (VIP, backend), so a backend
serving two VIPs appears twice, with each VIP's weight; its `VIP` column is the key `weight --vip` takes. Drain and
weight changes are live and are **not persisted**: they survive reconciles and reloads but not a restart.

| Option | Meaning |
| --- | --- |
| `--api HOST:PORT` or URL | Which `rivorad`; default `127.0.0.1:9870`. |
| `--api-key KEY` | Bearer token; default `$RIVORA_API_KEY`. |
| `--ca-file FILE` | Verify the server certificate against this PEM (or its CA); default `$RIVORA_CA_FILE`. Implies HTTPS. Preferred over `--tls-insecure`. |
| `--cert FILE --key FILE` | Present a client certificate for a server that trusts a client CA; defaults `$RIVORA_CLIENT_CERT`, `$RIVORA_CLIENT_KEY`. Replaces `--api-key`. |
| `--tls-insecure` | Skip certificate verification; default `$RIVORA_TLS_INSECURE`. Implies HTTPS. |

## `rivora` (cluster lifecycle)

Drives the same Helm chart as `helm`, through the Helm SDK, so no `helm` binary is needed. Each release
embeds exactly the chart it shipped with.

```sh
rivora install   --set rivorad.interface=eth0 --set addressPools[0].name=default --set addressPools[0].addresses='{10.0.0.0/24}'
rivora upgrade   -f values.yaml
rivora status                        # cluster-wide rollout status
rivora uninstall
rivora version
```

| Option | Default | Meaning |
| --- | --- | --- |
| `--namespace` | `rivora-system` | Namespace to install into. |
| `--release-name` | `rivora` | Helm release name. |
| `--kubeconfig`, `--context` | `$KUBECONFIG`, current context | Which cluster. |
| `-f`, `--values` | | Values file (repeatable, applied in order). |
| `--set` | | A value, `key=value` (repeatable, later wins). |
| `--create-namespace` | `true` | Create the namespace if it is missing (`install`). |

Like `helm upgrade`, `rivora upgrade` does **not** update CRDs: see [CRD lifecycle](../kubernetes/helm.md#crd-lifecycle).

## `rivora-doctor`

Checks a host before you install: privileges, kernel version (TCX, which full-NAT needs, arrived in
6.6), bpffs mounted, BTF present, `clang`/`bpftool` if you build on it, the API security settings in
force, and optionally an interface's driver.

| Flag | Meaning |
| --- | --- |
| `--json` | Machine-readable report. |
| `--strict` | Exit non-zero on warnings as well as failures. |
| `--require-tcx` | Treat a kernel older than 6.6 as a failure (a warning by default, since DSR-only nodes do not need TCX). |
| `--interface NAME` | Also report that interface's driver, a hint for native XDP. |
| `--root PATH` | Inspect another filesystem root, for example a mounted support bundle. |

Exit status is 0 on pass, 2 when a check fails (or warns, with `--strict`).

## Environment variables

### API security (`rivorad`)

| Variable | Effect |
| --- | --- |
| `RIVORA_API_KEY` | Admin key(s). Setting it turns authentication on. `id:NAME=KEY` names a key in the audit log; a comma-separated list rotates keys. |
| `RIVORA_API_READONLY_KEY` | Read-only key(s): may read the API and console, gets `403` on any change. Needs `RIVORA_API_KEY` too. |
| `RIVORA_TLS_CERT`, `RIVORA_TLS_KEY` | Serve HTTPS with this certificate. |
| `RIVORA_TLS_SELF_SIGNED` | Serve HTTPS with a certificate generated at start-up (new on every start). |
| `RIVORA_TLS_CLIENT_CA` | PEM CA bundle: accept client certificates it signed (mutual TLS). Needs TLS on. |
| `RIVORA_TLS_CLIENT_REQUIRED` | `1` turns away callers without a verified client certificate at the handshake. |
| `RIVORA_API_CERT_ADMIN_CNS` | Comma-separated certificate common names that get the admin role; any other verified certificate is read-only. |

Details, rotation and the audit trail: [API and console](api.md).

### Client (`rivoractl`)

`RIVORA_API_KEY`, `RIVORA_CA_FILE`, `RIVORA_TLS_INSECURE`, `RIVORA_CLIENT_CERT`, `RIVORA_CLIENT_KEY`, as
above.

### Kubernetes

`POD_NAMESPACE` and `NODE_NAME` default `-namespace` and `-node-name`; the chart sets both from the pod.
`KUBECONFIG` is honoured outside a cluster.

### Under systemd

`/etc/rivora/rivorad.env` (mode 0600, from `deploy/systemd/rivorad.env.example`) is the unit's
`EnvironmentFile`; put the variables above there. `RIVORAD_ARGS` in the same file is appended to the
command line (for example `-persist-datapath`). `scripts/install-systemd.sh` installs the unit and
refuses to expose the API on a non-loopback address without a key.
