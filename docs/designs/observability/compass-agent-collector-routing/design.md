# Agent OTLP rides the agent socket to a host collector

Tracker: RIG-4611

Amendment record of the observability-architecture design
([`../compass-observability-architecture/design.md`](../compass-observability-architecture/design.md),
§D3 and §T4). The parent stays frozen.

Ledger-impact: DL-514.

## Problem / Intent

Matt ruled (RIG-5144) that agents ship OTLP through a collector to an
operator upstream. Parent T4 picks no route. This record decides the
collector's upstream export (C-a), the agent route (C-b) and dev wiring (C-c).

## Rulings

Matt, RIG-5237 (2026-10-11 and 2026-10-12):

1. **OQ1 B: a collector on each runner host** forwards upstream.
2. **OQ2 B, transport A: agents export OTLP/HTTP over the agent socket.** The
   Runner gateway serves `/v1/traces` and `/v1/metrics` and relays to the host
   collector. No pasta, and no egress allowlist entry for OTLP.
3. **The socket stays the agent's only channel to its Runner.**

Amended frozen texts:

- `docs/designs/repo/compass-agent-effect-otel/design.md`, Open Questions
  item 1 and Decision 3 ("never the socket/vsock gateway"): lifted for agent
  telemetry, for one transport on every tier, Runner-stamped attribution and
  no agent egress.
- `docs/designs/observability/compass-agent-loop-otel/design.md`, Decision 3a:
  the join key is the gateway-stamped `compass.session.id`; the SDK id moves
  to `compass.agent.sdk_session.id`.
- Both agent deployer contracts
  (`compass-agent-effect-otel/otel-endpoint-deployer-contract.md`,
  `compass-agent-loop-otel/deployer-contract.md`): env-file endpoints are
  ignored; the gateway sets `service.name`.
- Parent D3 "agent-side emission stays exactly as shipped": superseded for the
  route. Off by default holds.

## Global Constraints

- D3 holds: no export endpoint keeps today's `nop` config bytes; only config
  and headers files touch disk.
- No secret in argv or the 0644 config. Upstream credentials live in the 0600
  headers file and server or runner env, never in agents. The host tier shares
  the Runner's uid (`go/internal/runtime/host_backend.go`): no such boundary.
- Only the gateway sets `compass.session.id`, `compass.telemetry.origin` and
  the agent's `service.name` on relayed telemetry.
- Every endpoint passes `otlpenv.ParseEndpoint` (CA1).
- Server and runner emit or relay only with an endpoint, and drop inherited
  `OTEL_*`.
- `DefaultCollectorImage` stays 0.140.0; devenv's 0.155.0 validates the same
  config.
- Name receivers generically, e.g. Grafana Alloy.
- Breaking (`!`): `--otel-external` takes a URL; env-file agent endpoints are
  ignored; compass-runner refuses per-signal and TLS `OTEL_EXPORTER_OTLP_*`
  keys.

## Approach

**C-a: the collector exports.** `collectorConfigYAML`
(`go/internal/stack/collector_container.go`) hard-codes `nop: {}`. With
`--otel-export-endpoint` (fallback `COMPASS_OTEL_EXPORT_ENDPOINT`) it renders
`otlphttp` behind `memory_limiter` (256 MiB, 64 MiB spike) and an 8 MiB
receiver cap. `--otel-export-headers-file` is copied 0600 to
`/run/compass/otlp-headers.yaml` under `--userns=keep-id:uid=10001,gid=10001`
and replaced on every `Start`.

**C-b: the host collector.** The Runner relays agent OTLP to the endpoint its
own SDK reads, `OTEL_EXPORTER_OTLP_ENDPOINT` plus `_HEADERS` (`setupOtel`,
`go/cmd/compass-runner/main.go`). `compass-stack` sets it on server and runner:

- `--otel-export-endpoint`: the bundled collector,
  `http://127.0.0.1:<CollectorHTTPPort>`. Without it nothing is set (D3).
- `--otel-external <URL>`: the operator's own host collector, which owns
  batching, retry and upstream (ruling 1). `--otel-export-headers-file` rides
  it as `_HEADERS` in `Env`; adding `--otel-export-endpoint` is a usage error.
