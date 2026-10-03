# Compass global search — top-bar search over agents / PRs / issues / chats

Status: Draft

Owner lane: compass-ux (design) → compass-ui (execution)

Tracker: RIG-1621 (Compass: global search bar in the top bar — agents / PRs /
issues / chats). Matt ruled the approach on the issue: build the persistent
top-bar bar in addition to the Cmd/Ctrl+K palette; search runs server-side
("we likely will need some of this to be over network? the client can't store
the entire full text all the time"); scope is "all issues/PRs etc.", not the
caller's assigned set.

Overlaps: the frozen command-palette substrate (DL-152 palette + keymap as
first-class surfaces, DL-232 palette host anatomy, DL-233 six destination
kinds — all in `docs/designs/DECISIONS.md` § UI shell; cited by row id, not
line, because the ledger is append-only and continuously edited, so a line
number into it rots by construction). This record ADDS a second search
surface and remote providers on that substrate. Its PR mints DL-346; see
§Ledger notes for the rows it deliberately does NOT touch.

## Problem / Intent

Compass has one search surface: the Cmd/Ctrl+K palette
(`apps/ui/src/components/Palette.tsx`, mounted at `apps/ui/src/App.tsx:190-192`
— `<Show when={store.paletteOpen()}><Palette /></Show>`). Its navigation mode
fuzzy-matches only client-held collections, so three gaps block "find anything
from the top bar":

1. **Issues are scoped to the caller's queue.** The issue provider reads
   `store.assignedIssues()` (`apps/ui/src/keyboard/destinations.ts:125`:
   `store.assignedIssues().map((w) => ({ id: w.id, title: w.title }))`), per
   DL-233. Matt ruled the scope is all issues/PRs.
2. **Chat messages are unsearchable from the UI.** The server already ships a
   complete, ACL-scoped message full-text search —
   `rpc SearchMessages(SearchMessagesRequest) returns (SearchMessagesResponse)`
   (`proto/compass/v1/comms.proto:113`) over a generated tsvector
   (`go/internal/store/migrations/0001_init.sql:288`:
   `search_tsv TSVECTOR GENERATED ALWAYS AS (to_tsvector('english', text_content)) STORED`)
   — but `grep SearchMessages apps/ui/src` returns zero matches: no UI code
   calls it.
3. **Issue/PR full text has no index at all.** `CREATE TABLE issues`
   (`0001_init.sql:670-702`) carries `title`, `body`, `summary`, `labels` …
   but no `search_tsv` column and no GIN index; `queries/issues.sql` has only
   `UpsertIssueForgeFields` / `SetIssueState` / `GetIssue` / `ListIssues`
   (lines 9, 37, 40, 47) — no `SearchIssues`; no `rpc SearchIssues` exists in
   any proto. Fuzzy title matching over the client-held board cannot cover
   bodies at scale — the client must not hold the full corpus.

Intent: a persistent search input in the center of the top bar
(`apps/ui/src/App.tsx:76` `<header class="topbar">`), querying the daemon over
gRPC-Web for issues, PRs, and chat messages, plus the client-held agent
roster — sharing ONE query path with the palette (the `DestinationProvider`
seam) so the two surfaces can never diverge.

## Global Constraints

Every task below inherits these; none restates them.

- **UI stack:** SolidJS v2 (`strict: true`, tabs per Biome). v2 has **no
  `createResource`** — async work uses the split `createEffect`
  (compute-tracks-query / apply-runs-async) pattern the palette already uses
  (`Palette.tsx:151-152`: "v2 has no createResource; async work lives in a
  split effect"). No destructured props.
- **Kobalte:** the palette's listbox rides the Kobalte Search primitive with
  `open` pinned true (DL-232); tooltips ride the installed
  `@kobalte/core@2.0.0-alpha.0` (DL-245). Reuse these; install nothing new.
- **Transport:** all daemon calls are gRPC-Web through the shell's
  `compass_rpc` proxy — a WebView `fetch` cannot dial the daemon's Unix socket
  (`apps/ui/src/daemon-transport.ts:4-6`). New RPCs ride the existing
  `CompassClient` / `CommsClient` store options (`apps/ui/src/store.ts:604,631`)
  — never a second transport.
- **Registry/palette contracts are frozen-plus-additive** (DL-152/DL-232):
  extend `DestinationProvider` implementations; do not widen `CommandRegistry`
  or re-anatomize the palette host.
- **Server scoping:** every new query runs through the store's tenant-armed
  path — "every pool-path `s.q.<Query>` then arms SET LOCAL ROLE + the
  `compass.tenant_id` GUC from ctx in one round-trip so RLS scopes it"
  (`go/internal/store/store.go:87-89`). Access class for issue search is the
  board's: `ListBoardIssues` is "authenticatedOpen: any authenticated account
  may read the board … the whole board is repo-scoped, with no per-account
  filter" (`proto/compass/v1/compass.proto:44-47`). Message search keeps the
  `channel_members` membership JOIN (`queries/messages.sql:81`).
- **Query/limit conventions:** `websearch_to_tsquery('english', …)` (safe
  human-query parsing, `go/internal/store/messages.go:509-511`), server-side
  `clampLimit` ("bounds this to maxPageLimit (200)", `messages.go:520`),
  empty-query rejected with `ErrInvalidArgument` (`messages.go:504-506`).
- **Migrations:** the squashed `0001_init.sql` is the ONLY migration and
  "pre-dogfood databases are recreated on schema change" (seed-forward RD-2,
  `0001_init.sql:308-310`); plain non-CONCURRENT indexes — "migrations run
  inside a transaction (store.go applyMigration)" (`0001_init.sql:306-308`).
  The new column/index therefore lands as an edit to `0001_init.sql`.

## Approach

### A1 — one query path: remote-capable `DestinationProvider`s, two surfaces

The palette's navigation mode already has exactly the seam a second surface
needs: `DestinationProvider` is async by contract —
`query(input: string): Promise<Destination[]>`
(`apps/ui/src/keyboard/commands.ts:84-87`), and its own doc anticipates this:
"the store-backed ones resolve synchronously-wrapped today, but … later kinds
may be genuinely async, so `queryDestinations` is race-safe now via the
latest-wins generation guard" (`apps/ui/src/keyboard/destinations.ts:8-11`).
`queryDestinations` guarantees per-provider isolation (`Promise.allSettled`,
one rejected provider drops only its group) and latest-wins staleness dropping
(`destinations.ts:157-162,170-173`).

So the design is: **new remote providers behind the existing seam, consumed by
both surfaces.**

- A remote **issues** provider calls the new `SearchIssues` RPC (A3) and
  REPLACES the assigned-scope provider (`destinations.ts:121-131`, which reads
  `store.assignedIssues()` per DL-233) — Matt ruled the scope is all
  issues/PRs.
- A remote **prs** provider derives from the same `SearchIssues` response via
  the existing pure `prRows()` mapping (A4) and replaces the
  `store.prs()`-backed provider (`destinations.ts:132-150`).
- A remote **messages** provider calls the EXISTING `SearchMessages` RPC (A5)
  under a new `DestinationKind` `"message"`.
- The **agent / channel / topic / view** providers stay local and synchronous:
  the roster and channel list are small, client-held, and streamed live
  (`destinations.ts:62-63`: "a streamed roster/issue update is reflected on
  the next keystroke").

**The provider factory must be widened to receive the clients — a provider
cannot reach one through the store.** `createStoreDestinationProviders(store)`
takes only the `AppStore`, and `AppStore` exposes no client, no transport and
no query client: `options.comms` / `options.compass` (`store.ts:604,631`) are
fields on `AppStoreOptions`, the *input* to `createAppStore`, and every read
of them is inside that closure. The `AppStore` interface declares zero fields
of type `CommsClient` or `CompassClient` (the only mentions in its 256-585
declaration range are doc comments). So "call the RPC through the store's
client option" is not implementable as stated, and the obvious repair —
hanging a client on `AppStore` — is forbidden by DL-128, which moved
server-state reads OFF the store onto query hooks over the generated Connect
clients, "the store keeping only client/UI state". Which seam replaces it is
OQ-7 (load-bearing): thread the clients as a second argument, or route both
searches through the DL-128 query layer that `store.assignedIssues` already
uses (`store.ts:822-836`). Every task below names the seam as "the chosen
client seam (OQ-7)" rather than assuming one.

The top-bar surface and the palette both call
`queryDestinations(providers, input, generation, currentGeneration)` over the
SAME provider array, so ranking, grouping, isolation, and staleness behavior
cannot diverge. No parallel search stack exists anywhere.

### A2 — the top-bar surface: a real input with an anchored results panel

A new `TopBarSearch` component mounts in the center of the existing topbar
(`apps/ui/src/App.tsx:76` `<header class="topbar">`, between the view tabs and
the `topbar-spacer` at `App.tsx:120`). It is a persistent text input; while it
holds a non-empty query and focus, an anchored panel below it renders the same
grouped destination rows the palette renders (group headers per kind, fuzzy
score order within a group).

It does NOT re-anatomize the palette: DL-232's host anatomy (fixed wrapper,
permanently-open Kobalte Search, own backdrop) stays untouched. The shared
parts are extracted, not duplicated: the `KIND_LABELS` / `KIND_ORDER` tables
(`Palette.tsx:63-79`) and the destination-row grouping move to a shared module
both surfaces import; each surface keeps its own host shell (overlay vs
anchored panel). The alternative — the bar as a button that merely opens the
palette — was rejected: Matt asked for a search bar, and a fake input that
teleports focus into an overlay is the antithesis of one; see
§Alternatives considered.

Focus/keyboard: the input lives in the existing `topbar` focus zone
(`apps/ui/src/keyboard/zones.ts:23`:
`export type FocusZone = "left" | "main" | "right" | "topbar";` — topbar is
F6-reachable). A `search.focusGlobal` command registers on the shared registry
so the palette's action mode can reach it (the commands-as-inventory rule,
DL-229); Escape clears/blurs back to the prior zone.

### A3 — server: `issues.search_tsv` + `SearchIssues` (the real new work)

Mirror the message pipeline end to end:

1. **Schema** (edit `0001_init.sql`, seed-forward per RD-2). The obvious
   spelling of this column does not compile, and the failure is worth
   recording because it is not obvious from the docs:

   ```text
   ERROR:  generation expression is not immutable
   ```

   A `GENERATED ALWAYS … STORED` expression must be IMMUTABLE, and
   `array_to_string` is only STABLE — measured on PostgreSQL 18.4,
   `pg_proc.provolatile = 's'` for both its `(anyarray, text)` and
   `(anyarray, text, text)` overloads. `labels::text` fails identically
   (`array_out` is also STABLE). The reason is polymorphism, not the array
   type: `anyarray` must stay conservative for element types whose output
   depends on a GUC (`timestamptz_out` is STABLE because it reads
   TimeZone), so the planner cannot know the element type is benign.

   So wrap it in a function that is IMMUTABLE *honestly* — for `TEXT[]` the
   concrete element output function `textout` is itself IMMUTABLE
   (`provolatile = 'i'`), so the wrapper asserts nothing false:

   ```sql
   CREATE FUNCTION compass_labels_text(labels TEXT[]) RETURNS TEXT
       LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
       SET search_path = pg_catalog
       AS $$ SELECT array_to_string(labels, ' ') $$;
   ```

   `SET search_path = pg_catalog` is load-bearing: an IMMUTABLE function
   that resolves unqualified names at runtime can be captured by a
   caller's `search_path`. `STRICT` is defensive only — `issues.labels` is
   `NOT NULL DEFAULT '{}'` (`0001_init.sql:685`) — but it matters because
   `||` propagates NULL through the whole expression, so one NULL input
   would collapse the entire `tsvector` to NULL rather than dropping one
   term.

   The column labels each field with a weight rather than concatenating
   them into one flat string:

   ```sql
   search_tsv TSVECTOR GENERATED ALWAYS AS (
       setweight(to_tsvector('english', title), 'A') ||
       setweight(to_tsvector('english', body), 'B') ||
       setweight(to_tsvector('english', summary), 'B') ||
       setweight(to_tsvector('english', compass_labels_text(labels)), 'C')
   ) STORED
   ```

   `setweight` is IMMUTABLE, so this keeps the property the wrapper exists
   to satisfy. The weights are not cosmetic — they are what makes ranking
   work at all. Unlike the messages column, which indexes ONE field
   (`text_content`), this one spans a ~40-character title and a
   multi-kilobyte body, and `ts_rank` applies no length normalization by
   default. Measured on 18.4 with a title hit in a short row against the
   same term buried in a ~10 KB body:

   | ranking | title hit | body hit |
   | --- | --- | --- |
   | flat concat (unweighted) | 0.06079 | 0.06079 |
   | weighted, `{0.1,0.2,0.4,1.0}` | 0.60793 | 0.24317 |

   Flat concatenation ranks them EXACTLY equal, so T2's "title hit above
   body hit" assertion is unsatisfiable without the weights. Queries pass
   the matching weights array to `ts_rank`. This has to be decided now, not
   later: the column is `GENERATED ALWAYS … STORED`, so changing its
   expression after dogfood data exists means a full table rewrite.

   Use the two-argument `to_tsvector(regconfig, text)` — the literal
   `'english'` makes it that overload, which is IMMUTABLE; the
   single-argument `to_tsvector(text)` form is only STABLE (it reads
   `default_text_search_config`) and would fail the same way.

   Plus `CREATE INDEX issues_search_idx ON issues USING gin (search_tsv);`,
   modeled on the messages pair (`0001_init.sql:288,321`). Plain
   non-CONCURRENT index (transactional migrations, `0001_init.sql:306-308`).
   Verified on 18.4: the DDL applies, a label search for `triage` matches a
   row labelled `Needs-Triage` (the wrapper's text goes through normal
   stemming, yielding `'needs-triag'`, `'need'`, `'triag'`), and
   `EXPLAIN` shows a `Bitmap Index Scan on issues_search_idx`.

   **Operational hazard, for whoever later edits the wrapper:** a
   `CREATE OR REPLACE` of `compass_labels_text` does **not** recompute
   stored rows. Measured — after replacing the body to append a sentinel,
   existing rows kept their old `search_tsv` with no error and no warning.
   Any change to the wrapper's semantics therefore requires an explicit
   backfill, and this is exactly the class of drift the gate cannot see.
   Rejected alternative: `array_to_tsvector(labels)` concatenated with
   `||`, which is IMMUTABLE and *does* compile, but bypasses the parser —
   labels land as raw lexemes (`'Needs-Triage'`, `'area/ui'`), so a search
   for `needs-triage` or `triage` matches **nothing**. It fails silently,
   which is worse than the error above.
2. **Query** (`queries/issues.sql`): `-- name: SearchIssues :many` selecting
   the same column list `ListIssues` returns (`issues.sql:48-50`), filtered
   `WHERE search_tsv @@ websearch_to_tsquery('english', $1)`, ranked
   `ORDER BY ts_rank('{0.1,0.2,0.4,1.0}'::real[], search_tsv, websearch_to_tsquery('english', $1)) DESC, number DESC`
   — the four-element weights array is `{D,C,B,A}` (PostgreSQL orders it
   lowest label first), matching the column's A/B/C labelling,
   `LIMIT $2` — the `SearchMessages` shape (`queries/messages.sql:77-86`)
   minus the membership JOIN: the board's access class is authenticatedOpen
   ("any authenticated account may read the board … no per-account filter",
   `compass.proto:44-47`), and tenant isolation comes from RLS via the
   tenant-armed query path (`store.go:87-89`), not from a per-row predicate.
3. **Store wrapper** `func (s *Store) SearchIssues(ctx context.Context, query string, limit uint32) ([]Issue, error)`:
   trim-empty → `ErrInvalidArgument`, `clampLimit`, exactly as
   `Store.SearchMessages` does (`messages.go:503-507`).
4. **RPC** on `CompassService` beside `ListBoardIssues`
   (`compass.proto:48`), authenticatedOpen access class:

   ```proto
   // Full-text search over the whole board (title/body/summary/labels),
   // best-match-first. authenticatedOpen: same access class as
   // ListBoardIssues — the board is repo-scoped with no per-account filter.
   rpc SearchIssues(SearchIssuesRequest) returns (SearchIssuesResponse);

   message SearchIssuesRequest {
     string query = 1;
     // Page size; the server clamps to a maximum.
     uint32 limit = 2;
     // Reserved: v1 board is unversioned (see
     // SubscribeEventsResponse.snapshot_seq); carried as 0.
     uint64 snapshot_seq = 3;
   }

   message SearchIssuesResponse {
     // Best-match-first.
     repeated Issue issues = 1;
   }
   ```

   The request mirrors `SearchMessagesRequest`'s conventions
   (`comms.proto:916-928`: clamped `limit`, `snapshot_seq` cursor) with the
   `oneof scope` omitted — the board has no channel-like scope — and
   `snapshot_seq` reserved-as-0 exactly like `ListBoardIssuesRequest`
   (`compass.proto:339-343`). The response reuses the wire `Issue`
   (`compass.proto:935`), so the client maps rows with the adapter it already
   has for `ListBoardIssues`.

### A4 — PRs: no server PR corpus exists; derive PR results from issue hits

There is no PR table to index. The wire `PullRequest` rides
`Issue.prs` (`compass.proto:969`:
`repeated PullRequest prs = 17; // every PR opened for this issue`), and the
server projection leaves it nil today — "tracker/prs left nil (their producing
slices own them)" (`go/internal/board/issue_projection.go:171`).
`forge_authored_artifacts` (`0001_init.sql:801`) records authored-artifact
coordinates for idempotency, not PR text. Indexing PR content server-side
would invent a corpus this design has no producer for.

