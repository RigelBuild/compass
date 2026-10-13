# Design: traceparent on every Go outbound hop (RIG-5321)

Child record of the trace-logs record
([`../compass-go-trace-logs/design.md`](../compass-go-trace-logs/design.md)),
called the parent here. The parent puts a span on every inbound server and
runner request, carries the traceparent on runner commands, classifies every
goroutine site, and puts ctx on every log line. This record does not edit it.
It adds the outbound hops the parent leaves out.

Ledger-impact: none — internal Go wiring, two internal config fields, and lint
config. No proto change in H1–H7 (Open Question 10 would add one internal
field), no public wire, and no decision-ledger row changes.

## Problem / Intent

Matt ruled: "need traceparents everywhere, make sure we're passing it through
all services/RPCs". After the parent, a request span reaches the edge of its
process and stops. The guest control client, every HTTP client, the Postgres
pool, the fabric DLQ, and the in-process event bus drop it. This record makes
every outbound hop in a process that has a provider carry the trace or record
a client span in it. A new hop that does neither fails a gate.

## Approach

One carrier per hop class. Each carrier already exists in the tree. This
record applies it to the hops that lack it.

| Hop class | Carrier | Existing use |
| --- | --- | --- |
| Connect client | `*otelconnect.Interceptor` first in `connect.WithInterceptors` | `runner.Dial` (`go/internal/runner/runner.go`) |
| HTTP client | `otelhttp.NewTransport`, through a new `otelx.HTTPTransport` | parent T2 `tracedIngress` (`otelhttp.NewHandler`) |
| Postgres | a `pgx.QueryTracer` that is also a `pgx.BatchTracer` | `pgtest.SQLCQueryCounter` (test only) |
| NATS | the `traceparent` message header | `Fabric.Publish` (`go/internal/fabric/event_fabric.go`) |
| In-process bus | `events.Stamped.Traceparent` | `Bus.PublishCtx` (`go/events/events.go`) |
| Hub → presence edge | a `traceparent` field on the queued edge | `heldEntry.traceparent` (`go/internal/delivery/dispatch.go`) |

A receiver restores a carried traceparent with the parent's (b′) idiom:
`otelx.ContextWithTraceparent(otelx.WithoutSpan(loopCtx), tp)`. It never uses
`trace.WithNewRoot()`.

What does not change:

- Inbound internal doors keep the sibling record's fresh-root invariant
  ([`../compass-server-runner-otel/design.md`](../compass-server-runner-otel/design.md),
  "Fresh-root invariant"). The server mints a new root and links the
  runner's span, because there is no `WithTrustRemote` in `go/`. The header
  is still passed on every hop, so the link is there.
- No proto field in H1–H7. A Connect hop uses the header. The one gap a
  header cannot close, runner frames on the long-lived `PublishEvents`
  stream, is Open Question 10.

### Hop census

Grounded at base `8e36acc8`, non-test Go. "Added" marks a hop the input census
missed.

Already carried, no change: `runner.Dial` (otelconnect), `Fabric.Publish` and
its subscriber (header), the deliver and steer controls (proto field),
`RelayCommsCall` (header plus `TriggerTraceparent` link), and runner commands
(parent T4).

| # | Hop | Binary | Today | Task |
| --- | --- | --- | --- | --- |
| 1 | Guest control, runner → guestd: `microvm.GuestClient` (`go/internal/runtime/microvm/dial.go`) | compass-runner | `return compassv1internalconnect.NewGuestControlClient(httpClient, "http://guest")`, no options | H1 |
| 2 | GitHub REST, notify reader, App token mint: `forgeHTTPClient` (`go/server/serve.go`) | compass-server | keeps or clones `http.DefaultTransport` | H2 |
| 3 | Linear forge: `forge.NewLinear` | compass-server | both `serve.go` callers pass no client; default `&http.Client{Timeout: 30 * time.Second}` | H2 |
| 4 | Trunk merge queue: `forge.NewTrunk` (added) | compass-server | `fc.TrunkHTTP` ("overrides the Trunk API client in tests") is nil in production; same default | H2 |
| 5 | Linear agent OAuth and GraphQL: `buildLinearResponder`, `buildLinearTokenSource` (`go/server`) | compass-server | `&http.Client{Timeout: linearAPITimeout}`, `&http.Client{Timeout: 30 * time.Second}` | H2 |
| 6 | Runner OIDC discovery and JWKS: `auth.NewRunnerVerifier` | compass-server | `auth.NewRunnerVerifier(clusters, tenant, nil, time.Now)`, so `http.DefaultClient` | H2 |
| 7 | Object store: `store.NewS3ObjectStore` | compass-server | `minio.New(cfg.Endpoint, &minio.Options{Creds, Secure, Region})`, minio's own transport | H2 |
| 8 | Postgres pool: `store.poolConfig` | compass-server | no `ConnConfig.Tracer`; only pgtest `OpenTraced` sets one | H3 |
| 9 | Fabric DLQ: `Fabric.publishDLQ` (added) | compass-server | sets `dlqHeaderSubject` and `dlqHeaderReason` only | H4 |
| 10 | Bus publish: the 14 non-test `Bus.Publish` sites (added) | compass-server | ctx is in scope at every caller and is not passed; `PublishCtx` has one caller | H5 |
| 11 | Bus tails: `linearagent.TailComms`, `presence.Publisher.Run` (added) | compass-server | read `event.Payload` only; no Go code reads `Stamped.Traceparent` | H6 |
| 12 | Hub → presence edges: `OnSessionLifecycle`, `OnSessionPromoted` (added) | compass-server | `hubEdge{kind, account, sessionID, state}`, no trace | H6 |

