# Compass reference links

## Problem / Intent

Tracker: RIG-5060. The Compass UI shows artifact references as dead text.
An agent writes `RIG-123`, `#42`, `RigelBuild/compass#42` or a commit SHA in
a message, a trace, or a channel topic, and the reader must copy it into a
browser. GitHub and Linear turn the same text into links. Compass should too,
on every surface: messages, session traces, channel headers, and the board.

Acceptance (Matt, RIG-5060):

- Known ref patterns (`RIG-123`-style tracker keys, `#123`, `owner/repo#123`,
  commit SHAs) link to the right forge or tracker URL.
- Ref prefixes resolve from the server's configured forges and trackers. The
  UI hard-codes no prefix.
- An unresolved ref stays plain text, never a broken link. This record reads
  "unresolved" as "no configured prefix, repo or template" (Open Question 6).
- Code spans and code blocks are not rewritten.

Two gaps block this today:

1. **The UI cannot learn the server's forges and trackers.** No RPC carries
   them. `GetServerInfoResponse` in `proto/compass/v1/compass.proto` carries
   only `version`, `api_version`, `rev` and `enrolled_runners`. The forge
   config lives in `ForgeConfig` in `go/server/serve.go` (`Host`, `SeedRepos`,
   App ids, Linear secret NAMEs), and the live repo set is the
   `forge_repo_subscriptions` table (`SeedRepos` is "a declarative SEED, not
   the live target set"). No Linear team-key list and no Linear workspace slug
   exist anywhere on the server. `registerLinearForgeCoordinate` registers
   Linear at the fixed `forge.LinearHost` (`"linear.app"`), and a Linear team
   key reaches the server only as `repo` on a request
   (`SubscribeForgeRequest.repo`: "GitHub owner/name; Linear team key").
2. **No render seam rewrites refs.** `MarkdownText`
   (`apps/ui/src/components/MarkdownText.tsx`) runs `rehypeInertRaw` →
   `rehypeProseBreaks` → `rehypeMentionChips`; nothing touches refs. The
   plain-text surfaces (`SessionTrace`, `ChannelHeader` in `ChannelView.tsx`,
   `TopicView`, `IssueCard`, `PrCard` in `Bridge.tsx`, the right sidebar)
   render bare text nodes. The wire already carries each artifact's canonical
   link (`Issue.url`, `PullRequest.url`, `TrackerRef.url`, adapted verbatim by
   `adaptIssue` / `adaptPullRequest` in `apps/ui/src/live/adapt.ts`), but no
   surface uses them for outbound navigation.

Non-goals: bare-URL autolinking on plain-text surfaces (markdown already does
it through GFM); GitLab `!123` merge-request refs and GitLab/Forgejo templates
(no client for either provider exists); in-app navigation to a referenced
issue (this record is outbound links only); links inside board cards (see
Approach § Board surfaces).

## Approach

### Source of truth: a new RPC

The server owns every prefix and every URL shape. A new read RPC,
`GetReferenceConfig`, returns two lists:

- **Forges.** For each code forge the server reads: its enabled repos and
  two URL templates (issue, commit). Today that is GitHub only. The repos are
  the enabled `forge_repo_subscriptions` rows for the configured host
  (`ListEnabledForgeRepoSubscriptions` in
  `go/internal/store/forge_cursors.go`). The templates come from
  `ForgeConfig.Host`, so a GitHub Enterprise host works with no UI change.
- **Trackers.** For each tracker: its team keys and one issue URL template.
  Today that is Linear only. The server reads the workspace URL key and the
  team keys from Linear's GraphQL API (`organization { urlKey }`,
  `teams { nodes { key } }`) with the credential it already holds. The
  `read` scope covers both. `tokenScope` in
  `go/internal/linearagent/client.go` does not change.

The UI knows no host, no prefix and no path shape. It fills `{repo}`,
`{number}`, `{sha}` and `{key}` into the server's templates. Resolution for
one token: template, else plain text.

GitHub's `/issues/N` URL redirects to `/pull/N` when N is a PR, so one
issue template serves both kinds. The UI never needs to know which kind a
`#N` is.

**Fixture mode.** `bootFixture` (`apps/ui/src/boot-fixture.ts`) builds a
store with no transport, so the RPC query cannot run there, as with
`modelRegistry`. The store takes a seed instead, `initialReferenceConfig`,
on the model of `initialIssues` and `initialComms` in `AppStoreOptions`.
`bootFixture` passes a fixture config, so the committed visual shots show
links.

**Structured fields do not parse text.** A surface that shows one
artifact's own key or number links it from that artifact's URL:
`Issue.url`, `TrackerRef.url` or `PullRequest.url`. The wire already carries
them (`adaptIssue` and `adaptPullRequest` in `apps/ui/src/live/adapt.ts`).
See Approach § Board surfaces.

**Access and tenancy.** The RPC is `authenticatedOpen` in
`classifyProcedure` (`go/internal/auth/admin_gate.go`), like
`GetModelRegistry`: any signed-in account may read it. Repo names are not
public, so it is not unauthenticated. The two halves differ in scope:

- The forge half is tenant-scoped. Subscription rows are keyed by tenant
  (`ON CONFLICT (tenant_id, forge_provider, forge_host, repo)`), and the
  store resolves the caller's tenant.
- The tracker half is deployment-scoped. The server holds one Linear token
  source (`buildLinearTokenSource` in `go/server/serve.go`), so every
  account sees the same workspace key and team keys. Open Question 7 asks
  whether that is acceptable.

For the self-host and managed split, see
`docs/concepts/self-host-and-managed.md` and
`docs/designs/meta/oss-core-managed-boundary/design.md`.

**Caching.** The UI fetches the config once per boot. A new subscription
shows after a reload. The server caches the Linear workspace: a good value
for 10 minutes, the last good value while a refresh fails, a 1-minute pause
after a failure, and a 3-second deadline on each fetch (T1).

Failure modes:

- The store read fails: the RPC fails with `Internal`. Grammar refs stay
  plain. Structured fields still link from their artifact URLs.
- A Linear refresh fails: the server serves the last good workspace. With
  no good value yet, it omits the tracker and logs a warning. The forge half
  waits at most the Linear deadline.
