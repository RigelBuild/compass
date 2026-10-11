# Agent containers export through the bundled collector

Tracker: RIG-4611

Amendment record of the observability-architecture design
([`../compass-observability-architecture/design.md`](../compass-observability-architecture/design.md),
§D3 and §T4). The parent stays frozen.

Ledger-impact: none. This record adds no `DL-NNN` decision. The PR that lands
it carries this declaration in its body for the design-ledger gate.

## Problem / Intent

Matt ruled on 2026-10-11 (RIG-5144) that agent containers ship OTLP to the
bundled collector, which forwards to an operator upstream. Parent T4 promises
"the Runner-materialized env pointing agent emission at the bundled
collector" but picks no route. This amendment decides the upstream export
(C-a), the agent route (C-b) and the dev wiring (C-c).

## Global Constraints

- D3 holds. With no export endpoint, the collector config is today's `nop`
  config, byte for byte. With one, nothing touches disk: no `file_storage`, no
  `sending_queue.storage`.
- No secret in a podman argv or the 0644 collector config. URL userinfo is a
  secret.
- The runner is co-located with the collector. `--otel-collector-port` is
  refused unless the runner's `--server` host is loopback (OQ1).
- A pasta forward needs Linux and rootless podman 4.6 or newer, the first
  release whose `podman info` reports pasta. On macOS, podman runs containers
  in a VM, so a forward would reach the VM's loopback, not the Mac's.
- The collector image stays `DefaultCollectorImage` (otelcol-contrib 0.140.1),
  so the exporter key is `otlphttp`.
- `baseRuleset` does not change.
- Describe an operator's receiver generically, e.g. Grafana Alloy.

## Approach

**C-a, upstream export.** `collectorConfigYAML` in
`go/internal/stack/collector_container.go` hard-codes `nop: {}`. A new
`--otel-export-endpoint` swaps it for one `otlphttp` exporter, behind a
`memory_limiter` and a 4 MiB request cap, because untrusted agents now reach
the collector. The queue stays in memory. Optional headers come from a 0600
YAML map, copied into the state dir so `:Z` relabels the copy, not the
operator's file. The image runs as uid 10001, so the mount needs
`--userns=keep-id:uid=10001,gid=10001`; without it the smoke failed with
`permission denied`. A host-loopback upstream is outside the collector's
netns, so the adapter forwards it with pasta and rewrites the URL host,
keeping an `https` name through `tls.server_name_override`. Both collector
versions validate this config.

**C-b, agent route: a pinned pasta forward.** The agent container gets
`--network pasta:-T,14318:<collector HTTP host port>,-U,none` and
`OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:14318`. passt(1) defines `-T` as
"TCP port forwarding from target namespace to init namespace", default
**auto**. podman-run(1) says "`-T none` and `-U none` are given" as podman's
defaults. Compass pins both, so `containers.conf` cannot widen them. The
packet leaves on `lo`, which `baseRuleset` in `go/internal/runtime/egress.go`
already accepts (`output oif lo accept`), so no port-scoped `EgressPolicy` and
no CB2. Port 14318 avoids a devenv 4318 inside the container.

Smoke (podman 5.8.6, armed ruleset, empty allowlist): the forward answered,
and a span reached the upstream with the header. `169.254.1.2`, unforwarded
loopback ports, a host-loopback UDP listener and a TCP port bound after start
all stayed shut. Controls: `-U,auto` leaked the UDP datagram, and `-T,auto`
opened the late port.

The host backend dials the host port directly. Other backends cannot forward,
so the runner logs that and starts agents without an emitter.

**C-c, dev wiring.** `devenv.nix` runs no collector today. CC1 runs nixpkgs
`otelcol-contrib` on a config from the bundled container's renderer.

## Alternatives considered

- **`host.containers.internal` (169.254.1.2).** Allowlists match hosts, not
  ports, so allowing it opens every non-loopback host port.
- **A shared podman network.** It gives agents L3 reach to each other.
- **Podman secrets for the headers.** They add secret state to `up` and
  `down`.
- **Host networking for the collector.** Receivers would bind 127.0.0.1,
  breaking the byte-for-byte `nop` config.
- **Hand-written Nix configs.** They restate D3 outside the tested renderer.
- **A runner relay on the agent socket.** See OQ2.

## Plan

Order: CA1, then CB1, then CC1.

