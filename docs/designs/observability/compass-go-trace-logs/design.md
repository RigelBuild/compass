# Design: trace context in every Go context and every log line (RIG-5186)

Sibling record of the T4b emission record
([`../compass-server-runner-otel/design.md`](../compass-server-runner-otel/design.md)),
which built `go/internal/otel`. This record does not edit it. It adds the log
half that T4b left out.

Ledger-impact: none — internal logging, lint config, and one additive
internal-only proto field (`SessionsResponse.traceparent`, the internal
runner wire). No public wire or decision-ledger row changes.

## Problem / Intent

compass-server and compass-runner emit OTel spans, but no log line carries a
trace id, so a log line cannot be joined to its trace. Many log calls also drop
the caller's `ctx`, so even a trace-aware handler could not see the span. Matt
asked, on the `noctx` PR (#2167): "Can we also require OTel
traceparent/tracing/spans through all of the contexts? and make sure those are
always part of the logs?" This record makes three things true: every request
`ctx` in the two binaries carries a span, every log call passes its `ctx`, and
the log handler writes `trace_id` and `span_id` whenever that `ctx` has a valid
span.

## Approach

Three legs. Each leg is needed. A handler with no span sees nothing. A span
with a ctx-less log call is invisible. A ctx-carrying call with no handler
writes no ids.

### Leg 1 — a trace-aware `slog.Handler` in `go/internal/otel`

`otelx.TraceHandler` wraps the existing text handler. In `Handle` it reads
`trace.SpanContextFromContext(ctx)`. When the span context is valid, it
adds `trace_id` and `span_id` to the record. Otherwise it changes nothing.
`WithAttrs` and `WithGroup` re-wrap, so a logger derived with `With` still
stamps. compass-server and compass-runner install it at their one
`slog.SetDefault` call (`go/cmd/compass-server/main.go`,
`go/cmd/compass-runner/main.go`; today both are
`slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))`).

T1 fixes the interface shown in its Plan entry. A review of that interface
found no flaw that blocks it. It found three gaps, and T1 closes them:

- **Missing cases in the tests.** The base tests cover a sampled SDK span, a
  derived `WithAttrs` logger, a group, and a bare ctx. They do not cover the
  provider-off path. With no provider installed, the global tracer returns a
  `nonRecordingSpan` that wraps the parent's span context
  (`go.opentelemetry.io/otel/internal/global` `tracer.newSpan`:
  `s := nonRecordingSpan{sc: trace.SpanContextFromContext(ctx), tracer: t}`).
  With no parent, that context is invalid, so no ids are written. T1 pins
  this, because it is the default runtime state. The base tests also do not
  cover an unsampled span. The SDK makes ids before it samples
  (`sdk/trace` `tracer.newSpan` calls `idGenerator.NewIDs`), so an unsampled
  span still stamps. T1 pins that too, so adding a sampler later cannot
  silently strip the ids.
- **Reserved keys are not enforced.** `trace_id`/`span_id` are reserved, but
  nothing rejects a caller that passes them. No non-test call does today. T1
  adds them to sloglint `forbidden-keys`. That gate can fail.
- **`WithGroup` nests the ids.** After `WithGroup("request")` the ids land
  at `request.trace_id`, and a query on `trace_id` misses them. This follows
  the `slog.Handler` contract, and a T1 test pins it. No non-test
  `WithGroup` or `slog.Group` call exists, so the gap is latent. See Open
  Question 4.

### Leg 2 — a span on every request ctx

"All contexts" in this record means every ctx that does work for one
request or one unit of work. Matt's ask is about joining logs to traces. A
ctx with no request behind it has no trace to join.

| ctx class | Span today | This record |
| --- | --- | --- |
| Unary inbound Connect RPC, server (all services, all doors) | yes, except SecretsService on the socket and dev doors — `buildDoors` (`go/server/serve.go`) mounts it with only `auth.AmbientIdentity(adminID), auth.AmbientStreamInterceptor(adminID)` (dev door also `auth.NewAdminGate`). Every other mount lists `otelIC` first by hand. | T2 routes every server mount through one helper, `otelx.HandlerInterceptors`, that always puts `otelIC` first |
| Long-lived inbound streams: RunnerService `Sessions` and `PublishEvents`; `SubscribeEvents`, `SubscribeAgentSession`, `SubscribeComms`; runner AgentGateway `Publish` and `Control` (after T3) | one `otelconnect` span for the stream's whole life. `WrapStreamingHandler` in `connectrpc.com/otelconnect` v0.9.0 starts it and ends it only when the handler returns. | keep that span for RPC metrics; strip it from the work ctx at handler entry; per-unit spans or restores where a unit carries a parent (Open Question 1). T2 (server), T3 (AgentGateway), T8 (runnerhub). |
| Short server streams `FetchAgentConfig`, `FetchSessionBlobs` | per call; ends with the fetch | no change |
| Inbound HTTP: GitHub webhook, Linear webhook, Linear session link | **no** — plain `netMux.Handle` in `buildNetworkServer` (`go/server/network_door.go`) | T2 wraps with `otelhttp` |
| Inbound Connect RPC, runner AgentGateway | **no** — `agentGatewayHandlerOptions` (`go/internal/runner/gateway/ack_bound_codec.go`) sets only the read limit and codecs | T3 |
| Queued unit from an HTTP ingress: GitHub webhook → `ingest.NotifyWebhookArm` and `ingest.BoardWebhookArm` queues; Linear webhook → `linearagent.Dispatcher` queue | **no** — `Enqueue(_ context.Context, ev forge.ForgeEvent)` drops the ctx; `Dispatcher.Enqueue(ev *SessionEvent) error` takes none; the work runs on the arm's loop ctx | T5 class (b′): the queue entry carries the enqueuer's traceparent |
| Queued unit, shipped: fabric delivery, held deliver | yes — the fabric restores the publisher's traceparent (`go/internal/fabric/event_fabric.go`); `heldEntry` carries `traceparent: otelx.Traceparent(ctx)` (`go/internal/delivery/dispatch.go`) | no change; never re-rooted |
| Runner command from the Sessions stream | **no** — `ServerLink.RunSessions` passes the caller's ctx; `otelconnect` `WrapStreamingClient` starts its span on an internal ctx, so `dispatcher.handle`/`execute` see no span | T4 |
| Lifetime work spawned by a runner command: stderr drains and reaper in `ServerLink.StartAgent`, `agentHost.retireOnExit` goroutines | inherits the Start command's ctx | T5 class (c): strip the span at the spawn |
| Stored lifetime ctx: `Gateway.baseCtx`, set by `gateway.Serve` from the Provision command's ctx; the shared `PublishEvents` client stream opens on it | inherits the command ctx for the socket's life | T5 class (c): strip in `Serve` |
| Outbound runner→server calls | yes — `otelconnect` client interceptor in `runner.Dial` | no change |
| Background unit of work (delivery sweeps) | yes — `delivery.sweep.*` roots in `go/internal/delivery/settle.go` | no change |
| Other background goroutines (40 sites, T5 census) | mixed | T5 classifies each one |
| Boot, shutdown, loop start/stop | no | no change — these lines carry no ids by design |

Rules for a ctx with no span:

- The handler writes the line without ids. It never mints a span to have
  something to log.
- A span starts only where a request or unit of work starts. A long-lived
  loop or stream never gives its work one span for its whole life. That span
  ends and exports only when the loop ends (never, on a crash), and every
  line in the loop would share one id.
- A stream handler keeps the `otelconnect` stream span for metrics, but
  clears it from its work ctx at entry with `otelx.WithoutSpan` (T2). This is
  the idiom the fabric already uses:
  `trace.ContextWithSpanContext(context.WithoutCancel(ctx), trace.SpanContext{})`
  then `otelx.ContextWithTraceparent` (`go/internal/fabric/event_fabric.go`).
- A detached goroutine that continues a request takes the request ctx, or
  `context.WithoutCancel(ctx)`. Both keep the span, because `WithoutCancel`
  keeps values. A goroutine that outlives the request (class (c)) clears the
  span. Never `context.Background()` mid-tree (the go-thread-context rule).
- `trace.WithNewRoot()` is only for a unit with no carried parent. It is
  never used on a path that restores a carried traceparent: the fabric,
  held deliver, and the (b′) queues.

Runner command spans (T4). The Sessions stream is one long-lived stream, so
per-message headers cannot carry the server's trace. T4b solved the same
problem for control ops by putting `traceparent` in the payload (DL-303). T4
adds the same field one level up, on the `SessionsResponse` envelope, so every
command can carry it, not only `deliver_control`. The server stamps it from
the ctx it holds when it builds the command. The runner starts its command
span as a child of it.

For `deliver_control` the op's own traceparent wins (`SteerControl` field 3,
`DeliverControl` field 4, in `agent.proto`). The two values differ. The server
stamps the op before `gatedDispatch` starts `delivery.dispatch`, so the op
holds the publisher's span id and the envelope holds the `delivery.dispatch`
span id. With the op as parent, the runner hop and the agent turn hang off the
same span. This is T4b's §Hop spans step 4 ("parented off the op's
`traceparent`"), so T4 implements that step and does not supersede it. The
envelope is the parent only for arms with no op traceparent, or an empty one.
That step is not in the code today: no file under `go/internal/runner` reads a
traceparent, and the `DeliverControl` arm of `dispatcher.execute` calls
`d.host.Deliver` with no span. The op itself is still relayed verbatim to the
agent.

