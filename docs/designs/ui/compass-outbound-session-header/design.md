# Outbound `X-POSTHOG-SESSION-ID` interceptor + boot reorder (RIG-2874 slice 2)

Status: Active
Tracker: RIG-2874

Note on Go citations: this design was authored in a working copy whose `go/` tree predates the merged server half. Every `go/**` file+line below was read from `origin/main` at squash commit `69669259` ("feat(otel): stamp the PostHog session id onto backend spans (J1 inbound half) (#996)"), via `git show origin/main:<path>`. All `apps/ui/**` and `packages/**` citations are the working copy.

## Problem / Intent

The J1 correlation seam is half-live. The server's inbound half is merged: `go/internal/otel/interceptor.go:66-79` (`NewSessionIDInterceptor`) stamps semconv `session.id` onto the handler span from the `X-POSTHOG-SESSION-ID` request header, and the CORS door admits the header (`go/server/network_door.go:168`). But nothing sends it — the UI's boot comment says so outright:

> `// The OUTBOUND half (sending the PostHog session id to the server so its`
> `// spans carry it) is deliberately not wired here.`

— `apps/ui/src/index.tsx:119-120`

That absence was Matt's RIG-3233 hold (2026-09-05), for two reasons, both now discharged:

1. **No inbound consumer** — expired. The server reader merged; `X-POSTHOG-SESSION-ID` appears in `go/internal/otel/interceptor.go` (const at `:44`), its test file, and the CORS builder on `origin/main`.
2. **Boot ordering** — the outbound header would have needed a lazy getter over a not-yet-existing analytics handle. Dissolved by reordering boot (approved by Matt today; see Approach): analytics is constructed *before* the clients, so the transport interceptor closes over a real object.

This record designs the outbound half: the boot reorder, an `Analytics.sessionId()` accessor, and a transport request interceptor that sets the header — with sender-side validation mirroring the server's limits.

## Approach

Two moves, in dependency order: reorder boot so analytics exists before the transport, then add a request interceptor (in `@compass/client`, beside the existing `traceresponse` response interceptor) fed by a lazy `sessionId` getter.

### The boot reorder (chosen — approved by Matt)

Current order in `apps/ui/src/index.tsx` `main()`:

1. `:91` — `const clients = createLiveClients(connection);`
2. `:93` — `const callerId = await bootCaller(root, () => resolveCaller(clients.compass));`
3. `:121-123` — `const analytics = createAnalytics(analyticsConfigFromEnv(), { traceId: () => clients.traceId.current });`
4. `:124` — `analytics.identify(callerId);`

Verified dependency edges:

- `createLiveClients(conn)` needs only the connection — `apps/ui/src/live/client.ts:57`: `export function createLiveClients(conn: ResolvedConnection): LiveClients {`. Pure construction, no I/O (`client.ts:50`: "Pure construction (no I/O)").
- `bootCaller(...)` needs the clients (`index.tsx:93`, above).
- `createAnalytics(config, deps?)` needs only config + an optional deps bag — `apps/ui/src/analytics/analytics.ts:127-129`: `export function createAnalytics(config: AnalyticsConfig | undefined, deps?: { posthog?: PostHog; traceId?: () => string | undefined })`. It is not downstream of clients or caller.
- `analytics.identify(callerId)` is a separate statement (`index.tsx:124`) needing only `callerId`.

Target order:

1. `createAnalytics(analyticsConfigFromEnv(), { traceId: () => clients.traceId.current })`
2. `createLiveClients(connection, { sessionId: () => analytics.sessionId() })`
3. `bootCaller(...)` → `callerId`
4. `analytics.identify(callerId)`

This **deletes** the lazy-getter-over-nothing problem rather than working around it: analytics exists before the transport is built, so the session-id getter the transport closes over references a real object — no mutable ref slot, no forward `let`, no init-order workaround.

The `traceId` getter stays a **forward** reference — and after the reorder its
laziness is load-bearing rather than merely tidy. Step 1 constructs analytics
with a closure over `clients`, which step 2 has not created yet, so the
closure body must not run until step 4 or later. This is the asymmetry that
makes the reorder work: a forward reference through a *lazy getter* is safe
(the binding is read at call time, long after step 2), whereas alternative (b)
below fails precisely because it forward-references a `let analytics` whose
binding is read during construction. Same direction, different timing — the
timing is the whole distinction. Two grounded reasons the getter is already
lazy, so no change is needed to it:

