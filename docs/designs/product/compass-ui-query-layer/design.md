# Compass UI query layer (@tanstack/solid-query + connect-query-core adoption)

Status: Draft
Tracker: SEA-1696
Ledger-impact: reserves one row (Compass UI query layer adoption); compass appends at ship

## Problem / Intent

The compass.v1 RPC surface is overwhelmingly unary request/response, but
`apps/ui` has no server-state cache at all. Counted from the generated service
descriptors:

- **CommsService** (comms_pb.ts:1964-2159): **16 unary** methods (`createUser`,
  `createAgent`, `listAccounts`, `createChannelGroup`, `listChannelGroups`,
  `listChannels`, `createChannel`, `updateChannelMembers`, `reparentAgent`,
  `openAgentWorkspace`, `listMessages`, `postMessage`, `listTopics`,
  `updateTopic`, `respondToAsk`, `searchMessages` — each declared
  `methodKind: "unary"`) plus **1 stream**: `subscribeComms`
  (`methodKind: "server_streaming"`, comms_pb.ts:2155-2159).
- **CompassService** (compass_pb.ts:1539-1726): **11 unary** methods
  (`getServerInfo`, `provisionAgentWorkspace`, `startAgentSession`,
  `stopAgentSession`, `removeAgentWorkspace`, `reloadAgentSession`,
  `getAgentStatus`, `issueToken`, `putAgentConfig`, `getAgentConfigInfo`,
  `deleteAgentConfig`) plus **2 streams**: `subscribeEvents`
  (compass_pb.ts:1559-1563) and `subscribeAgentSession`
  (compass_pb.ts:1659-1663).
- **SecretsService** (compass_pb.ts:1737-1771): **3 unary** methods
  (`setSecret`, `listSecrets`, `deleteSecret`) — a distinct service, not part
  of `CompassService`; `@compass/client` exports no SecretsService client today
  (index.ts:159-188), but a future secrets query rides the same shared
  `Transport` keyed by `SecretsService.method.listSecrets`, needing no
  `createLiveClients` change (§A2).

Today the UI loads that surface two ad-hoc ways, both cache-less:

1. **Unary reads are fire-once promises into signals.** The Backlog queue
   (store.ts:672-678):

   ```ts
   const loadAssignedIssues = () => {
     seam
       .listAssignedIssues(trackerConfig().handle)
       .then(setAssignedIssues)
       .catch(() => setAssignedIssues([]));
   };
   loadAssignedIssues();
   ```

   No caching, no dedup, no staleness/retry model, errors collapse to `[]`.
   Every future unary read (agent status, config info, secrets, search)
   re-invents this by hand.

2. **The one live stream replaces the whole `CommsState` wholesale.** The
   store runs the SubscribeComms driver for its lifetime (store.ts:802-816):

   ```ts
   if (options.comms) {
     const client = options.comms;
     const abort = new AbortController();
     if (getOwner()) onCleanup(() => abort.abort());
     void runCommsStream({
       client,
       callerId,
       mapMessage: adaptMessage,
       onState: adoptComms,
       signal: abort.signal,
       ...
   ```

   and each push replaces one signal: "replaced wholesale by each
   `runCommsStream` push" (store.ts:747-748), landing in
   `const [comms, setComms] = createSignal<CommsState>(...)`
   (store.ts:754-756) via `adoptComms` (store.ts:785-790).

**The concrete blocker (SEA-1655): paginated history cannot ride the
wholesale stream.** The snapshot reader eagerly pages *every* visible
channel's messages to exhaustion — `fetchSnapshot` "loads every visible
channel's messages eagerly" and explicitly notes "Lazy per-channel load would
change the accessor contract; out of scope" (stream.ts:99-108). A topic
history view needs demand-driven, per-channel/per-topic pages via
`listMessages` (`limit` + `beforeMessageId` cursor, comms_pb.ts:1424-1436) —
a cached, keyed, paginated unary read with an invalidation model, which
nothing in `apps/ui` provides today.