### Leg 3 — every log call passes its ctx, enforced by sloglint

golangci-lint v2.14.0 pins sloglint v0.12.0. Its settings struct
(`SloglintSettings` in `pkg/config/linters_settings.go`) has
a `Context` field with tag `mapstructure:"context"`, and sloglint's `contextOnly`
accepts `all` and `scope`. With `context: all`, any `Debug`/`Info`/`Warn`/
`Error` call on `slog` or a `*slog.Logger` is a finding. `scope` flags a call
only when the enclosing function has a `context.Context` or
`*http.Request` parameter.

Measured on `bd9a75d2`, default build tags:

- `context: all`: 140 findings in 35 files. 2 are in
  `internal/envelope/envelope_test.go`, so there are **138 non-test sites in
  34 files**.
- `context: scope`: 76 findings. These are mechanical: `slog.Info("m")`
  becomes `slog.InfoContext(ctx, "m")`. The other **62 have no ctx in
  scope**. Each one
  needs ctx threaded in as the first parameter of its function.

| Package | `all` | `scope` |
| --- | ---: | ---: |
| `internal/runner` (incl. `gateway`) | 36 | 22 |
| `server` | 25 | 20 |
| `internal/runnerhub` | 19 | 9 |
| `internal/fabric` | 9 | 0 |
| `cmd/compass-postgres` | 8 | 7 |
| `cmd/compass-mint-runner-token` | 6 | 5 |
| `internal/stack` | 5 | 2 |
| `internal/runtime` | 5 | 1 |
| `internal/ingest`, `internal/embedded`, `internal/appconfig`, `cmd/compass-server`, `cmd/compass-gen-cert` | 3 each | 3, 2, 0, 0, 0 |
| `internal/guestd`, `internal/forge`, `cmd/compass-runner` | 2 each | 2, 2, 1 |
| `internal/delivery`, `internal/comms`, `cmd/compass-stack`, `cmd/compass-app` | 1 each | 0 |

Lint also runs a second pass with tags `pgtest,podman,microvm,livegithub,
keyring_integration` (`go/moon.yml`), and these counts do not include it.
Each migration task re-counts under both passes.

`context: all` is repo-wide, so it also covers binaries that do not install
the handler. That is correct: the gate is about ctx threading, and a ctx costs
nothing there. See Open Question 5.

sloglint checks only the method name. Its `contextOnly` matches
`case "Debug", "Info", "Warn", "Error":` and never looks at the argument
(`go-simpler.org/sloglint` v0.12.0 `function_checks.go`). So
`slog.InfoContext(context.Background(), …)` passes it. A forbidigo pattern
closes that gap: `forbidigo` matches the `context.Background` and
`context.TODO` identifiers, outside `_test.go` files and `cmd/*/main.go` (T1).
A pattern on the call text `Context(context.Background()` would not work,
because forbidigo matches identifiers, not argument lists. At `bd9a75d2` the
pattern hits two sites: `pgshare.IsolatedSchemaDSN`
(`go/internal/pgshare/pgshare.go`, a test helper in a non-test file) and the
shutdown `drainTimeout` ctx in `go/server/serve.go`.

No non-test file imports the stdlib `log` package. Third-party libraries'
own loggers are out of scope.

### Cost and sampling

- `Handle` adds one ctx value lookup and, under a valid span, two hex
  encodings and a `Record.Clone`. That is small next to the text formatting
  it already does.