- `--otel-off`: no collector, nothing set.

A remote runner host's operator runs the collector (e.g. Grafana Alloy, or
otelcol-contrib with `compass-stack collector-config`). compass-runner refuses
a bad endpoint and per-signal or TLS keys, so relay and exporters agree.

**C-b: the gateway handlers.** `gateway.Serve`
(`go/internal/runner/gateway/gateway.go`) mounts `POST /v1/traces`,
`/v1/metrics` and `/v1/logs` (Resolved 2) only when `Deps.OTLPRelay` is set.
Each handler:

1. Resolves `g.sessions.Session(g.containerName)`; unbound or empty is 403, as
   `(*Gateway).Publish` fails closed.
2. Answers 415 unless `application/x-protobuf` with identity encoding.
3. Takes one of 4 per-Gateway slots and 16 MiB of a Runner-wide 128 MiB
   budget, else answers 503 with `Retry-After: 1` before reading.
4. Arms a 5 s read deadline (as `withBodyReadDeadline`,
   `go/server/network_door.go`), reads at most 4 MiB (413), and decodes with
   `go.opentelemetry.io/proto/otlp`, promoted to direct (400 if malformed).
5. Deletes `compass.session.id` from every scope, span, event, link, data
   point, exemplar and log record. On each resource it sets
   `compass.session.id` to the bound id that server spans carry
   (`go/internal/delivery/dispatch.go`), `compass.telemetry.origin=agent` and
   `service.name=compass-agent`.
6. Re-encodes (over 8 MiB is 413) and POSTs under `r.Context()` with an 8 s
   deadline, inside OTel JS's 10 s timeout. Upstream 2xx is 200; 429, 502, 503
   or 504 is 503 with its `Retry-After`; 413 stays; another 4xx is 400; else
   502. Only transient failures retry (`isExportHTTPErrorRetryable`).

**C-b: the agent.** When it relays, the Runner sets `COMPASS_AGENT_OTLP=1`, a
reserved `COMPASS_*` key the env file cannot set. After the env-file merge the
agent deletes every `OTEL_EXPORTER_OTLP_*` key and logs one `[compass-agent]`
line naming them. With the flag it writes the SDK session id as
`compass.agent.sdk_session.id`.

- `makeOtelLayer` (`packages/compass-agent/src/transport/otel-layer.ts`) builds
  exporters with `url: "http://compass-agent/v1/<signal>"` and
  `httpAgentOptions: { socketPath }` (Bun probe-proven).
- The SDK loop's exporters (`registerProviders`, `@oh-my-pi/pi-coding-agent`)
  read env once, at construction. A `Bun.serve` proxy on `127.0.0.1:0` checks
  a random per-process token header, caps bodies at 4 MiB and forwards with
  `fetch(…, { unix })`, as `runInGuestProbe`
  (`go/internal/runner/e2e_vsock_gateway_microvm_test.go`) does. Its endpoint
  and token are set only around `await telemetryHooks.init()`, so tool
  subprocesses never inherit them. Egress accepts `oif lo` (`NftScript`).

| Tier | Socket reaches the agent by | OTLP |
| --- | --- | --- |
| podman | bind mount at `/run/compass/agent.sock` | covered |
| microVM | guestd `gatewayProxy`, a byte splice to vsock | covered |
| host | `COMPASS_AGENT_SOCKET_PATH` in the state dir | covered; no boundary |
| apple-container | `--volume` over virtiofs | gap until the vsock transport lands (`docs/designs/platform/apple-container-macos-runner/design.md`) |

OTLP adds no gap of its own.

## Alternatives considered

- **Pasta forward** (earlier draft): podman only, agent-asserted attribution;
  its unshipped helpers are dropped and RIG-5249 becomes moot.
- **Direct export through the egress allowlist** (effect-otel ruling (a)):
  credentials in every agent, forgeable attribution.

## Plan

### CA1: collector upstream export

Files: `go/internal/stack/{collector_container,config}.go`,
`go/internal/stack/adapters/collector_container.go`,
`go/cmd/compass-stack/main.go`, new `go/internal/otel/otlpenv`.