So the pr provider stays a pure client-side derivation — exactly DL-233's
shape, re-pointed at the search response: map each hit through `prRows()`
(`apps/ui/src/board.ts:132-134`:
`return all.flatMap((issue) => openPrs(issue).map((pr) => ({ issue, pr })));`)
and emit `Destination{kind:"pr"}` rows navigating via
`store.selectIssue` + `store.setActiveRightTab("pr")`
(`destinations.ts:144-147`). A PR is found through its owning issue's text.
When the prs-producing server slice lands, the SAME provider transparently
gains real PR rows because `Issue.prs` starts arriving populated. Deeper
PR-body search is deferred (§Open Questions OQ-2).

**Ship this knowing PR results are fixture-only at v1.** `Issue.prs` has
zero Go writers — grepping `Prs:` across `go/` returns no matches, so the
field is populated only by the UI's own fixtures
(`apps/ui/src/stub-data.ts:238`). Against a live daemon `openPrs(issue)` is
empty for every issue, so the pr provider contributes no rows: PR search
will look complete in fixture mode and return nothing in production, with
no error to explain the difference. That is acceptable — it is strictly not
worse than today's PRs tab, which is empty in live mode for the same reason
— but it must not be mistaken for working PR search, and the v1 acceptance
criteria must not include a live-mode PR assertion that cannot pass. A
fixture-mode test is the only honest coverage until the producing slice
lands.