- No subscriptions and no Linear: an empty response. Only structured fields
  link.

### Ref grammar

The resolver splits text on whitespace. For each word it strips leading
`(`, `[`, `{`, `"`, `'` and trailing `.`, `,`, `;`, `:`, `!`, `?`, `)`,
`]`, `}`, `"`, `'`, and a trailing `'s` or `’s`. The stripped word must then
match one pattern in full:

- **Repo ref:**
  `^([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})#([1-9][0-9]{0,6})$`.
  It links when the repo, compared in lowercase, is a config repo. Any other
  repo stays plain, so a path such as `apps/ui#3` does not link.
- **Bare ref:** `^#([1-9][0-9]{0,6})$`. It links against the context repo
  (below). On a single-repo server, prose such as `#1 priority` links to
  issue 1 (Open Question 6).
- **Tracker key:** `^([A-Z][A-Z0-9]{0,9})-([1-9][0-9]{0,6})$`. It links
  when the prefix is a known team key. The match is case-sensitive:
  `rig-123` stays plain. `SHA-256` and `UTF-8` stay plain unless a team has
  that key. A team key such as `QA` or `API` makes `QA-1` link in prose
  (Open Question 2).
- **Commit SHA:** `^(?:[0-9a-f]{7,12}|[0-9a-f]{40})$` with at least one
  digit and at least one letter. It links against the context repo. Lengths
  13–39 stay plain on purpose: every Compass row id is 32 lowercase hex
  characters (`newID` in `go/internal/store/ids.go` hex-encodes 16 random
  bytes), and agents paste those ids into messages. A number (`1234567`), a
  word (`defaced`), uppercase hex and a 64-character digest also stay plain.
  An 8-hex color written without `#` (`ff00aa80`) still matches; that false
  positive is accepted.

Whole-word matching gives the boundary rules with no extra code:

- A URL is one word and matches no pattern. Nothing inside a URL links.
- A word wrapped in backticks keeps its backticks and matches nothing. Code
  typed into plain text stays plain.
- A branch name such as `compass-ui-1022-bridge-ui` matches nothing.

The runs always join back to the exact input text.

### Context repo for `#N` and SHAs

A bare `#N` or a SHA needs a repo. The resolver takes the first that
applies:

1. The surface's own repo (`RefContext.repo`). It is a GitHub `owner/name`
   only. A Linear-provider issue carries its team key in `repo`
   (`STUB_ASSIGNED_ISSUES` in `apps/ui/src/stub-data.ts` has
   `forge: LINEAR_FORGE, repo: "SEA"`), so `refContextOf` gives it no repo.
2. The only config repo (`RefIndex.soleRepo`), when the server reads
   exactly one. It comes from the RPC alone, so it does not change with what
   the board has loaded.
3. Otherwise the ref stays plain text.

The context repo must be a config repo, or the ref stays plain. Messages,
traces and channel headers carry no repo today. On a server with more than
one repo, their bare refs stay plain (Open Question 3).

### SHA links

A SHA fills `{sha}` in the commit template as written. GitHub accepts a
7-character prefix on small and medium repos: `1e4e1fe1` on
`RigelBuild/compass` returned HTTP 200. On a very large repo GitHub
returned 404 for 7- and 12-character prefixes, and only the full SHA worked
(checked on `torvalds/linux`). The minimum stays 7, git's default
abbreviation (Open Question 4). The `VcsPane` commit rows use the same rule
through `commitUrl`.

### One resolver, two render seams

A new module, `apps/ui/src/references/`, holds the pure resolver. It has no
Solid import, on the model of `mentionRuns`:

- `buildRefIndex(config)` builds the index.
- `refRuns(text, index, context)` returns text runs and ref runs.
- `commitUrl(index, repo, sha)` links one structured commit.

Two consumers use it:

1. **Markdown.** `rehypeRefLinks({ index, context })` runs after
   `rehypeMentionChips` in `MarkdownText`. It returns an attacher, the same
   form as `rehypeMentionChips(options): () => (tree: HastParent) => void`
   in `apps/ui/src/markdown/rehype-mention-chips.ts`. It walks text nodes
   with the same split-text-node method. It skips text under `code`, `pre`,
   `a`, and any element `rehypeMentionChips` emits. So code spans and code
   blocks are not rewritten, a GFM autolink is not linked twice, and a chip
   stays a chip. It emits `a` elements with `href` and
   `className: ["ref-link"]` only. The `a` override in `MarkdownText` adds
   `target`, `rel` and the click, so every markdown link has one click
   path. Today that override drops the class: it renders
   `<a href={safe() ?? "#"} target=… rel=… onClick=…>` and never reads
   `p.class`. T7 makes it pass `class={stringAttr(p.class)}`, as the `code`
   override already does.
2. **Plain text.** `<RefText text context />` renders the runs. Each ref run
   renders through `RefAnchor`. Structured fields use `RefAnchor` directly
   with their artifact URL.

Every ref anchor has `class="ref-link"`, `target="_blank"` and
`rel="noreferrer noopener"` (the value the `MarkdownText` override already
sets). A click calls `preventDefault()`, `stopPropagation()` and
`openExternal(href)`.

URL guard: a new `safeWebHref` in `apps/ui/src/safe-url.ts` calls the
existing `safeHref` and then allows `http:` and `https:` only (no
`mailto:`). The resolver and `RefAnchor` use it. No second URL parser is
written.

The index reaches components through a Solid context, `RefIndexContext`,
whose default is an empty index. This is the one new abstraction. It is
needed because `MarkdownText` is a leaf with many callers and is tested
without a store. Threading the index through props would touch every call
site. With the empty default, a test or a mount with no provider renders
plain text, as today. The store builds the index in a memo whose `equals`
compares `RefIndex.signature`. The signature changes only when the config
changes, so a board refresh never re-parses mounted messages.

### openExternal moves out of MarkdownText and guards itself