**Decision (fixed by Matt, 2026-08-03 — this record designs the *how*, not
the library choice):** adopt **`@tanstack/solid-query`** (the official Solid
adapter, latest 5.101.4) + **`@connectrpc/connect-query-core`**
(framework-agnostic Connect glue, latest 2.2.0) as the server-state query
layer for `apps/ui`, sitting beneath the store's accessor seam.

## Approach

### A1 — QueryClient boot + provider

One app-lifetime `QueryClient`, created in `index.tsx` beside the existing
store singleton and provided through `@tanstack/solid-query`'s
`QueryClientProvider` (the app is SolidJS — `solid-js: ^1.9.13`,
apps/ui/package.json:18 — so the provider is the Solid adapter's, never
React's).

Boot today builds clients, then the store, then renders
(index.tsx:33, 44-48, 65-72):

```tsx
const clients = createLiveClients(connection);
...
const store = createRoot(() =>
  createAppStore({
    comms: clients.comms,
    compass: clients.compass,
    ...
render(
  () => (
    <StoreContext.Provider value={store}>
      <App />
    </StoreContext.Provider>
  ),
  root,
);
```

Target shape: the `QueryClient` is constructed before the store (the store
consumes it — §A3) and the provider wraps the tree inside the existing
`StoreContext.Provider`:

```tsx
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, staleTime: 30_000 } },
});
const store = createRoot(() =>
  createAppStore({ ..., queryClient }),
);
render(
  () => (
    <StoreContext.Provider value={store}>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </StoreContext.Provider>
  ),
  root,
);
```

The store's `createRoot` singleton wiring (index.tsx:44-63) is untouched;
when the routing record's router lands (DL-127), `QueryClientProvider` nests with `HashRouter`
inside the same provider stack — the two records compose order-independently.

**Two-path access, one cache (a stated invariant, not an inconsistency).** The
store holds the `QueryClient` explicitly (it is built before `render()`, so its
root never sits under the provider — §A3); components read the *same* instance
through `QueryClientProvider`. Both paths are the one cache, so invalidations
and `setQueryData` from either side are a single source of truth; the
store-before-provider ordering is required, not merely tolerated.

### A2 — The connect-query-core → solid-query glue seam

`@connectrpc/connect-query-core` is framework-agnostic: `createQueryOptions`
returns `{ queryKey, queryFn, structuralSharing }` and
`createInfiniteQueryOptions` returns those plus
`getNextPageParam`/`initialPageParam`, both keyed by the generated method
descriptor + transport + input via `createConnectQueryKey` (per the
connect-query-es README: "the core (`createConnectQueryKey` and
`callUnaryMethod`) is not React specific so splitting off a
`connect-solid-query` is possible"). TanStack's `useQuery` in
`@tanstack/solid-query` v5 consumes exactly such an options object — passed
as a *function* returning options, Solid Query's reactive-options form.

**No first-party Solid connect-query binding exists** — connect-query ships
React hooks only (`@connectrpc/connect-query` is "an wrapper around TanStack
Query (react-query)", README). That is the sole reason this seam is
hand-written. It is **glue, not a fork**: a small typed module
(`apps/ui/src/live/query.ts`, ~40 lines) that maps a core descriptor to a
Solid Query call:

```ts
import { createQueryOptions, createInfiniteQueryOptions } from "@connectrpc/connect-query-core";
import { useQuery, useInfiniteQuery } from "@tanstack/solid-query";

/** Unary Connect method → a Solid Query. Options are a thunk so input
 *  signals stay reactive (Solid Query re-runs the thunk on change). The
 *  `QueryClient` is passed EXPLICITLY as `useQuery`'s second argument (an
 *  `Accessor<QueryClient>`), never resolved from context: store-internal
 *  queries have no `QueryClientProvider` ancestor (§A3), so the client is
 *  required, not optional. */
export function createConnectQuery<I extends DescMessage, O extends DescMessage>(
  schema: DescMethodUnary<I, O>,
  input: () => MessageInitShape<I> | SkipToken,
  opts: { transport: Transport; queryClient: QueryClient },
) {
  return useQuery(
    () => ({ ...createQueryOptions(schema, input(), { transport: opts.transport }) }),
    () => opts.queryClient,
  );
}
// createConnectInfiniteQuery mirrors this over createInfiniteQueryOptions,
// forwarding the same `() => opts.queryClient` accessor.
```

The generated method descriptors already exist — `CommsService.method
.listMessages` etc. from `CommsService: GenService<{...}>`
(comms_pb.ts:1964); no extra codegen plugin (`protoc-gen-connect-query`) is
needed, because connect-query-core accepts the plain `protoc-gen-es`
descriptors the repo already generates.

**Transport exposure (one additive seam change).** connect-query-core keys
and calls by `Transport`, not by client. Today `createLiveClients` returns
only clients, and each factory buries its own transport
(live/client.ts:30-35):

```ts
export function createLiveClients(conn: Connection): LiveClients {
  return {
    comms: createCommsWebClient(conn.baseUrl, conn.token),
    compass: createCompassWebClient(conn.baseUrl, conn.token),
  };
}
```

`LiveClients` grows a `readonly transport: Transport` field: one
`createGrpcWebTransport` built once in `createLiveClients` and shared by both
clients (the `@compass/client` factories already take a `Transport` form —
`createCommsClient(transport)` / `createCompassClient(transport)`,
packages/compass-client/src/index.ts:43-44, 101-102). Transport is still
chosen in exactly one place — the DL-106/107 seam is preserved, not moved
(§Composition). One-transport-instance also matters for cache identity:
connect-query keys embed "a key for a Transport reference", so all queries
and all `setQueryData`/invalidation calls must share the single instance.

### A3 — Queries sit beneath the store's accessor seam

**What does not change:** components keep reading store accessors. The store
remains "the seam the components were written against" (store.ts:12-13);
`assignedIssues: Accessor<Issue[]>` (store.ts:525), `messages()`,
`channels()` et al. keep their exact call sites and types.

**What changes:** inside the store, cache-less loaders become query-backed.
`createAppStore` gains a required `queryClient` option; a unary loader becomes
a query created in the store's root. The store's `createRoot` supplies the
owner these reactive computations need (index.tsx:44) — but it is **not** under
a `QueryClientProvider`: the store singleton is built before `render()` mounts
the provider (index.tsx:44-63 store, :65-72 render), so no provider ancestor
ever exists for the store's owner. Store-internal queries therefore MUST pass
the `QueryClient` explicitly (§A2's `() => opts.queryClient` accessor); relying
on context would throw `No QueryClient set` at boot. The provider (§A1) serves
*components*; the store uses the explicit client. The public accessor reads
`query.data` with today's fallback:

```ts
const issuesQuery = createTrackerIssuesQuery(() => trackerConfig().handle);
const assignedIssues: Accessor<Issue[]> = () => issuesQuery.data ?? [];
```

The `setTrackerConfig` re-load dance (store.ts:1691-1694 rebuilds the seam
and calls `loadAssignedIssues()` again) collapses: the handle is part of the
query key, so a config change re-keys and refetches automatically.

This boundary is exactly what the routing record (DL-127) already reserved: its
composition note states the query layer lands "beneath the store's accessor
seam" and that "the route-sync effect still writes only UI-state signals"
(compass-shell-routing/design.md:473-483) — routing is unaffected (§Composition).

### A4 — Streams into the cache

The genuinely new design surface: how `subscribeComms` (and later
`subscribeEvents`/`subscribeAgentSession`) coexists with the query cache.

**Decision: the stream keeps owning `CommsState`; it additionally fans
targeted invalidations into the query cache.** The `runCommsStream` →
`adoptComms` → `comms` signal path (store.ts:802-816, 785-790, 754-756) is
untouched: the four memo accessors (`accounts`/`channelGroups`/`channels`/
`messages`, store.ts:763-766) stay stream-fed. What is added: the store's
`onState`/event path notifies the query layer, so query-backed reads that
*overlap* the stream's domain (paginated `listMessages`/`listTopics` history,
`searchMessages`) never go stale silently:

- A tail event touching channel `C` (a new message, a topic change) triggers
  `queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema:
  CommsService.method.listMessages, cardinality: "infinite", input:
  { container: { case: "channelId", value: C } } , transport }) })` — and the
  `listTopics` key for `C` likewise. Partial keys make this cheap and exact.
- For the hot append path (a new message on the channel the user is viewing),
  `queryClient.setQueryData` on the newest `listMessages` page is a permitted
  optimization, not the correctness mechanism — the message-id dedup the
  stream driver already relies on ("message-id dedup ... absorbs the overlap",
  stream.ts:14-16) applies equally to a page that refetches after also being
  patched.
- **Bounding refetch amplification.** An `invalidateQueries` refetches an
  *active* infinite query across its whole retained page chain, so on a busy
  channel a naive fan-out is one full page-chain refetch per incoming message,
  growing as the user scrolls deeper. Two bounds apply: (1) set TanStack v5's
  `maxPages` on the `listMessages` infinite query so the retained (and
  refetched) chain is capped — the vendor mechanism built for exactly this; and
  (2) T4's fan-out is diff-driven, so it coalesces per-channel invalidations
  within one adopted push rather than firing per message. The `setQueryData`
  hot-path patch above further avoids a refetch on the common single-append
  case.

Why not dissolve `CommsState` into cache-per-entity now: the stream driver's
snapshot+tail+resync protocol with its two-cursor discipline
(stream.ts:1-30) *is* the consistency model for the live surface, and
`adoptComms` carries UI-local state across pushes (`preserveLocalAsks`,
store.ts:782-786) that a generic cache cannot. Re-homing that protocol onto
per-entity `setQueryData` writes would rewrite a tested driver for zero user
value. The full reconciliation fork is recorded in Open Questions (OQ-1)
with this as the recommendation.

Refetch-on-reconnect composes for free: the stream already re-snapshots on
resync (stream.ts:18-20), and the invalidation fan-out makes the query side
follow.

### A5 — Mutations → invalidation

Writes go through solid-query's `useMutation` (or a thin store method that
awaits the client call — the store's write paths stay methods), and each
write names the read keys it dirties:

- `postMessage` (store.ts:1475-1483 today calls `client.postMessage(...)`
  directly) → invalidate `listMessages` (infinite, channel input) and
  `listTopics` for the channel. Note the existing contract stays: the store
  deliberately does **not** locally insert — "the stream echo renders it"
  (store.ts:430-432) — so the live surface needs no optimistic write; the
  invalidation covers only the *paginated history* queries. The stream event
  for the same post triggers the same invalidation (§A4); the two are
  idempotent by key.
- `updateTopic` → invalidate `listTopics` (channel input) and the affected
  topic's `listMessages` filter key.
- `respondToAsk` (store.ts:1326-1331) → no query invalidation; asks live
  entirely in stream-owned `CommsState`.
- Tracker writes (`updateIssueStatus`) → invalidate the assigned-issues key.

Pattern, stated once as the rule: **every mutation invalidates by
`createConnectQueryKey` partial key, never by hand-written string key; stream
events invalidate the same keys, and idempotence makes the overlap safe.**

### A6 — Pagination for history

`listMessages` is the cursor-paged read: "Page size; the server clamps to a
maximum" (`limit`, comms_pb.ts:1424-1429) and "Page before this message id
(exclusive); empty = the newest page" (`beforeMessageId`,
comms_pb.ts:1431-1436), newest-first (comms_pb.ts:1466-1472). It maps to
`createInfiniteQueryOptions` with `pageParamKey: "beforeMessageId"`,
`initialPageParam: ""`, and `getNextPageParam: (lastPage) =>
lastPage.messages.at(-1)?.id` (undefined on a short/empty page = end of
history) — the exact shape connect-query-core is built for.

`listTopics` is **not** paginated on the wire: `ListTopicsRequest` carries
only `channelId` and `includeArchived` (comms_pb.ts:1568-1582) — no
limit/cursor fields. It is therefore a *plain* query keyed by
`(channelId, includeArchived)`, not an infinite query; if topic lists ever
grow pagination fields, the same infinite mapping applies. (The assignment
brief assumed both paginate; the gen file says otherwise, and the wire wins.)

`searchMessages` follows `listMessages`' pattern when it gains a UI surface;
its request exists today (comms_pb.ts:1735).

The `snapshotSeq` field on these requests ("Point-in-time snapshot cursor ...
0 = latest", comms_pb.ts:1438-1444) is a stream-driver concern; query-layer
reads pass the default `0` (latest) — the invalidation model, not a snapshot
boundary, is their consistency story.

## Alternatives considered

The library choice is Matt's ruling (2026-08-03); these record why the
rejected options lose, as settled rationale — they are not live options.

### @tanstack/solid-query alone, hand-written queryFns — rejected

Loses connect-query-core's generated, transport-scoped, type-branded query
keys (`createConnectQueryKey` embeds service name, method, input message, and
cardinality). Hand-maintained key arrays per RPC drift from the call sites
they describe; a mistyped key silently mis-invalidates — precisely the bug
class the invalidation model in §A5 must not have. The core package is ~zero
marginal weight over the keys alone and also supplies `callUnaryMethod`,
`structuralSharing` for protobuf messages, and the infinite-query option
factories.

### Solid createResource + a hand-rolled cache — rejected

`createResource` is per-owner: no cross-component cache, no dedup of
identical in-flight reads, no staleness/retry/invalidation model, and no
infinite-query primitive — weakest exactly where the SEA-1655 blocker bites
(paginated history). Building those on top is writing TanStack Query badly.

### Wait for a first-party connect-solid-query — rejected

None exists or is announced; the vendor's own README frames the Solid split
as merely *possible*. The hand-written seam in §A2 is small, typed, and
deletable the day a first-party binding ships.

## Global Constraints

- `@tanstack/solid-query` **floor 5.101.4**, `@connectrpc/connect-query-core`
  **floor 2.2.0** (current on npm as of 2026-08-03; re-check at impl time).
  Both compose with the existing `@connectrpc/connect@^2.1.0` +
  `@bufbuild/protobuf@^2.12.1` (packages/compass-client/package.json:11-13).
- **One `QueryClient`, one `Transport` instance** — query keys embed the
  transport reference; every query, invalidation, and `setQueryData` call
  shares the instances built at boot (§A1, §A2).
- **Queries sit beneath the store's accessor seam** — components read store
  accessors, never query objects directly (§A3). The routing record's (DL-127)
  route-sync effect and its pending-aware unknown-id fallback are unaffected.
- **Nothing above the transport boundary assumes local** (DL-106/107): all
  queryFns execute through the one transport `createLiveClients` exposes; the
  query layer imports zero shell API.
- **Keys via `createConnectQueryKey` only** — no hand-written key arrays.
- **The stream driver owns `CommsState`** (§A4/OQ-1 recommendation); the
  query layer never writes the `comms` signal.
- No `: any` / `as any`; `bun test --conditions browser`
  (apps/ui/moon.yml:37) stays green at every PR boundary.
- One new decision row lands in `docs/designs/product/DECISIONS.md`, appended
  by the ledger single-writer (compass) at ship (freeze = merge); this record
  reserves the id via its `Ledger-impact` line (see Ledger note) and does not
  edit `DECISIONS.md`.

## Plan

Every task inherits `## Global Constraints`. T1+T2 land as one PR (T1 alone
adds dead deps); T3-T5 are each independently green.

### T1 — Dependencies + transport exposure

Add `@tanstack/solid-query` and `@connectrpc/connect-query-core` to
`apps/ui/package.json`; extend `createLiveClients` (live/client.ts:30-35) to
build one shared `createGrpcWebTransport` and expose it as
`LiveClients.transport`, constructing both clients over it via the existing
`createCommsClient(transport)`/`createCompassClient(transport)` factories
(packages/compass-client/src/index.ts:43-44, 101-102).

Interfaces:

- consumes: `Transport` from `@connectrpc/connect`; existing
  `@compass/client` factories.
- produces: `LiveClients` gains `readonly transport: Transport`; behavior of
  `comms`/`compass` clients unchanged (same bearer interceptors).

Acceptance: existing suites green; the two clients verifiably share one
transport (unit test on `createLiveClients`).

### T2 — Glue seam + QueryClient boot + provider

Create `apps/ui/src/live/query.ts` (§A2): `createConnectQuery` /
`createConnectInfiniteQuery` mapping connect-query-core option factories to
`useQuery`/`useInfiniteQuery` from `@tanstack/solid-query`, options-as-thunk
for reactivity. Rework `index.tsx` (§A1): construct the `QueryClient`, pass
it into `createAppStore`, wrap the render tree in `QueryClientProvider`
inside the existing `StoreContext.Provider` (index.tsx:65-72).

Interfaces:

- consumes: `createQueryOptions`, `createInfiniteQueryOptions`,
  `createConnectQueryKey` from `@connectrpc/connect-query-core`; `useQuery`,
  `useInfiniteQuery`, `QueryClient`, `QueryClientProvider` from
  `@tanstack/solid-query`; `LiveClients.transport` (T1).
- produces: `createConnectQuery(schema, input: () => Init | SkipToken,
  { transport, queryClient }): UseQueryResult` (queryClient required, forwarded
  as `useQuery`'s `() => queryClient` accessor — §A2/F-store-root);
  `createConnectInfiniteQuery(schema, input, { transport, queryClient,
  pageParamKey, getNextPageParam })`; `AppStoreOptions` gains a required
  `readonly queryClient: QueryClient`.

Acceptance: app boots and renders unchanged; a throwaway query through the seam
round-trips against a `createRouterTransport` fake in a unit test — the spike
MUST exercise the query under a bare `createRoot` with an explicitly-passed
`QueryClient` and NO `QueryClientProvider`, proving the store-root usage pattern
(§A3) before T3 depends on it.

### T3 — First query: migrate the assigned-issues loader

Replace `loadAssignedIssues` (store.ts:672-678) with a query keyed on the
tracker handle; `assignedIssues()` reads `query.data ?? []` (§A3); delete the
`setTrackerConfig` manual re-load (store.ts:1691-1694 — re-key handles it).
The tracker seam is not yet a Connect RPC (tracker.ts:27-32 is the fixture
contract), so this query uses a plain solid-query key + queryFn over the
seam — proving the store-internal query pattern; it swaps to a
connect-query-core descriptor when the daemon RPC lands.

Interfaces:

- consumes: `useQuery` (via a store-internal helper), `TrackerSeam`
  (tracker.ts:27-32), `queryClient` option (T2).
- produces: unchanged public `assignedIssues: Accessor<Issue[]>`
  (store.ts:525); unchanged `setTrackerConfig(cfg)` signature.

Acceptance: Backlog view renders as today; `store.test.ts` assigned-issues
suites green (await-a-tick pattern already in place, store.test.ts:43-47);
a config change refetches without a manual reload call.

### T4 — Streams-into-cache fan-out

Implement §A4: the store's stream path (store.ts:802-816) additionally maps
each adopted state/event to targeted `queryClient.invalidateQueries` calls
with `createConnectQueryKey` partial keys (per-channel `listMessages`
infinite key, `listTopics` key). No change to `runCommsStream`, `adoptComms`,
or the four comms accessors.

Interfaces:

- consumes: `createConnectQueryKey` from `@connectrpc/connect-query-core`;
  `QueryClient.invalidateQueries`; the existing `onState` sink
  (stream.ts:70-72).
- produces: a store-internal `invalidateForCommsChange(prev: CommsState,
  next: CommsState): void` (diff-driven; channel-granular).

Acceptance: unit test — a fake stream push for channel C invalidates C's
`listMessages`/`listTopics` keys and no other channel's; suites green.

### T5 — Paginated history queries (SEA-1655 unblock)

Expose store-level query-backed reads for topic history: an infinite
`listMessages` query (`pageParamKey: "beforeMessageId"`,
`initialPageParam: ""`, `getNextPageParam` from the last message id, `maxPages`
set to bound the retained/refetched chain (§A4), §A6) and
a plain `listTopics` query, both channel-keyed and SkipToken-gated on a null
selection. Mutation invalidation (§A5) for `postMessage`/`updateTopic` lands
here with its consumers.

Interfaces:

- consumes: `createConnectInfiniteQuery`/`createConnectQuery` (T2);
  `CommsService.method.listMessages` / `.listTopics` descriptors
  (comms_pb.ts:2085-2111); T4's invalidation fan-out.
- produces: store accessors (shape finalized with the SEA-1655 topic-view
  implementer) e.g. `topicHistory(channelId, topicId)` exposing
  `data/fetchNextPage/hasNextPage/isPending`; `postMessage`/`updateTopic`
  paths gain key invalidation.

Acceptance: against a `createRouterTransport` fake serving three pages,
scrolling fetches pages in order, a `postMessage` invalidates and refetches
the newest page, and a stream echo of the same message does not duplicate it
(id dedup); suites green.

### Test story (cross-task)

Component tests mount with a fresh `QueryClient` per test (`retry: false`,
`gcTime: Infinity`) inside the provider stack; the routing record's
`mountApp` helper (compass-shell-routing/design.md §A4, T3) grows the
`QueryClientProvider` wrapper alongside `MemoryRouter` — one shared helper,
production-shaped tree. Fake servers use `createRouterTransport` from
`@connectrpc/connect` (the vendor's documented test path: "a transport that
can be used to test your application without needing to make any network
requests", connect-query-es README §Testing) — no HTTP, no mocks of the glue
seam itself.

## Tasks

- [ ] T1 — deps + `LiveClients.transport` (one shared transport)
- [ ] T2 — glue seam (`live/query.ts`) + `QueryClient` boot +
      `QueryClientProvider`
- [ ] T3 — assigned-issues loader → query (first proof)
- [ ] T4 — stream → cache invalidation fan-out
- [ ] T5 — infinite `listMessages` + `listTopics` queries + mutation
      invalidation

## Composition

### Shell routing — DL-127 (compass-shell-routing/design.md)

Queries sit beneath the store's accessor seam (§A3), exactly the boundary
the routing record reserved: its composition note already states the query
layer lands "beneath the store's accessor seam" and "does not change routing:
the route-sync effect still writes only UI-state signals"
(compass-shell-routing/design.md:473-483). Its §A3 **F2 pending-aware
unknown-id fallback** ("The effect must NOT redirect a param whose id is
merely *not yet loaded*", design.md:196-204) reads its "data ready" signal
from the query layer's loaded/pending state once this record lands — for
stream-owned channel data that signal remains first-snapshot arrival (§A4
keeps the stream owning `CommsState`), and for query-backed params it is the
query's `isPending`. The principle — never redirect a not-yet-loaded id — is
loading-primitive-agnostic, as the routing record states.

### DL-106/107 — the transport seam (native app record)

All server data flows through the one transport chosen in
`createLiveClients` — "The one place transport is chosen"
(live/client.ts:2-3) — and the query layer's queryFns execute through exactly
that instance (§A2 exposes it; it does not create a second one). The
embedded-vs-native difference stays a `fetch` swap below the store: the shell
transport is "a `fetch` implementation that routes gRPC-Web requests to the
Compass daemon through the shell" so that "`createGrpcWebTransport({ fetch })`
streams ... incrementally" (daemon-transport.ts:1-2, 9-10), and the client
package already carries the fetch-parameterized factories
(`createCommsClientOverFetch(fetch, ...)` →
`createGrpcWebTransport({ baseUrl, fetch, ... })`,
packages/compass-client/src/index.ts:133-137). Nothing in the query layer
names a shell API or assumes local; the ConnectionProvider signature is
SEA-1688's record, not designed here — this record consumes the clients and
transport as `createLiveClients` builds them.

## Open Questions

### OQ-1 — Stream/cache reconciliation: does `CommsState` survive? (load-bearing)

The one genuine fork. Options:

1. **Stream keeps `CommsState`; query cache gets targeted invalidations
   (recommended, designed in §A4).** Zero rewrite of the tested
   snapshot+tail+resync driver (stream.ts:1-30) and of `preserveLocalAsks`
   (store.ts:782-786); two data homes exist but with a crisp split
   (live surface = stream; paginated/on-demand reads = cache) and an
   idempotent invalidation bridge. Cost: the eager full-history snapshot
   (stream.ts:99-108) still loads at boot — this record does not trim it; the
   exit is a follow-up SEA issue (filed at freeze) that the crisp live-vs-cache
   split this record establishes is the prerequisite for.
2. **Dissolve `CommsState` into cache-per-entity**: the stream writes
   `queryClient.setQueryData` per entity key; the four accessors become
   query reads; one cache, one invalidation model, and the eager snapshot
   can shrink to "channels + accounts only". Cost: re-homing the resync
   protocol and local-ask preservation onto a generic cache rewrites the
   most consistency-critical driver in the app, with test migration to
   match — high risk, and it blocks the SEA-1655 unblock behind a rewrite.
3. **Hybrid with per-entity `setQueryData` for hot paths**: option 1 plus
   surgical newest-page patches. Booked in §A4 as a permitted optimization,
   not a separate fork.

**Recommendation: option 1 now**, with option 2 recorded as a possible
future record once paginated history is proven (it would supersede this
record's §A4, not amend it silently).

### OQ-2 — Where mutations live: store methods vs `useMutation` in components (minor)

The store's write paths are methods today (`postMessage`,
store.ts:1475-1483) and §A5 keeps them so, adding key invalidation inside.
The alternative — components using `useMutation` directly — leaks server
state above the accessor seam and forks the write path. Recommendation:
store methods stay the only write surface; `useMutation` is used inside the
store only where its pending/error state is needed by a surface. Flagged
only because the SEA-1655 composer implementer may prefer hook-local pending
state; the seam rule (§A3) should win.

## Ledger note

This record needs **one** new ledger row. The ledger single-writer (compass)
owns `docs/designs/product/DECISIONS.md` and appends at ship (freeze = merge);
this record does not edit it and does not self-allocate. Allocated **DL-128**
(compass, 2026-08-03): the routing record froze at one row (DL-127), releasing
the DL-128 that lane had reserved, and this record stacks on and merges right
after it — DL-127 → DL-128 keeps the ledger dense. This record's
`Ledger-impact` front-matter line is number-neutral; compass appends the DL-128
row at ship.

The immutable one-liner compass appends (final, single-writer):

> DL-128 — The Compass UI adopts `@tanstack/solid-query` +
> `@connectrpc/connect-query-core` as the server-state query layer beneath the
> store accessor seam: unary reads become `createConnectQueryKey`-keyed cached
> queries; paginated history rides infinite queries (`listMessages` by
> `beforeMessageId`); mutations and SubscribeComms stream events invalidate by
> partial key; the stream keeps owning `CommsState`.
