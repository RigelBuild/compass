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
   Runner gateway serves `/v1/traces` and `/v1/metrics` (`/v1/logs`: Resolved
   2) and relays to the host collector. No pasta, no OTLP egress entry.
3. **The socket stays the agent's only channel to its Runner.**

Amended frozen texts:

- `docs/designs/repo/compass-agent-effect-otel/design.md` Open Questions item
  1 and Decision 3: lifted for agent telemetry.
- `docs/designs/observability/compass-agent-loop-otel/design.md` Decision 3a:
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
  headers file and server or runner env, never in agents.
- Host-tier exception: agents share the Runner's uid
  (`go/internal/runtime/host_backend.go`), so they can read its env and reach
  any session's socket. Host-tier telemetry is one trust domain, not
  per-session evidence.
- Only the gateway sets `compass.session.id`, `compass.telemetry.origin` and
  the agent's `service.name` on relayed telemetry.
- Every endpoint passes `otlpenv.ParseEndpoint` (CA1): plaintext only to
  loopback.
- Server and runner emit or relay only with an endpoint, and drop inherited
  `OTEL_*`. `DefaultCollectorImage` stays 0.140.0.
- Breaking (`!`): `--otel-external` takes a URL; env-file agent endpoints are
  ignored; compass-runner refuses per-signal and TLS `OTEL_EXPORTER_OTLP_*`
  keys.

## Approach

**C-a: the collector exports (CA1).** `collectorConfigYAML`
(`go/internal/stack/collector_container.go`) hard-codes `nop: {}`. With
`--otel-export-endpoint` (fallback `COMPASS_OTEL_EXPORT_ENDPOINT`) it renders
`otlphttp` behind `memory_limiter` (`check_interval: 1s`, 256 MiB, 64 MiB
spike) and an 8 MiB receiver cap. `--otel-export-headers-file` is copied 0600
to `/run/compass/otlp-headers.yaml` on every `Start`.

**C-b, host collector (CB2).** The Runner relays agent OTLP to the endpoint its
own SDK reads, `OTEL_EXPORTER_OTLP_ENDPOINT` plus `_HEADERS` (`setupOtel`,
`go/cmd/compass-runner/main.go`). `compass-stack` sets it on server and runner;
any two of these flags is a usage error:

- `--otel-export-endpoint`: the bundled collector,
  `http://127.0.0.1:<CollectorHTTPPort>`. Without it nothing is set (D3).
- `--otel-external <URL>`: the operator's host collector, which owns batching,
  retry and upstream (ruling 1); `--otel-export-headers-file` rides it in `Env`.
- `--otel-off`: nothing set, overriding `COMPASS_OTEL_EXPORT_ENDPOINT`.

A remote runner host's operator runs the collector (e.g. Grafana Alloy).
compass-runner refuses a bad endpoint and per-signal or TLS keys. The relay
appends `/v1/<signal>` by `path.Join`, as the Go SDK's `otlpconfig` does.

**C-b, gateway handlers (CB1).** `gateway.Serve`
(`go/internal/runner/gateway/gateway.go`) mounts `POST /v1/traces`,
`/v1/metrics` and `/v1/logs` only when `Deps.OTLPRelay` is set. Each handler:

1. Resolves `g.sessions.Session(g.containerName)`. Unbound is 503 with
   `Retry-After: 1`: `agentHost.Start` starts the agent before recording its
   session.
2. Answers 415 unless `application/x-protobuf` with identity encoding.
3. Takes one of 8 per-Gateway slots (a flush exports five signals) and 4 MiB
   of a Runner-wide 128 MiB budget, else 503 with `Retry-After: 1`, unread.
   One 8 s deadline (as `withBodyReadDeadline`, `go/server/network_door.go`)
   covers read and upstream.
4. Reads at most 4 MiB (413). An allocation-free `protowire` pre-scan counts
   messages and repeated-scalar elements, packed or unpacked; over 65,536
   messages or depth 32 is 413. It reserves 416 B per message, 16 B per
   element (append doubling) and twice the body, else 503, then decodes with
   `go.opentelemetry.io/proto/otlp` (direct; 400 if malformed). 4 MiB of empty
   attributes measured 244 MiB decoded.
5. Deletes `compass.session.id` from every scope, span, event, link, data
   point, exemplar and log record. On each resource it sets
   `compass.session.id` to the bound id, `compass.telemetry.origin=agent` and
   `service.name=compass-agent`.
