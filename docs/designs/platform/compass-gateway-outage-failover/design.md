# Compass gateway outage failover (RIG-3029)

Ledger-impact: none (platform surface is ungoverned by the design ledger).

Tracker: RIG-3029

Extends the stable-name routing record
(`docs/designs/platform/compass-stable-name-routing/design.md`, "v1" below) and
the gateway record (`docs/designs/server/compass-server-llm-gateway/design.md`).

## Problem / Intent

v1 fails over only when its dry-run pool peek finds no usable credential or a
usage-limit mark. A provider outage passes that peek, so callers of the stable
name stay pinned to the down candidate.

Streaming hides the failure. `handleFormatEndpoint` and `handlePiNative` in the
fork's `packages/ai/src/auth-gateway/server.ts` return
`new Response(sseStream, { status: 200, ... })` at once, so a connect error or
5xx reaches the client as an `error` event inside a 200.

This record adds pre-first-byte failover: when a candidate fails before it
produces content, the gateway tries the next candidate and, for an outage,
marks that upstream down. Mid-stream failover is out of scope.

## Global Constraints

- Prerequisites: F1 and F2 need nothing new. C1 needs v1 P1
  (`StableNameResolver`, the dry-run peek), the gateway record's T3 caller
  identity, and RIG-5152. None is in the fork: `ModelResolver` in
  `packages/ai/src/auth-gateway/dispatch.ts` is still
  `(modelId: string) => Model<Api> | undefined`.
- Each attempt's session-state key must carry the caller identity. Today
  `sessionKeys` in `packages/ai/src/auth-gateway/session-state.ts` keys only on
  provider, model, and a client key or context hash, so two callers can share
  retained provider state. That gap predates this record; RIG-5152 tracks it.
- One routing-core change, opt-in behind the `candidateFailover` boot option.
  Without it every route behaves as today. The fork interface names no compass
  type, so it stays offerable upstream.
- No new runtime, service, or Go.
- Candidate choice is a pure function of the chain, pool state, marks, and the
  tried set, so v1's prompt-cache argument holds.
- One tenant's failures never change another tenant's routing.
- Compass gets fork changes only by bumping `tools/gateway-image/fork-pin.json`
  to a commit on fork `main`.
- Scope: the four chat routes (`/v1/chat/completions`, `/v1/messages`,
  `/v1/responses`, `/v1/pi/stream`).
- rumdl-clean; Conventional Commits with the
  `Co-authored-by: Matt Wilkinson <matt@rigel.build>` trailer.

## Approach

```text
next candidate ─▶ route prechecks ─▶ credential, lease ─▶ streamSimple
  hold events until the first meaningful event
    committed            ─▶ respond from this candidate
    failed before commit ─▶ abort cause, else classifyUpstreamFailure
      stop          ─▶ return the failure
      failover kind ─▶ onFailure, next candidate
```

### Commit point

An attempt commits at its first event for which `isMeaningfulCompletionEvent`
(`packages/ai/src/utils/empty-completion-retry.ts`) is true: a non-empty text,
thinking, or tool-call delta, or an image. `withReplaySafeStreamRetry` commits
on the same predicate. `start` does not count: the Anthropic provider pushes it
just above `// Retry loop for transient errors from the stream.`, before any
upstream byte.

Streaming: a relay `AssistantMessageEventStream` buffers events until commit,
then replays them and forwards the rest. A committed attempt is final: a later
error reaches the client as today. The response sends 200 and the SSE headers
once the first attempt's `streamSimple` returns, then a `: keepalive` comment
every 15 s until commit (OQ-3); SSE parsers skip comment lines (`readSseEvents`
in `packages/utils/src/stream.ts`). At commit the route runs `encodeStream` on
the relay. `x-litellm-model-id` names the requested model and
`x-litellm-model-api-base` is omitted; the log and usage event name the backend
that answered.

Non-streaming: the attempt drains `streamSimple` as `completeSimple` does,
through `resolveWithThinkingLoopRetries` (`packages/ai/src/stream.ts`,
exported unchanged). The first meaningful event lifts the budget, so a slow but
healthy completion is not cut, and thinking-loop re-dispatches stay on the same
candidate. The client sees nothing until the drain ends, so any classified
failure fails over, whatever partial content it holds.

A failure reaches the client only when no follower remains: as today's HTTP
error `Response` if headers are not sent yet, else as an error
`AssistantMessage` that ends the relay, which `encodeStream` writes as the
format's SSE error.