### CA1 — Collector upstream export

Files: `go/internal/stack/{config,collector_container}.go`,
`go/internal/stack/adapters/collector_container.go`,
`go/internal/runtime/podman.go`, `go/cmd/compass-stack/main.go`.

Interfaces:

```go
// stack
Config.CollectorExportEndpoint, Config.CollectorExportHeadersFile string
CollectorContainerSpec.ExportHeadersFile   string // "" = no mount, no keep-id
CollectorContainerSpec.UpstreamForwardPort uint16 // 0 = none
type CollectorYAMLParams struct {
	GRPC, HTTP, Health string // "" = 0.0.0.0:<container port>
	Export             string // "" = nop (D3)
	TLSServerName      string
	HeadersPath        string // "" = no headers key
}
func CollectorConfigYAML(p CollectorYAMLParams) string // replaces collectorConfigYAML
const CollectorHeadersMountPath = "/run/compass/otlp-headers.yaml"
const CollectorContainerUpstreamPort = "14319"

// runtime: Linux, podman >= 4.6, Host.Security.Rootless, Host.Pasta.Executable
func (p *PodmanCLI) VerifyPastaSupport(ctx context.Context) error
```

- Flags `--otel-export-endpoint` and `--otel-export-headers-file` fall back to
  `COMPASS_OTEL_EXPORT_*` env.
- `Validate` rejects a non-http(s) URL, userinfo, headers without an endpoint,
  an endpoint with `ExternalOTLPEndpoint`, and a loopback upstream that is off
  Linux or not `127.0.0.1`/`localhost`.
- A loopback upstream renders `127.0.0.1:14319`, with `TLSServerName` set for
  https. `Start` runs `VerifyPastaSupport`, adds
  `--network pasta:-T,14319:<port>,-U,none`, and copies the headers at 0600 as
  `ensureGatewayToken` does, mounted `:ro,Z`.

Red, then green:

- `TestCollectorConfigYAMLExportsUpstream` is red today: `otlphttp` and
  `memory_limiter` in all three pipelines, `max_request_body_size`, no `nop`,
  no `file_storage`. `TestCollectorConfigYAMLRealizesD3Posture` stays green.
- `TestCollectorContainerSpecForwardsLoopbackUpstream` (http and https
  rewrites; remote kept), `TestCollectorRunArgsExportContract`,
  `TestResolveConfigCollectorExportFlags` and `TestVerifyPastaSupport`.
- Smoke: `compass-stack up` delivers a span and header to a local receiver.

### CB1 — Route emitters to the collector

Files: `go/internal/runtime/{podman,agent}.go`,
`go/internal/runner/{spec,host,agent_exec}.go`,
`go/cmd/compass-runner/main.go`, `go/internal/stack/spec.go`.

Interfaces:

```go
// runtime
type LoopbackForward struct{ Port, HostPort uint16 } // zero = none
WorkloadSpec.LoopbackForward, AgentSpec.LoopbackForward LoopbackForward

// runner
const AgentCollectorPort = 14318
type CollectorRoute struct {
	Forward  runtime.LoopbackForward
	Endpoint string // "" = agents get no emitter
}
func ResolveCollectorRoute(ctx context.Context, engine runtime.WorkloadRuntime, hostPort uint16, server *url.URL) (CollectorRoute, error)
SpecDefaults.LoopbackForward runtime.LoopbackForward
AgentHostConfig.OTLPEndpoint, AgentEnv.OTLPEndpoint string
```

- `createArgs` renders a forward as `--network pasta:-T,<Port>:<HostPort>,-U,none`;
  the zero value leaves the argv unchanged.
- compass-runner gains `--otel-collector-port` (`COMPASS_OTEL_COLLECTOR_PORT`;
  0 is off).
- `ResolveCollectorRoute` errors on a non-loopback `server`. The host backend
  gets a direct endpoint, and podman a forward when `VerifyPastaSupport`
  passes. Any other case returns an empty route, which main logs.
- `runnerSpec` passes the flag only when exporting and not `cfg.microVM()`,
  like `--image`. `serverSpec` and `runnerSpec` then set
  `OTEL_EXPORTER_OTLP_ENDPOINT`.

Red, then green:

- `TestCreateArgsRendersLoopbackForward`,
  `TestExecSpecExportsOTLPEndpointOnlyWhenConfigured` and
  `TestBuildSpecMapsLoopbackForward`.