### A5 — chat: reach the shipped `SearchMessages`, add kind `"message"`

The messages provider calls the existing RPC unmodified —
`rpc SearchMessages` (`comms.proto:113`) through the chosen client seam
(OQ-7). The server scopes results to the caller's visible set server-side
("The server scopes results to the caller's visible set regardless",
`comms.proto:918-919`; membership JOIN at `queries/messages.sql:81`), so the
provider passes no scope and trusts the server.

**Navigation cannot go through `store.openTopic` unguarded.** `openTopic`
(`store.ts:1327-1332`) resolves the id against the client-held topic list and
returns silently when it misses:

```ts
const topic = topics().find((t) => t.id === topicId);
if (!topic) return;
```

That set is not the corpus `SearchMessages` covers. `topics` is built at the
snapshot boundary from `listTopics({ channelId, includeArchived: false })` per
joined channel, while the search spans every message in every channel the
caller belongs to, archived topics included. So a hit in the difference
renders, is selectable, and does nothing at all when chosen — the same
silent-failure class this record rejects for `array_to_tsvector`, and the
analogy to the topic provider (`destinations.ts:104`) inverts the real
invariant: that provider enumerates `store.topics()`, so its ids are in-set
by construction.

The provider therefore navigates on the wire data the search already
returned, not the client-held set: `Message` carries `topic_id`
(`queries/messages.sql:78`) and the route needs the channel id, so the
message destination must carry both and route directly. A hit whose topic is
genuinely unreachable is dropped at map time rather than rendered dead. T5
carries the test that separates these, the way T1's label-only test separates
the two column spellings.