`MarkdownText.tsx` holds the only `Browser.OpenURL` call
(`@wailsio/runtime`), inside `openExternal`, and `RefAnchor` needs it too.
T5 moves `openExternal` to `apps/ui/src/open-external.ts` with the same
signature, `export function openExternal(url: string): void`, and adds one
guard: it calls `safeHref(url)` and opens nothing when that returns null.
Today only the call site vets the URL. The `MarkdownText` comment says "The
caller has already scheme-sanitized the href (`safeHref`)". With three
callers (`MarkdownText`, `RefAnchor`, and the compass-settings `auth_url`),
the shared seam must vet it itself. `safeHref` keeps `mailto:`, so markdown
mail links keep working.

The compass-settings record plans the same file and the same signature
(its S5). T5 is its own task, so either record can take the move alone.
Whichever lands first does the move; the other imports it. The
`@wailsio/runtime` import zone stays two files: `open-external.ts` and
`daemon-transport.ts`.

### Board surfaces

`IssueCard` and `PrCard` (`Bridge.tsx`) are `<button>` elements. An `<a>`
inside a `<button>` is invalid HTML and breaks keyboard use (DL-097 §2). So
card text stays plain. The same rule holds at any site inside an
interactive ancestor, such as a topic tab or a row that is a button.

The board's links live in the right sidebar, which has no button ancestor.
They link from the artifact, not from parsed text:

- `.r-detail-issue` in `IssueDetailHead` and the `VcsPane` Issue row show
  `issueKey(...)` (`apps/ui/src/board-render.ts`). It returns the tracker id,
  `repo#n`, or `host/repo#n` in multi-forge mode, which the grammar never
  matches. They render `RefAnchor` with `issue.tracker?.url ?? issue.url`.
- `.pr-num` in `PrPane` renders `RefAnchor` with `pr.url`.
- `.commit-sha` rows in `VcsPane` render `RefAnchor` with `commitUrl(...)`,
  or plain text when it gives none.

Open Question 5 asks whether cards get their own outbound control.

### Surfaces covered

- **Messages** (`MessageStream` through `MarkdownText`): the rehype plugin.
- **Session traces** (`SessionTrace`): it does not use `MarkdownText`; it
  renders prose as raw text. `RefText` goes on `.block-text`,
  `.block-thinking`, `.plan-content` and `.notice-text`. Tool titles, tool
  output and diffs are code and stay plain. Traces have no context repo
  (Open Question 3).
- **Channel headers:** `.conv-topic` in `ChannelHeader` (`ChannelView.tsx`)
  and `.conv-name` in the `TopicView` header, through `RefText`.
- **Board:** in the right sidebar, `.r-detail-title`, the `VcsPane` Summary
  row and `.pr-title` through `RefText` with `refContextOf(artifact)`, plus
  the structured fields above.

## Alternatives considered

**Source of truth.**

- (a) RPC, plus a store seed for fixture mode. Chosen; see Approach.
- (b) Loaded artifacts only. Rejected. Most refs in messages name issues
  the board has not loaded. Whether a ref linked would depend on what
  happened to be on screen.
- (c) Hybrid: the RPC plus an index of every loaded artifact's canonical
  URL, with an exact match first. This was the first draft. It was rejected
  after review because its live gains are small. GitHub already redirects
  `/issues/N` to `/pull/N`. The server never fills `Issue.tracker`
  (`IssueProjection.record` in `go/internal/board/issue_projection.go`:
  "tracker/prs left nil"), so exact tracker links existed only in fixtures.
  Its costs were real. Bare `#N` and SHA links would depend on the loaded
  repos: one artifact from a second repo turns `soleRepo` off. Each new
  artifact would also change the index and re-parse every message. The seed
  covers fixtures, and structured fields link from their own URLs.

**Who owns the URL shape.** The server could send `provider` plus a web
base URL and let the UI keep per-provider paths. Rejected: the UI would
hard-code `/issues/`, `/commit/` and Linear's `/issue/`, which the
acceptance bars, and each new forge would need a UI change. The server
sends templates.

**Extend `GetServerInfo`.** Rejected. `GetServerInfo` is a static
build-info read. Reference config needs a store read and a Linear call, and
each can fail on its own. Mixing them makes server info fail when Linear
does.

**Resolve each ref on the server (a `ResolveRefs` RPC).** Rejected. It
costs a round trip per render, and links appear late as calls return. It is
also the only way to check that an issue exists (Open Question 6).

**Link any `owner/repo#N` on the GitHub forge, subscribed or not.**
Rejected for now. The acceptance says prefixes resolve from configured
forges. With a second GitHub host, the host would be a guess. Paths such as
`apps/ui#3` would link. The cost: refs to upstream and dependency repos stay
plain. The gate is one map lookup, so relaxing it later is cheap.

**Split words on `,` or `/` as well.** Rejected. `owner/repo#N` contains
`/`. A comma split would link inside a URL such as
`https://example.com/?ids=RIG-1,RIG-2`. The cost: `RIG-1,RIG-2` stays
plain, while `RIG-1, RIG-2` links.

**Linear workspace slug.** Read `organization.urlKey` at runtime (chosen),
add an operator flag, or use a URL with no slug. See Open Question 1.

**Share the write path's Linear client.** Rejected. The reference read
builds its own `forge.NewLinear`, as `buildLinearNotifyLane` does
(`client := forge.NewLinear(forge.LinearConfig{Token: tokens, Log: log})`).
A shared instance would share the write coordinate's rate gate, so a
write-path limit could block ref config, and ref reads would spend the
write path's budget. T1's caching bounds the read to one call a minute.

**Bare `#N` default.** Link against the first repo, or a server default
repo. Rejected for now: on a multi-repo server a guess produces wrong links,
and the acceptance prefers plain text to a wrong link. See Open
Question 3.

**Tokenizer.** A regex scan with lookbehind over the raw string, plus a
pass that masks URLs. Rejected: whole-word matching gives the same
boundaries with less code and no URL pass.

**Markdown seam.** A remark (mdast) plugin, or a DOM walk after render.
Rejected: the existing pipeline is rehype (`rehypeMentionChips`), `code`,
`pre` and `a` are explicit elements after GFM, and a DOM walk fights
Solid's render.

**Markdown element.** The plugin could emit a marker such as
`span[data-ref-href]` with its own component. Rejected: it adds a second
click path. Forwarding `class` in the existing `a` override is one line.

