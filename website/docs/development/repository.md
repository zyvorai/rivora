---
sidebar_position: 1
title: Repository, building and testing
---

# Repository, building and testing

## Layout

```text
bpf/                    xdp_ingress.c (the forwarding program) and tc_nat.c (full-NAT egress),
                        rivora_common.h: hand-rolled, no libbpf headers, no CO-RE
cmd/rivorad/            per-node daemon: BPF load/attach, static-YAML apply, Kubernetes reconcilers,
                        speaker, BGP, API
cmd/rivoractl/          CLI for rivorad's API
cmd/rivora/             cluster lifecycle CLI: install, upgrade, uninstall, status (Helm SDK)
cmd/rivora-controller/  Lease-elected IPAM and status Deployment
cmd/rivora-doctor/      host readiness checker
api/v1alpha1/           Go types for AddressPool, ServicePolicy and BGPPeer (dynamic client, no codegen)
api/gatewayapi/         the minimal Gateway API types Rivora reads and writes
internal/dataplane/     the map writer: allocators, UpsertVIP/RemoveVIP, health, adoption, port ranges
internal/bpfmaps/       Go mirrors of the BPF maps' C structs (the ABI)
internal/loader/        BPF object loading, pinning, attach and link persistence (cilium/ebpf)
internal/config/        static-YAML loading and validation, BGP config
internal/controller/    Service/EndpointSlice reconciler and ServicePolicy
internal/gatewayapi/    Gateway/TCPRoute/UDPRoute reconciler, attachment rules, status
internal/ipam/          address-pool expansion and allocation (pure Go)
internal/ipamctrl/      rivora-controller's reconcile logic
internal/speaker/       L2 ARP and NDP responder
internal/bgp/           gobgp speaker, peer changes, per-peer route limits
internal/bgppeers/      BGPPeer resource controller
internal/healthcheck/   active TCP and HTTP probes
internal/maglev/        weighted Maglev table generation
internal/initsync/      first-pass tracking for startup pruning
internal/api/           HTTP API, authentication, audit, embedded web console
internal/apiclient/     rivoractl's client
internal/metrics/       Prometheus collectors
internal/k8s/           client-go bootstrap
internal/installer/     the rivora CLI's logic and the embedded chart
internal/doctor/        rivora-doctor's checks
internal/tlsutil/       self-signed certificates for the API
internal/logging/       slog setup
web/                    the console's React source (built into internal/api/ui)
deploy/helm/rivora/     the Helm chart and CRDs (copied to internal/installer/chartdata)
deploy/systemd/         unit and env example for non-Kubernetes deployments
config/examples/        static-YAML examples (validated by CI)
scripts/                selftest-*.sh, deploy-remote.sh, install-systemd.sh, install-cli.sh
website/                this documentation (Docusaurus)
```

## Building

The BPF programs build and load **only on Linux**; the Go code builds anywhere (BPF-dependent tests skip
themselves off Linux).

```sh
make bpf                # clang -target bpfel: bpf/xdp_ingress.o and bpf/tc_nat.o
make build              # bin/rivorad, rivoractl, rivora-doctor, rivora-controller, rivora
make test               # go test ./...   (CI runs it with -race)
make fmt vet
make web                # rebuild the console into internal/api/ui (the bundle is committed)
make sync-chart         # copy deploy/helm/rivora into the CLI's embedded chart
```

Two copies are kept in sync by hand and checked by CI: the **chart** (`deploy/helm/rivora` and
`internal/installer/chartdata`: run `make sync-chart` and commit both) and the **console bundle**
(`web/` source and `internal/api/ui`: run `make web` and commit both).

## Testing

`go test -race ./...` for the control plane; `make selftest-all` (root, Linux) for the dataplane against real
traffic. See [Selftests and CI](../operations/selftests-ci.md) for what each proves and how the tests are
kept honest. Working from a Mac, build and run the selftests on a Linux host:

```sh
make deploy-remote H=<host> U=<user>
make deploy-remote-verify H=<host> U=<user>
```

When you change the dataplane:

- **Load it through the verifier.** A program that compiles can still be rejected by the kernel; the
  selftests load the real objects. `bpftool -d prog load bpf/xdp_ingress.o /sys/fs/bpf/x type xdp` prints the
  full verifier trace.
- **Add a selftest that fails without your change**, run it against the previous commit to prove that, and
  check it against deliberate mutations of your code.
- **Keep the Go ABI in step.** A change to a map's key or value in C needs the matching change in
  `internal/bpfmaps`, and a map whose shape changed will not load against pins from an older version:
  say so in the release notes.
- **Mind the BPF stack (512 bytes per call chain) and verifier bound tracking**; the code uses per-CPU scratch
  maps, `noinline` functions and per-length instantiation to stay within them.

## Documentation

This site is Docusaurus under `website/`: `make docs-serve` for a live preview, `make docs-build` for a
production build. It deploys on every push that touches `website/` (`pages.yml`). The top-level
[`README.md`](https://github.com/zyvorai/rivora/blob/main/README.md) is the overview; the reference lives here.

## Security and licence

Report vulnerabilities privately: see
[`SECURITY.md`](https://github.com/zyvorai/rivora/blob/main/SECURITY.md). Apache License 2.0.