This requires widening the frozen `DestinationKind` union
(`commands.ts:49-55`) with `"message"` — a D5/DL-233 overlap the driver must
ledger (§Open Questions OQ-1).

Remote providers resolve `[]` on empty/whitespace input without issuing an
RPC: the server rejects an empty query as invalid
(`messages.go:504-506`), and the palette's empty-query-passes-everything rule
(`destinations.ts:14`) stays a local-provider behavior — an empty top-bar
query shows local destinations only.

### A6 — network discipline: debounce upstream, latest-wins downstream

A network round trip per keystroke is unacceptable. Two layers, both already
half-built:

- **Debounce (new, upstream):** each surface's split `createEffect` — the
  exact pattern `Palette.tsx:160-185` ships (compute tracks `query()`, apply
  runs the async fetch; generation captured at issue, re-checked at resolve,
  `Palette.tsx:90-95,163-172`) — gains a ~150 ms debounce before invoking
  `queryDestinations`, so a typing burst issues one RPC set. The debounce
  wraps the whole provider query, keeping the two modes' timing identical.
- **Latest-wins (existing, downstream):** `queryDestinations`' generation
  guard drops a stale resolve wholesale (`destinations.ts:173`:
  `if (generation !== currentGeneration()) return null; // stale — drop wholesale`),
  so a slow keystroke-N response can never clobber keystroke-N+1. Unchanged.