Not in a task:

| Input row or hop | Why |
| --- | --- |
| 1–2: `Fabric.SendCommand`, `Fabric.PublishBindingChange` | Dormant. `SendCommand` has no caller outside the fabric, and its payload is a `SessionsResponse`, which carries tag 16 after parent T4. `sinks.go`: "No RoutingFabric is wired: single-Server MVP". Open Question 8. |
| 3: comms stream mapping | The function is `commsToResponse` in `go/internal/comms/subscribe.go`, not `mapping.go`. `toResponse` and `SubscribeAgentSession` in `go/server/service.go` are the same hop (added). Public wire with no receiver. Open Question 5. |
| 4–6, 9, 13: CLI, compass-app, compass-stack, and embedded clients | These processes install no provider and get no inbound trace, so a carrier records nothing. OCI is compass-stack only (`materializeGuest`), not the runner. Same class (added): `bridge` `NewTLSTarget`, the stack container health clients, and `pgx.ConnectConfig` in `go/internal/stack/adapters/dbprobe.go`. Open Question 6. |
| 12: subprocess env | No receiver reads `TRACEPARENT`. Open Question 4. |
| Runner Postgres and MinIO | Dropped. The runner links `internal/store` only through `internal/secrets`, and no runner path calls `store.Open` or `minio.New`. |
| guestd inbound | The GuestControl server has no consumer for an extracted ctx. Open Question 3. |

### Placement: keep carriers out of the CLI and the app

Parent T3 moved provider setup to `go/internal/otel/otelsetup`. `otelx` no
longer imports the OTel SDK or an exporter, and a depguard rule keeps it that
way. Measured with `CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w'`
(go1.27.2): before the split, linking `otelx` grew the CLI from 13,705,376 to
22,388,896 bytes (+63%); after it, the CLI at the parent stack tip is
13,709,472 bytes. The CLI links `internal/runner`, `internal/runtime/microvm`,
`internal/store`, and `internal/secrets`. The darwin compass-app links
`internal/runtime/microvm`, `internal/stack`, `internal/embedded`,
`internal/store`, and `internal/secrets`.

These processes install no provider, so a carrier there is inert code
(Open Question 6). Carriers that live in those packages are injected by the
caller:

- The guest control client takes `[]connect.ClientOption` from
  `MicroVMConfig`. compass-runner `main` builds the interceptor.
- The Postgres tracer uses upstream `otel` and `semconv` only. `internal/store`
  already imports `go.opentelemetry.io/otel` for its flush-failure counter.
- The object store takes a transport wrapper from `S3Config`. The server
  passes `otelx.HTTPTransport`.

### Off by default

Every carrier is inert when no provider is installed:

- `otelconnect.NewInterceptor` reads the globals at construction. The global
  tracer forwards to a provider installed later
  (`tracerProvider.setDelegate`, `go.opentelemetry.io/otel` v1.46.0
  `internal/global/trace.go`: "All Tracers provided prior to this function
  call are switched out"). So compass-runner can build the guest interceptor
  before `setupOtel`. compass-runner `main` already states the rule for
  `runner.Dial`: the interceptor "is mounted unconditionally and is inert
  against the no-op global".
- `otelhttp` and the pgx tracer start spans on the global tracer. With no
  provider, it returns a non-recording span that wraps ctx's span context.
- The bus, edge, and DLQ carriers move a string. An empty string restores
  nothing (`otelx.ContextWithTraceparent` returns ctx unchanged on empty or
  invalid input).

### Cost and sampling

- New client spans: per guest RPC (H1), per HTTP egress call (H2), per
  Postgres query or batch under a span (H3). H3 is the largest: every RPC that
  reads the store gets one span per query. A tenant-scoped query is one batch
  (the `scopedDBTX` arm statements plus the query), so it is one span.
- No new span for the bus, edge, or DLQ carriers. They only restore.
- `ExecStream` is one client span per agent life, with
  `otelconnect.WithoutTraceEvents()` so its stdio frames add no events.
- `otelhttp` adds client metrics with bounded attributes (method, server
  address and port, scheme, protocol, status). `url.full` is a span attribute
  only.
- The sampler stays `ParentBased(AlwaysSample)`.

### Alternatives considered

The library bar is the parent's: at least 1000 known importers on pkg.go.dev,
and a release in the last year.

- **A pgx tracer library.** `github.com/exaring/otelpgx` has 184 known
  importers (v0.13.0, 2026-10-08). `github.com/XSAM/otelsql` has 213, and the
  `uptrace` otelsql has 111. All are below the bar, and the `database/sql`
  ones do not fit a pgx pool. Rejected: H3 hand-rolls about 60 lines.
- **`otelhttp` for HTTP egress.** 2,776 known importers, v0.72.0 released
  2026-10-02. Parent T2 adds v0.71.0, tied to the otel v1.46.0 pin. Chosen.
- **An `otelx.ClientInterceptors` helper**, mirroring `HandlerInterceptors`.
  Rejected: both Connect clients in this record live in packages the CLI
  links (see Placement). Calling it there links otelconnect into the CLI as
  inert code.
- **An OTel env carrier for subprocesses.** otel v1.46.0 has none. The spec
  ([env carriers](https://opentelemetry.io/docs/specs/otel/context/env-carriers/))
  names the variable `TRACEPARENT`. Deferred, Open Question 4.
- **`WithTrustRemote` on internal doors**, so the server parents on the
  runner. Rejected: it breaks the sibling record's fresh-root invariant.

## Global Constraints

- **Builds on the parent.** Uses its `otelx.WithoutSpan` (T2), the `otelhttp`
  dependency (T2), `SessionsResponse.traceparent` (T4), and the
  `GuestStream.ctx` strip (T5). No edit to the parent record.
- **Import names.** As in the parent: `otelx.` is
  `github.com/RigelBuild/compass/go/internal/otel` (Go package `otel`).
  `otel.` is upstream `go.opentelemetry.io/otel`.
- **Off by default, per process.** Only compass-server and compass-runner
  install a provider. Every carrier is inert without one.
- **Never fail or block on trace machinery.** The only new error path is
  `otelconnect.NewInterceptor` at compass-runner startup, the same path
  `runner.Dial` has. `HTTPTransport` and the pgx tracer never return an
  error.
- **Link weight.** The SDK and exporters stay out of `otelx` (depguard). No
  new otelconnect import into a package the CLI or compass-app links (see
  Placement). H2 puts `otelhttp` in `otelx`, which the CLI links: +8,192
  bytes, measured at the parent stack tip.
- **Bounded attributes.** HTTP: otelhttp semconv, with every query value in
  `url.full` replaced by `REDACTED`. Postgres: `db.system`,
  `db.operation.name` (the sqlc query name), and the SQLSTATE on error. Never
  SQL text, query args, request or response bodies, or tokens.
- **Span lifetime.** A Postgres span starts only under a valid parent span. It
  never starts a root. Restores use the (b′) idiom. A stored ctx is never
  kept for its span.
- **ctx threading (go-thread-context).** No new `context.Background()` or
  `context.TODO()`. The parent's forbidigo rule enforces it.
- **No proto change in H1–H7.** If Open Question 5 or 10 flips, the fields
  are additive and regenerate with `moon run compass-proto:gen`.
- **Lint posture (F1).** A per-site suppression is
  `//nolint:forbidigo // <reason>` (nolintlint `require-explanation`,
  `require-specific`).
- **Record hygiene.** Markdownlint-clean. Each task is one stacked PR in one
  linear line.

## Plan

Stack order: H1, H2, H3, H4, H5, H6, H7, in one linear line on top of the
parent's T9. The parent's T2–T9 are open as
PRs #2255, #2259, #2262, #2265, #2267, #2268, and #2269.

- H1–H4 are independent of each other.
- H5 lands before H6. H6 restores the traceparents that H5 makes non-empty.
- H7 lands last. Its census table names the final sites.

### H1 — Guest control client spans (runner → guestd)

Owner: compass-ux.

- `microvm.GuestClient` gains a variadic `opts ...connect.ClientOption`,
  passed to `NewGuestControlClient`.
- `runtime.MicroVMConfig` gains `GuestClientOptions []connect.ClientOption`.
  `installSeamDefaults` sets `m.newGuestClient` to a closure that passes
  `m.config.GuestClientOptions`. Stop's direct dial passes them too. The
  Provision client and the `GuestExec` built from it (Exec, ExecStream,
  Signal) then carry the interceptor.
- `VM.Health` keeps calling `GuestClient` with no options. It polls every
  200 ms during boot (`healthPollInterval`), so a span per poll is noise.
- compass-runner `main` builds
  `otelconnect.NewInterceptor(otelconnect.WithoutTraceEvents())` before
  `selectEngine`, and passes `connect.WithInterceptors(ic)` through
  `selectEngine` into `BackendConfig.MicroVM`. An error fails startup, as
  `runner.Dial` does.
- `ExecStream` opens on the caller's ctx, so its span is a child of the
  command that started the agent and lasts the agent's life. Kill's Signal
  uses the stored `GuestStream.ctx`, which parent T5 strips, so its span is
  a root.

Interfaces:

- Consumes: `connectrpc.com/otelconnect` v0.9.0 (already linked by
  compass-runner).
- Changes (package `microvm`):

  ```go
  // GuestClient dials the guest control plane over hybrid vsock. opts are
  // passed to the generated client; the runner wiring supplies the
  // otelconnect interceptor.
  func GuestClient(vsockSocket string, port uint32, opts ...connect.ClientOption) compassv1internalconnect.GuestControlClient
  ```

- Changes (package `runtime`):

  ```go
  type MicroVMConfig struct {
  	// … existing fields …
  	// GuestClientOptions go to every guest control client except Health.
  	GuestClientOptions []connect.ClientOption
  }
  ```

- Changes (package `main`, compass-runner, unexported):

  ```go
  func (f backendFlags) selectEngine(guestOpts ...connect.ClientOption) (runtime.WorkloadRuntime, error)
  ```

Test cycle:

- Package `microvm`: `TestGuestClientPassesOptions` uses the unix-socket h2c
  guest from `TestGuestClient_HealthRoundTrip` (`fakeMuxer`). It passes an
  otelconnect interceptor built with `otelconnect.WithTracerProvider` over a
  span recorder. One Health call records one client span. Before: it does
  not compile, because `GuestClient` takes no options.
- Package `runtime`: `NewMicroVMRuntime` with `GuestClientOptions` holding a
  counting interceptor. One call through `m.newGuestClient` against an h2c
  guest stub counts one request. Before: zero.
- GuestStream strip pin (closes the parent T5 "No pin" row): `ExecStream`
  under a known span over a traced client, then `Kill`. The Signal client
  span has no parent. With the T5 strip removed, its parent is the known
  span.
- Stop's direct dial has no hermetic seam ("Stop's own dial stays direct").
  Review checks that it passes the options.

Acceptance: every guest control RPC except Health records a client span in
compass-runner. compass-app and the CLI link no new package.

### H2 — `otelx.HTTPTransport` on every server HTTP egress

Owner: compass-ux.

- **Helper.** Add `go/internal/otel/http_transport.go`. `HTTPTransport`
  returns `otelhttp.NewTransport(redactURL{base}, …)`. Under Open Question 1
  (b) it passes `otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator())`,
  an empty propagator, so no header leaves the process.
- **Redaction.** otelhttp sets `url.full` before it calls the inner
  RoundTripper, and keeps the query (it strips only userinfo). It has no
  redact option in v0.71 or v0.72. `redactURL.RoundTrip` sets `url.full`
  again, with each query value replaced by `REDACTED`, on
  `trace.SpanFromContext(r.Context())`. The SDK keeps the last value for a
  key. Today's egress queries hold no secret (`since`, `state`, `per_page`,
  `page`, and the OCI token `service` and `scope`). This guards new ones.
- **Wiring** (package `server`):
  - `forgeHTTPClient`: both return paths set
    `hc.Transport = otelx.HTTPTransport(hc.Transport)`. This covers GitHub
    REST, the notify reader, and both App token sources.
  - New `tracedHTTPClient(timeout)`. It replaces
    `&http.Client{Timeout: linearAPITimeout}` in `buildLinearResponder` and
    `&http.Client{Timeout: 30 * time.Second}` in `buildLinearTokenSource`. It
    is passed as `LinearConfig.Client` at both `forge.NewLinear` calls, and as
    the Trunk client when `fc.TrunkHTTP` is nil (both `mergeQueueProviders`
    calls).
  - `cfg.S3` gets `WrapTransport: otelx.HTTPTransport` before
    `store.NewS3ObjectStore`.
- **auth.** `NewRunnerVerifier` wraps each cluster client after
  `clusterHTTPClient` returns it:
  `hc.Transport = otelx.HTTPTransport(hc.Transport)`. `clusterHTTPClient`
  needs an `*http.Transport` for `caFile`, so it must run first. Its test's
  `hc.Transport.(*http.Transport)` assert stays green. `internal/auth` is
  linked by compass-server, compass-stack, and mint-runner-token, which all
  link `otelx` already.
- **store.** `S3Config` gains `WrapTransport`. `NewS3ObjectStore` builds
  `minio.DefaultTransport(cfg.UseTLS)`, applies `WrapTransport` when set, and
  passes the result as `minio.Options.Transport`.

Interfaces:

- Consumes: `otelhttp` v0.71.0 (parent T2), `go.opentelemetry.io/otel/propagation`,
  `go.opentelemetry.io/otel/semconv/v1.26.0` (`URLFull`).
- Produces (package `otel`, written `otelx.`):

  ```go
  // HTTPTransport wraps base (nil means http.DefaultTransport) in an otelhttp
  // client span. It redacts query values in url.full and injects no header.
  func HTTPTransport(base http.RoundTripper) http.RoundTripper
  ```

- Produces (package `server`, unexported):

  ```go
  // tracedHTTPClient is an http.Client with timeout and an HTTPTransport.
  func tracedHTTPClient(timeout time.Duration) *http.Client
  ```

- Changes (package `store`):

  ```go
  type S3Config struct {
  	// … existing fields …
  	// WrapTransport, when set, wraps minio's default transport.
  	WrapTransport func(http.RoundTripper) http.RoundTripper
  }
  ```

Test cycle:

- Package `otel`: `TestHTTPTransportRecordsRedactedSpan`. An `httptest`
  server, a span recorder installed as the global provider (restored in
  `t.Cleanup`, as `installInMemoryTracer` does), and a parent span. One GET to
  `/v?token=s3cret&page=2` records one client span, child of the parent, whose
  `url.full` ends in `?page=REDACTED&token=REDACTED`. The server sees no
  `traceparent` header. Before: `HTTPTransport` does not exist.
- Package `otel`, provider off: the same GET succeeds, and the server sees no
  header.
- Package `server`: under a recorder, `forgeHTTPClient` with no CA, with a CA
  (`httptest.NewTLSServer`), and `tracedHTTPClient` each make one GET. Each
  records one client span. Before: zero.
- Package `auth`: a `NewRunnerVerifier` JWKS fetch against an `httptest` TLS
  issuer, under a recorder, records a client span. Before: zero.
- Package `store`: `NewS3ObjectStore` with a counting `WrapTransport`. One
  object call against an `httptest` server counts one request. Before: the
  field does not exist.

Acceptance: every compass-server HTTP egress records a client span. No span
holds a query value. No third-party request carries `traceparent`.

Unverified: the error text that otelhttp copies into the span status comes
from the inner transport. It is not checked for URLs. A `net/http` transport
error is not expected to hold the query.

### H3 — Postgres spans from a pgx tracer

Owner: compass-ux.

- Add `go/internal/store/trace.go` with `spanTracer`. `poolConfig` sets
  `cfg.ConnConfig.Tracer = spanTracer{tracer: otel.Tracer(instrumentationScope)}`.
  Every `Open` caller gets it. Outside compass-server they have no provider,
  so it is inert.
- A span starts only when ctx already holds a valid span context. Background
  loops with no span get no span, so they mint no one-span traces.
- Query span name: the sqlc name, parsed from the sqlc header line
  (`sqlcHeader` in `go/internal/pgtest/query_counter.go`) as
  `pgtest.SQLCQueryCounter` does (`strings.CutPrefix`, then `strings.Cut` at
  the first space). With no header: `postgres`.
- Batch span: one span per `SendBatch`. Its name is the first sqlc name in
  `TraceBatchStartData.Batch.QueuedQueries`, else `postgres.batch`. The arm
  statements (`SET LOCAL ROLE`, `set_config`) carry no header, so a scoped
  query is named by its query. `TraceBatchQuery` adds no span.
- Attributes: `semconv.DBSystemPostgreSQL` and `semconv.DBOperationName(name)`.
  On error: `codes.Error` with the SQLSTATE from a `*pgconn.PgError`
  (`errors.AsType`), else the description `error`. Never the error text: a
  Postgres message can quote a value.
- `OpenTraced` (pgtest) composes instead of replacing:
  `cfg.ConnConfig.Tracer = multitracer.New(cfg.ConnConfig.Tracer, tracer)`.
  `multitracer.New` threads ctx through each tracer in order.
- There is no `CopyFrom` in `internal/store`, so no `CopyFromTracer`.

Interfaces:

- Consumes: `github.com/jackc/pgx/v5` v5.10.0 (`pgx.QueryTracer`,
  `pgx.BatchTracer`, `multitracer.New`), upstream `otel`, `semconv` v1.26.0,
  `codes`.
- Produces (package `store`, unexported):

  ```go
  // spanTracer records one client span per query or batch whose ctx already
  // holds a valid span. It never starts a root.
  type spanTracer struct{ tracer trace.Tracer }

  func (t spanTracer) TraceQueryStart(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryStartData) context.Context
  func (t spanTracer) TraceQueryEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceQueryEndData)
  func (t spanTracer) TraceBatchStart(ctx context.Context, c *pgx.Conn, d pgx.TraceBatchStartData) context.Context
  func (t spanTracer) TraceBatchQuery(ctx context.Context, c *pgx.Conn, d pgx.TraceBatchQueryData)
  func (t spanTracer) TraceBatchEnd(ctx context.Context, c *pgx.Conn, d pgx.TraceBatchEndData)
  ```

Test cycle:

- Default lane: unit tests call `spanTracer` directly with a recorder tracer
  and a nil conn. They cover a named query, an unnamed query, a batch whose
  first queued query is an arm statement, a `*pgconn.PgError` end, and a ctx
  with no span (zero spans). Before: `spanTracer` does not exist.
- pgtest lane (`go test -tags pgtest -race`): install the recorder before
  `Open`. Under a parent span, one tenant-scoped store call and one unscoped
  call each record one child span, named by sqlc name, with no SQL text
  attribute. With no parent span: zero spans. Before: zero spans in both.
- pgtest lane: a store from `OpenTraced` with a `SQLCQueryCounter` records
  the span and the count. With the composition removed, the span is lost.

Acceptance: every compass-server query under a request records one client
span with bounded attributes. The query-count pgtests stay green.

### H4 — The fabric DLQ keeps the traceparent

Owner: compass-ux.

- `publishDLQ` gains a `traceparent string` parameter and sets
  `dlq.Header.Set(traceparentHeader, tp)` when it is non-empty.
- `park` passes `traceparent(msg.Headers())`. `parkOnMaxDeliveries` passes
  `traceparent(raw.Header)` from the fetched `jetstream.RawStreamMsg`.
- It copies the header, not `otelx.Traceparent(ctx)`. On a callback error
  the subscriber calls `f.retryOrPark(ctx, msg, err)` with the subscription
  ctx, not `deliveryCtx`, and the advisory path has no delivery ctx.
- The DLQ header doc comment and `go/internal/fabric/SUBJECTS.md` name the
  new header.

Interfaces:

- Changes (package `fabric`, unexported):

  ```go
  func (f *Fabric) publishDLQ(ctx context.Context, subject string, data []byte, traceparent string, cause error) error
  ```

Test cycle: extend `TestCallbackErrorParksOnDLQ` (callback path) and
`TestAlwaysSlowCallbackExhaustsMaxDeliver` (advisory path). Each publishes
under a known span. The DLQ message's `traceparent` header equals the one
`Fabric.Publish` stamped. Before: the header is empty.

Acceptance: a parked event keeps its publisher's trace on both park paths.

### H5 — The in-process bus takes ctx on every publish

Owner: compass-ux.

Open Question 2 (a): `Bus.Publish` takes ctx and stamps
`otelx.Traceparent(ctx)`. `PublishCtx` is deleted. The compiler then rejects
a ctx-less publish. Changes:

- `go/events/events.go`: `Publish(ctx, payload)`; the one `PublishCtx`
  caller (`publishMessagePosted`) moves to it.
- `go/internal/comms/mapping.go`: `publishAccountChanged`,
  `publishChannelGroupChanged`, `publishChannelChanged`,
  `publishAgentWorkspaceChanged`, `publishMessageUpdated`, and
  `publishTopicUpserted` take ctx first. Their callers already hold one (for
  example `RespondToAsk`, which today calls `c.publishMessageUpdated(askMsg)`).
- `UpdatePinnedBoard` (`go/internal/comms/comms.go`), the two
  `go/internal/board/issue_projection.go` sites, and `republishPrsLocked`
  pass the ctx they hold.
- `runnerhub.LifecycleSink.PublishSessionStatus` and
  `board.Projection.PublishSessionStatus` take ctx. `deliverSession` passes
  its ctx.
- `runnerhub.presenceSource.PublishActivity`, `Hub.PublishActivity`, and
  `presence.Publisher.PublishActivity` take ctx.
- `presence.Publisher.publishIfChanged` and `server.publishReady` take ctx.
- Test call sites pass `t.Context()`.

A publish under a stripped stream ctx stamps an empty traceparent, the
shipped rule the parent T2 restates.

Interfaces:

- Changes (package `events`):

  ```go
  // Publish stamps the next seq, a timestamp, and ctx's traceparent (empty
  // with no active span), then fans out to live subscribers.
  func (b *Bus[P]) Publish(ctx context.Context, payload P) uint64
  ```

- Changes (package `runnerhub`):

  ```go
  type LifecycleSink interface {
  	PublishSessionStatus(ctx context.Context, status *compassv1.AgentSessionStatus)
  }
  // presenceSource.PublishActivity and Hub.PublishActivity:
  PublishActivity(ctx context.Context, agentAccountID store.AccountID, activity string)
  ```

Test cycle:

- `go/events`: the existing `PublishCtx` test, renamed, asserts the stamp
  under a span and an empty stamp without one.
- `go/internal/comms`: `RespondToAsk` under a known span. A bus subscriber
  reads the `MessageUpdated` event, and its `Traceparent` holds the known
  trace id. Before: empty.
- The signature change is the gate for every other site: a site that drops
  ctx no longer compiles.

Acceptance: every bus event published inside a span carries that span's
traceparent.

### H6 — Bus tails and presence edges restore the trace

Owner: compass-ux.

- `linearagent.TailComms` calls `d.OnCommsEvent` with
  `otelx.ContextWithTraceparent(otelx.WithoutSpan(ctx), event.Traceparent)`.
  The Linear `CreateActivity` call (an H2 client span) then joins the trace
  of the message that armed it.
- `presence.Publisher.Run` does the same for `handleEvent`, on replay and
  live events. Its store reads and its republish then join the event's trace.
- Presence edges: `OnSessionLifecycle` and `OnSessionPromoted` take ctx
  first. They stamp `otelx.Traceparent(ctx)` into a new
  `hubEdge.traceparent` and never store ctx. That keeps the `PresenceSink`
  doc rule ("must NOT store the caller's ctx"). `drainEdges` restores each
  edge's traceparent before `applyLifecycle` or `applyPromoted`. The four
  hub callers (`deliverSession`, `enroll`, `promoteSession`,
  `releaseSession`) pass the ctx they hold.
- Only some edges have a trace to carry:
  - Traced: `enroll` (the `Enroll` RPC span), `promoteSession` (the Start
    or Resume command's span), and `releaseSession` reached from
    `unbindSession` (the Stop command's span).
  - Deliver refusals: `commandRouter.complete` fires `onSessionUnknown` on a
    `NOT_FOUND` result for a send-only deliver, and `Handler.Sessions` runs
    `dropLostSessionDetached` on the stripped stream ctx. The server stamped
    that deliver's traceparent itself (parent T4), so no wire field is
    needed. `deliverRefusals` stores `{sessionID, traceparent}` instead of
    the session id, `onSessionUnknown` takes the traceparent too, and the
    callback restores it with the (b′) idiom before
    `dropLostSessionDetached`.
  - Untraced without a carrier: `deliverSession` and the `dropLostSession`
    call in `deliverSession`. They run on the `PublishEvents` work ctx, which
    parent T2 strips, and `PublishEventsRequest` and `SessionFrame` carry no
    traceparent. Some of these frames are caused by a traced command: a
    failed Reload reaches `agentHost.markErrored` and `PublishSessionState`
    on the Reload command's ctx (`go/internal/runner/host.go`). Open
    Question 10 adds a per-frame carrier for them. Frames with no command
    behind them, such as an agent exit, stay untraced.

Interfaces:

- Consumes: `otelx.ContextWithTraceparent`, `otelx.WithoutSpan`,
  `events.Stamped.Traceparent`.
- Changes (package `runnerhub`, the `PresenceSink` interface):

  ```go
  OnSessionLifecycle(ctx context.Context, account store.AccountID, sessionID string, state compassv1.AgentSessionState)
  OnSessionPromoted(ctx context.Context, account store.AccountID, sessionID string)
  ```

- Changes (package `presence`, unexported): `hubEdge` gains
  `traceparent string`.
- Changes (package `runnerhub`, unexported): `deliverRefusals` values become
  `{sessionID, traceparent string}`, filled in `send1` from
  `cmd.GetTraceparent()`, and `setSessionUnknown` takes
  `func(sessionID, traceparent string)`.

Test cycle:

- `linearagent`: `TailComms` over a bus, with a fake `Client` that records
  the ctx of `CreateActivity`. Run the tail under a loop span. Publish an
  arming `MessagePosted` under a known span. The recorded ctx has the known
  trace id, not the loop span. Before: it holds the loop ctx.
- `presence`: a fake `PresenceReads` records the ctx of `AgentHasOpenAsk`.
  An ask message published under a known span, and an `OnSessionLifecycle`
  call under another, each give a recorded ctx with that trace id. An event
  with an empty traceparent gives an invalid span context. Before: the loop
  ctx, or a compile error for the edge signature.
- `runnerhub`, through the real path: a `Hub` with a recording
  `PresenceSink`. `Enroll` under a known span gives a lifecycle edge with
  that trace id. A `PublishEvents` lifecycle frame sent with a client span
  gives an edge with an empty traceparent. A send-only deliver under a known
  span, refused with `NOT_FOUND`, gives a `DISCONNECTED` edge with that trace
  id. Before: the test does not compile against the old edge signature.

Acceptance: bus-tail work and the traced edges join the trace of the request
that caused them. Untraced edges carry no span. Loop spans never leak into
that work.

### H7 — Gate: no new uncarried egress

Owner: compass-ux.

- forbidigo pattern `^http\.DefaultClient$`, scoped to non-test files. Its
  message points at `otelx.HTTPTransport` and this record. forbidigo matches
  text, so `^http\.Client$` would also hit every type reference. It is not
  used.
- The five sites it hits keep a nolint with a reason. They are the CLI
  `httpClientFor` (no provider), `runner.Dial`'s nil default (the Connect
  interceptor carries the trace), the `auth.NewRunnerVerifier` nil default
  (wrapped after `clusterHTTPClient`), and the two `linearagent` nil defaults
  (test only; production passes `tracedHTTPClient`).
- `TestEgressCensus` in `go/internal/otel/egress_census_test.go`. It parses
  every non-test, non-generated `.go` file under `go/` with `go/parser`, so
  it sees every build tag. It collects each call to a `New…Client` from a
  `…connect` package, each `http.Client` composite literal, and each
  `minio.New`, `pgxpool.NewWithConfig`, `pgx.Connect`, `pgx.ConnectConfig`,
  and `nats.Connect`. Each is keyed by file, enclosing func, and callee, with
  a count per key, so a second constructor in a classified func changes the
  count. It compares the counts to a table of classes: `traced`,
  `noProvider`, or `testHelper`, each with a reason. A new site, a removed
  site, or a changed count fails with a message that names the key.

Interfaces:

- Produces: the forbidigo entry in `go/.golangci.yml`, and:

  ```go
  // package otel, test only
  type egressClass int // traced, noProvider, testHelper
  var egressSites map[string]struct{ class egressClass; count int } // key: "<file>:<func>:<callee>"
  func TestEgressCensus(t *testing.T)
  ```

Test cycle: as the parent's T1 gates. First commit: the forbidigo entry and
the census test with an empty table. Its red `compass-go:lint` and
`compass-go:test` output names the five `DefaultClient` sites and every
census site. The PR body quotes it. The next commit adds the nolints and the
table, and both go green. A table-driven case runs the collector over
fixture sources: one more constructor in a func that already has one, and
one fewer, each fail.

Acceptance: a new `http.DefaultClient`, Connect client, `http.Client`
literal, or pool constructor fails CI until it is classified.

## Tasks

Stack order: H1, H2, H3, H4, H5, H6, H7, on the parent's T9.

- [ ] H1 — guest control client options from compass-runner wiring; Health
  stays bare; GuestStream strip pin (Owner: compass-ux)
- [ ] H2 — `otelx.HTTPTransport` with `url.full` redaction, on forge,
  Linear, Trunk, linearagent, auth, and the object store (Owner: compass-ux)
- [ ] H3 — `store.spanTracer` for queries and batches, composed with
  `OpenTraced` (Owner: compass-ux)
- [ ] H4 — DLQ copies the `traceparent` header on both park paths
  (Owner: compass-ux)
- [ ] H5 — `Bus.Publish(ctx, …)` at every site; `PublishCtx` removed
  (Owner: compass-ux)
- [ ] H6 — bus tails and presence edges restore the publisher trace
  (Owner: compass-ux)
- [ ] H7 — forbidigo `http.DefaultClient` and `TestEgressCensus`
  (Owner: compass-ux)

## Open Questions

1. **Third-party egress: inject the header, record a span only, or skip?**
   Load-bearing: yes. It sets H2's propagator. Targets: GitHub, Linear,
   Trunk, the runner cluster OIDC issuers, and the object store.
   - (a) Inject `traceparent`. A vendor that reads it could join our trace.
     Cost: our trace ids leave the process, and no target is known to read
     them.
   - (b) Client span only. Our trace shows each call, its time, and its
     status. No header leaves the process.
   - (c) Skip. The trace has a hole at every external call.
   - **Recommendation: (b).** The span is what joins the call to our trace.
     The design assumes (b). (a) is a one-line change: drop the empty
     propagator in `HTTPTransport`.
2. **Bus API: one `Publish(ctx, …)`, or keep `Publish` and `PublishCtx`?**
   Load-bearing: no. Both need the same caller changes.
   - (a) One method. The compiler rejects a ctx-less publish. Cost: about 75
     test call sites change.
   - (b) Keep both and move the ctx-bearing sites to `PublishCtx`. A new
     ctx-less publish still compiles.
   - **Recommendation: (a).** It is the only gate this hop class can have.
3. **Should guestd extract the inbound traceparent?** Load-bearing: no.
   - (a) Extract only: `otelconnect.WithPropagator(propagation.TraceContext{})`
     on the `serveHandshake` handler. A real interceptor adds about 221 KB
     (+2.0%) to the 10,825,888-byte guestd.
   - (b) Install a provider in guestd. Full `otelx` adds 5.77 MB (+53%), and
     the guest would need an OTLP path out of the VM.
   - (c) Defer.
   - **Recommendation: (c).** guestd has two log calls, both outside RPC
     scope. It installs no `TraceHandler` and no provider, and it logs only
     to the serial console. H1's client span already times every guest RPC
     from the runner. The parent's T5 also marks guestd out of scope.
4. **Subprocess `TRACEPARENT` env?** Load-bearing: no. **Recommendation:**
   skip until a receiver exists. No Go or TypeScript code reads the variable;
   the only `TRACEPARENT` match is a version constant in
   `packages/compass-agent/src/trace-bridge.ts`. The agent already gets the
   trace through the deliver and steer fields.
5. **Public stream traceparent fields** (`SubscribeComms`,
   `SubscribeEvents`, `SubscribeAgentSession` responses)? Load-bearing: no.
   **Recommendation:** no. It is a public wire change with ledger impact and
   no receiver. The UI reads only the trace-response header
   (`traceResponseInterceptor`). `SessionInjection.traceparent` already
   covers the one join a subscriber needs.
6. **Clients in processes with no provider** (CLI, compass-app,
   compass-stack, embedded)? Load-bearing: no.
   - (a) Skip. The census gate lists them as `noProvider`.
   - (b) Inert interceptors now (about +61 KB on the CLI, measured). No test
     could fail on them.
   - (c) A provider in the CLI: +8.54 MB (+63%) and an `ExecuteContext`
     root.
   - **Recommendation: (a).** (b) is inert code. (c) is a new product
     surface.
7. **Split provider setup out of `otelx`?** Resolved: parent T3 did it (see
   Placement).
8. **Dormant core-NATS publishes** (`SendCommand`, `PublishBindingChange`)?
   Load-bearing: no. **Recommendation:** no change now. `SendCommand`'s
   payload carries the traceparent after parent T4. When a routing fabric is
   wired, `PublishBindingChange` stamps the header and its callback gains
   ctx.
9. **What counts as "require" here?** Load-bearing: no. The gates are
   forbidigo on `http.DefaultClient`, `TestEgressCensus`, and H5's
   `Publish(ctx, …)` signature. Not enforced: that a `traced` site really
   passes the interceptor or transport (review checks it), and egress inside
   a third-party SDK the census does not name. **Recommendation:** accept
   these gates.
10. **A per-frame traceparent on `PublishEvents`?** Load-bearing: no. Runner
    frames carry no trace, so presence edges from `deliverSession` and
    `dropLostSession` have none (H6), even when a traced command caused the
    frame, such as a failed Reload.
    - (a) Leave them untraced. The Reload failure edge drops the trace.
    - (b) Add `traceparent` to `PublishEventsRequest`, as parent T4 did for
      `SessionsResponse`. `PublishSessionState` and the event publisher
      stamp `otelx.Traceparent(ctx)`; it is empty when no command span is
      active. `Hub.Deliver` restores it on the stripped work ctx before
      `deliverSession`. Internal proto, buf-breaking clean.
    - **Recommendation: (b)**, as its own task after H6, because Matt asked
      for traceparents on every hop. Until it is ruled, H6 promises traces
      only for the edges it lists as traced.