- New spans: per AgentGateway unary RPC and per AgentGateway stream (T3), per
  webhook delivery (T2), per runner command (T4), per dequeued ingest unit
  (T5). There are no per-frame spans on `PublishEvents` or `Publish`: those
  carry agent frames at a high rate, and their lines carry no ids. otelconnect
  also adds per-message events on streams, so T3 sets
  `otelconnect.WithoutTraceEvents()` on the AgentGateway.
- `SetupTracerProvider` sets no sampler, so the SDK default
  `ParentBased(AlwaysSample)` applies. No change.

### Alternatives considered

The library-adoption threshold used here: at least 1000 known importers on
pkg.go.dev, and a release in the last year.

- **`otelslog` bridge (`go.opentelemetry.io/contrib/bridges/otelslog`)
  instead of a wrapper.** It sends records over OTLP as log records with the
  trace ids set. Its latest release is v0.21.0 (2026-10-02), but pkg.go.dev
  lists only 271 known importers, so it is below the threshold. It also adds
  a logs pipeline, which Matt did not ask for. "Part of the logs" means the
  ids are in the lines we already write. Rejected. Log export is Open
  Question 3.
- **sloglint `context: scope` instead of `all`.** It only flags the 76 calls
  that have a ctx in hand, and it never fires on the 62 that need ctx
  threaded in, so a new ctx-less helper would pass. It does not meet
  "require". Rejected.
- **A root span for any ctx that has none, minted inside the handler.**
  That gives every line an id, but the ids join to nothing: a one-line trace
  has no other spans. It also hides real gaps. Rejected.
- **Hand-rolled HTTP span middleware instead of `otelhttp`.** About 15 lines
  in `go/internal/otel`, no new dependency. `otelhttp` has 2,776 known
  importers on pkg.go.dev, and v0.71.0 was released 2026-08-26. It writes
  standard HTTP semconv attributes and metrics. v0.71.0 requires
  `go.opentelemetry.io/otel v1.46.0`, the version pinned in `go/go.mod`, so
  its version moves with that pin. Chosen: `otelhttp`. Open Question 6.

## Global Constraints

- **All-Go, one convention.** All span and log-trace code lives in
  `go/internal/otel` or uses it. No second provider, exporter, or handler
  type.
- **Import names.** In this record `otelx.` is the internal package
  `github.com/RigelBuild/compass/go/internal/otel` (Go package name `otel`),
  imported as `otelx` wherever the upstream package is also in scope, as
  `go/internal/delivery` already does. `otel.` is upstream
  `go.opentelemetry.io/otel` (for example `otel.Tracer`).
- **Off by default, per process.** A process with an unset
  `OTEL_EXPORTER_OTLP_ENDPOINT` installs no provider and sends nothing. Its
  lines carry ids only when its ctx holds a valid remote span context from an
  inbound `traceparent`, for example a runner command whose envelope carries
  the server's. Then a line carries the remote trace id and the remote parent
  span id, because the provider-off global tracer wraps the parent's span
  context. With no inbound traceparent, a line carries no ids.
- **Never fail or block on trace machinery.** A missing or malformed
  `traceparent` gives a fresh root span. It never gives a command error. The
  handler never returns an error of its own.
- **ctx threading (go-thread-context).** ctx is the first parameter. No
  `context.Background()`/`TODO()` outside the top of `main` and `_test.go`;
  forbidigo enforces this from T1. Detached work uses
  `context.WithoutCancel(ctx)`.
- **Reserved log keys.** `trace_id` and `span_id` are written only by
  `otelx.TraceHandler`. Lowercase hex, as `TraceID.String()` and
  `SpanID.String()` write them.
- **Span lifetime.** One span per request or unit of work. A long-lived
  stream or loop never lends its span to its work: stream handlers clear it
  with `otelx.WithoutSpan`, and class (c) spawns clear it at the spawn.
  `trace.WithNewRoot()` only where no parent is carried.
- **One interceptor helper.** Every Connect handler mount uses
  `otelx.HandlerInterceptors`. A raw `connect.WithInterceptors` on a handler
  mount is a review reject.
- **Span attributes stay bounded.** Command kind, op kind, and session id
  only, as T4b already uses. Never a message body or a secret. Metric labels
  stay op-kind only (T4b constraint).
- **Proto changes are additive and internal-only.** `runner.proto` tag 16,
  buf-breaking clean, regenerate with `moon run compass-proto:gen`.
- **Lint posture (F1).** No central exclusion for sloglint. A per-site
  suppression is `//nolint:sloglint // <reason>` (nolintlint
  `require-explanation`, `require-specific`).
- **No inert gate.** `context: all` turns on in the same PR as the last
  migration, never before and never behind a flag.
- **Record hygiene.** Markdownlint-clean. Each task is one stacked PR in a
  linear stack.

## Plan

Stack order: T1, T2, T3, T6, T5, T4, T7, T8, T9, in one linear stack.

- The handler goes live in T2, the same PR that clears the stream span from
  server stream handlers. No build has the handler stamping a stream span.
- T6 lands before T5 and T4. Rule: every log line that a T2–T5 test reads
  already passes its ctx at the commit where the test lands. At
  `bd9a75d2` the runner lines T5 pins (`logDrainLine`, `flushDropped`,
  `logUnexpectedExit` in `go/internal/runner/agent_exec.go`) take no ctx. T6
  is safe this early: before T4, no runner ctx carries a span except the
  AgentGateway unary requests from T3.
- T5 lands before T4. T4 puts a span on every command ctx, and T5's class
  (c) strips must already keep that span out of lifetime work.

### T1 — `otelx.TraceHandler` and the lint gates

Owner: compass-ux.

Add `go/internal/otel/loghandler.go` with the interface below, and its
tests. The tests cover seven cases:

- the four base cases: a sampled SDK span, a derived `WithAttrs` logger, a
  `WithGroup` logger (the ids sit inside the group), and a bare ctx;
- provider-off with no parent: no ids;
- provider-off with a remote parent from `otelx.ContextWithTraceparent`: the
  remote trace id and parent span id;
- unsampled (`sdktrace.WithSampler(sdktrace.NeverSample())`): both ids.

Add `forbidden-keys: [trace_id, span_id]` to the `sloglint` block in
`go/.golangci.yml`. sloglint v0.12.0 checks every `log/slog.String` key
against `forbidden-keys`, so the handler's own `slog.String("trace_id", …)` and
`slog.String("span_id", …)` will be flagged. Each carries
`//nolint:sloglint // TraceHandler is the one writer of the reserved trace keys`.