**Mixed latency is accepted and made invisible by the apply model:** local
(agent/channel/topic/view) and remote (issue/pr/message) providers settle at
different speeds, but `queryDestinations` awaits `Promise.allSettled` over all
of them and applies ONE map — the result set lands together at the slowest
provider's pace (a local-daemon Unix-socket round trip), never as groups
popping in one by one. A rejected/timed-out remote provider degrades to
local-only results via the existing per-provider isolation
(`destinations.ts:176-177`). Per-group streaming apply is explicitly deferred
(§Open Questions OQ-3).

## Alternatives considered

### Bar-opens-palette (no second result surface)

The top-bar element renders as an input but focusing it opens the Cmd+K
palette seeded with the typed query. Rejected: RIG-1621 asks for a search bar
in the top bar; a control that looks like an input and yanks focus into a
fixed overlay (DL-232's `.cx-palette` anatomy) is a bait-and-switch, and it
would still need every remote provider this design specifies — it saves only
the anchored panel, the cheapest part.

### Parallel search stack (dedicated search API + result types)

A `GlobalSearch` RPC returning a union result type, with its own client store
and renderer. Rejected: it duplicates what `DestinationProvider` +
`queryDestinations` already guarantee (async contract, per-provider isolation,
latest-wins, kind grouping — `destinations.ts:154-188`) and creates the
two-divergent-implementations problem this record exists to prevent. Per-kind
RPCs composed client-side also degrade independently — one slow corpus never
blocks the others' server work.

### Client-held full-text corpus

Ship issue bodies + message text to the client and search locally. Rejected by
Matt's ruling directly ("the client can't store the entire full text all the
time") — and the server already owns the indexes (`0001_init.sql:321`).

## Plan

Two lanes, landing separately: the Go/server lane (T1-T3) has no UI
dependency; the UI lane (T4-T7) depends on T3's generated client for the
issues/prs providers but can land T4 and the messages provider (T5) against
the already-generated `SearchMessages` client first.

### Server lane (Go)

#### T1 — `issues.search_tsv` column + GIN index

Edit `go/internal/store/migrations/0001_init.sql` (seed-forward, RD-2): add
the `compass_labels_text` IMMUTABLE wrapper (§A3.1), the generated column on
`CREATE TABLE issues` (`0001_init.sql:670-702`), and
`CREATE INDEX issues_search_idx ON issues USING gin (search_tsv);` beside the
other issues indexes, with a comment block matching the messages precedent
(`0001_init.sql:320-321`). The column is GENERATED ALWAYS … STORED over
`setweight`-labelled `title` (A), `body`/`summary` (B), and
`compass_labels_text(labels)` (C) per §A3.1, so re-poll upserts
(`queries/issues.sql:19-22` `ON CONFLICT … DO UPDATE SET title …, body …`)
keep it current with zero writer changes.

Neither the wrapper nor the weighting is a style choice. The direct
`array_to_string(labels, ' ')` spelling fails at migration time with
`generation expression is not immutable`; the IMMUTABLE-but-wrong
`array_to_tsvector` alternative makes label search silently match nothing;
and a flat unweighted concatenation makes T2's title-above-body ranking
assertion impossible to satisfy. §A3.1 has the measurements for all three.

`compass_labels_text` is the migration's FIRST function object — there is no
other `CREATE FUNCTION` in `0001_init.sql` — so two mechanics need stating.
It is schema-local, created into `current_schema()`, so the per-test-schema
isolation the pgtest suite relies on keeps parallel schemas from colliding;
no `IF NOT EXISTS` guard is needed, unlike the cluster-global roles. And the
grant block (`0001_init.sql:926-932`) grants only `ALL TABLES` / `ALL
SEQUENCES` under a comment claiming it covers every object above, which a
function would silently falsify: add
`GRANT EXECUTE ON FUNCTION compass_labels_text(TEXT[]) TO compass_app,
compass_system` rather than leaning on PostgreSQL's default PUBLIC EXECUTE,
so the file's explicit-grant posture stays true.

T1 also carries the `CREATE OR REPLACE` hazard as a comment at the
definition site, not only in this record: an editor stands at the function,
not at this file. The `messages_mentions_unrouted_idx` block
(`0001_init.sql:300-313`) is the precedent for that kind of inline
seed-forward reasoning.

- Interfaces: consumes the existing `issues` columns
  (`title`/`body`/`summary`/`labels`, `0001_init.sql:680-699`); produces
  function `compass_labels_text`, column `issues.search_tsv TSVECTOR`, and
  index `issues_search_idx`. No Go code.
- Test cycle: store test inserting an issue and asserting a
  `search_tsv @@ websearch_to_tsquery` hit on body text — plus a label-only
  hit (query `triage` against a `Needs-Triage` label), which is the
  assertion that fails under the `array_to_tsvector` spelling and passes
  under the wrapper. Without it the migration can regress to a form that
  compiles and finds nothing.

#### T2 — sqlc `SearchIssues` + store wrapper

Add `-- name: SearchIssues :many` to `go/internal/store/queries/issues.sql`
(select list identical to `ListIssues`, `issues.sql:48-50`; predicate/rank per
§A3.2), run sqlc, and add
`func (s *Store) SearchIssues(ctx context.Context, query string, limit uint32) ([]Issue, error)`
to the store, mirroring `Store.SearchMessages` (`messages.go:503-527`):
trimmed-empty query → `ErrInvalidArgument`, `clampLimit`. Tenant isolation is
the armed-GUC RLS path (`store.go:87-89`) — the query adds no tenant
predicate, like every other issues query, because `issues` carries ENABLE +
FORCE RLS and a `tenant_isolation` policy, so a per-row predicate would be
redundant.

Row mapping needs a NEW function, not the existing one: sqlc emits a distinct
row struct per query, a convention the codebase states outright at
`issues.go:226-227`. Add `issueFromSearchRow(r db.SearchIssuesRow) Issue`
beside `issueFromGetRow`/`issueFromListRow`, delegating to the shared
`issueFromColumns` (`issues.go:237`) — `issueFromListRow` will not compile
against a search row. `websearch_to_tsquery('english', $1)` appears in both
the WHERE and the ORDER BY, which is the `SearchMessages` shape
(`messages.go:516-521`) and collapses to one generated query parameter.

- Interfaces: consumes T1's column; produces
  `Store.SearchIssues(ctx, query string, limit uint32) ([]Issue, error)` and
  `issueFromSearchRow`.
- Depends on: T1 — sqlc's db-prepare validates against the live schema, so
  T1 must be in place before T2's generate can pass.
- Test cycle: store tests — rank order (title hit above body hit, which holds
  only with the §A3.1 `setweight` labelling), empty query rejected, limit
  clamped, cross-tenant row invisible.

#### T3 — `rpc SearchIssues` on `CompassService`

Add the rpc + `SearchIssuesRequest`/`SearchIssuesResponse` messages exactly as
quoted in §A3.4 to `proto/compass/v1/compass.proto` beside `ListBoardIssues`
(`compass.proto:48`); regenerate Go + TS. Handler in the server beside the
`ListBoardIssues` handler: call `Store.SearchIssues`, map rows through the
EXPORTED `board.IssueToProto` (`go/internal/board/issue_projection.go:164`) —
the lowercase `issueToProto` at :174 is package-private and the handler lives
in another package, so the exported form is the one to call —
`snapshot_seq` accepted and ignored (reserved-as-0, like
`ListBoardIssuesRequest`, `compass.proto:339-343`).

The access class is NOT declared in the proto; the proto comment only
documents it. Enforcement is a Go switch in `go/internal/auth/admin_gate.go`,
where `CompassServiceListBoardIssuesProcedure` sits in the `authenticatedOpen`
case (`:88`). The new procedure MUST be added there. Omit it and it falls
through to the `default` branch, which fails closed to `adminOnly` and
reports unclassified — and `classify_exhaustive_test` fails the build, which
is the gate that catches this.

- Interfaces: consumes T2's `Store.SearchIssues`; produces
  `rpc SearchIssues(SearchIssuesRequest) returns (SearchIssuesResponse)`, its
  `admin_gate.go` classification, and the generated TS client method the UI
  lane consumes.
- Depends on: T2.
- Test cycle: handler test — hit round-trips to wire `Issue`; empty query →
  InvalidArgument; unauthenticated → the service's standard auth failure.
  `classify_exhaustive_test` covers the gate registration.

### UI lane (TypeScript / Solid)

#### T4 — shared destination-surface module + `TopBarSearch` shell

Extract the palette's group-render tables (`KIND_LABELS`/`KIND_ORDER`,
`Palette.tsx:63-79`) and the destination-row option mapping into a shared
module under `apps/ui/src/keyboard/` both surfaces import; `Palette.tsx`
behavior is unchanged (DL-232 anatomy untouched). Add `TopBarSearch` mounted
in `App.tsx`'s `<header class="topbar">` (`App.tsx:76`) before
`.topbar-spacer` (`App.tsx:120`): a text input + anchored results panel
rendering grouped `Destination` rows via the shared module, fed by its own
split `createEffect` (the `Palette.tsx:160-185` pattern). Input participates
in the `topbar` focus zone (`zones.ts:23`); Escape clears and returns focus;
selecting a row runs `destination.navigate()` and clears. Register
`search.focusGlobal` on the shared registry (additive, DL-225's `unregister`
on unmount).

