# Agent containers export through the bundled collector

Tracker: RIG-4611

Amendment record of the observability-architecture design
([`../compass-observability-architecture/design.md`](../compass-observability-architecture/design.md),
§D3 and §T4). The parent stays frozen.

Ledger-impact: none.

## Problem / Intent

Matt ruled on 2026-10-11 (RIG-5144) that agent containers ship OTLP through
the bundled collector to an operator upstream. Parent T4 points agent
emission "at the bundled collector" but picks no route. This record decides
C-a (upstream export), C-b (agent route) and C-c (dev wiring).

## Global Constraints

- D3 holds: no export endpoint keeps today's `nop` config bytes, and only
  config and headers files touch disk.
- No secret in argv or the 0644 config; credentials stay in the 0600 headers
  file, then process env or 0600 files, never podman `-e`.
- `otlpenv.ParseEndpoint` admits http(s) URLs with a host, no userinfo, query
  or fragment, and loopback only as `127.0.0.1` or `localhost`.
- Server and runner emit only with an export endpoint or `--otel-external`,
  and drop inherited `OTEL_*`. An agent route, or `off`, clears the env
  file's `OTEL_EXPORTER_OTLP_*`.
- Pasta needs Linux and rootless podman 4.6+.
- `DefaultCollectorImage` stays 0.140.0 (`otlphttp`); devenv's 0.155.0
  validates the same config.
- `baseRuleset` is unchanged. Name receivers generically, e.g. Grafana Alloy.

## Approach

**C-a, upstream export.** `collectorConfigYAML`
(`go/internal/stack/collector_container.go`) hard-codes `nop: {}`.
`--otel-export-endpoint` swaps in one `otlphttp` exporter, behind a
`memory_limiter` and a 4 MiB request cap because untrusted agents now reach
the collector.

**C-b, agent route: a pinned pasta forward.** Agents get
`--network pasta:-T,14318:<target port>,-U,none`. passt(1) `-T` forwards
"from target namespace to init namespace", default **auto**. Traffic leaves
on `lo`, which `baseRuleset` (`go/internal/runtime/egress.go`) accepts. A
podman 5.8.6 smoke opened only the forward; `auto` controls leaked.

Podman appends `containers.conf` `pasta_options`, unseen by `podman info`: a
configured `-T 24460` reached an armed agent. So a canary,
`pasta:-T,none,-U,none`, must start and hold no sockets.

The agent merges `$HOME/.compass/env` into `process.env`; "File wins per key
it defines" (`main`, `packages/compass-agent/src/cli.ts`). The file cannot
set `COMPASS_*` (`isReservedEnvKey`), so the route travels as
`COMPASS_OTLP_ENDPOINT`.

**Agent targets.** compass-runner's `--otel-agent-endpoint` takes the
collector URL, the `--otel-external` URL, or `off`. Remote hosts join an
armed allowlist; loopback needs pasta.

**`--otel-external`.** D3 points every surface at it, yet no child receives
it. CB1 passes it on as an http(s) URL for `otlptracehttp`; the commit
carries `!`. `--otel-off` replaces the dummy `127.0.0.1:4317` tests pass.
The headers file now authenticates it as `OTEL_EXPORTER_OTLP_HEADERS`. Exec
env is podman `-e` argv (`execSpec`, `go/internal/runner/agent_exec.go`), so
agents read a 0600 `$HOME/.compass/otlp-headers` written over stdin (OQ4).

New leaf `go/internal/otel/otlpenv` shares the checks with compass-runner
and keeps the OTel SDK out of the stack core.

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
`go/internal/runtime/podman.go`, `go/internal/otel/otlpenv/otlpenv.go`,
`go/cmd/compass-stack/main.go`.

Interfaces:

```go
// stack
Config.CollectorExportEndpoint, Config.OTLPHeadersFile string
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

// otlpenv
func ParseEndpoint(raw string) (*url.URL, error)
```

- `--otel-export-endpoint` and `--otel-export-headers-file` fall back to
  `COMPASS_OTEL_EXPORT_*`. `Validate` runs `ParseEndpoint` and refuses
  headers with neither endpoint, both endpoints, and a loopback upstream off
  Linux.
- `collectorContainerSpec`, not the renderer, rewrites a loopback upstream to
  `127.0.0.1:14319` (`TLSServerName` for https) and adds
  `--network pasta:-T,14319:<port>,-U,none`; `startCollector` first calls
  `Deps.PastaVerifier`.
- `Start` renames a 0600 temp copy over the old headers, mounted with
  `--userns=keep-id:uid=10001,gid=10001`; unlike `ensureGatewayToken`, it
  never keeps an existing file.

Red, then green:

- `TestCollectorConfigYAMLExportsUpstream`, red today: `otlphttp` and
  `memory_limiter` in all three pipelines, `max_request_body_size`, no `nop`.
- `TestCollectorConfigYAMLRealizesD3Posture`: zero params give today's bytes.
- `TestParseEndpoint` accepts `127.0.0.1`, `localhost` and remote URLs;
  refuses `127.0.0.1:4317`, `otlp.example.com:4317`, `127.0.0.2`, `[::1]`,
  `0.0.0.0`, no host, userinfo, a query and a bare `#`.
- `TestCollectorContainerSpecForwardsLoopbackUpstream`,
  `TestResolveConfigCollectorExportFlags`, `TestVerifyPastaSupport`,
  `TestStartCollectorRefusesForwardWithoutPasta`,
  `TestCollectorStartReplacesHeadersCopy`; `TestCollectorRunArgsExportContract`
  (no header value in argv or config).
- Smoke: `compass-stack up` delivers a span and header to a local receiver.

### CB1 — Route every emitter

Files: `go/internal/runtime/{podman,agent,host_backend}.go`,
`go/internal/runner/{spec,host,agent_exec}.go`,
`go/internal/stack/{config,spec,stack}.go`,
`go/internal/stack/adapters/process.go`, `go/internal/otel/otlpenv`,
`go/cmd/{compass-runner,compass-stack}/main.go`,
`packages/compass-agent/src/cli.ts`, and the migrated callers.

Interfaces:

```go
// runtime
type LoopbackForward struct{ Port, HostPort uint16 } // zero = none
WorkloadSpec.LoopbackForward, AgentSpec.LoopbackForward LoopbackForward
func (p *PodmanCLI) VerifyNoPastaForwards(ctx context.Context, image string) error // canary

// runner
type pastaForwarder interface { // *runtime.PodmanCLI
	VerifyPastaSupport(ctx context.Context) error
	VerifyNoPastaForwards(ctx context.Context, image string) error
}
const AgentCollectorPort = 14318
const AgentOTLPOff = "off"
type AgentRoute struct {
	Forward   runtime.LoopbackForward
	Endpoint  string // "" = no agent emitter
	AllowHost string // remote target host
}
func ResolveAgentRoute(ctx context.Context, engine runtime.WorkloadRuntime, target *url.URL, image string) (AgentRoute, error)
SpecDefaults.LoopbackForward runtime.LoopbackForward
AgentHostConfig.OTLPEndpoint, AgentEnv.OTLPEndpoint string // COMPASS_OTLP_ENDPOINT
AgentHostConfig.OTLPHeaders string // runner env COMPASS_OTEL_AGENT_HEADERS
func parseAgentEndpoint(raw string) (target *url.URL, off bool, err error) // compass-runner

// otlpenv: YAML map → name=url.PathEscape(value),…
func HeadersValue(path string) (string, error)

// stack; serverSpec and runnerSpec take the encoded headers
Config.OTLPOff bool
func (c Config) bundlesCollector() bool // replaces ExternalOTLPEndpoint == "" gates
func childEnv(inherited, overlay []string) []string // adapters; drops inherited OTEL_*
```

```ts
// called after the env-file merge
export function applyCollectorRoute(route: string | undefined, headers: string, target = process.env): void
```

- `ResolveAgentRoute`: no target, empty; host tier (`runtime.TierOf`),
  direct; remote, verbatim plus `AllowHost`. Loopback needs a
  `pastaForwarder` passing `VerifyPastaSupport`, else an empty, logged route;
  then a failed canary errors; then the forward keeps scheme and host on
  port 14318.
- `--otel-agent-endpoint` (`COMPASS_OTEL_AGENT_ENDPOINT`) fails startup on a
  `parseAgentEndpoint` error; `off` yields `AgentRoute{Endpoint:
  AgentOTLPOff}`. `main` appends `AllowHost` to a configured policy after
  `ResolveEgress`.
- Before a URL-routed agent exec, `WriteAgentFile` writes `OTLPHeaders`,
  possibly empty, to `.compass/otlp-headers`.
- `createArgs` renders `--network pasta:-T,<Port>:<HostPort>,-U,none`; zero
  leaves argv unchanged. `envSlice` and `childEnv` (used by
  `ProcessSupervisor.Start`) drop inherited `OTEL_*`.
- Server `OTEL_EXPORTER_OTLP_ENDPOINT` and runner `--otel-agent-endpoint`
  carry the collector URL when exporting, else the external URL with one
  `Up`-time `HeadersValue` as `OTEL_EXPORTER_OTLP_HEADERS` and the runner's
  `COMPASS_OTEL_AGENT_HEADERS`. `--otel-off` gives the runner `off`.