- The value is only populated by replies: "the clients exist before this line, but the first trace id only lands once a call has returned" — `index.tsx:108-110`. (That comment documents the *getter's* laziness, not a boot-ordering constraint — do not read it as one.)
- `PostHogAnalytics` already reads it at capture time, not construction time: the getter is stored at `analytics.ts:61` (`this.traceId = traceId;`) and invoked at `analytics.ts:77` (`const traceId = this.traceId();`), documented at `analytics.ts:47-49`: "The trace-id source, read at CAPTURE time rather than construction time".

Both getters resolve at call time, pointing opposite ways: `traceId` reads transport state from analytics; `sessionId` reads analytics state from the transport. The layering rule at `analytics.ts:51-53` is preserved on both sides: "A getter, not the sink object, on purpose — analytics reads one string and has no business depending on compass-client's transport types, so the layering stays one-directional." Symmetrically, the transport receives a `() => string | undefined`, never an `Analytics` reference.

#### Two consequences of constructing analytics earlier

The `PostHogAnalytics` constructor is **not** inert — it calls
`client.init(config.key, {...})` at `analytics.ts:66-73`. Moving it ahead of
`bootCaller` therefore moves a real initialization earlier in boot. Both
resulting changes are acceptable, and both are stated here so a reviewer does
not have to rediscover them:

1. **The WhoAmI failure path now leaves posthog initialized but never
   identified.** `bootCaller` returning `undefined` is a stop signal — "it
   already painted the WhoAmI failure screen, so the app must not come up"
   (`index.tsx:94-95`) — and the function returns at `:96-97` before
   `analytics.identify(callerId)` ever runs. Under the current order analytics
   does not exist on that path; under the new one it is initialized and
   anonymous.

   Acceptable, but not free: none of the **five** headless-off flags this app
   actually sets suppresses **init-time network egress** — `client.init` mints
   an anonymous distinct id and session and dials PostHog's remote-config
   endpoint. So an analytics-enabled deployment now emits an anonymous PostHog
   session on a failed boot where it previously emitted nothing. Nothing is
   *captured* (`autocapture: false`, `capture_pageview: false`,
   `capture_pageleave: false`, `disable_session_recording: true`,
   `disable_surveys: true` at `analytics.ts:66-73`, and no code path calls
   `capture()` before the early return), and the client is left
   initialized-but-unidentified with no
   `shutdown()` — `analytics.shutdown()` is never invoked anywhere in
   `apps/ui/src` today, so the reorder changes nothing about teardown.

   A flag that *would* suppress that egress does exist, and this design
   **declines** it rather than claiming it is absent: `advanced_disable_flags`.
   The installed typings declare the key —
   `node_modules/.bun/node_modules/@posthog/types/dist/posthog-config.d.ts:1624`:
   `advanced_disable_flags?: boolean;` — and the remote-config loader
   short-circuits on it before any request is made
   (`apps/ui/node_modules/posthog-js/lib/src/remote-config.js:53-57`):

   ```js
   if (this._instance._shouldDisableFlags()) {
       // This setting is essentially saying "dont call external APIs" hence we respect it here
       logger.warn('Remote config is disabled. Falling back to local config.');
       return;
   }
   ```

   `_shouldDisableFlags()` reads exactly that config key
   (`apps/ui/node_modules/posthog-js/lib/src/posthog-core.js:4033-4038`:
   `if ('advanced_disable_flags' in originalConfig) { return !!originalConfig.advanced_disable_flags; }`).
   Declining it is a trade with a named cost: the same typings warn
   "Disabling this will also prevent remote configuration from loading, which
   could mean features like web vitals, surveys, and other features configured
   in PostHog settings are disabled unless explicitly enabled via client-side
   config" (`posthog-config.d.ts:1616-1620`). This record keeps remote config
   live and accepts the anonymous-session egress instead.

   So the trade is **chosen, not forced**. Two halves: the reorder itself is
   unavoidable given the transport needs the session-id getter at
   construction, so `createAnalytics` cannot move back behind the
   `if (!callerId)` gate; the *egress* is avoidable, by a flag declined above
   at the cost of remote config. Stated explicitly rather than silently,
   because it only bites a deployment with a PostHog key configured.
2. **`get_session_id()` may legitimately return an empty string early.** The
   installed typings say so: "This may be an empty string if the client is
   … not initialized" (`apps/ui/node_modules/posthog-js/dist/module.d.ts:4210`).
   The guard's `+` quantifier rejects `""`, so an early request simply carries
   no header — the same degraded state as analytics-off, and self-healing on
   the next request once a session exists. This is why the getter returns
   `string | undefined` and the interceptor re-reads it **per request** rather
   than caching one value at construction.

### The outbound interceptor

A new request interceptor in `packages/compass-client/src/index.ts`, sibling to `traceResponseInterceptor` (`index.ts:143-162`), composed by `callInterceptors` (`index.ts:170-179`) under the same omitted-means-off discipline already documented there: "no sink ⇒ no trace interceptor at all, so a caller that does not ask for correlation gets byte-identical behavior" (`index.ts:167-168`).

```typescript
/** The PostHog session-id REQUEST header the server's J1 interceptor reads
 *  (go/internal/otel/interceptor.go, PostHogSessionHeader). */
const posthogSessionHeader = "X-POSTHOG-SESSION-ID";

/** Sets X-POSTHOG-SESSION-ID on every request from a lazy session-id source.
 *  No usable value (undefined, empty, oversized, non-ASCII) ⇒ the header
 *  is not set at all — never an empty header. */
export function sessionIdInterceptor(
  sessionId: () => string | undefined,
): Interceptor {
  return (next) => (req) => {
    const id = sessionId();
    if (id !== undefined && isSendableSessionId(id)) {
      req.header.set(posthogSessionHeader, id);
    }
    return next(req);
  };
}
```

Wiring, one seam per layer, each mirroring an existing shape:

- `createCompassWebTransport` gains `opts.sessionId?: () => string | undefined` beside the existing `opts.traceSink` (`index.ts:212`: `opts?: { fetch?: typeof globalThis.fetch; traceSink?: TraceIdSink }`), threaded into `callInterceptors`.
- `callInterceptors` (`index.ts:170-179`) composes it exactly as it composes the trace sink: absent ⇒ no interceptor installed, `undefined` list when nothing is asked for.
- `createLiveClients` gains a deps bag: `createLiveClients(conn: ResolvedConnection, deps?: { sessionId?: () => string | undefined })`, passing it through to `createCompassWebTransport` beside `traceSink` (`apps/ui/src/live/client.ts:59-62` builds the transport with `{ fetch: conn.fetchImpl, traceSink: traceId }`). The optional-deps-bag shape copies `createAnalytics`'s own rationale at `analytics.ts:123-126`: the call site names the collaborator and every existing caller keeps working unchanged.
- `index.tsx` `main()` performs the reorder and passes `{ sessionId: () => analytics.sessionId() }`.

The interceptor applies to both unary and stream requests (Connect interceptors see every request). The server only *reads* it on unary (`NewSessionIDInterceptor` is a `connect.UnaryInterceptorFunc`, `interceptor.go:66`), which is harmless: an unread request header costs bytes, not correctness. See Open Questions for whether to gate it to unary.

## The `Analytics.sessionId()` addition

The `Analytics` interface (`analytics.ts:25-32`) is today `capture`/`identify`/`shutdown`:

```typescript
export interface Analytics {
  /** Record a product event with optional properties. */
  capture(event: string, props?: Record<string, unknown>): void;
  /** Associate subsequent events with a stable distinct id (the caller). */
  identify(distinctId: string): void;
  /** Tear down the identified session (logout / app teardown). */
  shutdown(): void;
}
```

There is no session-id accessor. Add one:

```typescript
/** The current PostHog session id, or undefined when analytics is disabled
 *  or the client has no session yet. Never an empty string. */
sessionId(): string | undefined;
```

- **`NoopAnalytics`** (`analytics.ts:36-40`, today three empty methods: `capture(): void {}` / `identify(): void {}` / `shutdown(): void {}`) adds `sessionId(): string | undefined { return undefined; }` — the disabled path stays zero-posthog-calls.
- **`PostHogAnalytics`** delegates to posthog-js's `get_session_id()`, verified against the installed package `posthog-js@1.418.10` (`apps/ui/package.json` pins `^1.418.10`; installed at `apps/ui/node_modules/posthog-js`). From `apps/ui/node_modules/posthog-js/dist/module.d.ts:4201-4212`, on `declare class PostHog` (`module.d.ts:2905`):

  > Returns the current session_id. … This may be an empty string if the client is not yet fully initialized.
  >
  > `get_session_id(): string;`

  Because it can return `""`, `PostHogAnalytics.sessionId()` maps empty to `undefined`:

  ```typescript
  sessionId(): string | undefined {
    const id = this.client.get_session_id();
    return id === "" ? undefined : id;
  }
  ```

`distinct_id` is **permanently excluded** from the header plane. It identifies a person; backend spans land in Grafana/Tempo, and J1 deliberately keeps identity out of that plane — the server comment says exactly this: "either side can be pivoted to the other by the shared key — without either system holding the other's data, which is what keeps the join billing-safe" (`origin/main` `go/internal/otel/interceptor.go:59-61`). Identity resolution stays PostHog-side, where `identify()` already has it (`analytics.ts:105-107`).

## Validation before send

The server enforces two limits and silently returns `""` (drops the value, no error) on violation — `origin/main` `go/internal/otel/interceptor.go:95-101`:

```go
func sessionIDFromHeader(req connect.AnyRequest) string {
  id := strings.TrimSpace(req.Header().Get(PostHogSessionHeader))
  if id == "" || len(id) > maxSessionIDLen || !utf8.ValidString(id) {
    return ""
  }
  return id
}
```

A UI that sends a violating value gets **silent data loss**, not an error:
`sessionIDFromHeader` returns `""` and no `session.id` is stamped on any span
of that request. The sender-side check cannot simply mirror the server's two
limits, because a third constraint binds *before* the value reaches the wire —
and it is the one that decides the design.

### The wire seam is a WebIDL ByteString, not a UTF-8 string

`req.header` is a fetch `Headers` object, and `Headers.set` takes a WebIDL
`ByteString`. The accept/throw column below was measured in this repo's
runtime (Bun) and verified identical in real Chromium, so Bun is an adequate
proxy for *that* property. Wire-serialization and server-receipt claims are
**browser-measured**, because Bun's `fetch` and `Bun.serve` do not agree with
a browser on non-ASCII header bytes — see the methodology warning below.

| Value | Measured behaviour |
| --- | --- |
| `0199a1b2-…` (ASCII UUID) | OK, readback 36 bytes |
| `sess-é` (U+00E9) | OK — accepted by `set`; a browser emits it as one **raw high byte**, which the server rejects as invalid UTF-8 and **DROPS** |
| `sess-日本語` (> U+00FF) | **THROWS `TypeError`** |
| `sess-😀` (> U+00FF) | **THROWS `TypeError`** |
| `sess-\ud800` (lone surrogate) | **THROWS `TypeError`** |
| `"a\r\nb"` (CRLF) | **THROWS `TypeError`** — `Header 'X-T' has invalid value` |

Two consequences, both fatal to a UTF-8-shaped validator:

1. A **well-formed** id containing any code point above U+00FF passes a
   `≤200 bytes` + well-formed check and then makes `header.set` **throw inside
   the interceptor**, failing the entire RPC. An analytics nicety that can
   kill every request is strictly worse than any sender-side rejection.
2. U+0080–U+00FF does *not* throw — it is accepted by `set` and then
   **dropped by the server**, silently. `Headers.set` takes it, the browser
   serializes it as a **single raw high byte** on the wire, Go's HTTP parser
   admits high bytes, and then `!utf8.ValidString(id)` (`interceptor.go:97`)
   is **true**, so `sessionIDFromHeader` returns `""` and no `session.id` is
   stamped. The same failure class as every other value the guard rejects —
   silent loss, not a wrong key.

   Wire bytes for `"sess-é"` as observed by a **raw TCP listener** — nothing
   between the socket and the hex dump, so these are the octets the client
   actually wrote:

   | Client runtime | Header value on the wire | Strict UTF-8 decode of those octets |
   | --- | --- | --- |
   | Node/undici `fetch` | `73 65 73 73 2d e9` — 6 bytes, raw high byte `e9` | **throws**; `utf8.ValidString` would be **false** |
   | Bun `fetch` | `73 65 73 73 2d c3 a9` — 7 bytes, the UTF-8 pair | OK |

   Undici implements the same WebIDL `ByteString` serialization a browser
   follows, and the shipping client here **is a browser**: `apps/ui` speaks
   gRPC-Web through `createCompassWebTransport`. Bun's `fetch` is the outlier,
   not the reference.

   End-to-end confirmation — real Chromium into a real Go `net/http` server
   whose handler ran a **verbatim copy** of `sessionIDFromHeader`:

   | Observation | Value |
   | --- | --- |
   | raw header bytes | `736573732de9` |
   | `len(raw)` | `6` |
   | `utf8.ValidString` | **false** |
   | `sessionIDFromHeader` | `""` — **dropped**, no `session.id` |

   > **Methodology warning — do not re-derive the opposite conclusion.** A
   > Bun-client-to-`Bun.serve` round-trip measures Bun's own encode/decode
   > pair, not the wire. Bun's `fetch` UTF-8-encodes the header (`c3 a9`), and
   > `Bun.serve` then reads those two octets as two Latin-1 characters and
   > re-encodes them (`c3 83 c2 a9`) — which is the entire source of the
   > apparent `sess-é` → `sess-Ã©` "mangling". It appears only when **both**
   > ends are Bun, and it is **not valid evidence about browser wire bytes**.
   > An earlier revision of this record concluded from exactly that setup that
   > the value arrives mangled-but-valid and the server *accepts* a wrong id.
   > That is retracted. Measure the wire with a raw socket, and the receiver
   > with a real browser against a real Go server.

   The server's own comment says why dropping is the right receiver-side
   answer: "a truncated or re-encoded id joins to nothing in PostHog, so it
   would add span weight while still failing the pivot, and a silent half-key
   is harder to notice than an absent one" (`interceptor.go:83-86`). But a
   dropped key is still a lost key, and the client is the only place that can
   tell an intended id from a non-transmissible one. That is precisely why the
   sender-side check cannot be UTF-8-shaped: a UTF-8-shaped validator finds
   `"sess-é"` perfectly well-formed and hands it to a wire that cannot carry
   it. (Secondary, and a JS-side fact only: for such a value `.length` (6)
   does not equal `TextEncoder().encode(id).length` (7), so a UTF-8-shaped cap
   would need `TextEncoder` to mean anything at all.)

### Therefore: reject anything non-ASCII, before `header.set`

`isSendableSessionId(id)` is a printable-ASCII test plus a length cap:

```ts
const SENDABLE = /^[\x21-\x7E]+$/;
const MAX_SESSION_ID_LEN = 200; // mirrors maxSessionIDLen (interceptor.go:53)

function isSendableSessionId(id: string): boolean {
  return SENDABLE.test(id) && id.length <= MAX_SESSION_ID_LEN;
}
```

This is strictly simpler than a UTF-8 validator, strictly **stronger** than
the server's own `≤200 bytes` + valid-UTF-8 pair, and strictly **narrower**
than `Headers.set` itself. That last direction is why "stronger" alone would
overreach: `Headers.set` accepts space (`0x20`) and tab (`0x09`) — measured in
the same runtime, both set and read back — and the guard rejects both, so the
guard is not stronger than `Headers.set` in both directions; it is a strict
subset of what `Headers.set` will take. The extra narrowing is deliberate: the
server trims and drops a whitespace-only value anyway
(`interceptor.go:96-98`), and a real `get_session_id()` contains neither byte.
Verified by execution — every value the guard accepts is `header.set`-safe and
has `.length === TextEncoder().encode(id).length`, so:

- **`TextEncoder` is not needed.** On accepted input `.length` *is* the UTF-8
  byte count, so the cap mirrors Go's `len()` exactly.
- **The well-formedness check is subsumed.** Lone surrogates are non-ASCII.
  This retires Open Question 3 entirely — no `isWellFormed`, no
  encode/decode round-trip fallback, no `lib` concern.
- **It rejects CRLF *before* `header.set`, which would otherwise throw on
  it.** `\x21-\x7E` excludes space (`0x20`) and every control byte, so a value
  containing `\r\n` never reaches `header.set`. CRLF is the same class as
  `> U+00FF`, not an injection that ships: `Headers.set` itself throws a
  `TypeError` on CRLF in this runtime (measured, table above), so the guard's
  job here is to fail the value quietly rather than fail the whole RPC. A
  UTF-8-validity check would have waved CRLF through to that throw.

The `≤200` cap still mirrors `interceptor.go:53`, whose rationale at `:46-52`
is: "The value is attacker-controlled … a span attribute flows to the trace
backend, so an unbounded copy would let one request carry an arbitrarily large
blob into Tempo. PostHog session ids are UUID-shaped; 200 leaves room for a
format change without admitting a payload."

The server's UTF-8 check (`interceptor.go:97`) is load-bearing against
**every** sender, this client included. Its own comment at `:88-94` names what
it holds back: "Go's HTTP parser rejects control bytes but ADMITS high bytes,
OTLP span attributes are proto3 strings, and the protobuf marshaller fails the
whole ExportTraceServiceRequest on an invalid one — so a single bad header
would drop every span batched with it, including other callers'. Since this
interceptor deliberately runs ahead of auth, that would be an unauthenticated
observability denial-of-service, surfaced only in the exporter's own logs."

Two things follow. First, the senders that can put a raw high byte on the wire
are not just hand-rolled ones — `curl`, a proxy, a custom client. **A browser
can too** (measured, above), so this client is inside the set that check
defends against, not outside it. Second, the check holds:
`sessionIDFromHeader` returns `""` before the invalid string can
become a span attribute, so the batch-poisoning path the comment describes is
**not reachable through this header**. The denial-of-service concern is the
server's, is about a whole OTLP export batch, and is already closed by the
server's own check; a single value dropped here is not a DoS — it costs
exactly one missing `session.id`.

The ASCII guard is therefore **defense-in-depth**: it keeps a value that
cannot survive the wire off the wire in the first place, at the only point
where the intended id and the transmitted bytes are still known to agree.

Real values are unaffected: `get_session_id()` returns a UUIDv7-shaped ASCII
string, so the guard is not an expected path — it is the belt to the server's
braces.

A value failing the guard ⇒ the header is not set (see Degradation), matching
the server's own drop-don't-repair stance: "a truncated or re-encoded id joins
to nothing in PostHog, so it would add span weight while still failing the
pivot" (`interceptor.go:83-85`).

## Degradation

Both degraded states are **no header at all** — never an empty-string header:

- **Analytics off.** `analyticsConfigFromEnv()` returns `undefined` ⇒ `createAnalytics` returns `NoopAnalytics` (`analytics.ts:131-133`: `if (!config) { return new NoopAnalytics(); }`), whose `sessionId()` returns `undefined` ⇒ the interceptor's guard skips `req.header.set` entirely. Zero posthog calls, per the module contract (`analytics.ts:9-10`: "the config is `undefined` and `createAnalytics` returns a no-op that never CALLS posthog").
- **Session id not yet available.** `get_session_id()` returns `""` before full init (`module.d.ts:4210`); `PostHogAnalytics.sessionId()` maps that to `undefined` ⇒ no header.

An empty header would be worse than none on both sides: the server would trim-and-drop it anyway (`interceptor.go:96-98`), and it would spend preflight/wire bytes asserting a correlation that does not exist. This mirrors the established discipline for `$ai_trace_id` at `analytics.ts:79-81`: "Not even an `$ai_trace_id: undefined` key".

## Alternatives considered

Five alternatives are recorded below, from two different passes. (a)-(c) were
the real options put to Matt, and the reorder won on the merits. (d) and (e)
came out of the red-team pass afterwards; they were never live choices, and
they are recorded so a reviewer who thinks of them does not have to re-derive
why each is declined.

### (a) A mutable `sessionId` ref slot mirroring `clients.traceId`

Mirror the inbound shape: a `{ current: string | undefined }` slot on `LiveClients`, written by analytics after construction, read by the transport. This is exactly how `traceId` works today — `TraceIdSink` at `packages/compass-client/src/index.ts:65-67`, created and handed to the transport at `apps/ui/src/live/client.ts:58-62` — and `LiveClients.traceId`'s own doc explains *why* that shape exists there: "the sink is WRITTEN by the transport layer and READ above it … boot builds the clients before analytics exists, so a shared mutable slot handed out here is what connects a writer and a reader that can never meet at construction" (`client.ts:42-46`).

Rejected: the slot is a workaround for a construction-order problem the reorder deletes. Once analytics exists first, the writer and reader *can* meet at construction, and the slot would be a second mutable cell to keep fresh (who writes it, when — on init? on session rotation? posthog rotates session ids on inactivity) with a staleness window the direct getter simply doesn't have. `traceId` keeps its sink because its ordering problem is real and stays real; copying the shape where the problem no longer exists copies only the liability.

### (b) A getter over a forward `let analytics`

Keep boot order as-is; declare `let analytics: Analytics | undefined` before `createLiveClients` and pass `() => analytics?.sessionId()`.

Rejected: it preserves the problem and adds a TDZ-shaped hazard — a temporarily-`undefined` binding that every reader must guard, checked by nothing. Any future call on the clients between construction and analytics assignment silently sends no header; TypeScript can't distinguish "not yet assigned" from "analytics off". The reorder produces the same lazy getter with the `undefined` window deleted instead of guarded, at zero extra mechanism.

### (c) Keep holding

Rejected: both hold reasons are discharged. The server reader is merged and
reachable, so UI requests **through the network door** are spans missing their
`session.id` — and the failure-path spans the server deliberately stamps
*before* the handler runs ("the traces most worth pivoting to from a product
funnel", `interceptor.go:69-72`) are exactly the ones going un-keyed. The
remaining technical objection (boot ordering) is dissolved by the reorder, not
deferred.

**Scope limit — the reader is installed on ONE of three doors.**
`NewSessionIDInterceptor` appears only in `go/server/network_door.go` (`:300`,
`:308`). The Unix-socket and dev doors install
`NewTraceResponseInterceptor` but not the session reader — measured on
`origin/main`: `NewSessionIDInterceptor` = 0 hits in `go/server/serve.go`,
while `TraceResponseInterceptor` = 4 there (POS-CTRL: the same instrument
finds the sibling interceptor in that file, so the zero is a real absence, not
a failed query). The dev door's CORS builder nevertheless *allows* the header
(`serve.go:1041`), which reads as intent.

The native shell has **two** modes, and only one of them lacks a reader.
`go/cmd/compass-app/main.go:8-14` (on `origin/main`) states both: "The app
runs in one of two modes (appconfig, resolved at launch): — EMBEDDED: it
supervises a private stack in-process via the compass-stack CLI … then dials
the stack's Unix socket over h2c … — CLIENT: it dials a headless Compass stack
over the authenticated TLS door (client.go, runClient)". The dispatch is the
`switch cfg.Mode` at `main.go:242-281`: the embedded arm builds
`bridge.NewPump(bridge.NewUnixTarget(socket))` (`main.go:268`), the client arm
calls `runClient` (`main.go:274`), which builds
`bridge.NewTLSTarget(cfg.ServerURL, caPEM)` (`client.go:41`) — and
`NewTLSTarget` "builds a Target that dials the daemon's TLS network door
(native-client mode) over HTTP/2-over-TLS" (`bridge/tls_target.go:13-16`),
"mirror[ing] the server's network door (network_door.go:115)"
(`tls_target.go:25-26`).

So the door mapping per surface is:

| Surface | Door dialed | Reads the header? |
| --- | --- | --- |
| Browser against the deployed stack | TLS network door | Yes |
| Native shell, CLIENT mode | TLS network door (`tls_target.go:13-16`) | Yes |
| Native shell, EMBEDDED mode | Unix socket (`main.go:268`) | No |
| Browser served by `vite dev`, pointed at the dev door (`serve.go`) | dev door | No |
| Browser served by `vite dev`, pointed at the deployed stack | TLS network door | Yes |

Two consequences a reader must not be surprised by: the native-EMBEDDED path
and any path dialing the **dev door** will send a header no interceptor reads,
so their spans still lack `session.id` after this ships; and **an implementer
smoke-testing against the dev door will correctly see no `session.id` and may
wrongly conclude the UI half is broken.** Verification must therefore run
against a surface that dials the **network** door. Per the table that is any
of: a browser against the deployed stack (however the bundle is served,
including by `vite dev`), and the native shell in **CLIENT** mode — the latter
a second correct verification surface because its TLS target *is* the network
door. What decides the outcome is the door the transport dials, never how the
bundle was served. Whether the socket and dev doors should install the reader
is a server-side question, tracked as Open Question 5 rather than assumed
either way.

### (d) posthog-js's own `TracingHeaders` extension

The vendor ships a mechanism for exactly this header: `declare class
TracingHeaders implements Extension` (`apps/ui/node_modules/posthog-js/dist/
module.d.ts:902`), which patches `fetch`/`XHR` to add PostHog correlation
headers for configured hostnames. The server's own comment identifies the
header as the one "posthog-js sends" (`interceptor.go:41-44`), so a reviewer
will reasonably ask why we hand-roll an interceptor.

Rejected on two grounds, the second decisive:

1. It **monkey-patches global `fetch`/`XHR`** (the class carries
   `_restoreXHRPatch` / `_restoreFetchPatch` / `_hostnamesForPatch`), reaching
   every request the app makes rather than just the Compass transport. Our
   interceptor is scoped to one transport by construction.
2. PostHog's tracing-headers feature also sends the **distinct-id** header —
   which would violate this design's permanent PII exclusion by putting user
   identity on requests whose spans land in Tempo. Adopting the vendor
   mechanism would silently reintroduce the exact thing the contract forbids.

### (e) Wrapping the transport `fetch` instead of a Connect interceptor

Attach at `conn.fetchImpl` / the platform fetch rather than as an interceptor.
Rejected for symmetry: `traceResponseInterceptor` (`index.ts:143-162`) already
establishes the interceptor as this package's shape for correlation headers,
and an interceptor sees the typed request rather than a raw `Request`. Named
only because it is the obvious competing attach point.

## Global Constraints

- TypeScript strict, Bun, Biome. SolidJS — no React. `bun:test` conventions as in the existing suites (fake-injection, `spyOn` not `mock.module` — `apps/ui/src/live/client.test.ts:13-14`).
- posthog-js pinned `^1.418.10`; no version bump in this work.
- Layering: `apps/ui/src/analytics` never imports transport types; `@compass/client` never imports analytics. Both sides exchange only `() => string | undefined` (`analytics.ts:51-53`).
- Header name `X-POSTHOG-SESSION-ID`, exactly (`interceptor.go:44`); already CORS-allowed (`network_door.go:168` on `origin/main`: `AllowedHeaders: append(connectcors.AllowedHeaders(), "Authorization", otel.PostHogSessionHeader)`).
- Sender-side guard is **printable ASCII (`\x21`–`\x7E`) and `.length ≤ 200`** — stricter than the server's own `≤200 bytes` + valid-UTF-8 pair, and a strict *subset* of what `Headers.set` itself accepts (it takes space and tab; the guard does not). The narrowing is driven by the `Headers.set` ByteString seam, which throws on code points above U+00FF and on CRLF, and accepts U+0080–U+00FF only for a browser to emit it as a single raw high byte that the server then rejects as invalid UTF-8 and DROPS (see Validation). On accepted input `.length` equals the UTF-8 byte count, so the cap mirrors `interceptor.go:53` exactly — inclusive on both sides, since the server's own check is `len(id) > maxSessionIDLen` (`interceptor.go:97`). No empty, no whitespace, no CRLF.
- Omitted-means-off: every new optional collaborator (transport `opts.sessionId`, `createLiveClients` deps bag) installs nothing when absent, preserving byte-identical behavior for callers that don't ask (`index.ts:167-169`).

## Plan

### T1 — `sessionIdInterceptor` in `@compass/client`

`packages/compass-client/src/index.ts`: add `posthogSessionHeader` const,
`isSendableSessionId(id: string): boolean` (printable-ASCII regex + `.length`
cap — **no `TextEncoder`, no `isWellFormed`**; see Validation), and
`sessionIdInterceptor` as specified in Approach. Extend
`callInterceptors(token?, traceSink?, sessionId?)` to append it when supplied;
extend `createCompassWebTransport`'s `opts` with
`sessionId?: () => string | undefined`.

The four per-client factories — `createCompassWebClient` (`index.ts:242`),
`createCompassClientOverFetch` (`:265`), `createCommsWebClient` (`:300`),
`createCommsClientOverFetch` (`:319`) — deliberately do **not** grow a
`sessionId` option. Audited: no production caller exists. Grepping the four
names across `apps/` and `packages/` matches only
`packages/compass-client/src/index.ts`, its test file, and a *comment* at
`apps/ui/src/boot-mode.ts:32` — the shipped path is
`createLiveClients` → `createCompassWebTransport` (`live/client.ts:57-62`, the
sole production transport construction), and both native-shell modes route
through it via `conn.fetchImpl`. Recorded so a future caller of those
factories does not assume the header rides along.

Interfaces:

- Consumes: `Interceptor` from `@connectrpc/connect` (already imported, `index.ts:8`).
- Produces: `export function sessionIdInterceptor(sessionId: () => string | undefined): Interceptor`; `createCompassWebTransport(baseUrl: string, token?: string, opts?: { fetch?: typeof globalThis.fetch; traceSink?: TraceIdSink; sessionId?: () => string | undefined }): Transport`.

Tests (extend `packages/compass-client/src/index.test.ts`). Two seams, and
which one a case uses is load-bearing:

- The **capture-the-request** seam — `captureRequest` at `index.test.ts:79-91`
  and the vendor-factory spy at `:36-50` — for the header-PRESENT cases and
  the rejections that cannot throw. It reports only the URL and the
  authorization header today (`index.test.ts:81`:
  `Promise<{ url: string; authorization: string | null }>`; `:90`:
  `return { url, authorization: headers.get("authorization") };`), so it must
  be widened to report `X-POSTHOG-SESSION-ID` as well.
- The **direct-interceptor** seam, following the `bearerAuthInterceptor`
  precedent in the same file at `index.test.ts:94-113`: build
  `const req = { header: new Headers() }` (`:103`) — a **real** `Headers`, not
  a stub, which is what makes the throw reachable — and a counting `next` that
  returns a `Symbol` sentinel (`:97-102`), then **`await`** the interceptor
  call (`:105-107`). For any case where a defect would make the interceptor
  **throw**.

  **Every case routed to this seam MUST assert all three of the following.**
  The triple is a property of the *seam*, not of any one case; a test that
  asserts only the first is throw-blind and goes GREEN on the exact defect
  this seam exists to catch:

  1. the header is **absent** from `req.header`;
  2. `next` ran exactly **once** — `expect(calls).toBe(1)`, as at `:110`;
  3. the awaited result **is** the sentinel `next` produced —
     `expect(result).toBe(sentinel)`, as at `:112` — i.e. nothing threw.

  Assertion 3 is the load-bearing one, and it is why the call must be
  `await`ed: it cannot pass if the interceptor threw before reaching `next`.
  An absence-only test is not an acceptable substitute for any of the three
  cases below.

Cases:

- getter returns a valid id ⇒ request carries `X-POSTHOG-SESSION-ID: <id>`
  (capture seam).
- getter returns `undefined` ⇒ header **absent** (not empty) — **direct seam,
  all three assertions above**. The guard can throw on this input, so absence
  alone is not enough. `captureRequest`'s capturing `fetch` always `throw`s
  (`index.test.ts:87`), so its gate `rejects.toThrow()` cannot tell "threw
  early" from "reached `next`" — it goes green on the very defect the case
  exists to catch. Assertion 3 (`result` is the sentinel) is what proves
  nothing threw.
- getter returns `""` ⇒ header absent — capture seam.
- oversized value (201 ASCII chars) ⇒ header absent — capture seam.
- **exactly 200 ASCII chars ⇒ header PRESENT** (capture seam). This is the
  boundary the `<=` in `id.length <= MAX_SESSION_ID_LEN` owns: a `<` typo
  reddens here and nowhere else, because the 201 case stays green under both
  operators. 200 is legal on the server too — its check is
  `len(id) > maxSessionIDLen` (`interceptor.go:97`), so equality passes on
  both sides and a client that refused 200 would be needlessly stricter than
  the wire contract.
- **valid, well-formed, non-Latin-1 id (e.g. `"sess-日本語"` or an emoji) ⇒
  header absent AND the request still succeeds** — **direct seam, all three
  assertions above**. Sketch:
  `const result = await sessionIdInterceptor(() => "sess-日本語")(next)(req)`,
  then the triple.

  `captureRequest` cannot witness the second half **for this case**, and
  reaching for it here makes the test green on exactly the defect it exists to
  catch. Three properties combine: its capturing `fetch` always
  `throw`s ("captureRequest: short-circuit before response",
  `index.test.ts:87`), so it gates on `await expect(run(fetch)).rejects.toThrow()`
  (`:89`); and `headers` is pre-initialized to an empty `new Headers()`
  (`:83`) and only reassigned inside that `fetch` (`:86`). So if the
  interceptor throws a `TypeError` at `header.set`, `fetch` is never reached,
  the readback returns `null`, and the mandatory `rejects.toThrow()` is
  satisfied by the interceptor's own throw. Both assertions pass on a broken
  interceptor. Do **not** "simplify" this case back onto `captureRequest`.

  Keep the value short: a long non-ASCII value is rejected by the length cap
  first and never reaches `header.set`, so it cannot exercise the throw.
- Latin-1 value (`"sess-é"`) ⇒ header absent — **capture seam**. This value
  does *not* throw (`Headers.set` accepts it — measured in both Bun and
  Chromium), so the direct seam has nothing to witness here; the header
  readback *is* the whole assertion. Depends on the `captureRequest` widening
  specified above — the seam must report `X-POSTHOG-SESSION-ID`, or this case
  cannot fail. What the ASCII guard buys: without it the value is accepted by
  `Headers.set` and sent as a single raw high byte, which fails the server's
  `utf8.ValidString` check, so `sessionIDFromHeader` returns `""` and the id
  is **dropped** — silently lost, exactly like the values the guard rejects
  outright (see Validation). Do not attempt to assert the transmitted bytes —
  that is a transport-encoding property, not this interceptor's contract.
- value containing `\r\n` ⇒ header absent — **direct seam, all three
  assertions above**. Same would-throw class as the non-ASCII case
  (`Headers.set` throws a `TypeError` on CRLF — see Validation).
- lone-surrogate value (`"\uD800"`) ⇒ header absent — **direct seam, all three
  assertions above**. Also would-throw.
- `callInterceptors` membership, all four directions, extending the existing
  `"callInterceptors installs only what was asked for"` describe
  (`index.test.ts:593`) and matching its count style (`expect(opts.interceptors)
  .toHaveLength(n)`, as at `:613`, `:624`, `:635`):
  - no `sessionId` (and no token, no sink) ⇒ `interceptors` is `undefined`,
    not `[]` — the existing `:600-606` case, unchanged.
  - `sessionId` **alone** ⇒ exactly one interceptor.
  - `sessionId` + `traceSink` ⇒ exactly two.
  - token + `traceSink` + `sessionId` ⇒ exactly three.

  The `sessionId`-alone case is the one that catches an append placed inside
  the existing early return: `callInterceptors` today is
  `const bearer = bearerInterceptors(token); if (!traceSink) { return bearer; }`
  (`packages/compass-client/src/index.ts:174-177`), so a session interceptor
  appended after that guard is skipped entirely whenever no trace sink is
  configured — and every other membership direction still passes.
- getter is called per-request: a fresh value on the second call is sent
  (capture seam), proving laziness in the forward direction.
- **self-healing across requests: getter returns `""` on the first call and a
  valid id on the second ⇒ the first request carries no header, the second
  carries it** (capture seam). This is the transition Degradation and the
  Approach both promise — "self-healing on the next request once a session
  exists" (`:134-135`) — and it is the direction the laziness case above does
  *not* cover: a construction-time cache or a first-value memo still passes
  forward-propagation while failing this. OQ2's anti-memoization concern rests
  on this case.

### T2 — `Analytics.sessionId()`

`apps/ui/src/analytics/analytics.ts`: add `sessionId(): string | undefined` to the `Analytics` interface (`:25-32`), `return undefined` in `NoopAnalytics` (`:36-40`), and the `get_session_id()`-delegating, empty-mapping implementation in `PostHogAnalytics` (`:45-115`).

Interfaces:

- Consumes: `PostHog.get_session_id(): string` (posthog-js `1.418.10`, `module.d.ts:4212`).
- Produces: `Analytics.sessionId(): string | undefined`.

Tests (extend `apps/ui/src/analytics/analytics.test.ts`; add `get_session_id` to the `FakePostHog` recorder at `:11-17`):

- disabled path: `sessionId()` returns `undefined` and makes ZERO posthog calls (extends the load-bearing off-by-default contract, `analytics.test.ts:7-8`).
- enabled path: returns exactly what the fake's `get_session_id` returns.
- enabled path, fake returns `""` ⇒ `undefined`.

### T3 — `createLiveClients` deps bag

`apps/ui/src/live/client.ts`: `createLiveClients(conn: ResolvedConnection, deps?: { sessionId?: () => string | undefined }): LiveClients`, threading `sessionId` into the `createCompassWebTransport` opts at `:59-62`. No `LiveClients` shape change.

Interfaces:

- Consumes: T1's transport opt.
- Produces: the widened `createLiveClients` signature; existing single-arg callers compile unchanged.

Tests (extend `apps/ui/src/live/client.test.ts`, same spy-the-factory seam as `:28-45`): the getter passed in deps is the one handed to the transport factory; omitted deps ⇒ transport opts carry no `sessionId`.

### T4 — the boot reorder + wiring

`apps/ui/src/index.tsx` `main()` (`:87-124`): move the `createAnalytics` statement (with its comment block `:100-120`) above `createLiveClients`; pass `{ sessionId: () => analytics.sessionId() }` to `createLiveClients`; keep `bootCaller` and `analytics.identify(callerId)` in place. Rewrite the `:119-120` "deliberately not wired" sentence to describe the now-wired outbound half and the two opposite-pointing lazy getters.

Interfaces:

- Consumes: T2's `sessionId()`, T3's deps bag.
- Produces: the new boot order; no exported surface changes.

Tests: the seam-level tests in T1-T3 cover every moving part *individually*.

**T4's own reorder has no automated coverage today, and this record does not
pretend otherwise.** Two facts make that a structural gap rather than an
oversight:

- The four reordered statements live inside the `main()` closure
  (`apps/ui/src/index.tsx:87-124`), which no test reaches. `main()` is not
  exported and is invoked only from the module's own top-level `else` arm
  (`index.tsx:58-63`: `} else { const bootConnectionForMode = …; … return
  main(root, connection); …`).
- Every e2e spec shares one webServer, and it is fixture-mode. There is a
  single `webServer` entry in `apps/ui/playwright.config.ts`, whose command is
  `bunx vite --port ${devPort} --strictPort --mode fixture`
  (`playwright.config.ts:87`) — so all three specs (`e2e/visual-smoke.spec.ts`,
  `e2e/dev-boot.spec.ts`, `e2e/advancing-hook.spec.ts`) run against it. In
  that mode `index.tsx:47` takes the other branch —
  `if (import.meta.env.MODE === "fixture") { void import("./boot-fixture") … }`
  — and returns down the fixture arm, so `main()` is never called. The fixture
  boot does not contain the reordered composition either: grepping
  `createAnalytics|createLiveClients|bootCaller|resolveCaller` across
  `apps/ui/src/boot-fixture.ts` matches **0** lines, with a POS-CTRL of **5**
  for `createStore|render|mount` in that same file — so the zero is a measured
  absence, not a failed grep.

Consequence: **visual-smoke is green whether the reorder is right, wrong, or
absent.** Do not cite `e2e/visual-smoke.spec.ts`, `e2e/dev-boot.spec.ts`, or
`e2e/advancing-hook.spec.ts` as the boot-order regression check. They must
stay green, but staying green proves nothing about T4.

T4's acceptance is therefore **manual**: run a `vite dev` boot (non-fixture
mode, so the `else` arm at `index.tsx:58` runs `main()`) **pointed at a stack
reached through the TLS NETWORK door** — the Vite dev server is only how the
bundle is served; what matters is which door the transport dials. Observe
`X-POSTHOG-SESSION-ID` on the wire on an outbound request. The network door is
the only door that reads the header (Alternatives (c)); a native-shell
**CLIENT**-mode boot is the second acceptable surface, for the same reason. A
check against the **dev door** shows a false
negative indistinguishable from a broken UI half.

#### OPTIONAL follow-up — not part of T4's required scope

Real automated coverage would need the composition lifted out of the `main()`
closure: export something like `composeBoot(deps)` taking the three factories
as injectable dependencies, have `main()` call it, and unit-test the **order**
— that `createAnalytics` is called before `createLiveClients`, and that the
getter handed to `createLiveClients` resolves to the analytics object step 1
built. That is what would redden on a reordered-back regression, which nothing
today does.

This is recorded as a **genuine design choice not yet made**, not as a decided
task: it adds an exported seam to the composition root purely for
testability, and whether that trade is worth making is Matt's call, not this
record's. T4 ships without it; the gap above is stated so the decision is
made knowingly rather than by omission.

### T5 — record hygiene

Update the module-header prose in `analytics.ts:1-16` (the "Built once at boot" framing) if the reorder changes what it asserts; verify no other comment states the old order as a constraint.

## Tasks

- [ ] T1: `sessionIdInterceptor` + validation + `callInterceptors`/`createCompassWebTransport` opts — `packages/compass-client/src/index.ts`, tests in `packages/compass-client/src/index.test.ts`
- [ ] T2: `Analytics.sessionId()` across interface/Noop/PostHog — `apps/ui/src/analytics/analytics.ts`, tests in `apps/ui/src/analytics/analytics.test.ts`
- [ ] T3: `createLiveClients` deps bag — `apps/ui/src/live/client.ts`, tests in `apps/ui/src/live/client.test.ts`
- [ ] T4: boot reorder + wiring + comment rewrite — `apps/ui/src/index.tsx`
- [ ] T5: stale-comment sweep — `apps/ui/src/analytics/analytics.ts` header prose

## Open Questions

1. **Gate the header to unary requests?** (Not load-bearing.) The server reads it only in a `connect.UnaryInterceptorFunc` (`interceptor.go:66`), but a Connect client interceptor also sees the long-lived `SubscribeComms` stream request, where the header is sent and ignored. Cost is a few dozen bytes on stream open; gating would need the interceptor to inspect `req.stream`, adding a branch for no observable win. Recommendation: send on all requests, note it in the interceptor comment.
2. **Session rotation freshness.** (Not load-bearing.) posthog-js rotates session ids on inactivity; the per-request getter picks up the new id on the next call by construction. No caching anywhere — confirm no reviewer asks for memoization, which would reintroduce staleness.
3. **`String.prototype.isWellFormed` — MOOT, not answered.** (Was load-bearing
   for T1; recorded so no implementer re-opens it.) The question was whether
   `isWellFormed` is available. It is not — but note the governing config is
   **`packages/compass-client/tsconfig.json`**, where the validator lands, not
   `apps/ui`'s: that file sets no `lib` at all (`{"extends":
   "../../tsconfig.base.json", "compilerOptions": {"types": ["bun"]},
   "include": ["src"]}`), so its lib defaults from `target: "ES2022"`
   (`tsconfig.base.json:3`), and `isWellFormed` is ES2024. (`apps/ui/
   tsconfig.json:4` pins the same `ES2022` for the app, but it does not govern
   this package — citing it would be evidence about the wrong compilation
   unit.) The ASCII guard then removed the need for any well-formedness test at
   all, since lone surrogates are non-ASCII, so neither `isWellFormed` nor an
   encode/decode fallback appears in T1. Noting *moot* versus *answered*
   because the two differ in what they leave behind: an answered question
   leaves a choice in the code, a moot one leaves nothing to choose.
4. **Whether `deps` on `createLiveClients` should instead extend `ResolvedConnection`.** (Not load-bearing.) The deps-bag shape was chosen to mirror `createAnalytics`'s documented rationale (`analytics.ts:123-126`) and keep `ResolvedConnection` a pure connection record. Flagged only because `conn.fetchImpl` shows the connection already carries one injected collaborator; Matt may prefer symmetry either way.
5. **Do the Unix-socket and dev doors need `NewSessionIDInterceptor` too?**
   (Load-bearing for *verification*, not for this record's code — the UI half
   is identical either way.) Measured on `origin/main`: the reader is
   installed only on the network door (`network_door.go:300`, `:308`), absent
   from `serve.go`'s socket and dev chains, yet the dev CORS builder allows the
   header (`serve.go:1041`). So dev-door spans, and native-shell **EMBEDDED**
   spans (which dial the Unix socket — `go/cmd/compass-app/main.go:268`), will
   not carry `session.id` after this ships. This is compass-server's call, not
   mine — route it to that lane rather than widening this record's scope.

   Meanwhile there are **two** correct verification surfaces, both dialing the
   network door: a browser against the deployed stack, and the native shell in
   **CLIENT** mode, whose `bridge.NewTLSTarget` "dials the daemon's TLS network
   door (native-client mode)" (`go/internal/bridge/tls_target.go:13-14`, built
   at `go/cmd/compass-app/client.go:41`). Verify on either. Do **not** verify
   on the dev door or in native-EMBEDDED mode: both show a false negative that
   looks exactly like a broken UI half.