T4 also lands the ~150 ms debounce upstream of `queryDestinations` in BOTH
surfaces' effects (§A6) — here, not in T6, because this is the task that
writes the effect, and the palette's existing effect
(`Palette.tsx:160-185`) is a one-line change in the same pass. Landing it
with the local providers is harmless and removes a T4↔T6 ordering knot.

- Interfaces: consumes the provider factory (signature per OQ-7) +
  `queryDestinations` (`destinations.ts:65,164`); produces
  `TopBarSearch: Component`, the shared group-render module, and command id
  `search.focusGlobal`.
- Depends on: nothing server-side (renders local providers day one).
- Test cycle: component tests — typing renders grouped rows; Enter navigates;
  Escape clears; latest-wins under an interleaved slow resolve; debounce
  collapses a typing burst to one provider query.

#### T5 — messages provider (kind `"message"`) over `SearchMessages`

Add `"message"` to `DestinationKind` (`commands.ts:49-55`) and a provider in
`destinations.ts` that (a) resolves `[]` for empty/whitespace input, (b)
otherwise calls the generated `SearchMessages` client through the chosen
client seam (OQ-7), mapping each hit to a `Destination{kind:"message"}`
titled from the message text (first-line truncation) and navigating on the
returned `topic_id` + channel id per §A5 — NOT through an unguarded
`store.openTopic`, which no-ops on any topic outside the client-held set.
Extend `KIND_LABELS`/`KIND_ORDER` with the `Messages` group (last). Both
surfaces gain chat search by construction.