### Setup failures

Each candidate first runs the route's model prechecks: `chatRouteRejection`,
and on `/v1/responses` the OpenAI image-file-reference check. The first
candidate fails as today; an incompatible follower is skipped with no mark and
no `onFailure`.

Credential setup runs on the attempt signal, before the budget is armed. Setup
failures never set a mark. No credential (`resolveGatewayApiKey`'s 401) gives
`credential_exhausted`. A thrown credential store call (`classifyGatewayError`,
usually 502), a synchronous `streamSimple` throw, or a setup still running at
`deadlineMs` gives `setup_error`. All fail over.

### Which failures fail over

The loop decides aborts first (Bounds). `classifyUpstreamFailure` in
`packages/ai/src/error/gateway.ts` sees only the rest. Its text is
`errorClassificationMessage ?? errorMessage`, as both handlers pass to
`classifyGatewayError`. Rows apply in order; "reason" is
`parseRateLimitReason(text)`.

| Upstream failure before commit | Result |
| --- | --- |
| Usage limit (`AIError.isUsageLimit` or `isUsageLimitOutcome`) | `credential_exhausted` |
| 401, or 403 that is not a concurrency cap | `credential_exhausted` |
| 403 concurrency cap (`isConcurrencyCapExclusion`), or 429 | `rate_limited` |
| 404 | `not_served` |
| Any other status below 500 except 408 | stop |
| 408 or 5xx | `server_error` |
| No status, and `isProviderRetryableError` is false | stop |
| No status; reason `RATE_LIMIT_EXCEEDED` or `CONCURRENT_LIMIT` | `rate_limited` |
| No status; reason `MODEL_CAPACITY_EXHAUSTED` or `SERVER_ERROR` | `server_error` |
| No status; any other reason (connect, DNS, TLS, socket close) | `transport` |

The retryable gate stops `parseRateLimitReason`'s substring matches (such as
`500` inside `205000`) from turning a deterministic error into a mark.

A usage limit, 401, or credential 403 arrives only after rotation is exhausted:
`refreshGatewayApiKeyAfterAuthError` in `dispatch.ts` records it, and v1's peek
honors that. A 403 concurrency cap is never rotated (`isRetryableUpstreamError`
in `packages/ai/src/stream.ts`), so it is marked as `rate_limited`. A 404 can
be request-specific, so `not_served` sets no mark.

`retryAfterMs` comes from headers (`getRetryAfterMsFromHeaders`) only for a
thrown error. An error `AssistantMessage` carries no headers, so there it comes
only from text (`extractProviderRetryHint`, `packages/ai/src/utils/retry-after.ts`).

### The provider-down mark

Only `transport`, `server_error`, and `rate_limited` set a mark. `transport`
keys on provider and `baseUrl`, since a host that refuses connections is down
for every model. `server_error` (including every budget timeout) and
`rate_limited` also key on model id.

TTL starts at `calculateRateLimitBackoffMs("SERVER_ERROR")` (20 s), or
`calculateRateLimitBackoffMs("RATE_LIMIT_EXCEEDED")` (30 s) for `rate_limited`
(`packages/ai/src/error/rate-limit.ts`). It doubles per consecutive failure:
20, 40, 80, 160, then a 300 s cap. It is never shorter than `retryAfterMs`
(also capped at 300 s). Strikes reset when the last mark expired over 300 s ago.

Marks are per tenant only (OQ-4), in memory in the compass boot entrypoint, one
map per gateway process. With several replicas, each learns an outage on its
own. Both deployment shapes run the same code, per
`docs/concepts/self-host-and-managed.md` and
`docs/designs/meta/oss-core-managed-boundary/design.md`. Until a mark exists,
each request that reaches the down candidate stalls for up to its attempt
budget, per tenant, per replica, per TTL.

v1's `StableNameResolver` walks the chain in order, skips candidates in `tried`,
and defers marked ones. A marked candidate wins only when no unmarked one has a
usable credential, so a single-candidate chain ignores its mark.

### Bounds

- **Attempts.** One per distinct candidate. The first comes from
  `bootOpts.resolveModel` as today; each follower from the side-effect-free
  `candidateFailover.next`. `undefined` from `next` means no follower, never a
  404. Compass returns `undefined` for a direct `provider/id` selector.
