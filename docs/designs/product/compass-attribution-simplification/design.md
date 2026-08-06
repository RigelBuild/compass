# Compass attribution simplification — attribution is a display fact

Status: Active
Tracker: SEA

> **Amends frozen contract (#1018, DL-068).** This record is a sibling
> amendment to `docs/designs/product/compass-issue-model/design.md` (merged in
> #1018): the merged record is frozen, so a later change ADDS a record. It
> ratifies Matt's 2026-08-01 ruling that agent attribution is a plain display
> fact — not a trust claim — reversing DL-068's hedge-unless-verified board
> render and the `verified`/forge-login cross-check of DL-050 and #995 OQ-1
> as applied to attribution display. #1018 and #1037 are NOT edited; the
> ledger routes readers of their stale prose to this truth (the
> supersede-by-routing convention). No wire build of the affected message has
> shipped.

## Problem / Intent

The frozen `AgentAttribution` message carries three fields and a
hedge-unless-verified render contract
(`compass-issue-model/design.md:137-144`):

```proto
message AgentAttribution {
  string agent_handle = 1;  // the agent handle CLAIMED by the header; not proof
  string owner_handle = 2;  // the owning USER's handle CLAIMED by the header
  bool verified = 3;        // set by the server's forge-login cross-check at
                            // ingestion (#995 OQ-1): true only when the artifact's
                            // forge author login equals Compass's own forge
                            // identity. The UI hedges the claim unless verified.
}
```

Matt ruled (2026-08-01) that the forged-owner-header threat this machinery
guards against is not worth designing around: "every issue tracker I'm aware
of lets you assign the owner of an issue to whoever, there's no gating. who
cares if someone 'forged' the owner of an issue? … It doesn't have any effect
on the content, or the ability for it to get into `main`." Attribution is a
plain display fact. The board render already ships the bare handle —
`authorLabel` returns `` `@${agent.agentHandle}` `` with no owner text and no
hedge (compass repo `apps/ui/src/board-render.ts:85-86`) — so the render and
the frozen contract disagree. This record makes the contract catch up.

## Approach

One idea: **attribution is a display attribute — parse-populated, rendered
bare, no trust ceremony.**

The message slims to the shape Matt originally green-lit:

```proto
// The Compass agent attribution parsed from the owner header at ingestion —
// a plain display fact (Matt, 2026-08-01). If a header names an agent, that
// is the displayed author; there is no cross-check, no verified bit, and no
// population gating. Never reaches an authz, routing, or ownership decision
// (DL-050 stands for decisions; display is not a decision).
message AgentAttribution {
  string agent_handle = 1;  // the authoring agent's handle, from the header
  reserved 2, 3;
  reserved "owner_handle", "verified";
}
```

Fields 2–3 and the names `owner_handle`/`verified` are reserved per proto
discipline: the frozen #1018 record published the 3-field shape, so the
numbers are burned even though no wire build has shipped them — grep of the
compass repo's `proto/compass/v1/compass.proto` and the generated client
`packages/compass-client/src/gen/compass/v1/compass_pb.ts` (both verified
this run) finds no `AgentAttribution` symbol.

**Population, plainly.** Attribution is populated directly from the parsed
owner header at ingestion — the same parse #1018/#1037 already specify ("the
ingestion translation fills the type's own `AgentAttribution` fields",
`compass-issue-model/design.md:599-600`). No forge-login cross-check, no
index-join gate, no verified bit. If a header names an agent, that is the
displayed author — which is what every issue tracker does with an owner
field.

**Render, both surfaces.** The board renders the bare `@handle` (shipped,
`board-render.ts:85-86`). The forge-tool path renders the same bare
`@handle` — no "claims to be" hedge, no "not a Compass agent" distinction.
This reverses the #995 T8 hedged-tool-render contract (the hedged wording and
its golden string test, `compass-server-ownership-layer/design.md:2314-2318`
and `:2352-2358`) downstream.

**`owner_handle` drops as plain normalization, not security.** The owner was
never per-artifact truth: it is a property of the agent account —
"`owner_user_id` … Server-set to the creating caller; not a client-chosen
field" (`proto/compass/v1/comms.proto:131-133`) — resolved server-side,
never restated per artifact. The shipped render already states the rule: "An
agent's owner is a property of the agent account
(AgentAccount.owner_user_id), never restated per artifact"
(`board-render.ts:83-84`). Carrying it per artifact is a denormalized
duplicate with no reader.