6. Re-encodes (over 8 MiB is 413) and POSTs under `r.Context()`. Upstream 2xx
   is 200; 429, 502, 503 or 504 is 503 with its `Retry-After`; 413 stays;
   another 4xx is 400; else 502.

**C-b, agent (CB3).** When relaying, the Runner sets the reserved
`COMPASS_AGENT_OTLP=1`. After the env-file merge the agent deletes and logs
every `OTEL_EXPORTER_OTLP_*` key, and records the SDK session id as
`compass.agent.sdk_session.id`.

- `makeOtelLayer` (`packages/compass-agent/src/transport/otel-layer.ts`) builds
  exporters with `url: "http://compass-agent/v1/<signal>"` and
  `httpAgentOptions: { socketPath }`.
- The SDK loop's exporters (`registerProviders`, `@oh-my-pi/pi-coding-agent`)
  read env at construction. A `Bun.serve` proxy on `127.0.0.1:0` checks a
  per-process token header, caps bodies at 4 MiB and forwards with
  `fetch(…, { unix, signal: req.signal })`. Endpoint and token are set only
  across `await telemetryHooks.init()`; a subprocess spawned during its
  dynamic import gains only what its socket already gives. Egress accepts
  `oif lo` (`NftScript`).

| Tier | Socket reaches the agent by | OTLP |
| --- | --- | --- |
| podman | bind mount at `/run/compass/agent.sock` | covered hermetically |
| microVM | guestd `gatewayProxy` splice to vsock | covered |
| host | `COMPASS_AGENT_SOCKET_PATH` | covered; one trust domain |
| apple-container | `--volume` over virtiofs | none: DL-338's slow-roll holds the agent channel |

**C-c: dev wiring (CC1).** devenv runs the same collector config.

## Alternatives considered

- **Pasta forward** (earlier draft): podman only, agent-asserted attribution;
  RIG-5249 becomes moot.
- **Direct export through the egress allowlist**: credentials in every agent,
  forgeable attribution.

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
- `otlpenv.ParseEndpoint(raw string) (*url.URL, error)`;
  `otlpenv.SignalURL(base *url.URL, signal string) *url.URL`.

Red→green:

- `TestCollectorConfigYAMLExportsUpstream` (red today): `otlphttp` and
  `memory_limiter` with `check_interval` in all three pipelines, an 8388608
  body cap, no `nop`.
- `TestCollectorConfigYAMLRealizesD3Posture`: container ports, empty `Export`
  and `HeadersPath`, today's bytes.
- `TestParseEndpoint`: `http` only to `127.0.0.1` or `localhost`, `https`
  anywhere; refuses bare `host:port`, `127.0.0.2`, `[::1]`, `0.0.0.0`,
  userinfo, query, fragment.
- `TestSignalURL`: `https://h/otlp` gives `/otlp/v1/traces`.
- `TestCollectorStartReplacesHeadersCopy`; `TestCollectorRunArgsExportContract`
  (no header value in argv or config).
- Smoke: `otelcol validate` on the pinned image; `compass-stack up` delivers a
  span and the header to a local receiver.

### CB1: gateway OTLP relay

Files: `go/internal/runner/gateway/{gateway,otlp}.go`,
`go/internal/otel/{otlprelay,otlpscan}`, `go/go.mod`.

Interfaces:

- `type OTLPRelay interface { Export(ctx context.Context, path string, body []byte) error }`;
  `Deps.OTLPRelay` (nil leaves the paths unmounted);
  `Deps.OTLPBudget *semaphore.Weighted` (`golang.org/x/sync`).
- `otlprelay.New(endpoint *url.URL, headers map[string]string) *Relay`; a
  non-2xx upstream returns `*otlprelay.StatusError{Code int, RetryAfter string}`.
- `otlpscan.Bound(body []byte, signal string) (uint64, error)`.
- `const maxOTLPBodyBytes = 4 << 20`, `maxOTLPInFlight = 8`,
  `maxOTLPMessages = 1 << 16`, `otlpRequestDeadline = 8 * time.Second`.
- Counter `compass.runner.otlp_relay.requests{signal,outcome}`, nil-guarded,
  no session label.

Red→green (hermetic, fake relay):

- `TestOTLPStampOverridesAgentSession`: forged ids on a resource, span, data
  point and log record, and `service.name=compass-server`, arrive as the bound
  id and `compass-agent`.