- **Attempt budget (OQ-2).** Only when a follower exists. Armed immediately
  before `streamSimple`: half the time left before `deadlineMs` (default
  120 000 ms), a hard deadline through the first meaningful event, covering
  in-flight provider retries. On expiry the loop aborts the attempt, even
  after a 2xx. A `done` or `error` event, or the attempt settling, disarms it,
  so a timeout counts only if its abort came first. The attempt also gets
  `maxRetryDelayMs: 10_000`; provider behavior varies, and the hard deadline
  bounds every route.
- **Deadline.** No attempt starts after `deadlineMs`; a credential setup still
  running then is aborted.
- **Abort.** Each attempt has its own `AbortController`, linked to the request
  controller from `mirrorRequestAbort`. Cancelling the hold body aborts the
  request, as `encodeStream`'s `onCancel` does today. After a failure the loop
  checks the request signal first: if aborted, it stops (499 as today). A
  budget timeout is next and is always `server_error`: the gateway cannot tell
  a stalled 200 from a 503 in provider retries, and several providers never
  call `onResponse`. With no response evidence, `start` versus other events no
  longer matters. Only other failures reach the classifier, the sole source of
  `transport` (connection-level errors).
- **Leases.** Each attempt acquires its own lease and releases it when that
  attempt's stream or result settles, as today's `events.result().finally`
  does. The next candidate does not wait for it.
- **Usage (OQ-5).** Every attempt goes through `recordGatewayUsage`, which
  skips zero usage. An attempt that did not answer uses request id
  `${requestId}:a${n}` and outcome `error`, because `onUsage` consumers dedupe
  on request id.

### Observability

- **Log.** `logger.warn("auth-gateway candidate failover", …)` per failed or
  skipped attempt, with the `auth-gateway request` log's keys (`requestId`,
  `format`, `model`, `resolvedProvider`, `resolvedModel`, `peer`) plus
  `attempt`, `failure`, `status`, `retryAfterMs`, `elapsedMs`, `outcome`
  (`retrying`, `skipped`, `exhausted`, `deadline`, `budget`), and the next
  candidate.
- **Counter.** Compass's `onFailure` increments the OpenTelemetry counter
  `compass.gateway.candidate_failures` (attributes `provider`, `failure`; no
  tenant). `packages/ai` has no OpenTelemetry dependency.

Non-chat routes behave as today.

## Alternatives considered

- **B: a compass wrapper hop.** It must parse four SSE formats to find the
  first content event, cannot see pool state to set or read marks, and adds a
  hop to every chat request.
- **An in-process failover api.** `registerCustomApi`
  (`packages/ai/src/api-registry.ts`) could register a synthetic api that walks
  candidates. Rejected: the route resolves a credential for the synthetic model
  first, so `resolveGatewayApiKey` returns 401; the session-state store is
  private to `startAuthGateway`; and headers would name the synthetic model.
- **Provider retries only.** They retry the same upstream.
- **Commit on `start`.** Anthropic emits `start` before its first upstream
  call, so an outage would commit.
- **Marks in the Server store.** Shared across replicas, but adds RPC work to
  every resolve.

## Plan

Order: F1 → F2a → F2b → F2c land on fork `main` against today's resolver. C1
waits for its prerequisites. Lane: compass-obs for every task.

### F1 — Fork: `classifyUpstreamFailure`

Add the classifier to `packages/ai/src/error/gateway.ts`. For an
`AssistantMessage` error, wrap `errorClassificationMessage ?? errorMessage` and
`errorStatus` in an `Error` with `status`, as `FinalizedProviderStreamError`
does.

Interfaces:

```ts
export type UpstreamFailureKind =
  | "transport"
  | "server_error"
  | "rate_limited"
  | "credential_exhausted"
  | "not_served"
  | "setup_error";

export interface UpstreamFailure {
  readonly kind: UpstreamFailureKind;
  readonly status?: number;
  readonly retryAfterMs?: number;
  readonly message: string;
}

/** Non-abort failures only. `undefined` means stop. `message` is `errorClassificationMessage ?? errorMessage`. */
export function classifyUpstreamFailure(
  model: Model<Api>,
  error: unknown,
  status: number | undefined,
  message: string | undefined,
): UpstreamFailure | undefined;
```

Test cycle (`packages/ai/test/auth-gateway-classify-error.test.ts`):

- `usage_limit_reached`, an opaque 429, 401, and a credential 403 give
  `credential_exhausted`.
