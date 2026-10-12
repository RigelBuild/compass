# Agent containers export through the bundled collector

Tracker: RIG-4611

Amendment record of the observability-architecture design
([`../compass-observability-architecture/design.md`](../compass-observability-architecture/design.md),
§D3 and §T4). The parent stays frozen.

Ledger-impact: none. This record adds no `DL-NNN` decision.

## Problem / Intent

Matt ruled on 2026-10-11 (RIG-5144) that agent containers ship OTLP through
the bundled collector to an operator upstream. Parent T4 promises "the
Runner-materialized env pointing agent emission at the bundled collector" but
picks no route. This record decides C-a (upstream export), C-b (agent route)
and C-c (dev wiring).

## Global Constraints

- D3 holds: with no export endpoint the collector config keeps today's `nop`
  bytes. Exported telemetry stays in memory; only config and headers files
  are written.
- No secret in a podman argv or the 0644 config. Endpoints are http(s) URLs
  without userinfo, query or fragment; credentials live in the 0600 headers
  file.
- Loopback means host `127.0.0.1` or `localhost`; other loopback forms are
  refused.
- Server and runner emit only with an export endpoint or `--otel-external`,
  and drop inherited `OTEL_*`. An agent route clears the env file's
  `OTEL_EXPORTER_OTLP_*`.
- Pasta needs Linux and rootless podman 4.6+.
- `DefaultCollectorImage` stays 0.140.0, so the exporter key is `otlphttp`;
  devenv's 0.155.0 validates the same config.
- `baseRuleset` is unchanged. Name operator receivers generically, e.g.
  Grafana Alloy.

## Approach

**C-a, upstream export.** `collectorConfigYAML`
(`go/internal/stack/collector_container.go`) hard-codes `nop: {}`. A new
`--otel-export-endpoint` swaps in one `otlphttp` exporter behind a
`memory_limiter` and a 4 MiB request cap, since untrusted agents now reach
the collector. Each start replaces a 0600 copy of the optional headers file,
mounted with `--userns=keep-id:uid=10001,gid=10001`.

**C-b, agent route: a pinned pasta forward.** Agents get
`--network pasta:-T,14318:<target port>,-U,none`. passt(1) defines `-T` as
"TCP port forwarding from target namespace to init namespace", default
**auto**. Traffic leaves on `lo`, which `baseRuleset`
(`go/internal/runtime/egress.go`) accepts. A podman 5.8.6 smoke opened only
the forward; `-U,auto` and `-T,auto` controls leaked.

Podman appends `containers.conf` `pasta_options`, unseen by `podman info`: a
configured `-T 24460` reached an armed agent. So a canary,
`pasta:-T,none,-U,none`, must start and hold no sockets; any configured `-T`
or `-U` fails it.

The agent merges `$HOME/.compass/env` into `process.env`, and "File wins per
key it defines" (`main` in `packages/compass-agent/src/cli.ts`). The file
cannot set `COMPASS_*` (`isReservedEnvKey`), so the route travels as
`COMPASS_OTLP_ENDPOINT`; after the merge the agent clears the file's
`OTEL_EXPORTER_OTLP_*` keys, so no `grpc` protocol or stale headers survive.

**Agent targets.** compass-runner's `--otel-agent-endpoint` names the agents'
target. The stack passes the collector URL `http://127.0.0.1:<HTTP port>`,
whatever its `ListenAddr`, or the `--otel-external` URL. A remote host joins
an armed allowlist (OQ4); a loopback target needs pasta, else agents get no
emitter.

**`--otel-external`.** D3 points every surface at it; today no child receives
it. CB1 passes it to server, runner and agents, so it becomes an http(s) URL:
the Go emitters use `otlptracehttp` (`go/internal/otel/provider.go`).
Refusing a bare `host:port` breaks callers, so CB1's commit carries `!` for
release-please. A new `--otel-off` replaces the dummy `127.0.0.1:4317` tests
pass: no collector, endpoint or route.

**C-c.** CC1 runs nixpkgs `otelcol-contrib` on the bundled renderer's config.

## Alternatives considered

- **`host.containers.internal`.** Allowlists match hosts, not ports.
- **A shared podman network.** Agents reach each other.
- **Podman secrets or host networking.** Secret state; breaks `nop` bytes.
- **Parsing `containers.conf`.** Several sources; the canary sees the merge.

## Plan

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
Deps.PastaVerifier PastaVerifier // buildDeps: runtime.NewPodmanCLI()
const CollectorHeadersMountPath = "/run/compass/otlp-headers.yaml"
const CollectorContainerUpstreamPort = "14319"