- `TestResolveCollectorRoute`: port 0, remote server (error), host, podman,
  failed pasta check and microVM (empty, no error).
- `TestRunnerSpecForwardsCollectorPortOnlyWhenExporting` (omitted on microVM)
  and its `serverSpec` twin.
- `TestPastaForwardReachesOnlyCollectorPort`, podman-gated like
  `TestKeepIDRemapMapsHostUIDToSpecUID`: an armed agent reaches the forward,
  not a host-loopback UDP listener or a TCP port bound after start. It goes red
  with `-U,auto` or `-T,auto`.

### CC1 — devenv runs the collector

Files: `devenv.nix`, `go/cmd/compass-stack/main.go`.

Interfaces: `compass-stack collector-config --grpc ADDR --http ADDR --health
ADDR [--otel-export-endpoint URL] [--otel-export-headers-file PATH]` prints
`CollectorConfigYAML` with the host headers path. `usageError` names five
subcommands.

- `processes.otel-collector` renders into the state dir, then execs
  `${pkgs.opentelemetry-collector-contrib}/bin/otelcol-contrib` on allocated
  loopback ports. The endpoint and headers come from the gitignored
  `devenv.local.nix`.
- `compass-server` and `compass-runner` start `after` it. With an endpoint,
  both emit to it, and the runner gets `--otel-collector-port`.

Red, then green: today `devenv up` with an endpoint delivers nothing to a
local receiver; afterwards an agent span arrives. `TestRunCollectorConfig`
pins the renderer's bytes.

## Tasks

- [ ] CA1 — Collector upstream export, limits, headers and pasta preflight.
- [ ] CB1 — Pinned pasta forward for agents; server and runner emit.
- [ ] CC1 — devenv runs the collector from the shared renderer.

## Open Questions

1. **Where the collector runs (load-bearing).** Runners can be remote, but the
   forward reaches only the runner host's loopback. (a) Refuse the flag unless
   `--server` is loopback (compass-stack, devenv). (b) A collector per runner
   host forwards to the server's; the flag becomes an endpoint. (c) The runner
   relays agent OTLP to a remote collector (OQ2 B). **Recommendation:** (a)
   now; (b) when a remote runner needs agent telemetry.
2. **Pasta or a runner relay (load-bearing).** A: ship pasta now as a named
   stepping stone; the microVM follow-up uses a relay over the agent socket,
   which guestd already forwards (`gatewayPortKey` in
   `go/internal/guestd/cmdline.go`). B: build the relay now, overwriting
   `compass.session.id` from the gateway binding
   (`g.sessions.Session(g.containerName)` in
   `go/internal/runner/gateway/publish.go`). B stops forgery on every tier, but
   the agent exporter needs a unix-socket transport. Either relay reverses the
   frozen ruling in `docs/designs/repo/compass-agent-effect-otel/design.md`
   Open Questions 1: OTLP goes to the collector, "never the socket/vsock
   gateway". **Recommendation:** A. Pasta is smoke-proven and RIG-4611 needs
   podman data now. If the reversal is approved, the relay replaces pasta on
   every tier.
3. **Emission gate.** Emitters are wired only with an export endpoint. A
   `$HOME/.compass/env` value still wins (`main` in
   `packages/compass-agent/src/cli.ts`: "File wins per key it defines"), and
   tool subprocesses inherit the endpoint. **Recommendation:** keep the gate;
   check the env file when verifying RIG-4611.
4. **Untrusted emitters.** The agent is the untrusted principal (`egress.go`)
   and gains a new reach. Every agent appears as `127.0.0.1`, so a forged
   `compass.session.id` can poison the D7 join. Billing reads the D5 event log.
   **Recommendation:** accept for single-tenant use; multi-tenant waits for
   OQ2 B.
5. **Unsupported backends.** MicroVM, apple-container and macOS podman get no
   agent emitter. **Recommendation:** accept until OQ2 resolves.
6. **Port collision.** An operator receiver on 4317/4318 collides with the
   bundled publish. **Recommendation:** follow-up compass-stack
   `--collector-*-port` flags.
7. **`--otel-external`.** No child receives that endpoint today.
   **Recommendation:** file a follow-up.
8. **Version skew.** devenv runs 0.155.0, the image 0.140.1.
   **Recommendation:** accept; both validate the rendered config.