- A 403 concurrency cap, and 429 `Too many requests`, give `rate_limited`.
- 404 gives `not_served`; 503 gives `server_error`; 400 stops.
- Status-less, as `createAnthropicSseStreamError` in
  `packages/ai/src/providers/anthropic.ts` builds them:
  `Anthropic stream error (rate_limit_error): Rate limit exceeded` gives
  `rate_limited`; `Anthropic stream error (overloaded_error): Overloaded` gives
  `server_error`.
- Status-less `fetch failed` gives `transport`.
- Status-less `invalid_request_error: prompt is too long: 205000 tokens` stops.
- A status-less error whose `errorMessage` reads as a socket failure but whose
  `errorClassificationMessage` reads `Too many requests` gives `rate_limited`.

### F2a — Fork: extract the per-attempt body

Move each handler's per-candidate work (prechecks, credential, lease, stream
options) into one function per route. No behavior change: the existing
`auth-gateway-*.test.ts` suite stays green unchanged. `resolveGatewayApiKey`
tags its error result; existing callers read only `status`, `type`, and
`message`.

Interfaces:

```ts
// dispatch.ts
export interface GatewaySetupFailure extends GatewayErrorClassification {
  readonly setupReason: "no_credential" | "store_error";
}
export async function resolveGatewayApiKey(
  storage: AuthStorage,
  model: Model<Api>,
  sessionId: string,
  signal: AbortSignal,
  peer: string,
): Promise<ResolvedApiKey | GatewaySetupFailure>;

// server.ts, module-private
type AttemptStart =
  | { readonly ok: false; readonly reason: "incompatible"; readonly failure: GatewayErrorClassification }
  | { readonly ok: false; readonly reason: "no_credential" | "store_error"; readonly failure: GatewayErrorClassification }
  | {
      readonly ok: true;
      readonly streamOpts: SimpleStreamOptions;
      readonly lease: AuthGatewaySessionStateLease;
      readonly account: () => string;
    };

function prepareFormatAttempt(
  route: { module: FormatModule; label: string },
  bootOpts: AuthGatewayRouteOptions,
  parsed: ParsedFormatRequest,
  model: Model<Api>,
  sessionId: string,
  signal: AbortSignal,
  peer: string,
  sessionStates: AuthGatewaySessionStateStore,
): Promise<AttemptStart>;

function preparePiNativeAttempt(
  bootOpts: AuthGatewayRouteOptions,
  req: Request,
  parsed: piNative.PiNativeParsedRequest,
  model: Model<Api>,
  sessionId: string,
  signal: AbortSignal,
  peer: string,
  sessionStates: AuthGatewaySessionStateStore,
): Promise<AttemptStart>;
```

### F2b — Fork: hold primitives

Add `packages/ai/src/auth-gateway/failover.ts`. Export
`isMeaningfulCompletionEvent` and `resolveWithThinkingLoopRetries` unchanged.

Interfaces:

```ts
export interface AttemptBudget {
  /** Feed every event. A meaningful event lifts the budget; `done` or `error` disarms it. */
  observe(event: AssistantMessageEvent): void;
  /** Disarms the budget and clears its timer. Call when the attempt settles or throws. */
  settle(): void;
  /** True only if the budget abort fired before any lift, terminal event, or `settle()`. */
  timedOut(): boolean;
}

/** Armed by the loop immediately before `streamSimple`. Aborts `attempt` at `endsAt` unless lifted or disarmed first. `undefined` never fires. */
export function armBudget(attempt: AbortController, endsAt: number | undefined): AttemptBudget;

export type HeldAttempt =
  | { readonly kind: "committed"; readonly relay: AssistantMessageEventStream }
  | { readonly kind: "failed"; readonly relay: AssistantMessageEventStream; readonly error: AssistantMessage };

/** Streaming: buffers until a meaningful event or `done` (committed) or an `error` (failed). */
export function holdUntilCommit(events: AssistantMessageEventStream, budget: AttemptBudget): Promise<HeldAttempt>;

/** Non-streaming: `completeSimple`'s drain, feeding `budget` from every dispatch. */
export function completeAttempt(
  model: Model<Api>,
  context: Context,
  options: SimpleStreamOptions,
  budget: AttemptBudget,
): Promise<AssistantMessage>;

/** Ends `relay` with an `error` event carrying `failure`; used once headers are sent. */
export function endWithFailure(
  relay: AssistantMessageEventStream,
  model: Model<Api>,
  failure: GatewayErrorClassification,
): void;

/** Writes `: keepalive` every 15 s until `body` resolves, then its bytes. `cancel` calls `onCancel`. */
export function sseHoldBody(
  body: Promise<ReadableStream<Uint8Array>>,
  onCancel: (reason: unknown) => void,
): ReadableStream<Uint8Array>;
```

