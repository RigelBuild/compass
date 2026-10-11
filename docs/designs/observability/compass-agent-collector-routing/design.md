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
  config, byte for byte. With one, nothing touches disk.
- No secret in a podman argv or the 0644 collector config. Endpoints carry no
  userinfo, query or fragment; credentials go in the 0600 headers file.
- The runner is co-located with the collector: `--otel-collector-port` is
  refused unless the runner's `--server` host is loopback (OQ1).
- Server and runner emit only with an export endpoint or `--otel-external`.
  A runner route beats any agent env-file endpoint.
- A pasta forward needs Linux and rootless podman 4.6 or newer (the first
  `podman info` reporting pasta). MicroVM, apple-container and macOS podman
  start agents without an emitter and log it.
- The collector image stays `DefaultCollectorImage` (0.140.0), so the exporter
  key is `otlphttp`. devenv's collector is 0.155.0; both validate the config.
- `baseRuleset` does not change. Describe an operator's receiver generically,
  e.g. Grafana Alloy.

## Approach

**C-a, upstream export.** `collectorConfigYAML` in
`go/internal/stack/collector_container.go` hard-codes `nop: {}`. A new
`--otel-export-endpoint` swaps it for one `otlphttp` exporter behind a
`memory_limiter` and a 4 MiB request cap, because untrusted agents now reach
the collector. The queue stays in memory. Each start replaces a 0600 copy of
the optional headers file, mounted with `--userns=keep-id:uid=10001,gid=10001`.
A host-loopback upstream is reached through a pasta forward.

**C-b, agent route: a pinned pasta forward.** The agent container gets
`--network pasta:-T,14318:<collector HTTP host port>,-U,none`. passt(1)
defines `-T` as "TCP port forwarding from target namespace to init
namespace", default **auto**. The packet leaves on `lo`, which `baseRuleset`
(`go/internal/runtime/egress.go`) accepts.