**Frozen records: superseded by routing, not edited.** #1018's message
definition (`compass-issue-model/design.md:137-144`) and attribution prose
(`:595-617`), and #1037's `verified`-carrying clauses, stay as written;
\#1037 stays green at its gate. A reader is routed from stale prose to the
new truth by the ledger: DL-094 plus the DL-068 flip.

## Alternatives considered

- **The trust model (forge-login cross-check + `verified` + hedged render —
  the prior design of this record and of DL-068/#995 OQ-1 as applied to
  attribution).** Rejected by Matt's 2026-08-01 ruling: the forged-header
  threat is imaginary — a forged owner has no effect on content or on
  anything reaching `main`, so the machinery guards nothing worth guarding.

## Plan

### Global Constraints

- **Attribution is display-only.** A parsed attribution never reaches an
  authz, routing, or ownership decision (DL-050's decision-surface ban
  stands; displaying a handle is not a decision).
- **Owner-from-account.** Any surface needing an agent's owner resolves it
  from `AgentAccount.owner_user_id` (`comms.proto:131-133`), never from a
  per-artifact field.
- **Proto field-number discipline.** `AgentAttribution` fields 2 and 3 and
  the names `owner_handle`/`verified` are reserved forever; never reused.
- **No wire build has shipped** (grep verified this run — §Approach), so the
  slim shape is authored directly; there is no migration.