Test cycle, on hand-driven `AssistantMessageEventStream`s: `start` then a
`text_delta` commits and replays both in order; a committed relay forwards a
later `error` unchanged; `start`, an empty `text_start`, then `error` fails.
The budget fires after silence, and `timedOut()` is then true. It never fires
after a meaningful event. After a `done` or `error` event, or `settle()`, a
late timer leaves `timedOut()` false. `sseHoldBody` writes comments before the
body bytes, and `cancel` calls `onCancel`.

### F2c — Fork: the loop and boot option

Wire the loop into both handlers when `bootOpts.candidateFailover` is set, and
add the log.

Interfaces:

```ts
// dispatch.ts — AuthGatewayRouteOptions gains `candidateFailover?: CandidateFailoverOptions`.
export interface FailoverContext {
  /** The inbound request, as an opaque token; the fork reads no identity from it. */
  readonly req: Request;
  /** `candidateKey` of every candidate tried or skipped so far, including the current one. */
  readonly tried: ReadonlySet<string>;
}

export interface CandidateFailoverOptions {
  /** The candidate after `ctx.tried`, without side effects; `undefined` if none. */
  next(modelId: string, ctx: FailoverContext): Promise<Model<Api> | undefined>;
  /** Called once per failed attempt, never for a skipped one. Must not throw. */
  onFailure(model: Model<Api>, failure: UpstreamFailure, ctx: FailoverContext): void;
  /** No attempt starts after this many ms from request start. Default 120_000. */
  deadlineMs?: number;
}

// failover.ts
export function candidateKey(model: Model<Api>): string; // provider \0 id \0 baseUrl
```

Test cycle: new `packages/ai/test/auth-gateway-failover.test.ts`, styled on
`auth-gateway-provider-session-state.test.ts` (`startAuthGateway` on
`127.0.0.1:0`, an injected `fetch`, two models with different `baseUrl`s). Red
today: with A down, the client gets a 200 carrying an error. Green:

- A fails (`fetch failed`, then 503): all four routes, streaming and not,
  answer from B; `onFailure` fires once, with A.
- A's budget fires when its `fetch` never resolves, when a Google model (its
  provider never calls `onResponse`) answers 200 and then stalls, and when
  Anthropic is still retrying a 503. Each gives `server_error`, the request
  signal stays live, and B answers.
- The client aborts while A is held: no follower starts, and the route
  returns 499.
- Non-streaming, A sends its first delta before the budget and finishes after
  it: A answers, with no failover.
- A emits a `text_delta`, then errors: B is never tried; the client gets A's
  text and A's error.
- A returns a usage limit with one credential: B answers (`credential_exhausted`).
- `storage.keys.getWithCredential` throws for A, or `streamSimple` throws
  synchronously for A: B answers, and `onFailure` gets `setup_error`, which
  sets no mark (C1).
- A's credential lookup outlasts the budget A would get: it is not cut, and A
  answers. A lookup still running at `deadlineMs` ends as `setup_error`.
- Streaming, B has no credential after headers are sent: the client gets an
  SSE error in the open 200 body.
- A fails with 503, then the follower fails a precheck and a third chat
  candidate answers. The follower is skipped without `onFailure` in two
  variants: it is an embedding model (`chatRouteRejection`), or it is not a
  Responses model on `/v1/responses` with an OpenAI file id.
- A answers 400: no failover. A direct selector fails: one attempt.
- `onUsage` fires once per nonzero attempt, with distinct request ids.
- Every attempt's lease is released: a probe in each attempt's
  `providerSessionState` is closed once the store fills to
  `AUTH_GATEWAY_MAX_SESSION_STATES`.

### C1 — Compass: marks, resolver, counter

Prerequisites: v1 P1, gateway T3, RIG-5152, and the compass boot entrypoint.
The gateway-image record names `packages/coding-agent/src/cli/gateway-boot.ts`;
the fork today wires `startAuthGateway` in `cli/auth-gateway-cli.ts`.

