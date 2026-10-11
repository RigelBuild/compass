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
  (`StableNameResolver`, the dry-run peek) and the gateway record's T3 caller
  identity. Neither is in the fork: `ModelResolver` in
  `packages/ai/src/auth-gateway/dispatch.ts` is still
  `(modelId: string) => Model<Api> | undefined`.
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
resolve ─▶ attempt: credential, lease, streamSimple or completeSimple
  hold events until the first meaningful event
    committed            ─▶ respond from this candidate
    failed before commit ─▶ classifyUpstreamFailure
      stop          ─▶ return the failure
      failover kind ─▶ onFailure, release lease, next candidate
```

### Commit point

An attempt commits at its first event for which `isMeaningfulCompletionEvent`
(`packages/ai/src/utils/empty-completion-retry.ts`) is true: a non-empty text,
thinking, or tool-call delta, or an image. `withReplaySafeStreamRetry` commits
on the same predicate, so the package keeps one meaning of "committed". `start`
does not count: the Anthropic provider pushes it just above
`// Retry loop for transient errors from the stream.`, before any upstream byte.

Until commit, a relay `AssistantMessageEventStream` buffers events; at commit
it replays them and forwards the rest. A committed attempt is final: a later
error reaches the client as today, and no follower is tried.

A streaming response sends 200 and the SSE headers when its first attempt's
`streamSimple` returns, where today's handlers return their `Response`. It then
writes a `: keepalive` comment every 15 s until commit (OQ-3). SSE parsers skip
comment lines (`readSseEvents` in `packages/utils/src/stream.ts`), so this
works for all four formats. At commit the route runs `encodeStream` on the
relay. On these streams `x-litellm-model-id` names the requested model and
`x-litellm-model-api-base` is omitted; the log and usage event name the backend
that answered.

Setup failures (no credential, or a synchronous `streamSimple` throw) are
failed attempts, classified like upstream failures. A failure reaches the
client only when no follower remains: as today's HTTP error `Response` if
headers are not sent yet, else as an error `AssistantMessage` that ends the
relay, which `encodeStream` writes as the format's SSE error.

Non-streaming has no commit point: the client gets nothing until
`completeSimple` settles, so any classified failure fails over, whatever
partial content the message holds.

### Which failures fail over

`classifyUpstreamFailure` in `packages/ai/src/error/gateway.ts` composes the
existing classifiers. Its text is `errorClassificationMessage ?? errorMessage`,
as both handlers already pass to `classifyGatewayError`. Rows apply in order;
"reason" is `parseRateLimitReason(text)`.

| Failure before commit | Result |
| --- | --- |
| Request aborted by the client | stop |
| Attempt aborted by the loop at its budget | `transport` |
| Usage limit (`AIError.isUsageLimit` or `isUsageLimitOutcome`) | `credential_exhausted` |
| 401, or 403 that is not a concurrency cap | `credential_exhausted` |
| 403 concurrency cap (`isConcurrencyCapExclusion`), or 429 | `rate_limited` |
| 404 | `not_served` |
| Any other 4xx except 408 | stop |
| 408 or 5xx | `server_error` |
| No status; reason `RATE_LIMIT_EXCEEDED` or `CONCURRENT_LIMIT` | `rate_limited` |
| No status; reason `MODEL_CAPACITY_EXHAUSTED` or `SERVER_ERROR` | `server_error` |
| No status; `isProviderRetryableError` (connect, DNS, TLS, socket close) | `transport` |
| Anything else | stop |

A usage limit, 401, or credential 403 reaches the gateway only after rotation
is exhausted: `refreshGatewayApiKeyAfterAuthError` in `dispatch.ts` records it
(`markReached` or `invalidateMatching`), which v1's peek honors, so no mark is
needed. A 403 concurrency cap is never rotated (`isRetryableUpstreamError` in
`packages/ai/src/stream.ts` excludes it), so it is marked as `rate_limited`. A
404 can be request-specific, so `not_served` sets no mark; later requests try
the candidate again and pay one fast failure, since providers do not retry 4xx.

`retryAfterMs` is `getRetryAfterMsFromHeaders(getHeadersFromError(error))`,
else `extractProviderRetryHint(provider, text)`
(`packages/ai/src/utils/retry-after.ts`).

### The provider-down mark

`transport`, `server_error`, and `rate_limited` set a mark;
`credential_exhausted` and `not_served` do not. `transport` keys on provider
and `baseUrl`, since a host that refuses connections is down for every model.
`server_error` and `rate_limited` also key on model id, since overload errors
are often per model.

TTL starts at `calculateRateLimitBackoffMs("SERVER_ERROR")` (20 s), or
`calculateRateLimitBackoffMs("RATE_LIMIT_EXCEEDED")` (30 s) for `rate_limited`
(`packages/ai/src/error/rate-limit.ts`). It doubles per consecutive failure:
20, 40, 80, 160, then a 300 s cap. It is never shorter than `retryAfterMs`
(also capped at 300 s). Strikes reset when the last mark expired over 300 s ago.