The union widening and the table extension are ONE atomic change:
`KIND_LABELS` is typed `Record<DestinationKind, string>` (`Palette.tsx:64`),
so adding `"message"` to the union without extending the table does not
typecheck.

- Interfaces: consumes `CommsClient.searchMessages` (generated from
  `comms.proto:113,916-933`) + the router; produces the `"message"` kind +
  provider.
- Depends on: T4 (shared module); OQ-1 and OQ-7 answered (both load-bearing).
- Test cycle: provider test with a `createRouterTransport` fake serving
  `CommsService.searchMessages` (the `query.test.ts:121-122` pattern); empty
  input issues no RPC; **and a hit whose topic is absent from the
  client-held set is not rendered as a dead row** — the assertion that
  separates a working chat search from a silently broken one.

#### T6 — remote issues + prs providers; retire the assigned-scope pair

Replace the `issues` and `prs` providers (`destinations.ts:120-150`) with one
`SearchIssues`-backed fetch through the chosen client seam (OQ-7): issue hits
map to `Destination{kind:"issue"}` navigating via `store.selectIssue` (as
today, `destinations.ts:128`); the same response derives pr rows through
`prRows()` (`board.ts:132-134`) navigating via `store.selectIssue` +
`store.setActiveRightTab("pr")` (`destinations.ts:144-147`). Empty input
resolves `[]` (remote), subject to OQ-4. The debounce belongs to T4, where
the effect it wraps is written — not here.

- Interfaces: consumes T3's generated `CompassClient.searchIssues` +
  `prRows()`; produces the remote issue/pr providers; removes the
  `store.assignedIssues()`/`store.prs()` provider reads (the accessors stay —
  Backlog and the board still read them, `store.ts:570-579`).
- Depends on: T3, T4; OQ-4, OQ-6 and OQ-7 answered.
- Test cycle: fake-transport provider tests — issue + derived pr rows from one
  response; provider rejection degrades to local-only groups. PR-row coverage
  is fixture-mode only (§A4: `Issue.prs` has no live writer).

#### T7 — styling + a11y pass on the bar

`.cx-*`-token styling for the input and panel (topbar composition per
`apps/ui/src/design/surfaces.md:41-48` — topbar on `--cx-bg`, 1px
`--cx-border` rules, shadows reserved for floating layers, so the anchored
panel MAY carry the floating-layer shadow), `aria-expanded`/listbox semantics
on the panel, focus ring per DL-151, and the CoachTip/`shortcutFor` treatment
for the `search.focusGlobal` chord if one is allocated (OQ-5).

- Interfaces: consumes T4's component; produces CSS + a11y attributes only.
- Depends on: T4.
- Test cycle: a11y assertions in the T4 component test file; visual check
  against the topbar composition rules.

## Tasks

- [ ] T1 (server): `issues.search_tsv` generated column + `issues_search_idx`
      GIN index in `0001_init.sql`
- [ ] T2 (server): sqlc `SearchIssues` + `Store.SearchIssues` wrapper
      (empty-query reject, `clampLimit`, RLS tenant path,
      `issueFromSearchRow`)
- [ ] T3 (server): `rpc SearchIssues` + request/response messages on
      `CompassService`; handler via `board.IssueToProto`; `admin_gate.go`
      authenticatedOpen registration; regen clients
- [ ] T4 (UI): shared destination-surface module + `TopBarSearch` in the
      topbar; `search.focusGlobal` command; the debounce for both surfaces
- [ ] T5 (UI): `"message"` `DestinationKind` + `SearchMessages`-backed
      provider (both surfaces), navigating on wire data
- [ ] T6 (UI): remote issues/prs providers over `SearchIssues`; retire
      assigned-scope providers
- [ ] T7 (UI): styling + a11y pass on the bar

## Ledger notes

**DL-346 is minted in this PR** (`docs/designs/DECISIONS.md`, `## UI shell`):
the server-side-corpus stance, the two-surface/one-provider-set shape, the
all-board scope, the `compass_labels_text` IMMUTABLE requirement, and the
fixture-only PR caveat.

Still outstanding, and deliberately NOT done here:

- **DL-233 needs a Status flip or an amending row.** It explicitly ledgered
  the assigned-issue seam, and this design moves the
  issue provider's scope to server-side all-issues search. Changing a frozen
  decision is not a record edit — the ledger's own convention is that
  "decision prose in frozen records is never edited"
  (`meta/compass-design-ledger/design.md:287-292`), so this is OQ-6 for Matt
  rather than a call this record makes.