Bump `tools/gateway-image/fork-pin.json` to F2c's commit. Add one
`ProviderDownMarks`. Make `StableNameResolver` skip `tried` and defer marked
candidates. Pass `candidateFailover`: `next` maps `ctx.req` to its caller
through a `WeakMap<Request, CallerIdentity>` filled by T3's `authorize`;
`onFailure` marks and counts.

Interfaces:

```ts
export class ProviderDownMarks {
  constructor(now?: () => number);
  /** Sets or extends a tenant's mark for `transport`, `server_error`, or `rate_limited`; ignores other kinds. Returns the TTL in ms, or 0. */
  mark(tenant: UserId, model: Model<Api>, failure: UpstreamFailure): number;
  /** True if this tenant's mark covers (provider, baseUrl) or (provider, baseUrl, id). */
  isDown(tenant: UserId, model: Model<Api>): boolean;
}

// StableNameResolver (v1 P1) gains the tried set:
resolve(callerUser: UserId, modelId: string, tried?: ReadonlySet<string>): Promise<Model<Api> | undefined>;
```

Test cycle: with an injected clock, TTLs run 20, 40, 80, 160, 300;
`retryAfterMs` raises the TTL; strikes reset after 300 s. A `transport` mark
covers every model on its `baseUrl`. A `server_error` mark, including one from
a budget timeout, covers only its model, so a sibling model on the same
`baseUrl` is not deferred. `credential_exhausted`, `not_served`, and
`setup_error` set no mark. A mark for one tenant never affects another.
Resolver: a marked first candidate is deferred; when all are marked the first
wins; a single candidate ignores its mark; `tried` is skipped; a direct
selector gets no follower. Smoke: the gateway image smoke passes at the new pin.

## Tasks

- [ ] F1 — `classifyUpstreamFailure` and table tests (compass-obs).
- [ ] F2a — Per-candidate extraction and tagged setup failures, no behavior
      change (compass-obs).
- [ ] F2b — `armBudget`, `holdUntilCommit`, `completeAttempt`,
      `endWithFailure`, `sseHoldBody` (compass-obs).
- [ ] F2c — Loop, `candidateFailover`, log, `auth-gateway-failover.test.ts`
      (compass-obs).
- [ ] C1 — Pin bump, `ProviderDownMarks`, resolver change, counter, wiring
      (compass-obs).

## Open Questions

- **OQ-1 — LOAD-BEARING: fork retry loop (A) or compass wrapper hop (B).**
  Tracked in RIG-5147. A changes the fork's routing core, which v1 avoided; B
  costs four SSE parsers, no pool access, and a hop. **Recommendation: A**,
  opt-in behind `candidateFailover`.
- **OQ-2 — LOAD-BEARING: hung or slow-retrying candidates.** Tracked in
  RIG-5147. Without a budget, a black-holed upstream or a 529 with long
  retry-after hints never fails over. **Recommendation:** the hard attempt
  budget in Bounds. Cost: when a follower exists, a first token slower than
  the budget is cut, even after a 2xx.
- **OQ-3 — LOAD-BEARING: silent hold or early headers.** Tracked in RIG-5147. A
  silent hold names the answering backend in headers, but can trip a proxy
  idle timeout (nginx defaults to 60 s) and drops the Anthropic route's 15 s
  pings. **Recommendation:** early headers plus `: keepalive` comments. Cost:
  streaming `x-litellm-model-id` names the requested model.
  `auth-gateway-response-headers.test.ts` runs without the option and is
  unchanged.
- **OQ-4 — LOAD-BEARING: mark scope.** Tracked in RIG-5147.
  **Recommendation:** per tenant only. Rejected: promoting a mark to
  process-wide once several tenants set it, because a tenant-triggerable
  failure would still steer other tenants. Cost: a shared outage's stall
  repeats per active tenant.
- **OQ-5 — LOAD-BEARING: usage for failed attempts.** Tracked in RIG-5147.
  Dropping it hides spend that failover creates. **Recommendation:** record
  every nonzero attempt under an attempt-qualified request id.
- **OQ-6 — not load-bearing: marks across replicas.** Tracked in RIG-5147.
  Revisit if a deployment runs many replicas.
- **OQ-7 — not load-bearing: mid-stream failover and non-chat routes.** Tracked
  in RIG-5147. Both stay out of scope.