Marks are per tenant only (OQ-4): a mark set by one tenant's request never
affects another tenant.

Marks live in memory in the compass boot entrypoint: one map per gateway
process. A self-hosted stack runs one process; with several replicas, each
learns an outage on its own. Both shapes run the same code, per
`docs/concepts/self-host-and-managed.md` and
`docs/designs/meta/oss-core-managed-boundary/design.md`. Until a mark exists,
each request that reaches the down candidate stalls for up to its attempt
budget. That repeats per tenant, per replica, per TTL.

v1's `StableNameResolver` walks the chain in order, skips candidates in `tried`,
and defers marked ones. A marked candidate wins only when no unmarked one has a
usable credential, so marks never remove the last usable candidate and a
single-candidate chain ignores its mark.

### Bounds

- **Attempts.** One per distinct candidate. The first comes from
  `bootOpts.resolveModel` as today; each follower from the side-effect-free
  `candidateFailover.next`. `undefined` from `next` means no follower, never a
  404. Compass returns `undefined` for a direct `provider/id` selector, so
  those get one attempt.
- **Attempt budget (OQ-2).** Only when a follower exists; the last attempt runs
  as today. The budget is a hard deadline through the first meaningful event:
  half the time left before `deadlineMs` (default 120 000 ms). It covers the
  provider's in-flight retries. When it expires the loop aborts the attempt,
  even after a 2xx, and classifies it `transport`. The attempt also gets
  `maxRetryDelayMs: 10_000`. Anthropic then fails fast on a longer retry-after
  hint (`maxRetryDelayMs` in `packages/ai/src/types.ts`); other providers only
  cap their sleep or ignore it, as the OpenAI-family transport does. The
  deadline covers all of them.
- **Deadline.** No attempt starts after `deadlineMs`.
- **Abort.** Each attempt has its own `AbortController`, linked to the request
  controller from `mirrorRequestAbort`. A budget abort stops only that attempt,
  so its follower still runs. A request abort stops every attempt. Cancelling
  the hold body aborts the request, as `encodeStream`'s `onCancel` does today.
- **Leases.** Keyed by provider and model (`sessionKeys` in
  `session-state.ts`), so each attempt acquires its own. A failed attempt's
  lease is released before the next starts.
- **Usage (OQ-5).** Every attempt goes through `recordGatewayUsage`, which
  skips zero usage. An attempt that did not answer uses request id
  `${requestId}:a${n}` and outcome `error`, because `onUsage` consumers dedupe
  on request id. A failed attempt can carry input usage: Anthropic sets
  `output.usage.input` at `message_start`.

### Observability

- **Log.** `logger.warn("auth-gateway candidate failover", …)` per classified
  failure, with the `auth-gateway request` log's keys (`requestId`, `format`,
  `model`, `resolvedProvider`, `resolvedModel`, `peer`) plus `attempt`,
  `failure`, `status`, `retryAfterMs`, `elapsedMs`, `outcome` (`retrying`,
  `exhausted`, `deadline`, `budget`), and the next candidate.
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
- **Provider retries only.** They retry the same upstream and cannot change
  candidate.
- **Commit on `start`.** Anthropic emits `start` before its first upstream
  call, so an outage would commit.
- **Marks in the Server store.** Shared across replicas, but adds RPC work to
  every resolve.

## Plan

Order: F1 → F2a → F2b → F2c land on fork `main` against today's resolver. C1
waits for v1 P1, gateway T3, and the compass boot entrypoint. Lane: compass-obs
for every task.

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
  | "not_served";

export interface UpstreamFailure {
  readonly kind: UpstreamFailureKind;
  readonly status?: number;
  readonly retryAfterMs?: number;
  readonly message: string;
}

/** `undefined` means stop. `message` is `errorClassificationMessage ?? errorMessage`. */
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
- 404 gives `not_served`; 503 gives `server_error`.
- Status-less Anthropic stream errors, as `createAnthropicSseStreamError` in
  `packages/ai/src/providers/anthropic.ts` builds them:
  `Anthropic stream error (rate_limit_error): Rate limit exceeded` gives
  `rate_limited`; `Anthropic stream error (overloaded_error): Overloaded` gives
  `server_error`. Neither carries a status.
- Status-less `fetch failed` gives `transport`.
- A status-less error whose `errorMessage` reads as a socket failure but whose
  `errorClassificationMessage` reads `Too many requests` gives `rate_limited`.
- 400 and `AbortError` stop.

### F2a — Fork: extract the per-attempt body

Move each handler's per-attempt setup (credential, lease, stream options) into
one function per route. It takes the attempt's signal and returns a
`GatewayErrorClassification` on failure, which the handler formats as today.
No behavior change: the existing `auth-gateway-*.test.ts` suite stays green
unchanged.

Interfaces (module-private in `server.ts`):