**Plain-text seam.** Render plain text through `MarkdownText`. Rejected:
markdown would reinterpret `*`, `_` and `#` in topic names and titles.

## Plan

Order: T1 → T2. T3 and T5 have no dependency and can start at once. T4
needs T2 and T3. T6 needs T4 and T5. T7 and T8 need T6. T9 is last.

### T1 — Linear workspace read (Go)

Add `Workspace` to `forge.Linear`. One paged GraphQL query reads the org URL
key and every team key the credential can see.

Interfaces:

```go
// go/internal/forge/linear.go
const (
	workspaceTTL          = 10 * time.Minute // a good value is reused this long
	workspaceRetryAfter   = 1 * time.Minute  // no new fetch this long after a failure
	workspaceFetchTimeout = 3 * time.Second  // deadline on one fetch
)

// LinearWorkspace is what the reference config needs from Linear.
type LinearWorkspace struct {
	URLKey   string   // organization.urlKey, e.g. "rigelbuild"
	TeamKeys []string // team keys the credential can see; sorted, unique
}

func (l *Linear) Workspace(ctx context.Context) (LinearWorkspace, error)
```

```graphql
query CompassWorkspace($after: String) {
  organization { urlKey }
  teams(first: 100, after: $after) {
    nodes { key }
    pageInfo { hasNextPage endCursor }
  }
}
```

Rules. The cache fields sit on `Linear` next to the `workflowStates` cache.
Times come from `l.now()`.

- A good value younger than `workspaceTTL`: return it. No request.
- Less than `workspaceRetryAfter` since the last failed fetch: no request.
  Return the last good value, or the last error if there is none.
- Otherwise fetch under `context.WithTimeout(ctx, workspaceFetchTimeout)`.
  On success, store and return the value. On failure, record the time and
  the error. Then return the last good value with a nil error and log a
  warning, or return the error if there is no good value.
- An empty `urlKey` is a failure, because no template can be built from it.
- One mutex guards the cache and is held across the fetch, so concurrent
  callers make one request. Each waits at most the fetch deadline.

Test cycle (red first), in `go/internal/forge/linear_test.go` with
`newTestLinear`, `scriptedRoundTripper` and `fakeTokenSource`:

- Two pages of teams: `URLKey` set, `TeamKeys` merged, sorted, unique.
- A second call inside the TTL sends no request (`rt.calls` unchanged).
- With `l.now` past the TTL, the next call sends one request.
- A failed refresh after a good value returns the good value and a nil
  error.
- A failure with no good value returns an error. A second call inside
  `workspaceRetryAfter` sends no request. A call after it sends one.
- A round tripper that blocks until the request context ends: the call
  returns an error within the fetch deadline.
- An empty `urlKey` returns an error.

Live probe: `TestLiveLinearWorkspace` in
`go/internal/forge/livegithub_test.go` (build tag `livegithub`), gated by
`requireLinear`. It asserts a non-empty `URLKey` and that `TeamKeys`
holds the lane's test team key. No check against live Linear has run for
this record yet, so this probe is the first proof that the credential can
read both fields.

Run `moon run compass-go:test compass-go:lint`.

### T2 — `GetReferenceConfig` RPC (proto and Go)

Interfaces:

```proto
// proto/compass/v1/compass.proto, in service CompassService after
// GetModelRegistry.
// GetReferenceConfig returns the forges and trackers the server reads, so
// the UI can turn ref text into outbound links. Any signed-in account may
// call it.
rpc GetReferenceConfig(GetReferenceConfigRequest) returns (GetReferenceConfigResponse);

message GetReferenceConfigRequest {}

message GetReferenceConfigResponse {
  repeated ReferenceForge forges = 1;
  repeated ReferenceTracker trackers = 2;
}

// A code forge the server reads, with the repos it serves.
message ReferenceForge {
  ForgeProvider provider = 1;
  // Enabled repos as "owner/name"; lowercase, sorted, unique.
  repeated string repos = 2;
  // Issue or PR link. Placeholders: {repo}, {number}.
  // Example: "https://github.com/{repo}/issues/{number}".
  string issue_url_template = 3;
  // Commit link. Placeholders: {repo}, {sha}.
  // Example: "https://github.com/{repo}/commit/{sha}".
  string commit_url_template = 4;
}

// An issue tracker the server reads, with its team keys.
message ReferenceTracker {
  ForgeProvider provider = 1;
  // Team keys; uppercase, sorted, unique. Example: "RIG".
  repeated string team_keys = 2;
  // Issue link. Placeholder: {key}, the full identifier such as "RIG-123".
  // Example: "https://linear.app/rigelbuild/issue/{key}".
  string issue_url_template = 3;
}
```

```go
// go/server/reference_config_service.go  (//go:build unix)
type linearWorkspaceReader interface {
	Workspace(ctx context.Context) (forge.LinearWorkspace, error)
}

type referenceSource struct {
	githubHost string                // cfg.Forge.resolved().Host
	linear     linearWorkspaceReader // nil when Linear is not configured
	log        *slog.Logger
}

func newReferenceSource(githubHost string, linear linearWorkspaceReader, log *slog.Logger) *referenceSource

// githubReferenceForge builds the GitHub entry. host "" means "github.com".
func githubReferenceForge(host string, repos []string) *compassv1.ReferenceForge

// linearReferenceTracker builds the Linear entry. The URL key is
// path-escaped into "https://" + forge.LinearHost + "/<key>/issue/{key}".
func linearReferenceTracker(ws forge.LinearWorkspace) *compassv1.ReferenceTracker

// withReferences sets the reference source. Only Serve calls it.
func (s *service) withReferences(src *referenceSource) *service

func (s *service) GetReferenceConfig(
	ctx context.Context,
	req *connect.Request[compassv1.GetReferenceConfigRequest],
) (*connect.Response[compassv1.GetReferenceConfigResponse], error)
```