- **No edits to frozen or in-flight records.** `compass-issue-model/design.md`
  (#1018) and `compass-server-ownership-layer-amendment/design.md` (#1037)
  are not modified; DL-094 + the DL-068 flip are the routing.
- **This PR is docs-only.** T1 is this PR; T2–T6 are downstream execution in
  the compass repo, enumerated so the executor spends zero discovery calls,
  and explicitly NOT part of this PR.

### T1 — this record + ledger delta (THIS PR)

Author this record; add DL-094 (the display-fact contract); flip DL-068 to
`Superseded by DL-094`. DL-092 is unchanged — it already stands `Active` and no
supersede is warranted (see §Ledger delta for the DL-092 judgment).

`Interfaces:` consumes the frozen records cited above; produces
`docs/designs/product/compass-attribution-simplification/design.md` and the
`DECISIONS.md` delta (DL-094 + the DL-068 flip). Gate: `design-ledger-gate`.

### T2 — proto: slim `AgentAttribution` (compass repo; NOT this PR)

When #1018's proto lands, author `AgentAttribution` directly in the slim
shape of §Approach in `proto/compass/v1/compass.proto`, with the `reserved`
lines. One message, used everywhere it appears (board stream, forge-tool
result arms, `CommentRef`, notifications) — no split, no sibling claim
shape.

`Interfaces:` produces `message AgentAttribution { string agent_handle = 1;
reserved 2, 3; reserved "owner_handle", "verified"; }` and its generated TS
in `compass_pb.ts`.

### T3 — server ingestion: parse-populate, no cross-check (compass repo; NOT this PR)

The ingestion parse populates `agent_handle` directly from the owner header
(the same parse #1018/#1037 already do). Remove the forge-login cross-check
and the `verified` computation from the attribution path; drop #1037's
unverified-default translation-test expectation when that surface is built.

`Interfaces:` consumes the T2 message and the existing `StripOwner` parse;
produces the population rule: header names an agent ⇒ `agent.agent_handle`
set; no header ⇒ `agent` unset.

### T4 — TS stub-mirror flip (compass repo; NOT this PR)

- `apps/ui/src/stub-data.ts:92-101` — the `AgentAttribution` interface: drop
  `ownerHandle: string` (`:95-96`) and `verified: boolean` (`:97-100`);
  reword the doc comment (`:88-91`, currently "handles plus a server-set
  trust bit") to the display-fact semantics.
- `apps/ui/src/stub-data.ts:636-640` — the `attrib()` factory, currently
  `return { agentHandle: handle, ownerHandle: "matt", verified };`: drop
  both params/fields, becoming `return { agentHandle: handle };`; reword its
  hedge doc comment (`:636-637`).
- `apps/ui/src/board-render.test.ts:181-197` — the `authorLabel` suite's
  local fixture builds the 3-field shape (`:182-186`) and its second test
  asserts the label "ignores the verified bit" (`:192-197`). After the flip
  the fixture becomes `{ agentHandle }` and the ignores-verified test is
  vacuous — replace it with the plain derived-from-handle assertion
  (`authorLabel({agentHandle: "nemo"}) === "@nemo"`).

`Interfaces:` consumes the T2 shape; produces the 1-field `AgentAttribution`
TS interface and updated `attrib()`/test fixtures; the golden `@handle`
strings stay green unchanged.

### T5 — forge-tool render: drop the hedge (compass repo; NOT this PR)

The tool-path attribution render simplifies to the bare `@handle` — remove
the "claims to be @atlas (Compass agent, owned by @matt)" hedge wording and
the "not a Compass agent" distinction
(`compass-server-ownership-layer/design.md:2314-2318`), and remove the
golden hedge-string test (`:2352-2358`). A parsed author is just shown.

`Interfaces:` consumes the T2/T3 attribution; produces the bare-handle tool
render and its updated tests (no golden hedge string).

### T6 — board render: no-change verify (compass repo; NOT this PR)

`apps/ui/src/board-render.ts:85-86` already renders the amended contract:

```ts
export function authorLabel(agent: AgentAttribution): string {
  return `@${agent.agentHandle}`;
}
```

It reads only `agentHandle` — no code change. Verify the golden copy tests
(`board-render.test.ts:188-190`) stay green; the doc comment's justification
(`:79-84`) may be simplified to the display-fact ruling while touching T4's
file, but no behavior changes.

`Interfaces:` none produced; consumes `authorLabel(agent: AgentAttribution):
string`.

## Tasks

- [x] T1 — this record authored; DL-094 added; DL-068 flipped; DL-092 left
      unchanged (already Active) (this PR)
- [ ] T2 — proto: slim `AgentAttribution` + reservations (compass repo,
      downstream PR)
- [ ] T3 — server ingestion: parse-populate `agent_handle`, no cross-check,
      no `verified` (compass repo, downstream PR)
- [ ] T4 — TS mirror: `stub-data.ts` interface + `attrib()` + test fixture
      flip (compass repo, downstream PR)
- [ ] T5 — forge-tool render: bare `@handle`, hedge + golden hedge test
      removed (compass repo, downstream PR)
- [ ] T6 — board render: verify no code change, golden tests green (compass
      repo, same downstream PR as T4)

## Ledger delta

Made in this PR, byte-consistent with `DECISIONS.md`.

- **DL-094 added** (UI shell section — co-located with DL-068, the row it
  supersedes): the display-fact contract.

  > Compass agent attribution is a plain display fact, not a trust claim:
  > `AgentAttribution` slims to `{agent_handle}` (field numbers 2–3 and
  > names `owner_handle`/`verified` reserved), populated directly from the
  > parsed owner header at ingestion with no forge-login cross-check and no
  > population gating; the board and the forge-tool path both render the
  > bare `@handle` with no hedge; owner is an account property
  > (`AgentAccount.owner_user_id`), resolved server-side, never restated per
  > artifact. Reverses DL-068's hedge-unless-verified board render and the
  > verified/cross-check of DL-050 and #995 OQ-1 as applied to attribution
  > display; routes around #1018/#1037 (unedited; no wire build shipped)

  Status `Active (Matt, 2026-08-01)`, Record → this record §Approach.

- **DL-068 flipped** to `Superseded by DL-094 (Matt, 2026-08-01)`; Decision
  cell untouched (immutable per ledger conventions, `DECISIONS.md:28-29`).
- **DL-092 stands Active** — unchanged by this PR (its row is not in this
  diff); its `Active (Matt, 2026-07-31)` stands and no supersede is
  warranted. Judgment on its
  parenthetical "(closing #995 OQ-1's field-set gate)": **immaterial — no
  corrective supersede row.** The row's load-bearing clauses — the forge
  proto family is not built, the `ForgeCall*` result arms retype to
  canonical types, and `AgentAttribution` supersedes `ForgeAuthor` — all
  remain true under this record. The parenthetical is an explanatory aside
  recording which open question the supersede answered at ratification time
  (2026-07-31, when the 3-field shape carried `verified`), not a live
  convention. The ledger's immutability rule (`DECISIONS.md:28-29`) already
  routes the overlap — DL-094,
  the newer row, explicitly names the reversal of "#995 OQ-1 as applied to
  attribution display". Burning a new id to restate a true row minus an
  aside would churn the ledger for zero contract change.