Interfaces:

- `Config.CollectorExportEndpoint`, `Config.CollectorExportHeadersFile string`.
- `CollectorConfigYAML(p CollectorYAMLParams) string`, with
  `CollectorYAMLParams{GRPC, HTTP, Health, Export, HeadersPath string}`.
- `CollectorContainerSpec.ExportHeadersFile string`;
  `const CollectorHeadersMountPath = "/run/compass/otlp-headers.yaml"`.
- `otlpenv.ParseEndpoint(raw string, withHeaders bool) (*url.URL, error)`.

Red→green:

- `TestCollectorConfigYAMLExportsUpstream` (red today): `otlphttp` and
  `memory_limiter` in all three pipelines, an 8388608 body cap, no `nop`.
- `TestCollectorConfigYAMLRealizesD3Posture`: zero params give today's bytes.
- `TestParseEndpoint`: accepts http(s) to `127.0.0.1`, `localhost` and remote
  hosts; refuses bare `host:port`, `127.0.0.2`, `[::1]`, `0.0.0.0`, userinfo,
  query, fragment, and remote `http` with headers.
- `TestCollectorStartReplacesHeadersCopy`; `TestCollectorRunArgsExportContract`
  (no header value in argv or config).
- Smoke: `compass-stack up` delivers a span and the header to a local receiver.

### CB1: gateway OTLP relay

Files: `go/internal/runner/gateway/{gateway,otlp}.go`,
`go/internal/otel/otlprelay`, `go/go.mod`.

Interfaces:

- `type OTLPRelay interface { Export(ctx context.Context, path string, body []byte) error }`;
  `Deps.OTLPRelay` (nil leaves the paths unmounted);
  `Deps.OTLPBudget *semaphore.Weighted` (`golang.org/x/sync`).
- `otlprelay.New(endpoint *url.URL, headers map[string]string) *Relay`; a
  non-2xx upstream returns `*otlprelay.StatusError{Code int, RetryAfter string}`.
- `const maxOTLPBodyBytes = 4 << 20`, `maxOTLPInFlight = 4`.
- Counter `compass.runner.otlp_relay.requests{signal,outcome}`, nil-guarded
  like `compass.delivery.dispatched`; no session label.

Red→green (hermetic, fake relay):

- `TestOTLPStampOverridesAgentSession`: forged ids on a resource, span, data
  point and log record, and `service.name=compass-server`, arrive as the bound
  id and `compass-agent`.
- `TestOTLPUnboundContainerForbidden` (403, relay uncalled); `TestOTLPBodyCap`
  (413); `TestOTLPRejectsCompressedBody` (gzip is 415).
- `TestOTLPSaturationShedsWith503`: a fifth request or empty budget, body unread.
- `TestOTLPRelayFailure`: 400 stays 400, 503 keeps `Retry-After`, transport
  error is 502.
- `TestOTLPUnmountedWhenRelayNil` (404); `TestRelayPostsWithHeaders`.
- microVM e2e `TestVsockGateway_InGuestOTLPStamped`: a guest POST reaches a
  fake `AgentHostConfig.OTLPRelay` with the bound id. The podman leg joins
  `TestSocketOwnedAndConnectableInContainer` once its probe image lands.

### CB2: runner and stack wiring

Files: `go/cmd/compass-runner/main.go`, `go/internal/runner/{host,agent_exec}.go`,
`go/internal/runtime/host_backend.go`, `go/internal/stack/{config,spec}.go`,
`go/internal/stack/adapters/process.go`, `go/cmd/compass-stack/main.go`.

Interfaces:

- `AgentHostConfig.OTLPRelay gateway.OTLPRelay`; `(*agentHost).gatewayDeps()
  gateway.Deps` replaces both `gateway.Deps{…}` literals in `host.go` and
  shares one budget.
- `AgentEnv.Telemetry bool` sets `COMPASS_AGENT_OTLP=1`.
- `otlpenv.HeadersValue(map[string]string) (string, error)`;
  `otlpenv.RunnerConfig(environ []string) (*url.URL, map[string]string, error)`.