Wiring in `Serve` (`go/server/serve.go`): `newService(...)` runs before
`buildForgeReadWiring`, so the source is set after it, through
`svc.withReferences(...)`, on the `withRev` pattern. Pass a Linear reader
only when `wiring.linearTokens != nil`. Otherwise pass an untyped `nil`, so
the interface is a true nil. The reader is its own instance,
`forge.NewLinear(forge.LinearConfig{Token: wiring.linearTokens, Log: log})`
(see Alternatives § Share the write path's Linear client).

Handler rules:

- `s.refs == nil`: an empty response. Tests build `newService` without it.
- GitHub repos come from `s.store.ListEnabledForgeRepoSubscriptions(ctx,
  store.ForgeProviderGitHub, s.refs.githubHost)`. An error returns
  `connect.CodeInternal`. No repos means no forge entry.
- Linear: `s.refs.linear.Workspace(ctx)`. An error logs a warning and adds
  no tracker entry. No team keys means no tracker entry.

Add `compassv1connect.CompassServiceGetReferenceConfigProcedure` to the
authenticatedOpen cases in `classifyProcedure`.

Test cycle (red first):

- After `compass-proto:gen`, `TestClassifyProcedureCoversEveryGeneratedProcedure`
  (`go/internal/auth/classify_exhaustive_test.go`) goes red. Add the
  procedure to `classifyProcedure`, to the open list in
  `TestAdminGateAllowsAnyAccountOnOpenRPCs`, and as a case in
  `TestClassifyProcedureClassifiesKnownAndUnknownProcedures`. All go green.
- Unit test, default lane, `go/server/reference_config_service_test.go`:
  - `githubReferenceForge("", …)` gives
    `https://github.com/{repo}/issues/{number}` and
    `https://github.com/{repo}/commit/{sha}`;
  - a GitHub Enterprise host gives `https://<host>/…`;
  - repos come out lowercase, sorted and unique;
  - `linearReferenceTracker` gives
    `https://linear.app/rigelbuild/issue/{key}`;
  - a URL key holding `/` is path-escaped.
- pgtest, `go/server/reference_config_service_pgtest_test.go`, with a
  fixture on the model of `newConfigFixture` that takes a
  `*referenceSource`:
  - no subscriptions and no Linear: an empty response;
  - two enabled subscriptions and one disabled: a forge with the two;
  - a subscription on another host: excluded;
  - a fake reader: a tracker entry;
  - a fake reader that errors: no tracker, the forge present, no RPC error;
  - no bearer token: `Unauthenticated`.

Run `moon run compass-proto:lint compass-proto:gen compass-proto:drift
compass-go:test compass-go:lint compass-ui:typecheck`. The pgtest suite runs
in CI's pgtest job, or locally with `COMPASS_TEST_DATABASE_DSN`.

### T3 — Pure resolver (UI)

Files: `apps/ui/src/references/types.ts`, `resolve.ts`, `resolve.test.ts`,
and `safeWebHref` in `apps/ui/src/safe-url.ts`.

Interfaces:

```ts
// apps/ui/src/references/types.ts
export interface ReferenceConfig {
	readonly forges: readonly ForgeRefConfig[];
	readonly trackers: readonly TrackerRefConfig[];
}
export interface ForgeRefConfig {
	readonly repos: readonly string[]; // "owner/name", lowercase
	readonly issueUrlTemplate: string; // {repo} {number}
	readonly commitUrlTemplate: string; // {repo} {sha}
}
export interface TrackerRefConfig {
	readonly teamKeys: readonly string[]; // uppercase
	readonly issueUrlTemplate: string; // {key}
}
export interface RefIndex {
	readonly forgeByRepo: ReadonlyMap<string, ForgeRefConfig>; // lowercase repo
	readonly trackerTemplateByKey: ReadonlyMap<string, string>; // team key
	readonly soleRepo: string | undefined; // set when exactly one config repo
	readonly signature: string; // equal signatures resolve every ref the same
}
export interface RefContext {
	readonly repo?: string; // a GitHub "owner/name" only
}
export type RefRun =
	| { readonly kind: "text"; readonly text: string }
	| { readonly kind: "ref"; readonly text: string; readonly href: string };

// apps/ui/src/references/resolve.ts
export const EMPTY_REF_INDEX: RefIndex;
export function buildRefIndex(config: ReferenceConfig | undefined): RefIndex;
export function refRuns(
	text: string,
	index: RefIndex,
	context?: RefContext,
): RefRun[];
/** The commit link for a structured row, or undefined when the repo is not
 *  a config repo or the SHA fails the SHA rule. */
export function commitUrl(
	index: RefIndex,
	repo: string,
	sha: string,
): string | undefined;
/** `{ repo }` for a GitHub-provider artifact, `{}` for any other. */
export function refContextOf(artifact: {
	readonly forge: ForgeRef;
	readonly repo: string;
}): RefContext;

// apps/ui/src/safe-url.ts
/** safeHref, then http: or https: only. */
export function safeWebHref(href: string | undefined): string | null;
```

When two forges list one repo, the first wins. A filled template that fails
`safeWebHref` gives a text run.

Test cycle (red first), table-driven in `resolve.test.ts`. Every case also
checks that the runs join back to the input:

- `RIG-123` with team `RIG` links to the template; `FOO-1` and `rig-123`
  stay plain; `SHA-256` stays plain with no `SHA` team.
- `#42` links with one config repo; stays plain with two repos and no
  context; links to `context.repo` when it is a config repo; stays plain
  when it is not; `#0`, `#42abc` and `a#42` stay plain.
- `o/r#42` links for a config repo; any other repo stays plain; `O/R#42`
  links with the lowercase repo.
- SHA, with one config repo: `1e4e1fe1` (8) and a 12-character SHA link; a
  40-character SHA links. These stay plain: a 32-character id shaped like
  `newID` output, 6, 13, 39 and 64 characters, `defaced`, `1234567`,
  uppercase hex. With two repos and no context, a SHA stays plain.
- `(RIG-123),` gives `(`, a link, `),`. `RIG-123's` links `RIG-123`.
- `` `RIG-123` `` stays plain.
- A URL such as `https://github.com/o/r/issues/42#issuecomment-1` stays
  plain.
- A template that fills to a `javascript:` URL stays plain.
- `EMPTY_REF_INDEX` gives one text run.
- Two indexes from the same config in a different order have the same
  `signature`.
- `commitUrl` gives undefined for a repo that is not a config repo.
- `refContextOf` gives `{}` for a Linear-provider issue with `repo: "SEA"`
  and `{ repo: "o/r" }` for a GitHub one.
- `safeWebHref` rejects `mailto:` and `javascript:` and returns an
  `https:` URL unchanged.

Run `moon run compass-ui:typecheck compass-ui:test`.

### T4 — Store query, fixture seed and index context (UI)

Interfaces:

```ts
// apps/ui/src/references/adapt.ts
export function adaptReferenceConfig(
	res: GetReferenceConfigResponse,
): ReferenceConfig;

// apps/ui/src/references/context.ts
// Default value: () => EMPTY_REF_INDEX.
export const RefIndexContext: Context<Accessor<RefIndex>>;
export function useRefIndex(): Accessor<RefIndex>;

// apps/ui/src/store.ts
export type ReferenceConfigState =
	| { readonly status: "offline" }
	| { readonly status: "pending" }
	| { readonly status: "ready"; readonly config: ReferenceConfig }
	| { readonly status: "error"; readonly message: string };
// AppStoreOptions gains:
readonly initialReferenceConfig?: ReferenceConfig;
// AppStore gains:
referenceConfig: Accessor<ReferenceConfigState>;
refIndex: Accessor<RefIndex>;

// apps/ui/src/stub-data.ts
export const STUB_REFERENCE_CONFIG: ReferenceConfig;
```

Store rules:

- With a transport: a `createConnectQuery` on `getReferenceConfig`, built
  the same way as `modelRegistryQuery`. Cached data wins over a failed
  refetch. The seed is ignored.
- No transport and a seed: `{ status: "ready", config: seed }`.
- No transport and no seed: `{ status: "offline" }`.
- `refIndex` is `createMemo(() => buildRefIndex(config), { equals: (a, b)
  => a.signature === b.signature })`, where `config` is set only when the
  state is `ready`.

`STUB_REFERENCE_CONFIG` names the fixture repo `rigelbuild/compass` with
GitHub templates, and the fixture team keys `RIG` and `SEA` with the
Linear template the fixture tracker URLs already use
(`https://linear.app/rigelbuild/issue/{key}`). `bootFixture` passes it as
`initialReferenceConfig`.

Provider: `mountShell` (`apps/ui/src/mount.tsx`) wraps the app in
`<RefIndexContext value={store.refIndex}>` inside its `StoreContext`.
`mountShell` is the one mount both `index.tsx` and `bootFixture` call, so
live and fixture boots share it. The test mount `mountApp`
(`apps/ui/src/test-router.tsx`) does the same.

Test cycle (red first), `apps/ui/src/store.reference-config.test.ts`, with
`createRouterTransport` as in `store.model-registry.test.ts`:

- The transport answers: the state is `ready`, and `refIndex()` links
  `RIG-1` through the template.
- No transport, no seed: the state is `offline`, and `RIG-1` stays plain.
- No transport, a seed: the state is `ready` with the seed.
- The RPC errors: the state is `error`, and `RIG-1` stays plain.
- A refetch with the same data keeps the same `refIndex()` object.

Run `moon run compass-ui:typecheck compass-ui:test`.

### T5 — openExternal move and guard (UI)

This task is shared with the compass-settings record (its S5). Skip it if
that record has already landed the file; check that the guard is there.

Interfaces:

```ts
// apps/ui/src/open-external.ts (moved from MarkdownText.tsx)
/** Opens url in the OS browser (Wails shell) or a new tab (plain browser).
 *  Opens nothing unless safeHref(url) accepts it. */
export function openExternal(url: string): void;
```

`MarkdownText.tsx` imports it and no longer imports `@wailsio/runtime`.

Test cycle (red first), `apps/ui/src/open-external.test.ts`:

- `javascript:alert(1)` and a `file:` URL open nothing: no
  `Browser.OpenURL` call and no `window.open` call.
- In a plain browser, an `https:` URL calls `window.open(url, "_blank",
  "noreferrer,noopener")` once.
- In the shell (`window._wails` set), an `https:` URL calls
  `Browser.OpenURL(url)` once.
- `afterEach` restores `mock.module("@wailsio/runtime", () =>
  realRuntime)`, `window._wails` and `window.open`. `mock.module` leaks
  across files in bun, as the `MarkdownText.test.tsx` comment says.

The existing `MarkdownText.test.tsx` link tests stay green. Run
`moon run compass-ui:typecheck compass-ui:test`.

### T6 — `RefAnchor` and `RefText` (UI)

Interfaces:

```ts
// apps/ui/src/components/RefText.tsx
/** One outbound ref link. Renders children as plain text when
 *  safeWebHref(href) rejects the URL. */
export function RefAnchor(props: {
	href: string;
	children: JSX.Element;
}): JSX.Element;
/** Plain text with refs linked through RefIndexContext. */
export function RefText(props: {
	text: string;
	context?: RefContext;
}): JSX.Element;
```

`RefAnchor` renders `<a class="ref-link" href target="_blank"
rel="noreferrer noopener">`. Its click calls `preventDefault()`,
`stopPropagation()` and `openExternal(href)`. `.ref-link` takes the same
token-based color and underline as markdown links. If the markdown link rule
is scoped to the markdown container, the shared rule moves to a selector
both match.

Test cycle (red first), `apps/ui/src/components/RefText.test.tsx`. Stub
`window.open` and restore it in `afterEach`:

- A ref renders `a.ref-link` with the right `href`, `target` and `rel`.
- A click calls `window.open` once with the `href`, prevents the default,
  and does not reach a parent `onClick`.
- `RefAnchor` with a `javascript:` href renders its children and no anchor.
- No provider: text only, no anchor.

Run `moon run compass-ui:typecheck compass-ui:test compass-ui:stylelint`.

### T7 — Markdown plugin (UI)

Interfaces:

```ts
// apps/ui/src/markdown/rehype-ref-links.ts
import type { Parent as HastParent } from "hast";
export function rehypeRefLinks(options: {
	readonly index: RefIndex;
	readonly context?: RefContext;
}): () => (tree: HastParent) => void;
```

`MarkdownText` changes:

- New prop `refContext?: RefContext`.
- It reads `useRefIndex()` and adds
  `rehypeRefLinks({ index: refIndex(), context: props.refContext })` after
  `rehypeMentionChips` in the `rehypePlugins` memo. A late `ready`
  re-renders with links.
- The `a` override passes `class={stringAttr(p.class)}`. Its `href`,
  `target`, `rel` and click logic do not change.

Test cycle (red first):

- `apps/ui/src/markdown/rehype-ref-links.test.ts`, on the `run(children)`
  pattern of `rehype-prose-breaks.test.ts`: a text `RIG-1` becomes an `a`
  with `className: ["ref-link"]`; text under `code`, `pre`, `a` and a
  mention chip is unchanged.
- In `MarkdownText.test.tsx` with a `RefIndexContext` provider:
  - a prose `RIG-123` renders one `a.ref-link` with the template URL,
    `target="_blank"` and `rel="noreferrer noopener"` (this proves the
    override forwards `class`);
  - `` `RIG-123` `` inline and a fenced block holding `RIG-123` render no
    ref anchor;
  - a GFM autolink whose URL holds `#42` renders one anchor and no nested
    anchor;
  - a mention chip stays a chip;
  - with no provider, the markup equals today's output;
  - a click on a ref anchor reaches `Browser.OpenURL` in the shell mock,
    with the file's existing `afterEach` restore.

Run `moon run compass-ui:typecheck compass-ui:test`.

### T8 — Plain-text and structured surfaces (UI)

Interfaces: none new. Exact sites:

| File | Element | Change |
| --- | --- | --- |
| `SessionTrace.tsx` | `.block-text`, `.block-thinking` | `<RefText text={item.text} />` |
| `SessionTrace.tsx` | `.plan-content` | `<RefText text={entry.content} />` |
| `SessionTrace.tsx` | `.notice-text` | `<RefText text={notice()?.text ?? ""} />` |
| `ChannelView.tsx` (`ChannelHeader`) | `.conv-topic` | `<RefText text={props.channel.topic} />` |
| `TopicView.tsx` | `.conv-name` | `<RefText text={t().name} />` |
| `RightSidebar.tsx` (`IssueDetailHead`) | `.r-detail-issue` | `RefAnchor` with `issue.tracker?.url ?? issue.url` around `issueKey(...)` |
| `RightSidebar.tsx` (`IssueDetailHead`) | `.r-detail-title` | `RefText` with `refContextOf(props.issue)`; the `title` attribute stays |
| `RightSidebar.tsx` (`VcsPane`) | Issue row `.v` | `RefAnchor`, as `.r-detail-issue` |
| `RightSidebar.tsx` (`VcsPane`) | Summary row `.v` | `RefText` with `refContextOf(props.issue)` |
| `RightSidebar.tsx` (`VcsPane`) | `.commit-sha` | `RefAnchor` with `commitUrl(index, repo, c.sha)` when defined, else plain |
| `RightSidebar.tsx` (`PrPane`) | `.pr-num` | `RefAnchor` with `props.pr.url` |
| `RightSidebar.tsx` (`PrPane`) | `.pr-title` | `RefText` with `refContextOf(props.pr)` |

For `.commit-sha`, `repo` is `refContextOf(props.issue).repo`. With no repo,
the row stays plain.

Test cycle (red first):

- `SessionTrace.test.tsx`: a text, a thinking, a plan and a notice item
  each holding `see RIG-123` render one `.ref-link` under a provider.
- Sidebar tests:
  - a Linear-provider issue (`repo: "SEA"`) with `#42` in its summary stays
    plain with two config repos;
  - a GitHub issue in `o/r` links `#42` in its summary to `o/r`;
  - `.r-detail-issue` links to `tracker.url` when the issue has a tracker,
    else to `issue.url`, in multi-forge mode too;
  - `.pr-num` links to `pr.url`;
  - a `.commit-sha` row links to the commit template.
- Invariant test: the fixture shell mounted at the board with an issue
  selected, and at an agent route. Ref text is present in card titles.
  `document.querySelector("button a, a a, [role=button] a, [role=link] a")`
  is `null`.

Run `moon run compass-ui:typecheck compass-ui:test`.

### T9 — Fixture and visual baselines (UI)

Add one line to a fixture message in `apps/ui/src/comms-stub.ts` that a
committed shot already shows. The line names `RIG-1022` and the same ref
inside a code span. The shot then shows one link and one unlinked code span.

Run `visual-gate` under `direnv exec` and re-capture every shot that
changes, and only those (DL-399). Other fixture content also changes shots
once the seed resolves it: `#864` in the `acc-compass-native` trace notice,
and the sidebar's issue key, PR number and commit rows. The PR body names
every changed shot. CI's `visual-gate` must re-render every committed shot
green.

Run `moon run compass-ui:ci`.

## Global Constraints

- **Public repo.** No private repo names, paths or PRs. Tracker IDs stay
  bare text.
- **No hard-coded prefix in UI app code.** No host, team key, repo or URL
  path under `apps/ui/src` outside fixtures and tests. Every URL comes from
  a server template or an artifact's own URL.
- **Linear token scope.** `tokenScope` in
  `go/internal/linearagent/client.go` does not change. A changed scope
  makes Linear revoke existing tokens.
- **Linear never blocks the forge half for long.** One Linear fetch has a
  3-second deadline. A failure serves the last good value.
- **Code is never rewritten.** No anchor under `code`, `pre` or `a`, and
  no match inside a backtick-wrapped word.
- **Unresolved means plain.** No anchor without a URL that `safeWebHref`
  accepts.
- **GitHub-only repo context.** `RefContext.repo` is set only for a
  GitHub-provider artifact, through `refContextOf`.
- **No nested interactive elements.** No `a` inside a `button`, an `a`, a
  `[role=button]` or a `[role=link]` (DL-097 §2).
- **Anchor shape.** `class="ref-link"`, `target="_blank"`,
  `rel="noreferrer noopener"`. A click goes through `openExternal`.
- **The opener vets URLs.** `openExternal` calls `safeHref` itself and
  opens nothing it rejects.
- **Wails import zone.** `@wailsio/runtime` is imported only in
  `apps/ui/src/open-external.ts` and `daemon-transport.ts`.
- **Styles.** Token-based only: no raw hex, no literal durations or easings
  (`compass-ui:stylelint`).
- **Gates per task.** UI: `moon run compass-ui:typecheck compass-ui:test`,
  biome clean (`moon run root:lint`). Go: `moon run compass-go:test
  compass-go:lint`; the new service file carries `//go:build unix`. Proto:
  `compass-proto:lint`, then `compass-proto:gen` with generated output
  committed, so `compass-proto:drift` passes.
- **Red first.** Each task's tests are written and seen failing before the
  code.
- **Visual baselines.** Captured under `direnv exec` (DL-399). The PR body
  names each changed shot.

## Tasks

- [ ] T1 — `forge.Linear.Workspace` with TTL, last-good value, failure
  pause, fetch deadline, tests and a live probe.
- [ ] T2 — `GetReferenceConfig` proto, gen, handler, `withReferences`
  wiring, `classifyProcedure` case, unit and pgtest tests.
- [ ] T3 — Pure resolver in `apps/ui/src/references/` and `safeWebHref`,
  with table tests.
- [ ] T4 — `referenceConfig` store query, `initialReferenceConfig` seed,
  `refIndex` memo, `RefIndexContext` in `mountShell` and `mountApp`, store
  tests.
- [ ] T5 — Move `openExternal` to `apps/ui/src/open-external.ts` with a
  `safeHref` guard (shared with compass-settings S5).
- [ ] T6 — `RefAnchor`, `RefText` and the `.ref-link` style, with tests.
- [ ] T7 — `rehypeRefLinks` in `MarkdownText`; the `a` override forwards
  `class`.
- [ ] T8 — The listed trace, header and sidebar sites; the no-nested-link
  invariant test.
- [ ] T9 — Fixture ref line; re-capture changed visual shots.

## Open Questions

1. **Linear workspace slug source.** Linear issue URLs carry the workspace
   slug (`https://linear.app/<slug>/issue/RIG-123`). The server has no
   slug today.
   - (a) Read `organization.urlKey` at runtime with the existing
     credential. No new config, no new scope.
   - (b) An operator flag (for example `--linear-workspace`). Explicit, but
     one more value to set and keep correct.
   - (c) Use `https://linear.app/issue/RIG-123` with no slug. Both forms
     return an app shell with HTTP 200, so whether the slug-less form opens
     the issue is not proven.

   **Recommendation:** (a). Decision needed: approve (a), or name (b) or
   (c).
2. **Which Linear team keys link.** Every armed key also links matching
   prose, so a team key such as `QA` or `API` links `QA-1` anywhere.
   - (a) Every team the server's Linear credential can see. A key from a
     team the reader cannot open shows Linear's access page, not a broken
     link.
   - (b) Only teams named in the server's forge scope grants or
     subscriptions. Narrower, but a ref to any other team stays plain.
   - (c) An operator list flag.

   **Recommendation:** (a). Decision needed: approve (a), or name (b) or
   (c).
3. **Bare `#N` and SHA on a multi-repo server.** Messages, traces and
   channel headers have no repo of their own.
   - (a) Plain text unless the surface has a repo or the server reads
     exactly one repo.
   - (b) Add a `default_repo` field to `GetReferenceConfigResponse`, set by
     the operator.
   - (c) Bind a channel to a repo. No channel-to-repo model exists today.
   - (d) For a session trace and an agent's home DM, use the agent's repo
     when it has exactly one. The store derives one in `agentRepos`
     (`apps/ui/src/store.ts`), but today it returns a fixed fixture name
     (`name: "RigelBuild/compass"`), so a real per-agent repo must land
     first.

   **Recommendation:** (a) now; (d) once `agentRepos` derives real repos.
   Decision needed: approve (a), or ask for (b), (c) or (d) now.
4. **Short SHAs on very large repos.** GitHub returned 404 for 7- and
   12-character prefixes on `torvalds/linux`. The grammar links 7–12
   characters or exactly 40.
   - (a) Keep that band and accept the rare 404.
   - (b) Link only full 40-character SHAs. Most SHAs agents write are
     short, so most would stay plain.
   - (c) Expand short SHAs on the server through the forge API. Costs API
     budget per SHA.

   **Recommendation:** (a). Decision needed: approve (a), or name (b) or
   (c).
5. **Board card refs.** Card text cannot hold links (Approach § Board
   surfaces).
   - (a) No links on cards. Refs link in the right sidebar.
   - (b) Rebuild cards so the title link sits outside the card button. This
     changes the card structure DL-097 §2 set.
   - (c) Add a small outbound icon button next to each card.
   - (d) An outbound `role="link"` chip inside the card, the compromise
     `PrCard` already uses for its in-app `card-issue-link` (DL-097 §2),
     with its keyboard path as a board command (DL-221).

   **Recommendation:** (a). Decision needed: approve (a), or name (b), (c)
   or (d).
6. **What "unresolved" means in the acceptance.** "An unresolved ref stays
   plain text, never a broken link."
   - (a) Unresolved means no configured prefix, repo or template. A
     resolved ref can still reach a not-found page: `#1` in prose on a
     single-repo server, `RIG-999999`, or a short SHA on a very large repo.
   - (b) Unresolved means the artifact does not exist. This needs a
     server check per ref, such as a `ResolveRefs` RPC or a list of known
     numbers, at a cost in round trips or index size.

   **Recommendation:** (a); the Problem section states it. Decision needed:
   confirm (a), or ask for (b).
7. **Tenancy of the tracker half.** The forge half is tenant-scoped. The
   Linear half comes from the one deployment-wide token source, so every
   account sees the same workspace key and team keys. The core must not
   assume it is single-tenant (`docs/concepts/self-host-and-managed.md`).
   - (a) Accept deployment scope and document it in the RPC comment. Board
     issues are already readable by any signed-in account
     (`ListBoardIssues` is authenticatedOpen).
   - (b) Return trackers only to the bootstrap tenant.
   - (c) Per-tenant Linear credentials. Out of scope for this record.

   **Recommendation:** (a). Decision needed: approve (a), or name (b) or
   (c).