- `Validate` runs `ParseEndpoint` on `ExternalOTLPEndpoint` and admits the
  headers file with it; `--otel-off` excludes all three.
- Migration: `go/e2e/fixture.go` sets `OTLPOff`;
  `go/cmd/compass-stack/{container_postgres,nats}_podman_test.go` pass
  `--otel-off`; `TestExternalOTLPUpDown` and `otlp.example.com:4317`
  fixtures use URLs; the `ci.yml` comment names `--otel-off`.

Red, then green:

- `TestCreateArgsRendersLoopbackForward`, `TestBuildSpecMapsLoopbackForward`,
  `TestEnvSliceDropsInheritedOTELKeys`;
  `TestExecSpecSetsCollectorRouteOnlyWhenConfigured`, including `off`.
- `TestResolveAgentRoute` covers each branch; a counting fake proves an
  unsupported host skips the canary.
- `TestParseAgentEndpoint` (table): empty, `off`, each `TestParseEndpoint`
  refusal.
- `TestStartDropsInheritedOTELKeys` (a `trapecho` child): an inherited base
  endpoint with the route off; an inherited traces endpoint with it on.
- `TestServerAndRunnerSpecOTLPEndpoint`: collector, external (headers in
  `Env`, never `Args`), `nop`, off; a `0.0.0.0` `ListenAddr` keeps loopback.
- `TestHeadersValue` escapes `,`, `;` and `%` and refuses a newline;
  `TestStartWritesAgentOTLPHeaders`: stdin, never exec argv.
- `TestResolveConfigOTelOff`; `TestConfigValidate` refuses `127.0.0.1:4317`.
- `cli.test.ts`: the route beats a file endpoint, clears file protocol and
  headers, and exports `otlp-headers`; `off` with a file endpoint leaves no
  endpoint and skips telemetry init; no route keeps a file endpoint.
- Podman-gated: `TestPastaForwardReachesOnlyCollectorPort` (no host UDP or
  late TCP; red with `auto`) and `TestVerifyNoPastaForwardsRefusesConfOptions`
  (a `-T` in `CONTAINERS_CONF_OVERRIDE`).

### CC1 — devenv runs the collector

Files: `devenv.nix`, `go/cmd/compass-stack/main.go`.

Interfaces: `compass-stack collector-config --grpc ADDR --http ADDR --health
ADDR [--otel-export-endpoint URL] [--otel-export-headers-file PATH]` prints
`CollectorConfigYAML`, endpoint verbatim, host headers path. `usageError`
names five subcommands.

- When the gitignored `devenv.local.nix` sets
  `env.COMPASS_OTEL_EXPORT_ENDPOINT`, `processes.otel-collector` execs
  `${pkgs.opentelemetry-collector-contrib}/bin/otelcol-contrib` on it. Server
  and runner start `after` it, with `OTEL_EXPORTER_OTLP_ENDPOINT` and
  `--otel-agent-endpoint` at `http://127.0.0.1:<http port>`.

Red, then green: `devenv up` with an endpoint delivers no spans today, then
server, runner and agent spans. `TestRunCollectorConfig` pins a loopback
upstream.

## Tasks

- [ ] CA1 — Collector upstream export.
- [ ] CB1 — Route every emitter.
- [ ] CC1 — devenv runs the collector.

## Deferred (non-load-bearing)

- Default-network `pasta_options` forwards already reach agents (RIG-5249).

## Open Questions

1. **Remote runners.** Forwards reach only the runner host's loopback. (a)
   Name a remote `--otel-agent-endpoint`. (b) Run a forwarding collector
   there. **Recommendation:** (a); (b) on demand.
2. **Pasta or a runner relay (load-bearing).** A: pasta now; microVM later
   relays over the agent socket (`gatewayPortKey`,
   `go/internal/guestd/cmdline.go`). B: a relay now, stamping
   `compass.session.id` from `g.sessions.Session(g.containerName)`
   (`go/internal/runner/gateway/publish.go`). Any relay reverses OQ1 of
   `docs/designs/repo/compass-agent-effect-otel/design.md`.
   **Recommendation:** A; an approved relay replaces pasta everywhere.
3. **Forged session ids (load-bearing).** Agents stamp `compass.session.id`,
   so a forged id corrupts D7 joins; billing reads the D5 event log. (a) D7
   treats Plane B ids as untrusted until a relay stamps them. (b) Hold the
   route until OQ2 B. **Recommendation:** (a); multi-tenant waits for OQ2 B.
4. **External targets.** Allowlists match hosts, not ports, and agents can
   read the external headers. (a) Accept: the operator named both, and
   `--otel-export-endpoint` keeps credentials in the collector. (b) Refuse,
   silencing agents under `--otel-external`. **Recommendation:** (a); (b)
   breaks D3.