`containers.conf` `pasta_options` are appended to that string: in the smoke,
`-T 24460` opened that host port to an armed agent, and `podman info` does not
show it. So the runner runs a canary, `pasta:-T,none,-U,none`, which must
start and hold no sockets. A configured `-T` or `-U` makes pasta fail ("-T
none conflicts with previous options").

The agent merges `$HOME/.compass/env` after its exec env, and "File wins per
key it defines" (`main` in `packages/compass-agent/src/cli.ts`). The file
cannot set `COMPASS_*` keys (`isReservedEnvKey`), so the route travels as
`COMPASS_OTLP_ENDPOINT`. After the merge the agent copies it to
`OTEL_EXPORTER_OTLP_ENDPOINT` and drops per-signal endpoint keys.

Smoke (podman 5.8.6, armed, empty allowlist): only the forward opened, and a
span reached the upstream. `-U,auto` and `-T,auto` controls leaked.

The host backend dials the host port directly; its children inherit
`os.Environ()` (`envSlice` in `go/internal/runtime/host_backend.go`).

**`--otel-external`.** D3 points Compass surfaces at this endpoint, but no
child receives it. CB1 passes it to server and runner, amending D3's input to
an http(s) URL, because the Go emitters use `otlptracehttp`
(`go/internal/otel/provider.go`). Agents stay as shipped.

**C-c, dev wiring.** CC1 runs nixpkgs `otelcol-contrib` on a config from the
bundled container's renderer.

## Alternatives considered

- **`host.containers.internal`.** Allowlists match hosts, not ports.
- **A shared podman network.** Agents get L3 reach to each other.
- **Podman secrets, or host networking.** Secret state; breaks the `nop`
  bytes.
- **Parsing `containers.conf`.** Several sources; the canary reads podman's
  merge.
- **The route in `$HOME/.compass/env`.** That is the secrets channel.
- **A runner relay on the agent socket.** See OQ2.

## Plan

Order: CA1, then CB1, then CC1.

### CA1 — Collector upstream export

Files: `go/internal/stack/{config,deps,stack,collector_container}.go`,
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
type PastaVerifier interface{ VerifyPastaSupport(ctx context.Context) error }
Deps.PastaVerifier PastaVerifier // buildDeps wires runtime.NewPodmanCLI()
const CollectorHeadersMountPath = "/run/compass/otlp-headers.yaml"
const CollectorContainerUpstreamPort = "14319"

// runtime: Linux, podman >= 4.6, Host.Security.Rootless, Host.Pasta.Executable
func (p *PodmanCLI) VerifyPastaSupport(ctx context.Context) error
```

- Flags `--otel-export-endpoint` and `--otel-export-headers-file` fall back to
  `COMPASS_OTEL_EXPORT_*` env.
- `Validate` rejects a non-http(s) URL, userinfo, query, fragment, headers
  without an endpoint, an endpoint with `ExternalOTLPEndpoint`, and a loopback
  upstream off Linux or not `127.0.0.1`/`localhost`.
- A loopback upstream renders `127.0.0.1:14319` (`TLSServerName` for https)
  and adds `--network pasta:-T,14319:<port>,-U,none` after `startCollector`
  calls `Deps.PastaVerifier`.
- `Start` renames a 0600 temp copy of the headers over the old one; unlike
  `ensureGatewayToken`, it never keeps an existing file.

Red, then green:

- `TestCollectorConfigYAMLExportsUpstream` is red today: `otlphttp` and
  `memory_limiter` in all three pipelines, `max_request_body_size`, no `nop`,
  no `file_storage`.
- `TestCollectorConfigYAMLRealizesD3Posture` compares
  `CollectorConfigYAML(CollectorYAMLParams{})` with today's exact bytes.
- `TestCollectorContainerSpecForwardsLoopbackUpstream`,
  `TestResolveConfigCollectorExportFlags`, `TestVerifyPastaSupport`, and
  `TestStartCollectorRefusesForwardWithoutPasta` (a failing verifier stops
  `Up`).
- `TestCollectorStartReplacesHeadersCopy` (0600, rotation, source untouched)
  and `TestCollectorRunArgsExportContract` (no header value in argv or config).
- Smoke: `compass-stack up` delivers a span and header to a local receiver.

### CB1 — Route emitters to the collector

Files: `go/internal/runtime/{podman,agent,host_backend}.go`,
`go/internal/runner/{spec,host,agent_exec}.go`,
`go/cmd/compass-runner/main.go`, `go/internal/stack/{config,spec}.go`,
`packages/compass-agent/src/cli.ts`.

Interfaces:

```go
// runtime
type LoopbackForward struct{ Port, HostPort uint16 } // zero = none
WorkloadSpec.LoopbackForward, AgentSpec.LoopbackForward LoopbackForward
// canary: --network pasta:-T,none,-U,none; error on failure or any socket
func (p *PodmanCLI) VerifyNoPastaForwards(ctx context.Context, image string) error

// runner
const AgentCollectorPort = 14318
type CollectorRoute struct {
	Forward  runtime.LoopbackForward
	Endpoint string // "" = agents get no emitter
}
func ResolveCollectorRoute(ctx context.Context, engine runtime.WorkloadRuntime, hostPort uint16, server *url.URL, image string) (CollectorRoute, error)
SpecDefaults.LoopbackForward runtime.LoopbackForward
AgentHostConfig.OTLPEndpoint, AgentEnv.OTLPEndpoint string // execSpec sets COMPASS_OTLP_ENDPOINT
```

```ts
// cli.ts: called in main right after the env-file merge
export function applyCollectorRoute(env: Record<string, string | undefined>): void
```

- `createArgs` renders `--network pasta:-T,<Port>:<HostPort>,-U,none`; the
  zero value leaves the argv unchanged.
- compass-runner gains `--otel-collector-port` (`COMPASS_OTEL_COLLECTOR_PORT`;
  0 is off).
- `ResolveCollectorRoute` errors on a non-loopback `server` or a failed
  canary. Host gets a direct endpoint, podman a forward when
  `VerifyPastaSupport` passes; otherwise the route is empty and main logs it.
- `envSlice` drops inherited `OTEL_*` keys before the per-agent overlay.
- `runnerSpec` passes the flag only when exporting and not `cfg.microVM()`.
  `serverSpec` and `runnerSpec` set `OTEL_EXPORTER_OTLP_ENDPOINT` to the
  collector or `--otel-external` URL; `otlp.example.com:4317` fixtures become
  URLs.

Red, then green:

- `TestCreateArgsRendersLoopbackForward`,
  `TestExecSpecSetsCollectorRouteOnlyWhenConfigured`,
  `TestBuildSpecMapsLoopbackForward` and `TestEnvSliceDropsInheritedOTELKeys`.
- `TestResolveCollectorRoute`: port 0, remote server and failed canary
  (errors); host, podman, no pasta and microVM.
- `cli.test.ts`: the route beats file endpoints; with no route, a file
  endpoint stays.
- `TestServerAndRunnerSpecOTLPEndpoint`: collector, external URL or none.
- Podman-gated, like `TestKeepIDRemapMapsHostUIDToSpecUID`:
  `TestPastaForwardReachesOnlyCollectorPort` (no host-loopback UDP or
  late-bound TCP; red with `-U,auto` or `-T,auto`) and
  `TestVerifyNoPastaForwardsRefusesConfOptions` (`CONTAINERS_CONF_OVERRIDE`
  with `pasta_options = ["-T", "<port>"]`).

### CC1 — devenv runs the collector

Files: `devenv.nix`, `go/cmd/compass-stack/main.go`.

Interfaces: `compass-stack collector-config --grpc ADDR --http ADDR --health
ADDR [--otel-export-endpoint URL] [--otel-export-headers-file PATH]` prints
`CollectorConfigYAML` with the host headers path. `usageError` names five
subcommands.

- `processes.otel-collector` renders into the state dir, then execs
  `${pkgs.opentelemetry-collector-contrib}/bin/otelcol-contrib`, configured
  from the gitignored `devenv.local.nix`. Server and runner start `after` it.

Red, then green: today `devenv up` with an endpoint delivers nothing to a
local receiver; afterwards it receives `compass-server`, `compass-runner` and
`compass-agent` spans. `TestRunCollectorConfig` pins the renderer's bytes.

## Tasks

- [ ] CA1 — Collector upstream export, limits, headers and pasta preflight.
- [ ] CB1 — Pinned forward, route precedence, `--otel-external`.
- [ ] CC1 — devenv runs the collector from the shared renderer.

## Deferred (non-load-bearing)

- Port collision with an operator receiver on 4317/4318 waits for
  compass-stack `--collector-*-port` flags.
- `pasta_options` forwards already reach agents on podman's default network;
  checking every start is a separate egress change (RIG-5249).

## Open Questions

1. **Where the collector runs (load-bearing).** The forward reaches only the
   runner host's loopback. (a) Refuse the flag unless `--server` is loopback.
   (b) A collector per runner host forwards to the server's. (c) The runner
   relays agent OTLP to a remote collector (OQ2 B). **Recommendation:** (a)
   now; (b) when a remote runner needs agent telemetry.
2. **Pasta or a runner relay (load-bearing).** A: pasta now as an explicit
   stepping stone; the microVM follow-up uses a relay over the agent socket,
   which guestd already forwards (`gatewayPortKey` in
   `go/internal/guestd/cmdline.go`). B: build the relay now; it overwrites
   `compass.session.id` from `g.sessions.Session(g.containerName)`
   (`go/internal/runner/gateway/publish.go`), but the agent exporter needs a
   unix-socket transport. Either relay reverses
   `docs/designs/repo/compass-agent-effect-otel/design.md` Open Questions 1:
   "never the socket/vsock gateway". **Recommendation:** A; an approved relay
   replaces pasta on every tier.
3. **Forged session ids (load-bearing).** The untrusted agent stamps
   `compass.session.id`, and every agent arrives as `127.0.0.1`. A forged id
   files one session's telemetry under another, corrupting every D7 join and
   dashboard on that key. Billing reads the D5 event log, unaffected. (a) D7
   treats Plane B session ids as untrusted until a relay stamps them. (b) Hold
   the route until OQ2 B. **Recommendation:** (a); multi-tenant waits for
   OQ2 B.