// runtime: Linux, podman >= 4.6, rootless, Host.Pasta.Executable
func (p *PodmanCLI) VerifyPastaSupport(ctx context.Context) error
```

- `--otel-export-endpoint` and `--otel-export-headers-file` fall back to
  `COMPASS_OTEL_EXPORT_*`. `Validate` refuses a bad URL, headers without an
  endpoint, an endpoint with `ExternalOTLPEndpoint`, and a loopback upstream
  off Linux or in another form.
- `collectorContainerSpec`, not the renderer, rewrites a loopback upstream to
  `127.0.0.1:14319` (`TLSServerName` for https) and adds
  `--network pasta:-T,14319:<port>,-U,none`; `startCollector` first calls
  `Deps.PastaVerifier`.
- `Start` renames a 0600 temp copy over the old headers; unlike
  `ensureGatewayToken`, it never keeps an existing file.

Red, then green:

- `TestCollectorConfigYAMLExportsUpstream`, red today: `otlphttp` and
  `memory_limiter` in all three pipelines, `max_request_body_size`, no `nop`
  or `file_storage`.
- `TestCollectorConfigYAMLRealizesD3Posture`: zero params give today's bytes.
- `TestCollectorContainerSpecForwardsLoopbackUpstream`,
  `TestResolveConfigCollectorExportFlags`, `TestVerifyPastaSupport`,
  `TestStartCollectorRefusesForwardWithoutPasta`.
- `TestCollectorStartReplacesHeadersCopy` (0600, rotation, source untouched);
  `TestCollectorRunArgsExportContract` (no header value in argv or config).
- Smoke: `compass-stack up` delivers a span and header to a local receiver.

### CB1 — Route every emitter

Files: `go/internal/runtime/{podman,agent,host_backend}.go`,
`go/internal/runner/{spec,host,agent_exec}.go`,
`go/internal/stack/{config,spec,stack}.go`,
`go/internal/stack/adapters/process.go`,
`go/cmd/{compass-runner,compass-stack}/main.go`,
`packages/compass-agent/src/cli.ts`, and the migrated callers.

Interfaces:

```go
// runtime
type LoopbackForward struct{ Port, HostPort uint16 } // zero = none
WorkloadSpec.LoopbackForward, AgentSpec.LoopbackForward LoopbackForward
// canary: errors on failure or any socket
func (p *PodmanCLI) VerifyNoPastaForwards(ctx context.Context, image string) error

// runner
type pastaForwarder interface { // *runtime.PodmanCLI
	VerifyPastaSupport(ctx context.Context) error
	VerifyNoPastaForwards(ctx context.Context, image string) error
}
const AgentCollectorPort = 14318
type AgentRoute struct {
	Forward   runtime.LoopbackForward
	Endpoint  string // "" = no agent emitter
	AllowHost string // remote target host
}
func ResolveAgentRoute(ctx context.Context, engine runtime.WorkloadRuntime, target *url.URL, image string) (AgentRoute, error)
SpecDefaults.LoopbackForward runtime.LoopbackForward
AgentHostConfig.OTLPEndpoint, AgentEnv.OTLPEndpoint string // COMPASS_OTLP_ENDPOINT

// stack
Config.OTLPOff bool
func (c Config) bundlesCollector() bool // replaces ExternalOTLPEndpoint == "" gates
func childEnv(inherited, overlay []string) []string // adapters; drops inherited OTEL_*
```

```ts
// main passes env.COMPASS_OTLP_ENDPOINT after the env-file merge
export function applyCollectorRoute(route: string | undefined, target = process.env): void
```

- `ResolveAgentRoute`: no target, empty; host tier (`runtime.TierOf`),
  direct; remote, verbatim plus `AllowHost`. Loopback needs a
  `pastaForwarder` passing `VerifyPastaSupport`, else an empty, logged route;
  then the canary runs, and its failure errors; then the forward keeps scheme
  and host on port 14318.
- compass-runner gains `--otel-agent-endpoint`
  (`COMPASS_OTEL_AGENT_ENDPOINT`) and appends `AllowHost` to a configured
  policy after `ResolveEgress`.
- `createArgs` renders `--network pasta:-T,<Port>:<HostPort>,-U,none`; zero
  leaves the argv unchanged. `envSlice` and `childEnv` drop inherited
  `OTEL_*`; `ProcessSupervisor.Start` uses `childEnv`, which covers the stack
  `embedded.RunStackUp` execs.
- `serverSpec` sets `OTEL_EXPORTER_OTLP_ENDPOINT` and `runnerSpec` adds
  `--otel-agent-endpoint`: the collector URL when exporting, else the
  `--otel-external` URL, else neither.
- `--otel-off` excludes `--otel-external` and the export flags; `Validate`
  refuses a non-URL `ExternalOTLPEndpoint`.
- Migration: `go/e2e/fixture.go` sets `OTLPOff`; the `127.0.0.1:4317` runs in
  `go/cmd/compass-stack/{container_postgres,nats}_podman_test.go` pass
  `--otel-off`; `TestExternalOTLPUpDown` passes `http://127.0.0.1:4318`;
  `otlp.example.com:4317` fixtures become URLs; the `ci.yml` comment names
  `--otel-off`.