- DL-152/DL-232 stay Active untouched: the palette surface, host anatomy, and
  registry contract are consumed, not changed.

## Open Questions

- **OQ-1 (load-bearing): does `DestinationKind` gain `"message"`?** The union
  is frozen by D5/DL-233 (`commands.ts:49-55` cites "D5:425-429"). Without a
  new kind, chat results have no group and the bar cannot show them; the only
  alternative is a chat-less bar, which contradicts RIG-1621's title
  ("agents / PRs / issues / chats"). **Recommendation:** add `"message"` as an
  additive widening ledgered by the driver (the same additive posture as
  DL-225's registry `unregister`). Blocks T5.
- **OQ-2 (non-load-bearing): PR-body full-text search.** A4 finds PRs through
  their owning issue's text because no server PR corpus exists
  (`issue_projection.go:171` — prs left nil; no PR table in
  `0001_init.sql`). **Recommendation:** defer until the prs-producing server
  slice lands; the provider seam already accommodates it. The design is
  correct without it.
- **OQ-3 (non-load-bearing): per-group streaming apply.** §A6 applies one
  settled map, so local groups wait for the slowest remote provider
  (`destinations.ts:170-172` awaits `Promise.allSettled`). Against a
  local-daemon Unix socket this is milliseconds. **Recommendation:** keep the
  single-apply model; revisit only if a remote forge-backed daemon appears.
- **OQ-4 (load-bearing): empty-query behavior for the issue group.** Today an
  empty palette query browses ALL assigned issues (`destinations.ts:14`:
  "an empty query passes everything"); the remote provider resolves `[]` on
  empty input (server rejects empty queries, `messages.go:504-506`), so the
  palette loses its empty-query issue browse. **Recommendation:** accept —
  the Backlog view (`store.assignedIssues`, `store.ts:577-579`) is the browse
  surface; the palette/bar are for typed queries. If you want the browse
  kept, the fallback is an empty-input browse over titles ALREADY IN MEMORY,
  ranked by the existing `fuzzyScore` and never touching bodies — which is
  categorically different from the client-held full-text corpus §Alternatives
  rejects, and so not a contradiction of your ruling. It still needs a
  choice of collection, because they give different answers:
  `store.assignedIssues()` preserves today's DL-233 behavior, while
  `store.issues()` is the whole client-held board. Blocks T6's exact provider
  shape, hence load-bearing.
- **OQ-5 (non-load-bearing): a dedicated focus chord for the bar.** `/` and
  `Ctrl+Shift+F` are conventional; wave-1 leader allocation is frozen
  (DL-252) and `/` may collide with future composer
  affordances. **Recommendation:** ship `search.focusGlobal` registered but
  unbound (palette-reachable per DL-229); allocate a chord in a keymap wave.
- **OQ-6 (load-bearing): how does DL-233's assigned-issue scope get
  retired?** DL-233 ledgered the palette issue provider as scoped to
  `store.assignedIssues`; this design replaces that with server-side
  all-board search on your explicit "all issues/PRs etc." instruction, so the
  frozen row and the shipped design disagree. The ledger forbids editing
  frozen decision prose (`meta/compass-design-ledger/design.md:287-292`), so
  the options are (a) flip DL-233's Status cell to Superseded with a pointer
  to DL-346, or (b) leave DL-233 Active and let DL-346 stand as the amending
  row that narrows it. **Recommendation:** (a) — the scope change is a
  reversal, not a narrowing, and leaving it Active means two Active rows give
  contradictory answers about what the issue provider searches. Blocks T6.
- **OQ-7 (load-bearing): which seam hands a provider its RPC client?** A
  provider cannot reach one today: `createStoreDestinationProviders(store)`
  receives only the `AppStore`, which declares no client, no transport and no
  query client — `options.comms`/`options.compass` (`store.ts:604,631`) are
  inputs to `createAppStore` and never escape its closure. Hanging a client
  on `AppStore` is the obvious fix and is forbidden by DL-128, which moved
  server-state reads OFF the store onto query hooks over the generated
  Connect clients. Options: (a) widen the factory to
  `createStoreDestinationProviders(store, clients?)` and thread the clients
  from the two call sites, leaving `AppStore` and DL-128 untouched — note
  this changes a DL-233-ledgered function's signature, additively; or (b)
  route both searches through the DL-128 query layer, the pattern
  `store.assignedIssues` already uses (`store.ts:822-836`).
  **Recommendation:** (a) — the providers are called from an async
  contract that already returns promises, so they need a client, not a
  reactive hook, and (b) would put `useQuery` inside a non-component async
  function. Blocks T4, T5 and T6.
- **OQ-8 (load-bearing): does the Cmd+K palette's issue scope change too, or
  only the new bar?** Your "all issues/PRs etc." ruling was given on
  RIG-1621, which is about the top-bar bar; §A1 extends it to the palette by
  sharing one provider set, which silently changes what a shipped surface
  returns. Options: (a) both surfaces share the all-board provider set —
  upholds §A1's no-divergence thesis, one implementation, but changes
  existing palette behavior; (b) the palette keeps assigned-scope and only
  the bar searches the whole board — literal to RIG-1621, but needs two
  provider sets and forfeits this record's central guarantee.
  **Recommendation:** (a), which also makes OQ-6's DL-233 supersession
  strictly necessary rather than optional.
