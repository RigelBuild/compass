# Compass server ownership layer — amendment: one canonical type family

Status: Active
Tracker: SEA

> **Extends #995 (frozen).** This record is a sibling amendment to
> `docs/designs/product/compass-server-ownership-layer/design.md` (merged in
> #995). The merged record is frozen; per sealed convention a later change ADDS
> a record. This amendment reconciles #995's forge-shaped proto type family to
> the single canonical `compass.v1` family frozen by
> `docs/designs/product/compass-issue-model/design.md` (#1018).

## Problem / Intent

The #995 Decision 5 proto block adds a **forge-shaped family to the wire**:
`message Issue` (#995 design.md:361-372, fields `repo, number, title, body, state, url,
ForgeAuthor author, forge_author_login, labels, updated_at_unix_ms`),
`ForgeAuthor` (:385-390, itself flagged "the pre-OQ-1 draft, not a frozen
contract"), `IssueComment` (:392-401), `PullRequest` (:403-417, carrying
`ForgeChecksSummary checks`), and `ForgeChecksSummary`/`ForgeCheck`
(:421-431) — all slated for `agent_gateway.proto` by task T1 (:1625-1635).

Matt's type-architecture ruling (2026-07-31, ratified as DL-069,
DECISIONS.md:127) supersedes that family: Compass owns a **single** canonical
`compass.v1` `Issue`/`PullRequest` type pair; "the server translates raw forge
data into these types at ingestion, the raw forge shape is never a proto/wire
type" (DL-069). The frozen #1018 record names this amendment as the
reconciliation site:

> "(PR #995's earlier sketch of forge-proxy proto messages under these names
> in `agent_gateway.proto` — #995 design.md:361-372, :403-417, :1625 — is
> superseded by this model and reconciled in a sibling amendment to that
> record; this record does not depend on any forge proto message existing.)"
> — #1018 design.md:105-108

This amendment is that sibling. It removes the second, forge-shaped wire
family from #995's plan and points every surface that would have carried it at
the canonical `compass.v1` types. Nothing here is a retrofit of shipped wire
surface: the #995 T1 proto is **not yet built** — `grep` of the live proto
tree (`proto/compass/v1/`, compass repo) finds no `ForgeCall`, `ForgeAuthor`,
or `RelayForgeCall` symbol, and no `go/internal/forge` package exists. The
amendment edits the frozen plan's remaining work, not deployed protocol.

## Approach

### What this amendment asserts

The forge domain messages (`Issue` / `IssueComment` / `PullRequest` /
`ForgeChecksSummary` / `ForgeCheck`) and `ForgeAuthor` **do not go on the
wire**. Three layers, one type family:

1. **Ingestion boundary — internal Go value types, unchanged.** The forge
   Provider layer stays exactly as #995 T2a specifies it: "New package
   `go/internal/forge`: the `Provider` and `Service` interfaces verbatim from
   Decision 3, the value types (`Author`, `Issue`, `Comment`, `PullRequest`,
   `Checks`, `CreateIssue`, `CreatePR`, `IssueFilter`)" (#995
   design.md:1659-1661). These are server-internal Go structs at the
   ingestion boundary — precisely the "raw forge payload … server-internal Go
   data that exists only inside the ingestion translation" #1018 describes
   (design.md:42-46). T2a survives verbatim; only what its Service layer
   *emits across the wire* changes.

2. **The wire — the canonical `compass.v1` family only.** What crosses any
   proto boundary is #1018's frozen family, defined in `compass.proto`
   (package `compass.v1`), not `agent_gateway.proto`:
   - `Issue` (#1018 design.md:169-197) — "The board unit: a Compass Issue —
     the forge issue's fields PLUS the Compass agent attribution PLUS the
     Compass machinery, translated from raw forge data at server ingestion"
     (:165-168).
   - `PullRequest` (:202-218), carrying `ChecksSummary checks = 13` and the
     full review state.
   - `ChecksSummary`/`Check` (:222-232) — replaces #995's
     `ForgeChecksSummary`/`ForgeCheck` (:421-431); field-for-field the same
     shape (`head_sha`, `state`, `checks` / `name`, `state`, `url`,
     `required`), now Compass-owned.
   - `AgentAttribution` (:137-144: `agent_handle`, `owner_handle`, `bool
     verified`) — replaces #995's `ForgeAuthor` (:385-390). This also
     RESOLVES #995's OQ-1 field-set gate on that message: `agent_account_id`
     and `session_id` are deleted (both flagged for likely deletion at #995
     design.md:387-389), and the `verified` bool #995 OQ-1 contemplated is in,
     "set by the server's forge-login cross-check at ingestion (#995 OQ-1)"
     (#1018 design.md:140-143).

3. **Delivery to the UI — the projection + `SubscribeEvents`.** The canonical
   `Issue` "rides `SubscribeEventsResponse` as a new oneof variant"
   (`Issue issue = 16`, #1018 design.md:284-298), computed by the server
   projection composing the existing `go/internal/board` recorded-state
   pattern (:349-358, :371-384). Lifecycle mutation is the sole
   `UpdateIssueState` RPC (:487-496). None of that is this amendment's
   surface — it is #1018's, cited here as the replacement read path the UI
   uses.

### The #995 T1 add-list, item by item

The #995 T1 brief (design.md:1625-1635) is the authoritative enumeration of
everything #995 adds to the proto tree. Disposition of each item under this
amendment:

| # | T1 item | Disposition | Why |
| --- | --- | --- | --- |
| 1 | `Forge` RPC on `AgentGateway` (#995 :303-306) | **SURVIVES** | The agent-facing tool carrier. Its envelope types are unchanged; only result payload types reconcile. The read ops survive answered from the projection/store (OQ-A ruled option 3, §Resolved decisions). |
| 2 | `ForgeCallRequest` (#995 :310-323) | **SURVIVES** | Carries `call_id` + a oneof of the op requests below — no forge domain type in it. |
| 3 | `ForgeCallResult` (#995 :325-337) | **RECONCILED** | The envelope survives; its oneof result arms retype: `Issue issue` → `compass.v1.Issue`, `ListIssuesResponse issues` (internally retyped, item 11), `PullRequest pull_request` → `compass.v1.PullRequest`, and the two `IssueComment` arms (`issue_comment`, `pr_comment`) retype to the ruled `CommentRef` (§Resolved decisions OQ-B). |
| 4 | `ForgeCallError` (#995 :343-347) | **SURVIVES** | Scalars only (`code`, `message`, `retry_after_ms`). |
| 5 | `message Issue` (#995 :361-372) | **REMOVED** | Replaced by `compass.v1.Issue` (#1018 :169-197). Every #995 field has a canonical home: `repo`/`number`/`title`/`body`/`url`/`labels` map directly; `state` → `forge_state` (:181); `author` → `agent` (`AgentAttribution`, :183-184); `forge_author_login` → `forge_account` (:185-186); `updated_at_unix_ms` is dropped (no canonical counterpart — the projection's stream ordering supersedes a per-artifact timestamp; flag at fold if a consumer needs it). |
| 6 | `message IssueComment` (#995 :392-401) | **REMOVED** | Canonical `Issue` deliberately carries no comment list, and #1018's `Comment` (:277-281) exists only inside a PR review thread — there is no standalone canonical issue-comment message. The two carriers that embedded `IssueComment` take the ruled `CommentRef` (§Resolved decisions OQ-B). |
| 7 | `message PullRequest` (#995 :403-417) | **REMOVED** | Replaced by `compass.v1.PullRequest` (#1018 :202-218); same field mapping as item 5, plus `checks` → `ChecksSummary` and the added `ChangedStats`/`Review`/`ReviewThread` surface the canonical type carries. |
| 8 | `message ForgeAuthor` (#995 :385-390) | **REMOVED** | Replaced by `compass.v1.AgentAttribution` (#1018 :137-144). Resolves #995 OQ-1's field-set gate as described above. |
| 9 | `ForgeChecksSummary`/`ForgeCheck` (#995 :421-431) | **REMOVED** | Replaced by `compass.v1.ChecksSummary`/`Check` (#1018 :222-232), field-for-field identical. |
| 10 | The seven op requests (#995 :433-473): `CreateIssueRequest`, `CommentOnIssueRequest`, `GetIssueRequest`, `ListIssuesRequest`, `CreatePullRequestRequest`, `CommentOnPullRequestRequest`, `GetPullRequestRequest` | **SURVIVE** | Confirmed per-request this run: every field in all seven is a scalar (`string`/`uint64`/`uint32`/`bool`/`repeated string`) — no forge domain type appears in any request shape (#995 :433-473). Read-op requests (`GetIssueRequest`, `ListIssuesRequest`, `GetPullRequestRequest`) survive (OQ-A ruled option 3 — answered from the projection/store, §Resolved decisions). One width note: #995 keeps `uint64` issue/pull numbers in these requests (:444-446, :470-472) while the canonical result types carry `uint32 number` (#1018 :178); narrow the surviving request fields to `uint32` to match the canonical width — free now, buf-breaking after the surface ships. |
| 11 | `ListIssuesResponse` (#995 :454-456) | **RECONCILED** | `repeated Issue issues = 1` retypes to `repeated compass.v1.Issue`; the read op is answered from the projection/store (OQ-A ruled option 3). |
| 12 | `SubscribeForgeRequest`/`Response`, `UnsubscribeForgeRequest`/`Response`, `ForgeArtifactKind` (#995 :1027-1043) | **SURVIVE** | All scalar/enum fields (`repo`, `kind`, `number`, `subscription_id`); no domain type. |
| 13 | `ForgeNotification`/`ForgeNotificationKind` (#995 :1191-1212) | **RECONCILED + RELOCATED (DL-161)** | The message and kind enum survive but move to the new leaf `forge.proto` (with `ForgeArtifactKind` + `CommentRef`) to break the notification-placement cycle. Three arms reconcile: `ForgeChecksSummary checks = 9` → `compass.v1.ChecksSummary`; `IssueComment comment = 8` → the ruled `CommentRef` (§Resolved decisions OQ-B); and `string provider = 2` (#995 :1193) is retyped to `compass.v1.ForgeRef forge = 2` — the shipped decision (forge.proto `ForgeRef forge = 2`), so the notification path carries the one `compass.v1` forge-identity vocabulary rather than a second bare-string one. |
| 14 | `runner.proto`: `RelayForgeCall` + `RelayForgeCallRequest`/`Response` (#995 :485-493) | **SURVIVES** | Pure wrapper: `ForgeCallRequest call` / `ForgeCallResult result` by reference; reconciliation is inherited from items 2-3. |
| 15 | `SessionsResponse.forge_notification = 7` (#995 :1633) | **SURVIVES** | Carries `ForgeNotification`, reconciled internally (item 13). |
| 16 | `agent.proto`: `AgentControl.forge_notification = 9` (#995 :1633-1635) | **SURVIVES** | Same carrier logic as item 15. |
| 17 | `gen-fence` grep extension (#995 :1636-1639) | **RECONCILED** | The extension is still required for the surviving internal family (`ForgeCall\|RelayForgeCall\|ForgeNotification\|ForgeArtifactKind`), but it MUST NOT fence the canonical types: `Issue`/`PullRequest`/`ChecksSummary`/`Check`/`AgentAttribution` are PUBLIC `compass.proto` symbols that legitimately generate into the public trees. See Global Constraints for the cross-file import this creates. |

Net: the **carrier surface survives** (RPCs, envelopes, requests,
subscriptions, notification kinds, relay leg) and the **domain payload family
is deleted** — six messages removed, four carrier arms retyped to `compass.v1`.

**Multi-forge request addressing (a reconciliation consequence).** The
surviving op requests and `SubscribeForgeRequest` address artifacts by
`(repo, number)` with no provider/host (#995 :433-473, :1027-1043), while the
canonical result types now carry `ForgeRef` (#1018 :146-160, DL-091:
repo-only coordinates collide under multiple connected forges). #995 even
names subscriptions "(provider, repo, kind, number)" (:944-945) though its
wire request carries no provider. Ruled (Matt, 2026-07-31, §Resolved
decisions): request addressing stays single-forge for a GitHub-first v1 (repo
unambiguous); adding an optional `ForgeRef`/provider to the op requests and
`SubscribeForgeRequest` is a named additive follow-up, free to add later
without a buf-breaking change.

### Two structural consequences: a cross-file import and a new leaf file

**(1) The carrier imports the canonical family.** The #995 record placed the
domain messages in `agent_gateway.proto` (internal-only). The canonical family
lives in `compass.proto` — public, package `compass.v1` (#1018
design.md:99-101). Retyping the result arms therefore makes
`agent_gateway.proto` import `compass/v1/compass.proto`. This edge is already
present transitively (`agent_gateway.proto` pulls `agent.proto` which imports
`compass.proto`, `agent.proto:25`); making it direct changes no gen wiring the
lanes don't already handle. The two internal gen lanes treat the canonical
types differently — the TS lane regenerates them via `--include-imports`, the Go
lane M-redirects them to the public `go/gen` package — and the gen-fence must
keep the canonical symbols unfenced.

**(2) A new internal leaf `forge.proto` (DL-161).** The types
`AgentControl.forge_notification` reaches (`ForgeNotification`/
`ForgeNotificationKind`, `ForgeArtifactKind`, `CommentRef`) do NOT live in
`agent_gateway.proto` as #995 T1 first directed — that placement is a circular
import, since `agent_gateway.proto` already imports `agent.proto` and
`agent.proto` would then have to import back for the notification type. They
move into a new internal-only leaf `proto/compass/v1/forge.proto` that imports
only `compass.proto`; `agent.proto`, `agent_gateway.proto`, and `runner.proto`
all import the leaf without a cycle. This adds one internal file to all three
gen lanes (buf.gen.yaml exclude + both internal-lane inputs) and to the
gen-fence symbol set. Wire contract, field numbers, and generated names are
unchanged — only the source file each type lives in moves. Detail in Global
Constraints below.

## Alternatives considered

- **Keep #995's forge-proxy family as a "translation-source" proto** — i.e.
  ship both families and define the ingestion translation as a proto→proto
  mapping (`agent_gateway.Issue` → `compass.v1.Issue`). Rejected: DL-069 rules
  "the raw forge shape is never a proto/wire type" (DECISIONS.md:127), and
  #1018 is explicit that the raw forge payload "is server-internal Go data
  that exists only inside the ingestion translation; it is never exposed, not
  to the UI and not as a separate `compass.v1` message" (#1018
  design.md:42-46). A second wire family is exactly the wart being removed —
  and it would force permanent double-maintenance of every field.

- **Delete the `Forge` RPC family entirely and route agent forge access
  through some other surface.** Rejected: the carrier is ratified
  independently of the payload types — DL-049 ("Forge tools ride the existing
  `AgentGateway` socket as a sibling `ForgeCall*` family relayed by
  `RelayForgeCall`", DECISIONS.md:100) governs *how agent forge calls travel*,
  and nothing in DL-069 touches it. The write path (create/comment) has no
  substitute surface at all; only the read ops had a candidate alternative
  (OQ-A, now ruled — read ops survive on the gateway, answered from the store).

- **Rename rather than remove — keep the forge-shaped `Issue`/`PullRequest`
  messages under new names (`ForgeIssue`, `ForgePullRequest`) as internal-only
  wire types.** Rejected: DL-069's wart is a **parallel family for the same
  board artifact** — two messages (`ForgeIssue` and `compass.v1.Issue`) for one
  issue, forcing permanent double-maintenance of every field. Renaming keeps
  that duplication, and it would re-open #995 OQ-1 that #1018's
  `AgentAttribution` already closes (#1018 design.md:137-144). (This is NOT an
  objection to a message carrying forge fields as such — the canonical types
  themselves are forge fields plus Compass attribution plus machinery; the
  ruled `CommentRef` is the same pattern and duplicates no canonical type, see
  §Resolved decisions OQ-B.)

## Global Constraints

Every task below inherits these; they restate the frozen gates that bind the
amended proto work, confirmed against the live tree this run.

- **No AI tool, agent-product, or persona names** anywhere in the record, the
  proto comments, or code comments. Describe behavior directly.
- **Cite ledger ids in proto comments.** The reconciled result arms and the
  ingestion translation carry `DL-069` (and this amendment's row) in their
  comments, mirroring how #1018's proto sketch cites DL-033/DL-091 (#1018
  design.md:114-117).
- **buf breaking stays clean by construction.** None of the #995 T1 surface
  exists on `main` yet — the live proto tree has no `ForgeCall`, `ForgeAuthor`,
  or `RelayForgeCall` symbol (grep of `proto/compass/v1/`, this run) — so the
  amended add-list is still a purely additive change and passes the `breaking`
  task (`buf breaking … --against origin/main`, #995 design.md:1490-1491) with
  no new exemption. Nothing is ever removed from a shipped wire surface.
- **Files touched** (amending #995 T1's list, design.md:1625-1639):
  the NEW leaf `proto/compass/v1/forge.proto` (`ForgeNotification`/
  `ForgeNotificationKind`, `ForgeArtifactKind`, `CommentRef`; imports only
  `compass.proto` — DL-161), `proto/compass/v1/agent_gateway.proto` (carrier
  family + reconciled arms; imports the leaf), `proto/compass/v1/runner.proto`
  (`RelayForgeCall`, `SessionsResponse.forge_notification = 7`; imports the
  leaf), `proto/compass/v1/agent.proto` (`AgentControl.forge_notification = 9`;
  imports the leaf), the `gen-fence` grep in `proto/moon.yml`, and the three gen
  templates the new leaf is wired into — `buf.gen.yaml` (added to
  `exclude_paths`), `buf.gen.internal-go.yaml` (added to `paths` + `M`-maps),
  and `buf.gen.agent-ts.yaml` (added to `paths`). Note a line-drift against
  #995's cite: the gen-fence grep script sits at `proto/moon.yml:151` (task
  `gen-fence` opens at :121), not :141 as #995 recorded — confirmed by reading
  the live file this run.
- **The gen-fence extension fences the carrier family only.** Extend the grep
  with the unanchored `ForgeCall|RelayForgeCall|ForgeNotification|
  ForgeArtifactKind` family exactly as #995 T1 specifies (design.md:1636-1639)
  — but the canonical `Issue`/`PullRequest`/`ChecksSummary`/`Check`/
  `AgentAttribution` symbols are PUBLIC `compass.proto` types that must
  generate into the public trees and MUST NOT be added to the fence. Those
  four patterns are safe not because of word-bounding (word-bounding guards
  substring collisions like `SessionFrame` vs `AgentSessionFrame`,
  `proto/moon.yml:138-150`) but because none of them prefixes a public symbol
  — `ForgeRef`/`ForgeProvider` share only the `Forge` stem, no full-pattern
  match. The surviving internal-only requests are NOT matched by any pattern
  (`CreateIssueRequest`, `CommentOnIssueRequest`, the `Get*`/`List*` requests,
  `ListIssuesResponse`, `SubscribeForge*`/`UnsubscribeForge*`, and
  `CommentRef`); this gap existed in #995 too — extend the fence with these
  word-boundable names (none collides with a public symbol) or accept the gap
  explicitly.
- **Cross-file import — already satisfiable, handled per-lane.** Retyping the
  result arms makes `agent_gateway.proto` import `compass/v1/compass.proto`.
  This is not a new dependency: `agent_gateway.proto` already transitively
  pulls `compass.proto` via its `agent.proto` import (`agent.proto:25`), and
  both internal gen lanes already handle it. Per lane: the TS lane
  (`buf.gen.agent-ts.yaml` → `packages/compass-agent/src/gen`) runs
  `--include-imports`, so the canonical types already regenerate into the
  agent gen tree (`compass_pb.ts` is present today); the Go lane
  (`buf.gen.internal-go.yaml` → `go/internal/gen`; #995 design.md:1506-1508)
  deliberately M-maps `compass.proto` to the PUBLIC `go/gen` package and MUST
  NOT gain `--include-imports` — its header warns that doing so triggers a
  duplicate-registration init panic. A1 confirms this and that
  `buf.gen.yaml`'s `exclude_paths` still excludes every internal file. The
  placement correction (DL-161) adds one more internal file to keep in that
  fence: the new `proto/compass/v1/forge.proto` leaf is added to
  `buf.gen.yaml`'s `exclude_paths`, and to the `paths` inputs of both internal
  lanes (`buf.gen.internal-go.yaml` with its `M`-maps → `compassv1internal`,
  and `buf.gen.agent-ts.yaml`). forge.proto imports only `compass.proto`, so it
  introduces no new cross-package edge beyond the one described here.
- **Enum sentinel rule.** Any surviving new enum (`ForgeArtifactKind`,
  `ForgeNotificationKind`) keeps its `_UNSPECIFIED = 0` sentinel: the
  `ENUM_ZERO_VALUE_SUFFIX` exemption covers `comms.proto` only (#995
  design.md:1516-1520).
- **Design-PR gates.** This amendment ships as a design PR: it MUST carry its
  DECISIONS.md delta in the same PR (`tools/design-ledger-gate`), a
  `Spec-impact:` line in the PR body or a `docs/specs/` touch
  (`tools/spec-impact-gate`), and pass root `markdownlint`. The spawning
  design-owner applies the ledger rows proposed in §Ledger delta.

## Plan

This amendment edits #995's not-yet-executed task briefs, not shipped code.
Each task below names the #995 task it amends and carries the exact symbol
deltas. Execution order: A1 → A2 → A3 (A2/A3 consume A1's regenerated types).
OQ-A, OQ-B, and multi-forge addressing are ruled (§Resolved decisions); the
task briefs below reflect those rulings.

### A1 — Proto: amend the T1 add-list to the reconciled surface

Amends #995 T1 (design.md:1621-1651). **Placement corrected (Matt, 2026-08-05):** #995 T1
and this record's original draft directed `ForgeNotification` into
`agent_gateway.proto` while also adding `AgentControl.forge_notification` to
`agent.proto`. That is a **circular import** — `agent_gateway.proto` already
imports `agent.proto` (for `AgentFrame`/`AgentControl`), so a back-reference
from `agent.proto` to a type defined in `agent_gateway.proto` is a cycle buf
rejects (`buf lint`: `compass/v1/agent.proto: detected cyclic import while
importing "compass/v1/agent_gateway.proto"`). Every other `AgentControl`
payload type is defined in `agent.proto`; forge is the first `agent_gateway.proto`
call family that also needs an `AgentControl` notification variant, which is why
the defect surfaced only here. The fix is a new internal-only leaf
`proto/compass/v1/forge.proto` that imports **only** `compass/v1/compass.proto`
and holds every type reachable from `AgentControl.forge_notification`
(`ForgeNotification`, `ForgeNotificationKind`, `ForgeArtifactKind`, and the
`CommentRef` reference); `agent.proto`, `agent_gateway.proto`, and `runner.proto`
all import the leaf without a cycle. The wire contract, field numbers, and
generated Go/TS names are identical to the shapes below — only the source file
each type lives in changes (DL-161).

Add to `proto/compass/v1/forge.proto` (new leaf): `ForgeNotification` /
`ForgeNotificationKind`, `ForgeArtifactKind`, and `CommentRef`. Add to
`proto/compass/v1/agent_gateway.proto`: the `Forge` RPC;
`ForgeCallRequest`/`ForgeCallResult`/`ForgeCallError`; the seven operation
requests and `ListIssuesResponse`; `SubscribeForgeRequest`/`Response`,
`UnsubscribeForgeRequest`/`Response`. Do **NOT** add the six domain
messages (`Issue`, `IssueComment`, `PullRequest`, `ForgeAuthor`,
`ForgeChecksSummary`, `ForgeCheck`) — instead import
`compass/v1/compass.proto` and retype the carrier arms:

`Interfaces:`

- `ForgeCallResult.result` oneof: `compass.v1.Issue issue = 2` (create_issue /
  get_issue), `ListIssuesResponse issues = 4`, `compass.v1.PullRequest
  pull_request = 5` (create_pull_request / get_pull_request); the two former
  `IssueComment` arms (`issue_comment = 3`, `pr_comment = 6`) take the ruled
  `CommentRef` (§Resolved decisions OQ-B; defined in the `forge.proto` leaf) — a
  write ack sets `url` + `comment_id`.
- `ListIssuesResponse { repeated compass.v1.Issue issues = 1; }`
- `ForgeNotification` (in `forge.proto`): `compass.v1.ChecksSummary checks = 9`;
  `comment = 8` takes the same ruled `CommentRef`, with `body` + `forge_account` +
  (for a Compass-agent commenter) `agent` set. Its `forge` field is a
  `compass.v1.ForgeRef` (one forge-identity vocabulary, §item 13), not a bare
  `string provider`.
- `runner.proto`: `rpc RelayForgeCall(RelayForgeCallRequest) returns
  (RelayForgeCallResponse)` + `SessionsResponse.forge_notification = 7`;
  `agent.proto`: `AgentControl.forge_notification = 9` (its type
  `ForgeNotification` resolved through the `forge.proto` import) — field numbers
  verbatim from #995 T1 (design.md:1631-1635).
- `gen-fence` grep (`proto/moon.yml`) extended with unanchored
  `ForgeCall|RelayForgeCall|ForgeNotification|ForgeArtifactKind` plus the
  word-boundable internal-only requests (`CommentRef`, the op requests,
  `SubscribeForge*`/`UnsubscribeForge*`); canonical `compass.v1` symbols NOT
  fenced. The new `forge.proto` is wired into all three gen lanes: the public
  `buf.gen.yaml` exclude, the `buf.gen.internal-go.yaml` inputs + `M`-maps
  (→ `compassv1internal`), and the `buf.gen.agent-ts.yaml` inputs.
- Read ops survive (OQ-A ruled option 3): `GetIssueRequest`,
  `ListIssuesRequest`, `GetPullRequestRequest`, `ListIssuesResponse`, and the
  `get_issue`/`list_issues`/`get_pull_request` oneof members + the `issues`
  result arm all stay; the read path is answered from the projection/store
  (see A2).

`Test cycle:` unchanged from #995 T1 (design.md:1648-1651): `direnv exec .
moon run compass-proto:ci` — lint, breaking (additive → clean), drift,
gen-fence (now also proving the canonical types did NOT get fenced and DID
generate into the internal lanes).

### A2 — Go: the ingestion translation replaces proto-mirroring in the Service layer

Amends #995 T2a's Service emission surface and T3's `ForgeCaller` seam
(design.md:1653-1670, :1921-1923). The `go/internal/forge` value types
(`Author`, `Issue`, `Comment`, `PullRequest`, `Checks`, `CreateIssue`,
`CreatePR`, `IssueFilter`) and the `Provider` interface survive verbatim as
internal Go types. What changes: the Service layer's wire-facing methods
return canonical types, and the forge→canonical mapping is one exported,
tested translation site — the ingestion translation #1018 mandates
(design.md:42-46, :371-384).

`Interfaces:`

```go
package forge

// Translate is the single forge→canonical mapping site (DL-069): every field
// of the internal value types lands in the canonical compass.v1 shapes here
// and nowhere else. Owner-header parse results land in AgentAttribution;
// verified is set by the forge-login cross-check (#995 OQ-1 / #1018 ruling).
func TranslateIssue(in Issue, attr *compassv1.AgentAttribution) *compassv1.Issue
func TranslatePullRequest(in PullRequest, attr *compassv1.AgentAttribution) *compassv1.PullRequest
func TranslateChecks(in Checks) *compassv1.ChecksSummary
```

The `ForgeCaller` seam (#995 :1921-1923) retypes accordingly, e.g.:

```go
GetPullRequest(ctx context.Context, account store.AccountID, req *compassv1internal.GetPullRequestRequest) (*compassv1.PullRequest, error)
```

(`compassv1` = the public generated package for `compass.proto`;
`compassv1internal` keeps carrying the internal envelopes.) Subscribe/
Unsubscribe signatures are unchanged (scalar payloads).

**Read-path population contract (OQ-A/F1).** `TranslateIssue` above maps a
forge value type to a forge-SUBSET canonical `Issue` — it cannot populate the
server-side `id` or lifecycle machinery (state/priority/assignee/prs/tracker),
which have no source in a live forge read. Under OQ-A's ruled option 3,
the read ops do NOT call `TranslateIssue` directly: the Service answers
`get_issue`/`list_issues`/`get_pull_request` from its projection/store (a
fully-populated canonical `Issue`, `id` set), composing a forge fetch +
`TranslateIssue` only for an artifact the store does not track (subset
`Issue`, `id` empty ⇒ not addressable by `UpdateIssueState`). That read path
takes the store handle, not just the forge value type. The population contract
is stated here, not left implicit.

`Test cycle:` T2a's RED-first table tests extend with translation
round-trips: every internal value-type field asserted present in the
canonical output; owner-header parse → `AgentAttribution` including the
unverified default.

### A3 — Agent + notification surfaces consume the canonical types

Amends #995's agent-side task (`forge.ts` tool family, design.md:2293-2349)
and the notification path (`NotifyForgeAccount`/`DetectChanges`,
design.md:2084-2089, :2254). No structural change: the same seams carry the
retyped payloads generated by A1.

`Interfaces:`

- `RunnerTransport.forge(req: ForgeCallRequest): Promise<ForgeCallResult>` —
  unchanged signature; the result arms now deserialize to the generated
  canonical `Issue`/`PullRequest` TS types (from the internal agent gen lane,
  which now emits `compass_pb.ts` via the A1 import).
- `func (h *Hub) NotifyForgeAccount(ctx context.Context, account
  store.AccountID, n *compassv1internal.ForgeNotification) (delivered int,
  err error)` — unchanged; `ForgeNotification`'s retyped arms ride through.
- `func DetectChanges(prev Snapshot, now Snapshot)
  []*compassv1internal.ForgeNotification` — unchanged signature; emits OQ-B
  comment refs and `compass.v1.ChecksSummary`.

`Test cycle:` the #995 T9 E2E (design.md:2369-2372) runs unchanged as the
smoke: subscribe → fabricated upstream comment → notification reaches the
agent's control lane → chat ping lands.

## Tasks

- [ ] A1 — amend T1: carrier family added, six domain messages NOT added,
      result arms retyped to `compass.v1`, gen-fence extended
      (carrier-only), `compass-proto:ci` green.
- [ ] A2 — amend T2a/T3: `TranslateIssue`/`TranslatePullRequest`/
      `TranslateChecks` as the sole mapping site; `ForgeCaller` retyped;
      translation round-trip tests green.
- [ ] A3 — amend agent/notification briefs: retyped payloads through
      unchanged seams; T9 E2E green.
- [x] OQ-A (read ops answered from projection/store), OQ-B (single `CommentRef`
      mirroring `Issue` authorship), and multi-forge addressing (single-forge
      v1) ruled by Matt (2026-07-31); folded into the briefs above.

## Ledger delta

Proposed DECISIONS.md rows — pure append under **Comms & tools** (the #995
forge rows' home, DECISIONS.md:99-103). On current `main` (after #1035
renumbered the duplicate DL-071 to DL-091) the highest allocated id is DL-091
(DECISIONS.md:129), DL-082..089 are reserved for #1021 (SEA-1570, in flight),
and DL-056..066 is an older unexplained gap. The next free id above the
reserved band is **DL-092**, which this amendment proposes. (The single-writer
owns the final id; only the id cell changes.) The placement-correction row
**DL-161** was allocated later, at correction time: after the design corpus
migrated into this repo (#179), it takes the next free id above the whole
reserved band (the compass-ux-foundation record #186 reserves DL-148..160), so
it clears every declared block — ledger ids are non-sequential by convention,
grouped by theme not issued in order.

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-092 | #995's forge-shaped proto domain family (`Issue`/`IssueComment`/`PullRequest`/`ForgeAuthor`/`ForgeChecksSummary`/`ForgeCheck` in `agent_gateway.proto`) is not built: per DL-069 the forge Provider layer's value types stay internal Go data at the ingestion boundary, the `ForgeCall*` carrier family survives with its result arms retyped to the canonical `compass.v1` types, and `AgentAttribution` supersedes `ForgeAuthor` (closing #995 OQ-1's field-set gate) | Active (Matt, 2026-07-31) | [ownership amendment §What this amendment asserts](compass-server-ownership-layer-amendment/design.md#what-this-amendment-asserts) |
| DL-161 | The forge notification/carrier types that `AgentControl.forge_notification` reaches (`ForgeNotification`/`ForgeNotificationKind`, `ForgeArtifactKind`, `CommentRef`) live in a new internal-only leaf `proto/compass/v1/forge.proto` (imports only `compass.proto`), NOT in `agent_gateway.proto` as #995 T1 / this record's original §A1 add-list directed: that placement is a circular import (`agent_gateway.proto` already imports `agent.proto`, so `agent.proto` referencing an `agent_gateway.proto` type is a cycle buf rejects). The `ForgeCall*` carrier + op requests stay in `agent_gateway.proto`; the leaf is imported by `agent.proto`/`agent_gateway.proto`/`runner.proto` acyclically. Wire contract, field numbers, and generated names unchanged — placement only | Active (Matt, 2026-08-05) | [ownership amendment §A1](compass-server-ownership-layer-amendment/design.md#a1--proto-amend-the-t1-add-list-to-the-reconciled-surface) |

No existing #995 row flips: DL-048 (ownership layer), DL-049 (`ForgeCall*`
carrier on the gateway socket), DL-050 (owner header), DL-051 (`go/internal/
forge` provider), DL-052 (server-only credential), DL-053 (subscriptions),
DL-054 (notifications v1), and DL-055 (ownership index) all govern surfaces
this amendment keeps — none of them ledgered the domain-message shapes, which
lived only in #995's Decision-5 proto block (design.md:356-431). Matt's OQ-A
ruling keeps the read ops (answered from the projection/store), so DL-049's
"sibling `ForgeCall*` family" wording stands unchanged and no narrowing row is
needed.

## Resolved decisions (Matt, 2026-07-31)

**OQ-A (RULED — option 3) — the `Forge` read ops survive, answered from the
projection/store.** The read ops (`get_issue`/`list_issues`/`get_pull_request`)
stay on the `AgentGateway` lane (the egress-sealed agent holds no server token
and cannot subscribe to `CompassService.SubscribeEvents`; DL-076,
DECISIONS.md:109), and the server answers them from its projection/store —
returning a fully-populated canonical `Issue`/`PullRequest` (server-side `id`
set, lifecycle machinery present) for a tracked artifact, composing a live
forge fetch + `TranslateIssue` only for an artifact the store does not track
(subset shape, `id` empty ⇒ not addressable by `UpdateIssueState`). This
dissolves the critic-F1 population gap (the canonical `Issue` REQUIRES a
server-side `id`, #1018:170-173, plus machinery a live forge PROXY read cannot
populate — DL-055's ownership index only covers Compass-authored artifacts),
changes NO wire surface (only the op's server-side semantics), and spends no
forge rate budget on artifacts the server already tracks (a proxy
`get_pull_request` would otherwise spend several forge API calls per op filling
reviews/threads/checks, #1018 PullRequest :202-218, against the shared 5000/hr
budget). A2's read path gains a store-join step (its signature takes the store
handle, not just the forge value type); the population contract is stated in
A2, not left implicit.

**OQ-B (RULED — one `CommentRef`, authorship mirroring `Issue`) — the
`IssueComment` replacement.** #995 used `IssueComment` in two carriers: the
`ForgeCallResult` arm for the two comment writes (#995 design.md:329, :332)
and `ForgeNotification.comment = 8` (:1199-1200). Both take ONE message:

```proto
message CommentRef {
  string url = 1;                       // the forge comment permalink
  uint64 comment_id = 2;                // the forge comment id
  string body = 3;                      // the comment text (unset on a write ack)
  string forge_account = 4;             // the commenter's forge login; always set on a notification
  compass.v1.AgentAttribution agent = 5; // set only when the commenter is a Compass agent; unset for a human
}
```

The write-result arms set `url` + `comment_id` (a write tool needs only an
acknowledgement + link); the notification arm sets `body` + `forge_account` +
(when the commenter is a Compass agent) `agent`. This is deliberately the same
authorship pattern the canonical `Issue` already uses — `forge_account` always
set, `agent` unset for a non-Compass (human) author (#1018 Issue :183-186) —
so a comment's authorship reads identically to an issue's, one grain down. It
does NOT reintroduce the DL-069 wart: that wart is a **parallel
`Issue`/`PullRequest` family** (two messages for one board artifact forcing
double-maintenance), and `CommentRef` duplicates no canonical type — #1018's
`Comment` (:277-281) exists only as a member INSIDE a PR review thread, there
is no standalone canonical issue-comment message for `CommentRef` to shadow. A
carrier that adds Compass attribution to a forge comment is the same shape the
canonical types themselves are (forge fields + `AgentAttribution` + Compass
machinery), not a second copy of an existing one.

**Multi-forge request addressing (RULED — single-forge v1).** The surviving
op requests and `SubscribeForgeRequest` address artifacts by `(repo, number)`
with no provider/host, while the canonical RESULT types carry `ForgeRef`
(DL-091: repo-only coordinates collide under multiple connected forges).
Request addressing stays single-forge for a GitHub-first v1 (repo unambiguous);
adding an optional `ForgeRef`/provider to the op requests and
`SubscribeForgeRequest` is a named additive follow-up, free to add later
without a buf-breaking change.

**Assumption 1 (non-load-bearing) — ledger id.** The dispatch brief named
"next id DL-072+", but DECISIONS.md already allocates through DL-091 (DL-072..081,
DL-090, DL-091; #1035's renumber took DL-091), so this record proposes DL-092 as described in §Ledger
delta. The single-writer owns the final id; only the id cell changes.

**Assumption 2 (non-load-bearing) — `updated_at_unix_ms` is dropped.** #995's
`Issue.updated_at_unix_ms` (design.md:371) and
`PullRequest.updated_at_unix_ms` (:416) have no counterpart on the canonical
types (#1018 design.md:169-218 carries no timestamps). Assumed intentional:
the projection's stream ordering and `SubscribeEventsResponse.at_unix_ms`
supersede per-artifact freshness stamps. If an agent-side tool needs it,
it is an additive canonical field later — not a reason to keep a forge wire
type.

**Assumption 3 (non-load-bearing) — gen-fence cite drift.** #995 cites the
gen-fence grep at `proto/moon.yml:141`; the live file has the script at
`proto/moon.yml:151` (read this run). Treated as ordinary line drift, not a
semantic change.
