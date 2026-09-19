---
sidebar_position: 3
title: Production runbook
---

# Production runbook

Operational procedures for a live `rivorad`/`rivora-controller` deployment
— what to check first, and how to read the signals covered in
[Observability](../kubernetes/helm.md#observability).

## Health and metrics endpoints

Both binaries serve the same three routes on port `9871` (plain HTTP, no
auth — see [Helm chart: Observability](../kubernetes/helm.md#observability)):

| Route | Meaning |
| --- | --- |
| `/healthz` | Process is alive and answering HTTP. |
| `/readyz` | `rivorad` only: the BPF maps are readable. `rivora-controller`: always ready once started. |
| `/metrics` | Prometheus text exposition. |

`rivorad` additionally serves `/api/v1/status`, `/api/v1/vips`,
`/api/v1/backends` on its main API port (`9870`, loopback-only) — see
[Securing the API](https://github.com/zyvorai/rivora#securing-the-api).
`rivoractl status` / `rivoractl vips` wrap the same data for human
reading.

## A VIP stopped responding

1. **Which node owns it?** For a static-YAML VIP, it's whichever node has
   that VIP in its `-config`. For a K8s-managed VIP, check
   `kubectl get service <svc> -o jsonpath='{.status.loadBalancer}'` for the
   assigned address, then check which node's `rivorad` currently has a
   healthy backend for it — `rivora_vip_backends` and
   `rivora_backend_healthy` (Prometheus) or `rivoractl vips --format json`
   (per-node) both show this.
2. **Are there healthy backends?** `rivora_backend_healthy == 0` for every
   backend behind the VIP means the dataplane is correctly withdrawing
   traffic from a VIP with nothing to send it to — the active health
   checker (`internal/healthcheck`) found every backend unreachable. Check
   the backends themselves, not rivorad, first.
3. **Is the VIP announced?**
   - DSR/static-YAML with no HA: the VIP must be reachable at L2 from
     clients — confirm ARP resolves to the Rivora node's MAC.
   - K8s-managed (NAT-only): the ARP+NDP speaker
     (`internal/speaker`) announces it — confirm exactly one node holds
     the speaker's Lease (`kubectl get lease -n rivora-system`) and that
     node is the one with healthy backends.
   - BGP/BFD HA: confirm the VIP's `/32` or `/128` is actually in your
     router's RIB (`show ip bgp` / `show bgp ipv6 unicast` on the peer) —
     see [BGP/BFD HA](./bgp.md). A withdrawn route with all backends
     healthy usually means a BFD/BGP session flap, not a Rivora bug.
4. **Check `rivora_dataplane_scrape_errors_total`.** A `1` means the last
   `/metrics` scrape failed to read the BPF maps at all — proceed to
   "BPF maps became unreadable" below rather than chasing VIP-specific
   causes.

## BPF maps became unreadable

`rivora_dataplane_scrape_errors_total == 1` or `/readyz` returning 503
both mean `rivorad` can no longer read `/sys/fs/bpf/rivora-lb`. This is
not expected in normal operation — the maps are pinned for the daemon's
whole lifetime (see [Restarting rivorad](#restarting-rivorad)). Likely
causes: something outside Rivora removed the pin directory (a manual
`rm -rf /sys/fs/bpf/rivora-lb`, or a node's `/sys/fs/bpf` got remounted),
or the node ran out of the kernel's locked-memory limit for BPF maps
under heavy VIP/backend churn. Check `dmesg` for BPF/memory-related
errors, then restart `rivorad` — pinned maps still on disk are reused; if
they were actually removed, a restart rebuilds them from the current
config/K8s state (some in-flight NAT connection affinity is lost in that
case, existing DSR flows are not since the backend owns the reply path).

## API keys: read-only access, rotation, and probing

The API has two roles. `RIVORA_API_KEY` is the **admin** key (full access,
including the mutating calls below); `RIVORA_API_READONLY_KEY` may read but gets a
`403` on anything that changes state. Hand the read-only key to dashboards and to
anyone who only needs to look, so a leaked screen-share can't drain a backend.

**Rotating a key with no outage** (either variable accepts a comma-separated list):

1. Generate the new key: `openssl rand -hex 24`.
2. Set `RIVORA_API_KEY=<new>,<old>` (in `/etc/rivora/rivorad.env`, or the chart's
   `rivorad.apiKey`, escaping the comma with `--set`) and restart `rivorad`. Both
   keys now work.
3. Move every client to `<new>`.
4. Set `RIVORA_API_KEY=<new>` and restart. `<old>` now gets `401`.

Rotate a read-only key the same way with `RIVORA_API_READONLY_KEY`.

**Watching for probing.** A publicly bound API attracts scanners. Alert on
`rate(rivora_api_auth_failures_total{reason="unauthenticated"}[5m])`; a steady
rise means someone is guessing keys (`forbidden` means a read-only key was used to
try to change something). Rejections are also logged, throttled to about one line
per 10 seconds with a count of how many were swallowed, showing the source
address and path but never a key. Behind a proxy the address is the proxy's.

**Trusting the certificate.** With `RIVORA_TLS_CERT`/`RIVORA_TLS_KEY`, verify it
rather than skipping verification: `rivoractl --ca-file cert.pem ...`. The
auto-generated self-signed certificate changes on every start, so it can only be
skipped with `--tls-insecure`.

`rivorad` refuses to start, before touching the kernel, if the key settings are
unsafe: a read-only key with no admin key, a key in both roles, or a setting with
no usable key in it.

**Who did it: named keys and an audit trail.** Write a key as `id:NAME=KEY`
(`RIVORA_API_KEY=id:alice=...,id:bob=...`; NAME is letters, digits, `.`, `_` or `-`, up to 64) and
every state-changing request (drain, undrain, weight) is logged as
`api change user=alice role=admin method=POST path=... status=200 remote=...` and counted in
`rivora_api_changes_total{user,code}`, whether it succeeded or not. A change refused because the caller's
key or certificate is read-only is always logged, naming them (`reason=forbidden user=viewer`), and is
never throttled. A key without `id:` is named by its position (`key-1`), and the audit line never contains
a key. Reusing a name is refused at start-up.

**Client certificates (mutual TLS).** Set `RIVORA_TLS_CLIENT_CA` to a PEM CA bundle (TLS must be on,
via `RIVORA_TLS_CERT`/`RIVORA_TLS_KEY` or `RIVORA_TLS_SELF_SIGNED`) and a client certificate signed by
it authenticates its holder with no key: the common name is the identity (`cert:ops` in the audit trail),
common names listed in `RIVORA_API_CERT_ADMIN_CNS` (comma-separated) get the admin role, and any other
verified certificate is read-only. By default a certificate is optional, so bearer keys keep working
beside it; add `RIVORA_TLS_CLIENT_REQUIRED=1` to turn away every caller without one at the TLS
handshake. With certificates on, authentication is on even if no key is set. Give `rivoractl` its own:
`rivoractl --cert ops.pem --key ops.key --ca-file server.pem drain 3` (or `RIVORA_CLIENT_CERT`/
`RIVORA_CLIENT_KEY`). A certificate is verified against the CA only; **revocation is not checked**, so keep
them short-lived or rotate the CA. Per-user roles finer than admin/read-only are not implemented.

## Draining a backend or shifting weight (live)

Use these for maintenance and canary shifts without editing config or
restarting `rivorad`. Backend IDs come from `rivoractl backends`.

```bash
rivoractl backends                 # ID, address, weight, STATE, counters
rivoractl drain 12                 # no NEW flows; established flows keep flowing
rivoractl undrain 12
rivoractl weight 12 5              # override Maglev weight to 5 on every VIP using backend 12
rivoractl weight 12 5 --vip 10.0.0.1:80:tcp   # only that VIP
rivoractl weight 12 0              # clear the override; configured weight applies again
```

- The same operations are `POST /api/v1/backends/{id}/drain`, `/undrain` and
  `/weight` (body `{"weight": 5, "vip": "addr:port:proto"}`, `vip` optional).
  They require `Content-Type: application/json` and, when `RIVORA_API_KEY` is
  set, the bearer token.
- An operator drain is tracked separately from a Kubernetes-driven one
  (terminating endpoint), so neither silently undoes the other. `STATE` shows
  `draining (operator)` for yours, and `rivora_backend_draining` is `1` for either.
- A failed health probe still wins over a drain: a backend that is down stays
  down.
- Weight overrides survive Kubernetes reconciles and static-config re-syncs, and
  are dropped if the backend leaves the VIP. Weights above 1000 are rejected.
- **Not persisted.** Drains and overrides live in `rivorad`'s memory: a restart
  (or pod recreation) reverts to the configured state. Put lasting changes in
  the config or the Service.
- Check a config file before deploying it with `rivoractl validate FILE`; it
  runs the same loader `rivorad` starts with and needs no running daemon.

## XDP attach mode (generic, native, auto)

The XDP program can attach in two ways, chosen with `xdpMode` (or `-xdp-mode`
under `-kubernetes`, and `rivorad.xdpMode` in the Helm chart):

| Mode | What it is | When |
| --- | --- | --- |
| `generic` (default) | Runs in the kernel's network stack after the driver has already built an skb. Works on any interface, including veth, VLANs and bonds. | Anything without native support, and the safe default. |
| `native` | Runs inside the NIC driver, before an skb exists. Much faster, and the reason to use XDP on real hardware. Needs driver support. | Physical NICs and SR-IOV VFs whose driver supports XDP. |
| `auto` | Tries native, falls back to generic and logs a warning. | When you want native where available without a per-host setting. |

`generic` stays the default so nothing changes unless you opt in. `native` is
strict on purpose: if the driver can't do it, `rivorad` refuses to start and
says so, rather than quietly running at generic speed. Use `auto` if you want the
fallback.

**Check what is really attached** rather than trusting the config: the mode flag
on the interface's first `ip link` line (`xdp` is native, `xdpgeneric` is
generic), or `bpftool net list` (`driver` or `generic`). The start-up log also
reports `xdp_mode=`, and warns when `auto` fell back. Note the `prog/xdp` line
under it appears for both modes and doesn't tell them apart.

Things to know:

- **Forwarding modes still matter.** DSR forwards with `XDP_TX`; the loader's
  own notes record that native `XDP_TX` on veth does not reliably cross a bridge
  (which is why generic is the default). Full-NAT forwards with `XDP_PASS` and
  is exercised natively on veth by `scripts/selftest-xdpmode.sh`. DSR under
  native mode has not been tested here: try it on your own hardware first.
- **Changing the mode is a one-off gap.** The kernel allows only one mode per
  interface, so switching replaces the link. With `-persist-datapath` the old
  pin is dropped and the new mode's link pinned (exactly one XDP pin remains);
  restarting in the *same* mode afterwards hot-swaps as usual. `xdpMode` is read
  at start-up, so a reload (SIGHUP) that changes it logs `NOT applied`.
- **Multiple interfaces** are still not supported: one `interface` per node.

## Port ranges and multiple ports

A static-config VIP can own a **range** of ports, for protocols that use many (passive
FTP, RTP media, game servers), and can list several ports at once
(`config/examples/port-ranges.yaml`):

```yaml
- {address: 192.0.2.20, portRange: "30000-30100", protocol: tcp, mode: nat,
   healthCheck: {port: 21}, backends: [{address: 10.0.0.21}, {address: 10.0.0.22}]}
- {address: 192.0.2.22, ports: [80, 443], protocol: tcp, mode: nat,
   backends: [{address: 10.0.0.25, port: 8080}]}
```

- **A range VIP keeps the port the client used**: a connection to `:30042` reaches the
  backend on `:30042`, in NAT and DSR alike. So its backends carry no `port`, and because
  there is then no single backend port to probe, `healthCheck.port` is **required**.
- **`ports: [a, b]` is shorthand** for one VIP per port sharing the rest of the entry
  (each port gets its own counters and Maglev table, as if you had written them out).
- **An exact-port VIP inside a range takes precedence** for its one port, so a single port
  can be carved out to go somewhere else. Two *ranges* on the same address and protocol
  must not overlap; that is rejected when the config loads.
- The API reports a range as `vipPort` (first) and `vipPortEnd` (last), and metrics label
  it `vip="192.0.2.20:30000-30100"`, and both `rivoractl vips` and the web console
  show `30000-30100`.
- **How it works:** the range is stored in a BPF LPM trie as a few aligned blocks
  (30000-30100 is five), checked only when no exact-port VIP matched. With
  `-persist-datapath` a restart adopts ranges from the pinned maps like any other VIP.
  A range VIP is limited to about 16k trie blocks in total across all ranges.
- **Kubernetes:** a Service already gets one VIP per entry in `spec.ports`, so several
  ports work as they always did. Kubernetes has no port-range Service, so ranges are for
  static configs.

## VLANs, fragments, IP options and ICMP

Traffic that is not a plain, untagged, unfragmented packet is handled, with the limits
below stated plainly. `scripts/selftest-edgecases.sh` exercises all of it.

- **VLAN tags.** 802.1Q and QinQ (802.1ad + 802.1Q, up to two tags) are stepped over, in
  full-NAT and DSR, IPv4 and IPv6. A DSR rewrite only changes the MACs, so tags go through
  unchanged. Attach to the parent interface. The tag must be **in the frame** XDP sees: a NIC
  that strips it into metadata (RX VLAN offload) hides it, and a DSR `XDP_TX` would then send
  the frame back out untagged. If VLAN traffic misbehaves, turn RX/TX VLAN offload off
  (`ethtool -K <if> rxvlan off txvlan off`); the selftest does this on its veths.
- **IPv4 options.** A packet with IP options (header longer than 20 bytes) is balanced; it
  used to be passed through untouched.
- **IPv4 fragments.** Only a datagram's first fragment carries the port, so only it can be
  matched to a VIP. It is balanced normally and its backend remembered (keyed by source,
  destination, IP ID and protocol; LRU, so an abandoned datagram ages out); the later
  fragments follow it, in DSR and NAT. In NAT the backend's reply fragments are un-NATed the
  same way on the way out. **Not handled:** a later fragment that arrives *before* its first
  (reordering) is not steered, and that datagram is lost, as with any stateless per-packet
  balancer.
- **ICMP errors (path-MTU discovery).** An ICMP "destination unreachable" (including
  "fragmentation needed"), "time exceeded" or "parameter problem" addressed to a VIP quotes
  the reply the backend sent. Left alone it lands on whichever node owns the VIP and the
  backend never learns the path MTU. It is now sent to the backend that owns the connection it
  quotes: unchanged under DSR (the backend owns the VIP), and under NAT with its destination
  rewritten and the quoted packet rewritten to the backend's address (checksums fixed). An
  ICMP quoting a connection that does not exist is **not** steered. IPv6 "packet too big" and
  the other ICMPv6 errors are handled the same way. Echo (ping) to a VIP is untouched. Only
  TCP and UDP flows are matched, and only where the quoted header has no IP options.
- **IPv6 extension headers.** Hop-by-Hop and Destination Options headers (up to four headers,
  248 bytes in all) are stepped over to reach the TCP/UDP header, in full-NAT and DSR, and stay
  in the packet. **Not handled, and passed through untouched:** a Routing header (it changes what
  the checksum's destination is), AH and ESP (ESP hides the ports), and any longer chain. ICMPv6
  errors are steered only when the ICMPv6 header directly follows the IPv6 header, and only for a
  flow whose quoted packet has none, so path-MTU discovery for a flow that carries extension
  headers is not steered.
- **IPv6 fragments.** Handled like IPv4's: the first fragment carries the ports, is balanced
  normally, and its backend is remembered (keyed by source, destination, the Fragment header's
  identification and protocol; LRU); later fragments follow it. In full-NAT the backend's
  fragmented replies are un-NATed on the way out the same way. The same limit applies: a later
  fragment that arrives *before* its first fragment is not steered, so that datagram is lost.
  `scripts/selftest-ipv6-ext.sh` runs 4000-byte datagrams (three fragments each way) and
  extension-header TCP through NAT and DSR; the L3 DSR modes handle these packets by the same code
  but are **not** covered by that selftest, and an IPv6 fragment that grows past the path MTU under
  a tunnel is answered with a "packet too big" like any other packet.

## L3 DSR (IP-in-IP and GRE)

Plain `mode: dsr` rewrites the frame's MAC, so a backend must sit on the load balancer's own L2
segment. **L3 DSR** lifts that: `mode: dsr-ipip` or `mode: dsr-gre` wraps each packet in a tunnel
to the backend, which can be any number of routed hops away. The backend unwraps it and answers the
client directly from the VIP, so replies still bypass the load balancer
(`config/examples/l3-dsr.yaml`).

```yaml
tunnelSource: 198.51.100.1     # outer source; unset = the attached interface's own address
vips:
  - {address: 192.0.2.30, port: 443, protocol: tcp, mode: dsr-ipip,
     backends: [{address: 10.20.0.11, port: 443}]}     # no mac: reached by address
```

- **IPv4 VIPs** get an IPv4 outer header (IP-in-IP, or GRE over IPv4); **IPv6 VIPs** get an IPv6
  one (IPv6-in-IPv6, or GRE over IPv6). `tunnelSource` is the IPv4 source and `tunnelSource6` the
  IPv6 one; each defaults to the attached interface's own address of that family. Start-up logs the
  addresses in use. A tunnel VIP with no source to use is refused rather than sent unattributable.
- **On each backend** you need a tunnel endpoint that accepts packets to its own address from any
  remote (`ip tunnel add tun0 mode ipip local <backend-ip>`; `mode gre`; `ip -6 tunnel add ... mode
  ip6ip6` or `ip6gre`), the VIP on `lo`, and reverse-path filtering off on the tunnel device.
- **MTU.** The tunnelled packet is 20 bytes larger (24 with GRE, 40/44 for IPv6) and XDP cannot
  fragment it. When a packet fits the client's path but not the load balancer's path to the backend,
  the load balancer answers the client as a router would: "fragmentation needed" for an IPv4 packet
  with DF set (which TCP's is), "packet too big" for IPv6, advertising the link's MTU less the tunnel
  overhead, from the VIP. The client's path-MTU discovery then shrinks its segments and the connection
  works (`selftest-l3dsr.sh` checks a 20000-byte transfer over a 1400-byte link, and the MTU the client
  learns). Two cases are not covered: an IPv4 packet **without** DF (nothing can carry it, and it takes
  the kernel-fallback path below), and a path where the smaller link is beyond the first hop (the
  router there sends its own ICMP to the tunnel source, not to the client). Where you can, give the
  path to the backends a larger MTU.
- **How it is forwarded.** After encapsulation the outer header is routed with `bpf_fib_lookup`, and
  the frame goes straight out of the egress interface, so the load balancer needs a route to the
  backend and IP forwarding enabled. If the lookup cannot answer (next-hop neighbour not resolved
  yet, a VLAN sub-interface, or too big for the egress MTU) the packet is handed to the kernel to
  resolve and forward. The load balancer's own health probes to the backend keep the neighbour entry
  warm, so this is the exception. That fallback path re-enters the load balancer's stack with one of
  its own addresses as the source, which the kernel drops as a martian unless
  `net.ipv4.conf.<if>.accept_local=1`; if you see the first packets to a backend vanish, that is why.
- **Fragments, ICMP errors, VLANs and IP options** are handled as for the other modes (see above):
  each fragment is tunnelled individually and the backend reassembles.
- **Not supported:** GRE keys, checksums or sequence numbers, and VXLAN/Geneve. K8s-managed VIPs are
  NAT-only, so these modes are for static configs. Changing `tunnelSource` needs a restart.

## Kubernetes Service semantics: session affinity and `externalTrafficPolicy`

For `type: LoadBalancer` Services, `rivorad` reads two Service fields.

**`sessionAffinity: ClientIP` is honoured always.** All connections from one client
address go to the same backend while the backend set is unchanged, and when it
changes Maglev moves only a small share of clients. The same behaviour is available
to static configs as a VIP's `sessionAffinity: clientIP`
(`config/examples/session-affinity.yaml`). Two differences from Kubernetes to know
about:

- **There is no timeout.** Stickiness is a pure function of the source address and the
  current backend set, so the Service's `sessionAffinityConfig.clientIP.timeoutSeconds`
  is ignored.
- **A client behind a NAT or proxy is one client.** Everyone behind one address hashes
  identically, so a large NAT'd population lands on a single backend. That is what
  source-address affinity means, but it is easy to trip over with a corporate proxy.

**`externalTrafficPolicy: Local` is honoured only where it is safe**, and otherwise
treated as `Cluster` (which is what it always was here). Both preserve the client
address, since the NAT rewrites only the destination, so what you lose without `Local`
is only the locality of delivering on the node that has the pod.

| Configuration | `Local` |
| --- | --- |
| `-bgp` on, `-speaker=false`, node name known | **Honoured**: a node uses only its own endpoints, and a node with none programs no VIP, so it withdraws the route rather than attracting traffic it would have to forward elsewhere. |
| L2 speaker on (`-speaker=true`, the default) | Treated as `Cluster`. The speaker elects **one** node to answer ARP/NDP for **every** VIP, whether or not it runs a Service's pods; honouring `Local` there would blackhole a Service whenever the elected node has none of its pods. |
| BGP and the L2 speaker both on | Treated as `Cluster`; run with `-speaker=false` to honour `Local`. |
| Neither BGP nor L2 | Treated as `Cluster`: nothing stops a node without the pods from receiving the traffic. |
| BGP on, speaker off, but no node name | Treated as `Cluster`; set `-node-name` (the Helm chart sets `NODE_NAME` from `spec.nodeName`). |

Whenever a Service asks for `Local` and it isn't honoured, `rivorad` logs a warning
**once per Service** saying why (`externalTrafficPolicy: Local is being treated as
Cluster for this Service`), and its start-up log states whether `Local` is honoured on
that node. Endpoints with no node (external addresses, hand-authored slices) are not
assumed local; a local terminating endpoint stays in as draining; node names must
match exactly.

Not implemented: topology-aware routing (`trafficDistribution`), `internalTrafficPolicy`
(this is a LoadBalancer implementation), and the Service's `healthCheckNodePort`
(that exists for external load balancers to probe a node; here `rivorad` is the load
balancer).

## Health checks: TCP and HTTP

By default each backend is probed with a TCP connect to its service port. That
proves the port accepts connections and nothing more: a backend whose app is
wedged, returning 500s or still warming up stays "healthy" and keeps receiving
traffic. Give a VIP an HTTP probe to judge the app itself:

```yaml
vips:
  - address: 10.0.0.100
    port: 80
    protocol: tcp
    mode: nat
    healthCheck:
      type: http
      path: /healthz          # default "/"
      expectStatus: 200-299   # "200" or a range; default 200-399
      port: 8081              # optional: a separate health port
      host: app.internal      # optional Host header
    backends: [{address: 10.0.1.11, port: 8080}]
```

- **What passes:** a `GET` that answers within the probe `timeout` with a status in
  `expectStatus`. Redirects are **not followed**, only counted, so a `302` passes
  the default `200-399` and fails `expectStatus: 200`. Anything else fails: a
  connect error, a timeout, a backend that accepts and never replies, a service
  that isn't HTTP.
- **Every probe is a fresh connection**, so a backend that has stopped accepting
  new connections is noticed rather than masked by a kept-alive one. Proxy
  environment variables are ignored: the probe reaches the backend itself.
- **`port`** probes another port than the service port. Use it when health is
  served separately, and for a UDP VIP (there is no TCP port to connect to).
  It applies to `tcp` probes too.
- **Timing is still global** (`healthCheck.interval`, `timeout`, `failThreshold`,
  `successThreshold`), so one failed probe doesn't flap a backend out.
- **A backend is probed once, however many VIPs list it**, so VIPs that share a
  backend address and port must agree on its probe. `rivorad` and
  `rivoractl validate` refuse a config where they don't, naming the backend,
  instead of letting one VIP silently win.
- **Editing only a VIP's `healthCheck` and reloading (SIGHUP) takes effect
  immediately**; the backend keeps its current health state until the new probe
  says otherwise. Removing `healthCheck` returns it to a TCP connect.
- **Not covered yet:** `https` (there is no TLS probe), gRPC, and per-backend or
  per-Service probe settings. VIPs created from Kubernetes Services or Gateways
  have no probe setting and always use the TCP connect.

To see it working, `rivora_backend_healthy` drops to `0` for a backend whose
health endpoint fails while a plain TCP connect to its service port still
succeeds, and `rivora_vip_dropped_packets_total{reason="no_healthy_backend"}`
starts counting if that was the VIP's only backend.

## Reloading the config without a restart

For a static-YAML node, edit `/etc/rivora/config.yaml`, check it, then reload:

```bash
rivoractl validate /etc/rivora/config.yaml   # same loader rivorad uses; no daemon needed
sudo systemctl reload rivorad                # sends SIGHUP; watch the log for the result
```

`rivorad` re-reads the file and makes the programmed VIPs match it: VIPs you
removed are torn down, new ones added, and changed ones updated. A VIP whose
definition is identical is not touched at all, so reloading an unchanged file
rewrites no BPF map and disturbs no flow. Health checking and BGP pick up the
new backends immediately. Operator drains and weight overrides (see above)
survive a reload.

What a reload does **not** do:

- **A file that fails validation is rejected whole.** The log says
  `config reload rejected; keeping the running config`, and nothing changes.
- **Startup-only settings are ignored, with a warning.** `interface`,
  `apiListen`, `healthCheck`, `rateLimit` and `bgp` are read once; if you changed
  any, the log says `NOT applied` and names them. Restart `rivorad` to apply.
- **The first NAT VIP needs a restart.** The `tc_nat` egress program (which
  un-NATs replies) is only loaded at startup if some VIP uses `mode: nat`. A
  reload that would introduce NAT to a node started DSR-only is rejected, since
  programming the VIP would break its return path.
- **Partial failure is reported, not hidden.** If one VIP can't be applied (for
  example the shared Maglev table is full) the others still are, the log says
  `config reload partly applied` with the error, and the next reload retries it.
- It is **not available with `-kubernetes`**, where the Service/EndpointSlice
  reconcilers own the VIP set and there is no file to re-read.

## Why is traffic being dropped, or not load-balanced?

Each VIP counts, on the XDP path, why a packet it matched did not reach a
backend. `rivora_vip_dropped_packets_total{vip,reason}` counts real drops:

| `reason` | What it means | First thing to check |
| --- | --- | --- |
| `rate_limited` | A SYN exceeded the per-source `rateLimit`. | Is it an attack, or is the limit set too low for legitimate clients? Compare with the source addresses in your flow logs. |
| `no_healthy_backend` | The VIP matched but every backend in its Maglev probe window is down or draining, so there was nowhere to send it. | `rivora_backend_healthy` for that VIP; the backends themselves, not `rivorad`. |

`rivora_vip_unserved_packets_total{vip}` is deliberately **not** a drop. It counts
packets that matched a VIP but could not be served (no service configuration or
backend entry), so they were passed to the kernel stack instead of being
load-balanced. A VIP that answers with a reset, or is reachable but skipping the
load balancer, will show here. It should always be zero; a nonzero, rising value
means the maps and the config disagree, so check `rivoractl vips` and restart or
reload `rivorad`.

The two drop reasons across all VIPs sum to the node-wide
`rivora_dropped_packets_total`, so a gap between them would itself be a bug.
Counters run from when the BPF maps were pinned, and a VIP that is removed and
added back starts from zero.

Suggested alerts:

```yaml
# A VIP is dropping because it has nothing to send to.
- alert: RivoraVIPNoHealthyBackend
  expr: sum by (vip) (rate(rivora_vip_dropped_packets_total{reason="no_healthy_backend"}[5m])) > 0
  for: 2m
# Traffic is matching a VIP but skipping the load balancer.
- alert: RivoraVIPUnserved
  expr: sum by (vip) (rate(rivora_vip_unserved_packets_total[5m])) > 0
  for: 2m
```

## BGP session and route health

With `bgp.enabled`, `rivorad` exports the speaker's state as `rivora_bgp_*`
(the series are absent, not zero, when BGP is off):

| Metric | Meaning |
| --- | --- |
| `rivora_bgp_peer_up{peer,asn}` | `1` when the session is ESTABLISHED. `0` means that peer is not receiving this node's VIP routes. A configured peer gobgp fails to report shows as `0`, not as missing. |
| `rivora_bgp_peer_state_changes_total{peer}` | Session-state transitions since start. A rising value on a peer that is mostly up is a flapping session (or a BFD-detected drop). |
| `rivora_bgp_advertised_routes{family}` | VIP host routes advertised right now (`ipv4` `/32`, `ipv6` `/128`). A VIP is advertised only while it has a healthy backend, so this dropping is the health-gating working. |
| `rivora_bgp_route_update_errors_total{op}` | Failed `advertise`/`withdraw` attempts. A resync retries, so a value that stops rising was transient; one that keeps rising means routes are not reaching the peers. The usual cause is an IPv6 VIP with no `ipv6NextHop` configured. |

```yaml
- alert: RivoraBGPPeerDown
  expr: rivora_bgp_peer_up == 0
  for: 2m
- alert: RivoraBGPPeerFlapping
  expr: increase(rivora_bgp_peer_state_changes_total[10m]) > 4
- alert: RivoraBGPRouteUpdatesFailing
  expr: increase(rivora_bgp_route_update_errors_total[10m]) > 0
```

`peer_up == 0` for every peer while `rivora_backend_healthy` is `1` is a network
or peer-side problem; `peer_up == 1` with `advertised_routes == 0` means the VIPs
have no healthy backend.

## The flow tables are filling up

`connection_affinity_map` (sticky per-flow backend choice) and `nat_reverse_map`
(full-NAT return path) are fixed-size LRU tables, with separate IPv6 siblings.
When one is full the kernel evicts the least-recently-used flow, so a live
connection can be re-hashed onto a different backend (affinity) or lose its
un-NAT mapping (nat_reverse) and reset.

Watch `rivora_conntrack_entries / rivora_conntrack_capacity` per `table`. A
sustained ratio above ~0.8 means you are close to evicting live flows; the
usual causes are a SYN flood (see the rate-limit example config) or many
long-lived idle connections. Entry counts are sampled at most every 10 seconds.
The table sizes are compile-time today, so the remedies are rate limiting and
spreading VIPs across nodes.

## Restarting rivorad

Safe under normal conditions: `internal/loader` pins BPF maps under
`/sys/fs/bpf/rivora-lb` and reuses them on the next `Load()` rather than
recreating them, so connection affinity, Maglev table state, and
packet/byte counters all survive a restart. What does *not* survive: any
in-flight full-NAT connection whose `nat_reverse_map` entry needs a
kernel-side conntrack-like return path that depended on the *old*
process's XDP/TCX attachment briefly detaching during the restart window
— expect a short blip for existing full-NAT flows, not for DSR (backend
owns the reply path) or for new connections after the daemon comes back.

### Restarting without a traffic gap (opt-in)

By default the XDP/TCX links belong to `rivorad`'s own file descriptors, so the
kernel detaches them when it exits and the VIP is unowned until the new process
has started, loaded and re-attached: on an idle test host that is a few dozen
milliseconds of refused connections, and longer on a busy or slow node.

Start `rivorad` with `-persist-datapath` to close that gap. It pins each link
under `/sys/fs/bpf/rivora-lb/links/`, so the attached program keeps forwarding
(against the pinned maps) while `rivorad` is down, and the next start swaps its
own program into the existing link in place instead of detaching and
re-attaching. The start-up log says which links were `hot_swapped`. With
systemd, put `RIVORAD_ARGS=-persist-datapath` in `/etc/rivora/rivorad.env`.

`scripts/selftest-restart.sh` measures this: it restarts `rivorad` under a
steady stream of connections and compares a control run (the default) against a
persisted one. On the reference host the control run drops a handful of
connections per restart and the persisted run drops none.

What you take on when you enable it:

- **It keeps forwarding after `rivorad` exits.** `systemctl stop rivorad` no
  longer takes the VIP down. To actually remove the datapath, run
  `rivorad -detach` (it removes the pinned links and leaves the maps).
- **Nothing is health-checking while `rivorad` is down.** Backend health is
  frozen at its last value, so a backend that dies during the restart window
  still receives new flows until the new process probes it.
- **State is whatever the maps held.** The new process re-applies its config on
  top of the pinned maps, exactly as a default restart does, and the same
  compatibility rule holds: a `rivorad` whose BPF maps changed shape won't load
  against maps from an older version. Read the release notes before upgrading
  across such a change.
- **Start-up reconciles against what the maps already hold (static-YAML
  mode).** `rivorad` reads the VIPs, Maglev extents and backends back from the
  pinned maps, so a VIP that stays in the config keeps the IDs it had, and one
  you removed while `rivorad` was down is torn down at start-up (the log says
  `removed VIPs left programmed by a previous config`). This matters more than
  it sounds: without it, the removed VIP's service ID is reused by the first VIP
  in the new config, and traffic still addressed to the removed VIP is answered
  by another VIP's backends. `scripts/selftest-adopt.sh` reproduces that on an
  older build and passes on this one.
- **With `-kubernetes` the same recovery happens, but removal waits for the
  reconcilers.** At start-up `rivorad` reads the pinned VIPs back (so nothing new
  is given an ID a leftover still holds) and leaves them forwarding: most are still
  wanted, and removing them at once would drop connections until the Service and
  Gateway reconcilers caught up. Each is claimed as its Service or Gateway is
  reconciled. Once both reconcilers have finished their first full pass, whatever
  nothing claimed (a Service deleted while `rivorad` was down) is removed, and the log
  says `removed VIPs left programmed by an earlier run that no Service or Gateway wants any more`.
  If a Service keeps failing to reconcile, that pass never finishes; after two minutes
  `rivorad` logs `not removing the VIPs recovered at start-up that nothing has claimed`
  and removes nothing (one of them might belong to the failing Service), so fix the
  Service and restart, or remove the entry by hand. Until a leftover is claimed,
  `rivoractl` weight changes on it are refused with "not reconciled yet".
- **Changing `interface` or dropping every `mode: nat` VIP leaves the old link
  attached.** The start-up log warns about persisted links this config no
  longer manages; run `rivorad -detach` to clear them.

A `helm upgrade`/pod restart on the DaemonSet follows the same path — one
node at a time (`kubectl rollout status daemonset/<name>-rivorad`), never
all nodes simultaneously, so surviving nodes keep serving each VIP
throughout.

## rivora-controller failover

`rivora-controller` is Lease-elected (`leaderelection.NewLeaderElector`,
15s lease/10s renew/2s retry — see `cmd/rivora-controller/main.go`); the
`rivora_controller_leader` gauge is `1` on exactly one replica at a time.
If no replica reports `1` for longer than the lease duration, IPAM
allocation and `Service.Status`/`Gateway.status` patching have stalled —
check `kubectl get lease -n rivora-system rivora-controller` for the
current holder and its renew time, and check that replica's logs for
Kubernetes API errors (RBAC, API server unavailability) rather than
assuming a code-level bug.

## CRD schema changes

`helm upgrade`/`helm uninstall` never touch the `AddressPool` CRD once
installed — see [CRD lifecycle](../kubernetes/helm.md#crd-lifecycle).
Before upgrading to a chart version whose CRD schema changed, diff
`deploy/helm/rivora/crds/addresspool-crd.yaml` against what's installed
(`kubectl get crd addresspools.rivora.zyvor.dev -o yaml`) and
`kubectl apply` the new version yourself first — a chart upgrade that
assumes a newer schema than what's installed will surface as `rivora-controller`
reconcile errors reading `AddressPool` objects, not as a Helm failure.