Red, then green:

- `TestCreateArgsRendersLoopbackForward`,
  `TestExecSpecSetsCollectorRouteOnlyWhenConfigured`,
  `TestBuildSpecMapsLoopbackForward`, `TestEnvSliceDropsInheritedOTELKeys`.
- `TestResolveAgentRoute`: no target, host, remote, no capability, http and
  https forwards, failed canary; with pasta unsupported a counting fake
  proves the canary never ran.
- `TestStartDropsInheritedOTELKeys` (a `trapecho` child): an inherited base
  endpoint with the route off; an inherited
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` with it on.
- `TestServerAndRunnerSpecOTLPEndpoint`: collector, external, `nop`, off; a
  `0.0.0.0` `ListenAddr` keeps the loopback collector URL.
- `TestResolveConfigOTelOff`; `TestConfigValidate` refuses `127.0.0.1:4317`.
- `cli.test.ts`: the route beats a file endpoint and clears a file `grpc`
  protocol and headers; with no route a file endpoint stays.
- Podman-gated: `TestPastaForwardReachesOnlyCollectorPort` (no host-loopback
  UDP or late-bound TCP; red with `-U,auto` or `-T,auto`) and
  `TestVerifyNoPastaForwardsRefusesConfOptions` (a `-T` in
  `CONTAINERS_CONF_OVERRIDE`).

### CC1 — devenv runs the collector

Files: `devenv.nix`, `go/cmd/compass-stack/main.go`.

Interfaces: `compass-stack collector-config --grpc ADDR --http ADDR --health
ADDR [--otel-export-endpoint URL] [--otel-export-headers-file PATH]` prints
`CollectorConfigYAML` with the endpoint verbatim and the host headers path.
`usageError` names five subcommands.

- When the gitignored `devenv.local.nix` sets
  `env.COMPASS_OTEL_EXPORT_ENDPOINT`, `processes.otel-collector` renders into
  the state dir and execs
  `${pkgs.opentelemetry-collector-contrib}/bin/otelcol-contrib`. Server and
  runner start `after` it with `OTEL_EXPORTER_OTLP_ENDPOINT`, and the runner
  with `--otel-agent-endpoint`, both `http://127.0.0.1:<http port>`.

Red, then green: `devenv up` with an endpoint delivers nothing to a local
receiver today, and afterwards delivers `compass-server`, `compass-runner` and
`compass-agent` spans. `TestRunCollectorConfig` pins a loopback upstream.

## Tasks

- [ ] CA1 — Collector upstream export.
- [ ] CB1 — Route every emitter.
- [ ] CC1 — devenv runs the collector.

## Deferred (non-load-bearing)

- Default-network `pasta_options` forwards already reach agents (RIG-5249).

## Open Questions

1. **Remote runners.** A forward reaches only the runner host's loopback, so
   a remote runner names a remote `--otel-agent-endpoint` (a), or its host
   runs a forwarding collector (b). **Recommendation:** (a); (b) on demand.
2. **Pasta or a runner relay (load-bearing).** A: pasta now; microVM later
   relays over the agent socket guestd forwards (`gatewayPortKey`,
   `go/internal/guestd/cmdline.go`). B: a relay now, stamping
   `compass.session.id` from `g.sessions.Session(g.containerName)`
   (`go/internal/runner/gateway/publish.go`); agents need a unix-socket
   exporter. Any relay reverses Open Questions 1 of
   `docs/designs/repo/compass-agent-effect-otel/design.md`, "never the
   socket/vsock gateway". **Recommendation:** A; an approved relay replaces
   pasta everywhere.
3. **Forged session ids (load-bearing).** Untrusted agents stamp
   `compass.session.id`, so a forged id files telemetry under another session
   and corrupts D7 joins; billing reads the D5 event log. (a) D7 treats
   Plane B session ids as untrusted until a relay stamps them. (b) Hold the
   route until OQ2 B. **Recommendation:** (a); multi-tenant waits for OQ2 B.
4. **Remote agent targets.** Allowlists match hosts, not ports, so agents
   reach every port on that host. (a) Accept: the operator named it. (b)
   Refuse, silencing agents under `--otel-external`. **Recommendation:** (a);
   (b) breaks D3.