Add the forbidigo pattern `^context\.(Background|TODO)$`. Its message cites
go-thread-context. Add an exclusion rule scoped to forbidigo, that text, and
path `(_test\.go|cmd/[^/]+/main\.go)$`. Fix the two sites it hits:

- `pgshare.IsolatedSchemaDSN` (`go/internal/pgshare/pgshare.go`). Use
  `ctx := t.Context()` for the create path only. `t.Context()` is canceled
  before Cleanup functions run, so the `t.Cleanup` closure must use
  `cctx := context.WithoutCancel(ctx)` for its `pgx.Connect` and
  `DROP SCHEMA`. Today a failed cleanup connect returns silently
  (`return // best-effort teardown`), so without this every pgtest would leak
  its schema and nothing would fail.
- `drainDoors` (`go/server/serve.go`). It takes a ctx and derives its bound
  from `context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)`. Its
  three callers change: the production caller in `go/server/serve.go`,
  `go/server/drain_test.go`, and `go/server/serve_seed_pgtest_test.go`. The
  production caller's `//nolint:contextcheck // drainDoors deliberately takes
  no ctx …` is removed. nolintlint runs with `allow-unused: false`, so a stale
  directive would be a finding.

T1 does not change the two mains. T2 wires the handler.

Interfaces:

- Consumes: `log/slog`, `go.opentelemetry.io/otel/trace`.
- Produces (package `otel`, import path
  `github.com/RigelBuild/compass/go/internal/otel`, written `otelx.` here):

  ```go
  // TraceHandler adds trace_id and span_id from the record's ctx to every
  // log line written inside a valid span.
  type TraceHandler struct{ inner slog.Handler }

  func NewTraceHandler(inner slog.Handler) *TraceHandler
  // Enabled defers to inner.
  func (h *TraceHandler) Enabled(ctx context.Context, level slog.Level) bool
  // Handle clones r and adds the two ids when ctx holds a valid span
  // context, then calls inner.
  func (h *TraceHandler) Handle(ctx context.Context, r slog.Record) error
  // WithAttrs and WithGroup re-wrap the derived inner handler.
  func (h *TraceHandler) WithAttrs(attrs []slog.Attr) slog.Handler
  func (h *TraceHandler) WithGroup(name string) slog.Handler
  ```

- Changes (package `server`, unexported):

  ```go
  func drainDoors(ctx context.Context, d drainSet) error
  ```

Test cycle:

- `TestTraceHandlerStampsActiveSpan` fails before, because `NewTraceHandler`
  does not exist. After, all seven cases pass.
- An `IsolatedSchemaDSN` pgtest runs a subtest that calls the helper. After
  the subtest ends and its cleanup has run, the parent queries
  `information_schema.schemata` for that schema and asserts zero rows. If
  only `t.Context()` were used, the schema would remain and this would fail.
- The lint gates have no fixture harness. They are proven by the gate itself,
  in the PR. First commit: the `forbidden-keys` and forbidigo config, with no
  nolint and no fixes. Its red `compass-go:lint` output names the two
  handler `slog.String` sites and the two `context.Background()` sites. The
  PR body quotes that output. The next commit adds the nolints and the fixes,
  and lint goes green.

Acceptance: the handler stamps exactly when ctx holds a valid span context.
The linter rejects a caller-written `trace_id` and a mid-tree
`context.Background()`. pgtest schemas are dropped.

### T2 — Server: one interceptor helper, stream span strip, HTTP ingress, wiring

Owner: compass-ux.

- **Helper.** Add `otelx.HandlerInterceptors`. It always puts `ic` first, then
  `NewTraceResponseInterceptor()`, then the caller's interceptors in order.
  Every server handler mount uses it:
  - `buildDoors` (`go/server/serve.go`): socket and dev doors, all four
    services. This closes the SecretsService gap.
  - `buildNetworkServer` (`go/server/network_door.go`).
  - `mountGatewayServices` (`go/server/network_door.go`).
  - `runnerhub.NewMountedHandler` (`go/internal/runnerhub/handler.go`).

  The auth order stays as it is. For example the network door becomes
  `otelx.HandlerInterceptors(otelIC, otelx.NewSessionIDInterceptor(),
  auth.BearerInterceptor(st), auth.BearerStreamInterceptor(st),
  auth.NewAdminGate(adminID))`.
- **Stream strip.** Add `otelx.WithoutSpan`. The long-lived stream handlers
  call `ctx = otelx.WithoutSpan(ctx)` on entry, so their work ctx carries no
  span. The `otelconnect` stream span stays, so stream RPC metrics are kept.
  The handlers are:
  - `service.SubscribeEvents` and `service.SubscribeAgentSession`
    (`go/server/service.go`);
  - `Comms.SubscribeComms` (`go/internal/comms/subscribe.go`);
  - `Handler.Sessions` and `Handler.PublishEvents`
    (`go/internal/runnerhub/handler.go`).

  A publish under the stripped ctx stamps an empty traceparent
  (`Bus.PublishCtx` stamps `otelx.Traceparent(ctx)`, `go/events/events.go`). That
  is the shipped "empty when no active span" rule. It is not a new path.
- **HTTP ingress.** Wrap the GitHub webhook, Linear webhook, and Linear
  session-link handlers in `tracedIngress`. It calls `otelhttp.NewHandler`
  with `WithPublicEndpointFn` always returning true. These endpoints face the
  internet, so an inbound `traceparent` becomes a link, never a parent. Add
  `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0` to
  `go/go.mod`. Its version is tied to the otel v1.46.0 pin.
- **Wiring.** Set
  `slog.SetDefault(slog.New(otelx.NewTraceHandler(slog.NewTextHandler(os.Stderr, nil))))`
  in `go/cmd/compass-server/main.go` and `go/cmd/compass-runner/main.go`.

Interfaces:

- Consumes: `connectrpc.com/otelconnect` v0.9.0, `otelhttp` v0.71.0.
- Produces (package `otel`, written `otelx.`):

  ```go
  // HandlerInterceptors is the one interceptor option for a Connect handler
  // mount: ic first (it must be non-nil), then NewTraceResponseInterceptor,
  // then rest in order.
  func HandlerInterceptors(ic *otelconnect.Interceptor, rest ...connect.Interceptor) connect.HandlerOption

  // WithoutSpan returns ctx with no active span. It keeps ctx's values and
  // cancellation.
  func WithoutSpan(ctx context.Context) context.Context
  ```

- Produces (package `server`, unexported):

  ```go
  // tracedIngress wraps an internet-facing HTTP handler in a server span that
  // links, never parents, any inbound traceparent.
  func tracedIngress(h http.Handler, operation string) http.Handler
  ```

Test cycle:

- **Default lane** (`go test -race ./...`, moon `compass-go:test`):
  - A unit test of `HandlerInterceptors` asserts the order (span, then
    traceresponse, then `rest`).
  - A `WithoutSpan` test asserts the span context is invalid and a ctx value
    survives.
  - A `tracedIngress` test sends a request with an inbound `traceparent`. It
    asserts one server span with no parent and one link.
- **Proof over the doors** (build tag `pgtest`, CI step
  `go test -tags pgtest -race` in `.github/workflows/ci.yml`; `buildDoors`
  needs a store). The test extends the `cors_pgtest_test.go` harness. It sends
  one unary RPC per service per door, with an in-memory span recorder. An
  auth rejection is fine, because the span interceptor is outermost. It
  asserts one `SpanKindServer` span per call. The services are:
  - socket and dev doors: CompassService, CommsService, SecretsService,
    UsageService;
  - network door: those four, plus RunnerService, GatewayCredentials,
    GatewayRegistry, GatewayAuth.

  AgentGateway (runner, T3) and GuestControl (guest binary) are not server
  mounts. Before T2: the socket and dev SecretsService rows fail.
- **Stream strip proof.** Each test reads the ctx at a seam that already
  receives the handler's ctx. Each fails without the strip, because the seam
  then sees the stream span.
  - Default lane, `go/internal/runnerhub`. Mount `Handler` with
    `otelx.HandlerInterceptors` and a span recorder. Give the hub a fake
    `SessionBindingStore` that records the ctx of each call.
    - `PublishEvents`: an enrolled runner sends one session frame for a
      session id the hub has not cached. `Hub.Deliver` reaches
      `ResolveSessionBinding(ctx, …)` through `frameSession` and
      `lookupSessionBinding`. The recorded ctx has an invalid span context.
    - `Sessions`: answer a deliver with NotFound, as
      `lost_session_test.go` does. The `setSessionUnknown` hook calls
      `dropLostSessionDetached(ctx, …)`, which reaches
      `SessionBindingTenant`. The recorded ctx has an invalid span context.
    - The recorder holds one stream span per RPC after close.
  - pgtest lane. Open the store with `store.OpenTraced` and a
    `pgx.QueryTracer` that records the ctx of each query, keyed by sqlc
    query name as `pgtest.SQLCQueryCounter` does. Queries from the auth
    interceptors run before handler entry and still carry the stream span,
    so the test reads only the handler body's queries: the subscriber check
    in `SubscribeAgentSession` and `MessagesHeadSeq` in `SubscribeComms`.
  - `SubscribeEvents` has no seam. Its body passes ctx only to `forward`,
    which only selects on `ctx.Done()`. Its strip follows the Leg 2 rule and
    no test pins it, because no test could fail.

Acceptance:

- Every server handler mount goes through `HandlerInterceptors`.
- Every unary server RPC and every HTTP ingress records a server span.
- No stream handler's work ctx carries the stream span.
- Both binaries write ids.

### T3 — Runner: AgentGateway spans, with the stream span stripped

Owner: compass-ux.

`gateway.Serve` builds an `otelconnect` interceptor with
`otelconnect.WithoutTraceEvents()`. It passes the interceptor to
`agentGatewayHandlerOptions`, which puts `otelx.HandlerInterceptors(ic)`
first. A construction error returns from `Serve` before the socket exists.
`Gateway.Publish` (`go/internal/runner/gateway/publish.go`) and
`Gateway.Control` (`go/internal/runner/gateway/control.go`) call
`otelx.WithoutSpan(ctx)` on entry.

Interfaces:

- Consumes: `otelx.HandlerInterceptors`, `otelx.WithoutSpan`.
- Produces (package `gateway`, unexported):

  ```go
  func agentGatewayHandlerOptions(ic *otelconnect.Interceptor) []connect.HandlerOption
  ```

Test cycle: a test calls `Serve` on a temp socket path with an in-memory
span recorder. Unary `Comms` gives one server span, and the outbound
`RelayCommsCall` client span is its child. A streaming `Control` RPC gives
one server span when it closes. Before T3: no spans.

The strip in `Publish` and `Control` has no seam at `bd9a75d2`. `Control`
passes ctx to `controlProducer.serve`, which only selects on `ctx.Done()`.
`Publish` does not read ctx. `acquirePublisher` opens on `Gateway.baseCtx`
(T5), not on the handler ctx. The strip follows the Leg 2 rule, and no test
pins it, because no test could fail.

Acceptance:

- Every AgentGateway RPC records a span.
- No AgentGateway stream lends its span to its work.

### T5 — Background work: classify every goroutine site

Owner: compass-ux. Lands after T6 and before T4.

The census has two predicates.

Goroutine sites: a non-test `go` statement, or a `.Go(` call
(`errgroup.Group.Go`, `sync.WaitGroup.Go`), under these roots:

- `go/server`;
- `go/internal/runner`, `runnerhub`, `fabric`, `delivery`, `comms`,
  `ingest`, `linearagent`;
- `go/cmd/compass-server`, `go/cmd/compass-runner`.

Command, run from the repo root:

```sh
rg -n --type go -g '!*_test.go' '^\s*go [A-Za-z_(]|\.Go\(' go/server go/internal/runner go/internal/runnerhub go/internal/fabric go/internal/delivery go/internal/comms go/internal/ingest go/internal/linearagent go/cmd/compass-server go/cmd/compass-runner
```

At `bd9a75d2` it prints 40 sites:

| File | Sites |
| --- | ---: |
| `server/sinks.go` | 8 |
| `server/serve.go` | 4 |
| `internal/runner/agent_exec.go` | 4 |
| `internal/runner/host.go` | 3 |
| `internal/runner/dispatch.go` | 3 |
| `internal/fabric/event_fabric.go` | 3 |
| `server/linear_responder.go` | 2 |
| `internal/runnerhub/relay_comms.go` | 2 |
| `server/git_credential.go`, `internal/runnerhub/router.go`, `internal/runnerhub/hub.go`, `internal/runner/run.go`, `internal/runner/gateway/socket.go`, `internal/runner/gateway/session_state.go`, `internal/fabric/runner_fabric.go`, `internal/fabric/routing_fabric.go`, `internal/fabric/fabric.go`, `internal/delivery/settle.go`, `cmd/compass-server/main.go` | 1 each |

The PR body re-runs the command and gives every site a class:

- **(a) continues a request.** It takes the request ctx, or
  `context.WithoutCancel` of it. Fix any site that does not.
- **(b) unit of work with no carried parent** (a tick, a sweep, a wake):
  start a root span per unit with `trace.WithNewRoot()`, named
  `<pkg>.<unit>`, like `delivery.sweep.*`.
- **(b′) queued unit that carries its enqueuer's trace.** The queue entry
  stores `otelx.Traceparent(ctx)` at enqueue, as `heldEntry` does in
  `go/internal/delivery/dispatch.go`. At dequeue the worker builds
  `otelx.ContextWithTraceparent(otelx.WithoutSpan(loopCtx), tp)` and starts a
  span from it. An empty `tp` gives a root. Never `WithNewRoot` here.
  - `ingest.NotifyWebhookArm`: `Enqueue` uses its ctx (today `_`). Each
    dequeued event's span is a **child** of the webhook's `otelhttp` span.
  - `ingest.BoardWebhookArm`: `Enqueue` uses its ctx. `drainBatch` and
    `retryPending` can merge several events into one pass, so each pass is a
    **root** with one **link** per carried traceparent.
  - `linearagent.Dispatcher`: `Enqueue` gains `ctx` as its first parameter,
    and so does the session-sink interface in `go/server/linear_webhook.go`.
    The dequeued event's span is a **child** of the Linear webhook span.
  - The fabric (`go/internal/fabric/event_fabric.go`) and held deliver
    (`fireHeld`, `go/internal/delivery/settle.go`) are shipped (b′) paths.
    They are listed here and not changed.
- **(c) loop or lifetime work.** No span. When it is spawned from a ctx that
  holds a span, it calls `otelx.WithoutSpan(ctx)` at the spawn. This covers:
  - the two pipe drains and the reaper started in `ServerLink.StartAgent`
    (`go/internal/runner/agent_exec.go`). `StartAgent` builds
    `lifeCtx := otelx.WithoutSpan(ctx)` after `engine.ExecStreaming`, then
    `drainCtx, stopDrains := context.WithCancel(lifeCtx)`. The reaper uses
    `lifeCtx` for `shouldLogExit` and `logUnexpectedExit`;
  - the two `agentHost.retireOnExit` goroutines
    (`go/internal/runner/host.go`), which pass `otelx.WithoutSpan(ctx)`;
  - sender pumps, watchers, and signal handlers.

Stored lifetime ctxs: a non-test struct field that holds a
`context.Context`. `containedctx` is on (`default: all`), so each one carries
a `nolint:containedctx`. Command, run from the repo root:

```sh
rg -n --type go -g '!*_test.go' 'nolint:containedctx' go
```

At `bd9a75d2` it prints four fields:

| Field | Set from | Class |
| --- | --- | --- |
| `Gateway.baseCtx` (`go/internal/runner/gateway/gateway.go`) | `gateway.Serve(ctx, …)` builds `socketCtx, socketCancel := context.WithCancel(ctx)`. Its callers in `go/internal/runner/host.go` pass the Provision command ctx. `acquirePublisher` opens the shared `PublishEvents` client stream on it. | (c): `Serve` builds `socketCtx` from `otelx.WithoutSpan(ctx)`. Without this, after T4 every shared stream is parented on one Provision span for the socket's life. |
| `sessionPublisher.ctx` (`go/internal/runner/gateway/publisher.go`) | `g.baseCtx` (shared publisher), or the `sendCtx` in `SocketListener.PublishSessionState` | First source: covered, because it is stripped in `Serve`. Second source: `agentHost.markErrored` has two kinds of caller. From `retireOnExit`, the ctx is already stripped. From `reloadLocked` (three calls), the ctx is the Reload command ctx: class (a). The publish is bounded by `stateSendTimeout` and closed in the same call, so its stream span is a short child of the command, not a lifetime leak. |
| `GuestStream.ctx` (`go/internal/runtime/microvm/exec.go`) | `GuestExec.ExecStream(ctx, …)`, from `StartAgent`'s ctx | (c): stored as `otelx.WithoutSpan(ctx)`. No pin: the guest control client has no `otelconnect` interceptor and the file has no log call, so no test could fail. |
| `supervisor.serveCtx` (`go/internal/guestd/supervisor.go`) | guest binary | out of scope: the guest binary installs no provider |

Interfaces:

- Consumes: `otelx.Traceparent`, `otelx.ContextWithTraceparent`,
  `otelx.WithoutSpan`, `otel.Tracer(<package instrumentation scope>)`.
- Produces:

  ```go
  // package linearagent
  func (d *Dispatcher) Enqueue(ctx context.Context, ev *SessionEvent) error
  ```

  Inside `ingest` and `linearagent`, each queue element becomes a struct
  that holds the event and a `traceparent string`. That type is unexported.

Test cycle:

- Per (b) site: one unit under a span recorder gives one root span. Before:
  zero spans.
- Per (b′) arm: `Enqueue` under a known span, then dequeue. The notify and
  Linear spans have that span as parent. The board pass links to it.
  Before: no span, or an unrelated root.
- Regression pins for (c). Each reads a log line that T6 already gave a ctx.
  Each runs under a known span and a `TraceHandler` over a buffer.
  - `StartAgent` with a fake engine whose stderr writes one line. After T6,
    `logDrainLine` passes `drainCtx`. The line has no `trace_id`. With the
    strip removed, it carries the known span's id.
  - `retireOnExit`, through `newHostGatewayFixture`
    (`go/internal/runner/host_gateway_test.go`) with a relay whose
    `PublishEvents` returns an error. After T6, `markErrored` logs
    "reporting agent exit to server" with its ctx. The line has no
    `trace_id`. With the strip removed, it carries the span's id.
  - `Serve`, in package `gateway`. `Deps.Events` is a wrapper `EventRelay`
    that records the ctx passed to `PublishEvents` and then delegates to a
    real client. Call `Serve` under the span, then open the shared
    publisher. The recorded ctx has an invalid span context. With the strip
    removed, it holds the span.

Acceptance:

- Every site in the census has a class.
- Webhook work joins its ingress trace.
- Lifetime work carries no request ids.
- The fabric and held-deliver traces are unchanged.

### T4 — Runner: one span per Sessions command, parented by the server

Owner: compass-ux. Lands after T6 and T5.

Add `string traceparent = 16;` to `SessionsResponse` in
`proto/compass/v1/runner.proto`, with a comment in the style of the T4b
fields, and regenerate.

The server stamps it with `otelx.Traceparent(ctx)` at each place in
`go/internal/runnerhub` that builds a `SessionsResponse`:

- `Hub.relay`, which covers `Provision`, `Start`, `Stop`, `StartResume`,
  and the other relay callers;
- `Hub.DispatchControl`, which today discards its ctx with `_ = ctx`;
- `Hub.SignalConfigVersion` (`config_signal.go`) and
  `Hub.SignalSecretsVersion` (`secrets_signal.go`). Today neither takes a
  ctx. Both gain one:
  - the `configSignaler` interface (`go/server/agent_config_service.go`)
    and its caller `service.signalConfigVersion(ctx, version)`;
  - the `secretsSignaler` interface (`go/server/secrets_service.go`) and its
    caller `secretsService.bumpSecretsVersion(ctx)`;
  - the `gitCredentialBroker.signal` field (`go/server/git_credential.go`)
    becomes `func(context.Context) error`. The wiring
    `signal: hub.SignalSecretsVersion` is unchanged. Its two callers pass
    their ctx. `credential(ctx, …)` runs under the git-credential RPC span.
    `refreshSnapshot(ctx, …)` runs on the background path
    `startGitCredentialRefresh` → `group.Go` → `run` → `refreshDue`. T5
    gives that `group.Go` site class (b), so each tick has its own root span
    and the signal envelope carries it.
  - every test fake of the two interfaces, which the compiler lists.

In the runner, `commandSpan` starts span `runner.sessions.command`. It sets
`compass.runner.command` to the oneof arm name, a bounded set. Both
`dispatcher.handle` and the inline `ConfigVersion` path in `runSessions` use
it.

Parent precedence:

- For `deliver_control`, the parent is the op's own traceparent: field 3 of
  `SteerControl`, field 4 of `DeliverControl`. This is T4b §Hop spans step 4.
  `compass.op.kind` and `compass.session.id` are also set.
- Otherwise, or when the op's traceparent is empty, the parent is the
  envelope's `traceparent`.
- When both are empty or malformed, the span is a fresh root.

Interfaces:

- Consumes: `otelx.Traceparent`, `otelx.ContextWithTraceparent`,
  `otelx.WithoutSpan`.
- Produces: proto field `SessionsResponse.traceparent` (tag 16, internal)
  and generated `func (x *SessionsResponse) GetTraceparent() string`.
- Produces (package `runnerhub`, changed signatures):

  ```go
  func (h *Hub) SignalConfigVersion(ctx context.Context, version string) error
  func (h *Hub) SignalSecretsVersion(ctx context.Context) error
  ```

- Produces (package `server`, unexported, changed):

  ```go
  type configSignaler interface {
      SignalConfigVersion(ctx context.Context, version string) error
  }
  type secretsSignaler interface {
      SignalSecretsVersion(ctx context.Context) error
  }
  // gitCredentialBroker field
  signal func(context.Context) error
  ```

- Produces (package `runner`, unexported):

  ```go
  // commandSpan starts the per-command span on otelx.WithoutSpan(ctx). The
  // parent is the op traceparent for deliver_control, else the envelope
  // traceparent; empty or malformed gives a fresh root.
  func commandSpan(ctx context.Context, cmd *compassv1internal.SessionsResponse) (context.Context, trace.Span)
  ```

Test cycle: a runner test drives `runSessions` with a fake `sessionStream`,
an in-memory span recorder, and a `TraceHandler` over a buffer. The fake
`SessionHost` returns an error from `Start`, so `dispatcher.errorResult`
logs "session command failed" with `d.log.Log(ctx, …)`. That line passes its
ctx at `bd9a75d2`, so the test does not depend on T6.

- A `start` command with envelope traceparent `E`: the recorder's command
  span has parent `E`, and the "session command failed" line carries `E`'s
  trace id and the command span's id.
- A `deliver_control` with op traceparent `O` and envelope `E`: the parent
  is `O`, never `E`.
- Provider-off case: with no SDK provider, the same line still carries `E`'s
  trace id.

Before: no span and no ids. A runnerhub test asserts that `Hub.relay`,
`Hub.DispatchControl`, and the two signal methods stamp their ctx's
traceparent on the envelope.

Acceptance:

- Each runner command runs in the server request's trace.
- The deliver hop is parented on the op.
- An empty envelope gives a fresh root and never an error.

### T6–T9 — Thread ctx into every log call; turn on `context: all`

Owner: compass-ux.

Four PRs, split by package so each has about 30–40 sites. T6 lands right
after T3, before T5 and T4, so the runner lines their tests read already pass
ctx. T7–T9 land after T4.

- Where a ctx is in scope, use the `scope` fix (`XContext(ctx, …)`).
- Otherwise add `ctx context.Context` as the first parameter of the
  enclosing function and pass it from every caller.
- Never write `context.Background()`; T1's forbidigo rule rejects it.
- Each PR re-runs sloglint in `all` mode under both build-tag passes and
  lists any tagged-file sites it found.

The PRs:

- **T6:** `internal/runner` (36) and `cmd/compass-runner` (2). This includes
  `logDrainLine`, `flushDropped`, and `logUnexpectedExit`
  (`go/internal/runner/agent_exec.go`), which gain a leading ctx, and the
  `markErrored` error line in `go/internal/runner/host.go`.
- **T7:** `server` (25) and `cmd/compass-server` (3).
- **T8:** `internal/runnerhub` (19), `internal/fabric` (9),
  `internal/ingest` (3), `internal/delivery` (1), `internal/comms` (1).
  Logs reached from `Handler.Sessions` and `Handler.PublishEvents` use the
  stripped ctx from T2. T8's test pins two cases:
  - a `Hub.Deliver` log for a `PublishEvents` frame has no `trace_id`;
  - a `RelayCommsCall` log carries that RPC's span.
- **T9:** the rest:
  - `cmd/compass-postgres` (8), `cmd/compass-mint-runner-token` (6),
    `cmd/compass-gen-cert` (3), `cmd/compass-stack` (1), `cmd/compass-app` (1);
  - `internal/stack` (5), `internal/runtime` (5), `internal/embedded` (3),
    `internal/appconfig` (3), `internal/guestd` (2), `internal/forge` (2);
  - the 2 test sites in `internal/envelope/envelope_test.go`.

  T9 also sets `context: all` in the `sloglint` block of
  `go/.golangci.yml`.

Interfaces:

- Consumes: `otelx.WithoutSpan` (T8).
- Produces: changed signatures only, each with a new leading
  `ctx context.Context`. Each PR body lists every changed signature.

Test cycle: before T9, `golangci-lint run` with `context: all` reports the
remaining sites. After T9, both lint passes are clean, and a new
`slog.Info("x")` anywhere in the module fails lint.

Acceptance: no `Debug`/`Info`/`Warn`/`Error` call without ctx remains, and
the linter rejects a new one.

## Tasks

Stack order: T1, T2, T3, T6, T5, T4, T7, T8, T9.

- [ ] T1 — `otelx.TraceHandler` (seven cases), sloglint `forbidden-keys`,
  forbidigo `context.Background`/`TODO`, `pgshare` cleanup ctx,
  `drainDoors(ctx, …)` (Owner: compass-ux)
- [ ] T2 — `otelx.HandlerInterceptors` on every server mount,
  `otelx.WithoutSpan` in server stream handlers, `otelhttp` on three HTTP
  ingresses, handler wiring in both mains (Owner: compass-ux)
- [ ] T3 — runner AgentGateway spans; stream span stripped; a test that
  covers a streaming RPC (Owner: compass-ux)
- [ ] T6 — ctx into log calls: `internal/runner`, `cmd/compass-runner`
  (Owner: compass-ux)
- [ ] T5 — classify the 40 goroutine sites and the 4 stored lifetime
  ctxs; (b′) traceparent on the webhook and Linear queues; (c) strips in
  `StartAgent`, `retireOnExit`, and `gateway.Serve`, with regression pins
  (Owner: compass-ux)
- [ ] T4 — `SessionsResponse.traceparent` (tag 16), server stamping, ctx on
  the two signal methods, runner per-command span, op-first parent for
  `deliver_control` (Owner: compass-ux)
- [ ] T7 — ctx into log calls: `server`, `cmd/compass-server`
  (Owner: compass-ux)
- [ ] T8 — ctx into log calls: `runnerhub`, `fabric`, `ingest`,
  `delivery`, `comms` (Owner: compass-ux)
- [ ] T9 — ctx into the remaining log calls; turn on sloglint
  `context: all` (Owner: compass-ux)

## Open Questions

1. **What should a long-lived stream do with its span?** Load-bearing: yes.
   This affects T2, T3, and T8, and changes how T4b's mounts behave.
   Affected streams: RunnerService `Sessions`/`PublishEvents`;
   `SubscribeEvents`, `SubscribeAgentSession`, `SubscribeComms`;
   AgentGateway `Publish`/`Control`. For each, `otelconnect` holds one span
   for the life of the stream.
   - (a) `otelconnect.WithFilter` skips those procedures, and the handlers
     start their own per-unit spans. Cost: `WrapStreamingHandler` checks the
     filter and returns `next(ctx, conn)` before it builds any stream state,
     so these RPCs also lose their duration and count metrics.
   - (b) Keep the stream span, and clear it from the work ctx at handler
     entry with `otelx.WithoutSpan`, the idiom in
     `go/internal/fabric/event_fabric.go`. Add per-unit spans where a unit
     exists (T4 commands, T5 queues). Cost: one long span per stream in
     the backend. Work lines on the stream itself carry no ids.
   - (c) Accept stream-lifetime ids as a stated exception to the Span
     lifetime constraint. Cost: thousands of lines share one span id. That
     span exports only when the stream closes, and never after a crash.
   - **Recommendation: (b).** It keeps the RPC metrics that (a) would lose,
     and (c) breaks the constraint. The design assumes (b).
2. **Should runner commands join the server's trace through the envelope
   field, or start a fresh root per command?** Load-bearing: yes. This sets
   T4's scope and needs the proto change.
   - (a) Add `SessionsResponse.traceparent`. Runner command logs share the
     server request's trace id. For `deliver_control` the op wins, as T4b
     planned. Cost: one internal proto field, stamped at four builders.
   - (b) Fresh root per command, with no proto change. Every runner line
     still has ids, but they join only runner spans.
   - **Recommendation: (a).** Matt asked for traceparent through all
     contexts. (b) meets the letter but breaks the join across the hop. The
     design assumes (a).
3. **Should logs be exported over OTLP?** Load-bearing: no.
   **Recommendation:** not in this record. `otelslog` has 271 known
   importers, below the 1000-importer threshold. The ids in the stderr lines
   already let a reader join a log line to its trace. If export is wanted
   later, a collector `filelog` receiver can parse `trace_id` from these
   lines.
4. **`WithGroup` puts the ids inside the group.** Load-bearing: no.
   - (a) Keep this. It follows the `slog.Handler` contract, and no caller
     groups today.
   - (b) Track groups in `TraceHandler` and write the ids at the top level.
     The handler would have to rebuild grouped attrs, which is much more
     code.
   - **Recommendation: (a).** Revisit when the first non-test `WithGroup`
     lands.
5. **`context: all` covers binaries outside server and runner** (postgres,
   gen-cert, mint-runner-token, stack, app). Load-bearing: no.
   - (a) Migrate them too: T9, about 25 sites. The gate stays module-wide,
     with no exclusions.
   - (b) Add a path-class exclusion for those `cmd/` dirs. This is smaller,
     but it is a hole in a gate that is meant to be total.
   - **Recommendation: (a).**
6. **Should the ingress span use `otelhttp`, or a hand-rolled one?**
   Load-bearing: no. **Recommendation:** `otelhttp` v0.71.0.
   - It has 2,776 known importers and was released 2026-08-26.
   - Its version is tied to the otel v1.46.0 pin.
   - The new dependency changes the container images' FOD hash, as the T4b
     deps did.
7. **What counts as "require"?** Load-bearing: no. The gates are:
   - sloglint `context: all`, which fails on any log call without ctx;
   - sloglint `forbidden-keys`, which fails on a caller-written `trace_id`;
   - forbidigo on `context.Background`/`TODO` outside mains and tests;
   - the T2 pgtest door test, which fails when a server mount has no span.

   sloglint checks only the method name, so
   `InfoContext(context.Background(), …)` passes it. The forbidigo rule closes
   that gap. No gate proves that every goroutine runs under a span: class (c)
   work has none on purpose, so such a gate would be wrong or could never
   fail. The ban on a raw `connect.WithInterceptors` at a handler mount is
   enforced by review only. forbidigo cannot tell a handler option from a
   client option. **Recommendation:** accept these gates.