- `Config.OTLPOff bool`; `ExternalOTLPEndpoint` becomes a URL;
  `childEnv(inherited, overlay []string) []string` and `envSlice` drop
  inherited `OTEL_*`.

Red→green:

- `TestServerAndRunnerSpecOTLPEndpoint`: export, external and off; headers in
  `Env`, never `Args`.
- `TestStartDropsInheritedOTELKeys`, `TestEnvSliceDropsInheritedOTELKeys`,
  `TestExecSpecTelemetryFlag`.
- `TestResolveConfigOTelModes`: external plus export is refused.
- `TestRunnerConfig`: refuses a bad endpoint, a per-signal or certificate key,
  and remote `http` with headers.
- Migration to `--otel-off`: `go/e2e/fixture.go`, `TestContainerPostgresUpDown`,
  `TestExternalDatabaseUpDown`, `TestNatsUpDown`, `TestExternalNatsUpDown`. To
  a URL: `TestExternalOTLPUpDown`, `TestResolveConfigCollectorFlags`,
  `TestConfigValidate`, `TestExternalOTLPSkipsCollector`.

### CB3: agent exporters over the socket

Files: `packages/compass-agent/src/transport/{index,otel-layer,otlp-proxy}.ts`,
`packages/compass-agent/src/cli.ts`.

Interfaces:

- `makeOtelLayer(socketPath: string): Layer.Layer<never>` (`Layer.empty`
  without the flag), called by `createUnixSocketTransport`.
- `startOtlpProxy(socketPath: string): Promise<{ endpoint: string; headers: string; close(): Promise<void> }>`.
- `scrubOtlpEnv(env: NodeJS.ProcessEnv): string[]` returns the dropped keys;
  `isTelemetryEndpointConfigured` keys on the flag.

Red→green (bun:test, a Unix-socket server in a temp dir):

- A transport span reaches `/v1/traces` only with the flag.
- A no-argument `OTLPTraceExporter` reaches the socket through the proxy; a
  tokenless request is refused.
- A child spawned after init sees no `OTEL_EXPORTER_OTLP_*`.
- `scrubOtlpEnv` drops and logs an env-file endpoint.
- Migration to the flag: `cli.test.ts`'s env-file endpoint, "all four
  loop-OTel env keys…", "endpoint set ⇒ env defaults…" and "deployer-set
  OTEL_SERVICE_NAME…" tests; `otel-layer.test.ts`'s three endpoint tests.

### CC1: devenv runs the collector

Interfaces: `compass-stack collector-config --grpc --http --health
[--otel-export-endpoint] [--otel-export-headers-file]` prints
`CollectorConfigYAML`; `usageError` names five subcommands.

devenv `processes.otel-collector` runs `otelcol-contrib` when the gitignored
`devenv.local.nix` sets `env.COMPASS_OTEL_EXPORT_ENDPOINT`; server and runner
then point at it.

Red→green: `TestRunCollectorConfig`; `devenv up` with an endpoint delivers an
agent span upstream (none today).

### CD1: operator docs

Interfaces: none. `docs/self-host.md` gains an observability section (export
flags, `--otel-external`, `--otel-off`, remote-runner `OTEL_EXPORTER_OTLP_*`);
each deployer contract points there.

Red→green: none; rumdl and the design-ledger gate pass.

## Tasks

- [ ] CA1: collector export
- [ ] CB1: gateway relay
- [ ] CB2: runner and stack wiring
- [ ] CB3: agent socket exporters
- [ ] CC1: devenv collector
- [ ] CD1: operator docs

## Resolved decisions

1. **SDK-loop exporters use the token proxy, not an SDK patch:** no fork, no
   new dependency. Agent-owned providers would miss the SDK's metric and log
   hooks, which only `initTelemetryExport`'s private module registers.
2. **`/v1/logs` is mounted with the same stamp.** An env-file endpoint enables
   SDK logs today (`resolveSignalConfig`), so log content is unchanged.
3. **The remote host collector is operator-run:** a runner-only
   `compass-stack` mode stays additive until RIG-2485 needs it.