- `TestOTLPUnboundContainerRetryable` (503); `TestOTLPBodyCap` (413);
  `TestOTLPRejectsCompressedBody` (415).
- `TestOTLPSaturationShedsWith503`: a ninth request or empty budget, unread.
- `TestOTLPDecodeBound`: 70,000 empty attributes is 413; `otlpscan.Bound`
  covers measured allocation, including two million unpacked `BucketCounts`.
- `TestOTLPRelayFailure`: 400 stays 400, 503 keeps `Retry-After`, transport
  error is 502.
- `TestOTLPUnmountedWhenRelayNil` (404); `TestRelayPostsWithHeaders`.
- microVM e2e `TestVsockGateway_InGuestOTLPStamped` against a fake
  `AgentHostConfig.OTLPRelay`.

### CB2: runner and stack wiring

Files: `go/cmd/compass-runner/main.go`, `go/internal/runner/{host,agent_exec}.go`,
`go/internal/runtime/host_backend.go`, `go/internal/stack/{config,spec}.go`,
`go/internal/stack/adapters/process.go`, `go/cmd/compass-stack/main.go`.

Interfaces:

- `AgentHostConfig.OTLPRelay gateway.OTLPRelay`; `(*agentHost).gatewayDeps()
  gateway.Deps` replaces both `host.go` literals, sharing one budget.
- `AgentEnv.Telemetry bool` sets `COMPASS_AGENT_OTLP=1`.
- `otlpenv.HeadersValue(map[string]string) (string, error)`;
  `otlpenv.RunnerConfig(environ []string) (*url.URL, map[string]string, error)`.
- `Config.OTLPOff bool`; `ExternalOTLPEndpoint` becomes a URL;
  `childEnv(inherited, overlay []string) []string` and `envSlice` drop
  inherited `OTEL_*`.

Red→green:

- `TestServerAndRunnerSpecOTLPEndpoint`: each mode; headers in `Env`, never
  `Args`.
- `TestStartDropsInheritedOTELKeys`, `TestEnvSliceDropsInheritedOTELKeys`,
  `TestExecSpecTelemetryFlag`.
- `TestResolveConfigOTelModes`: two OTel flags are refused; `--otel-off` beats
  the env fallback.
- `TestRunnerConfig`: refuses a bad endpoint and per-signal or certificate keys.
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
  tokenless request is refused; a client abort cancels the forward.
- A child spawned after init sees no `OTEL_EXPORTER_OTLP_*`.
- `scrubOtlpEnv` drops and logs an env-file endpoint.
- Migration to the flag: `cli.test.ts`'s env-file endpoint, "all four
  loop-OTel env keys…", "endpoint set ⇒ env defaults…" and "deployer-set
  OTEL_SERVICE_NAME…"; `otel-layer.test.ts`'s three endpoint tests.

### CC1: devenv runs the collector

Interfaces: `compass-stack collector-config --grpc --http --health
[--otel-export-endpoint] [--otel-export-headers-file]` prints
`CollectorConfigYAML`; `usageError` names five subcommands.

devenv `processes.otel-collector` runs `otelcol-contrib` when the gitignored
`devenv.local.nix` sets `env.COMPASS_OTEL_EXPORT_ENDPOINT`.

Red→green: `TestRunCollectorConfig`; `devenv up` with an endpoint delivers an
agent span upstream.

### CD1: operator docs

Interfaces: none. `docs/self-host.md` gains an observability section (flags,
remote-runner `OTEL_EXPORTER_OTLP_*`); each deployer contract points there.

Red→green: none; rumdl and the ledger gate pass.

## Tasks

- [ ] CA1: collector export
- [ ] CB1: gateway relay
- [ ] CB2: runner and stack wiring
- [ ] CB3: agent socket exporters
- [ ] CC1: devenv collector
- [ ] CD1: operator docs

## Resolved decisions

1. **SDK-loop exporters use the token proxy, not an SDK patch:** no fork.
   Agent-owned providers would miss the SDK's metric and log hooks, which
   only its dynamically imported, unexported `telemetry-export-otlp` registers.
2. **`/v1/logs` is mounted with the same stamp:** env-file endpoints enable SDK
   logs (`resolveSignalConfig`).
3. **The remote host collector is operator-run;** a runner-only mode is additive.