```ts
type AttemptStart =
  | { readonly ok: false; readonly failure: GatewayErrorClassification }
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

Add `packages/ai/src/auth-gateway/failover.ts` and export
`isMeaningfulCompletionEvent`.

Interfaces:

```ts
export type HeldAttempt =
  | { readonly kind: "committed"; readonly relay: AssistantMessageEventStream }
  | { readonly kind: "failed"; readonly relay: AssistantMessageEventStream; readonly error: AssistantMessage }
  | { readonly kind: "budget"; readonly relay: AssistantMessageEventStream };

/** Buffers until a meaningful event or `done` (committed), an `error` (failed), or `budgetEndsAt` (budget). */
export function holdUntilCommit(
  events: AssistantMessageEventStream,
  budgetEndsAt: number | undefined,
): Promise<HeldAttempt>;

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

Test cycle, on a hand-driven `AssistantMessageEventStream`: `start` then a
`text_delta` commits and replays both in order; a committed relay forwards a
later `error` unchanged; `start`, an empty `text_start`, then `error` fails;
silence past the budget gives `budget`. `sseHoldBody` writes comments before
the body bytes, and `cancel` calls `onCancel`.

### F2c — Fork: the loop and boot option

Wire the loop into both handlers when `bootOpts.candidateFailover` is set, and
add the log.

Interfaces:

```ts
// dispatch.ts — AuthGatewayRouteOptions gains `candidateFailover?: CandidateFailoverOptions`.
export interface FailoverContext {
  /** The inbound request, as an opaque token; the fork reads no identity from it. */
  readonly req: Request;
  /** `candidateKey` of every candidate tried so far, including the current one. */
  readonly tried: ReadonlySet<string>;
}

export interface CandidateFailoverOptions {
  /** The candidate after `ctx.tried`, without side effects; `undefined` if none. */
  next(modelId: string, ctx: FailoverContext): Promise<Model<Api> | undefined>;
  /** Called once per classified failure. Must not throw. */
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
- A's `fetch` never resolves, or A answers 200 and then sends nothing: A is
  aborted at its budget, the request signal stays live, and B answers.
- A emits a `text_delta`, then errors: B is never tried, and the client gets
  A's text and A's error.
- A returns a usage limit with one credential: B answers; the failure is
  `credential_exhausted`.
- Streaming, B has no credential after headers are sent: the client gets an
  SSE error in the open 200 body.
- A answers 400: no failover.
- A direct selector fails: one attempt, the original error.
- Both down: the last error as today.
- The client disconnects during the hold: no further attempt starts.
- `onUsage` fires once per nonzero attempt, with distinct request ids.
- Every attempt's lease is released: as in
  `auth-gateway-provider-session-state.test.ts`, a probe put in each attempt's
  `providerSessionState` is closed once the store fills to
  `AUTH_GATEWAY_MAX_SESSION_STATES`.

### C1 — Compass: marks, resolver, counter

Prerequisites: v1 P1, gateway T3, and the compass boot entrypoint. The
gateway-image record names `packages/coding-agent/src/cli/gateway-boot.ts`; the
fork today wires `startAuthGateway` in `cli/auth-gateway-cli.ts`.

Bump `tools/gateway-image/fork-pin.json` to F2c's commit. Add one
`ProviderDownMarks`. Make `StableNameResolver` skip `tried` and defer marked
candidates. Pass `candidateFailover`: `next` maps `ctx.req` to its caller
through a `WeakMap<Request, CallerIdentity>` filled by T3's `authorize`;
`onFailure` marks and counts.

Interfaces:

```ts
export class ProviderDownMarks {
  constructor(now?: () => number);
  /** Sets or extends a tenant's mark; ignores `credential_exhausted` and `not_served`. Returns the TTL in ms. */
  mark(tenant: UserId, model: Model<Api>, failure: UpstreamFailure): number;
  /** True if this tenant's mark covers (provider, baseUrl) or (provider, baseUrl, id). */
  isDown(tenant: UserId, model: Model<Api>): boolean;
}

// StableNameResolver (v1 P1) gains the tried set:
resolve(callerUser: UserId, modelId: string, tried?: ReadonlySet<string>): Promise<Model<Api> | undefined>;
```

Test cycle: with an injected clock, TTLs run 20, 40, 80, 160, 300;
`retryAfterMs` raises the TTL; strikes reset after 300 s. A `transport` mark
covers every model on its `baseUrl`; a `server_error` mark covers only its
model. A mark set for one tenant never affects another. Resolver: a marked
first candidate is deferred; when all are marked the first wins; a single
candidate ignores its mark; `tried` is skipped; a direct selector gets no
follower. Smoke: the gateway image smoke passes at the new pin.

## Tasks

- [ ] F1 — `classifyUpstreamFailure` and table tests (compass-obs).
- [ ] F2a — Per-attempt extraction, no behavior change (compass-obs).
- [ ] F2b — `holdUntilCommit`, `endWithFailure`, `sseHoldBody`, exported
      commit predicate (compass-obs).
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
