# Compass ask immutability and cancel (RIG-3804)

## Problem / Intent

A posted ask can be rewritten by its author, and there is no way to close an
ask without answering it. The author update path,
`Store.UpdateMessageBlocksAsAuthor` (`go/internal/store/messages.go`), checks
only authorship, channel participation, and that each ask block carries an
`ask_id`:

```go
for i, b := range blocks {
	if b.Ask != nil && b.Ask.AskID == "" {
		return Message{}, fmt.Errorf("%w: block %d ask has no ask_id (an update must carry the existing id)", ErrInvalidArgument, i)
	}
}
```

It never compares the new ask with the stored one. Three defects follow:

1. **Re-pointed pick.** The author keeps an option id and changes its label. A
   user's staged pick now records an id that means something else.
2. **Arity flip.** The author flips a question from multi-select to
   single-select and keeps the ids. A carried two-option answer then fails
   `validateQuestionAnswer` with `ErrInvalidArgument`.
3. **Answer wipe.** The update mapper `askFromWireForUpdate`
   (`go/internal/comms/mapping.go`) builds the ask from the wire with only
   `AskID` and the agent-owned question fields. `askQuestionsFromWire` never
   maps `chosen_option_ids`, `custom_text`, or `timed_out`, and the mapper
   never maps `answered`. So any author update of a message that holds an
   answered ask writes `Answered=false` and empty answers back. That reopens a
   closed ask, and a second `RespondToAsk` then succeeds and posts a second
   `ask_answer` message.

Today no production code reaches this path. The only caller of
`UpdateMessageBlocksAsAuthor` is `Comms.CommitAgentUpdate`
(`go/internal/comms/agent_caller.go`), and only tests call that. The
relayed `conversation_updated` frame that once drove it is removed
(`proto/compass/v1/agent.proto`: "Fields 1 (conversation_posted) and 2
(conversation_updated) are REMOVED"), and `CommsCallRequest` has no edit
arm. The defects are latent. The UI defends against arity and option-id
changes (`sameQuestions` in `apps/ui/src/store.ts`; its tests say
"Designed-for, not yet wired"). It ignores labels on purpose ("at the cost of a
relabelled-in-place option re-pointing a pick"), so nothing defends against
defect 1.

Intent: an ask is **immutable once posted**, and its author can **cancel**
it. Immutability holds by construction: the dead author-update path is
removed, so after append only the answer and the cancel write an ask's message.
Cancel is a server-owned terminal state next to `answered`. A cancelled ask
closes with no answer and renders as withdrawn.

## Global Constraints

Every task below inherits these. A task brief may add to them, never relax them.

- **Spelling.** The state is `cancelled` (double l) everywhere: proto field,
  JSONB key, Go field, TS field, CSS class. The proto tree already spells it
  this way (`compass.proto`, the check-run `state` comment: `"cancelled"`). User
  copy says "withdrawn".
- **Proto is additive.** New field `Ask.cancelled = 4` (`comms.proto`). New
  messages `CancelAskRequest` / `CancelAskResponse` in `agent_gateway.proto`,
  beside `SetAgentStatusRequest`. New oneof arm `cancel_ask = 13` on both
  `CommsCallRequest.call` and `CommsCallResult.result`. No new `CommsService`
  RPC. Field numbers were checked free: `Ask` uses 1-3; `CommsCallRequest` uses
  1-12 (10 is `trigger_traceparent`); `CommsCallResult` uses 1-12. Never
  renumber an existing field. Regenerate with `moon run compass-proto:gen`;
  `compass-proto:drift` and `compass-proto:gen-fence` stay green.
- **No SQL migration.** The new JSONB key is `cancelled,omitempty`, so every
  stored row decodes to `false`. Changed queries live in
  `go/internal/store/queries/*.sql`. Regenerate with
  `moon run compass-go:sqlc-gen` and commit `go/internal/store/db`.
- **Server-owned state.** Only `AnswerAsk` sets `Answered`. Only `CancelAsk` sets
  `Cancelled`. The two are never both true. No inbound wire value sets either.
  After append, nothing else writes an ask.
- **Row lock.** Every write that reads an ask's stored state and then writes the
  message's blocks holds `FOR UPDATE OF m` on that message row for the whole
  read-modify-write, in one `beginTenantTx` transaction.
- **The one-respond-per-ask invariant is untouchable.** A cancelled ask takes
  no respond. The agent still has no ask-answering tool (DL-028's clause, carried
  by DL-212): cancel is not answering.
- **Errors** use the store sentinels in `go/internal/store/errors.go`, wrapped
  with `%w`, and reach the wire only through `edgeError`
  (`go/internal/comms/context.go`). No new sentinel and no new edge mapping.
- **Each slice lands green on its own.** A slice that regenerates proto also
  carries every Go change the regeneration needs to compile, and every guard test
  the new field or arm trips (`TestAskFromWireDropsEveryServerOwnedField`,
  `requireEveryArmCovered`). Its gate is the proto gates plus the `compass-go`
  build and the slice's scoped Go tests.
- **TypeScript** is strict: no `any`, no `as`, no `@ts-ignore`. The one
  sanctioned seam (`as unknown as CommsClient` in `createFakeComms`,
  `apps/ui/src/live/comms-fake.ts`) stays as is. SolidJS v2 idioms only: no
  `produce`, `createResource`, `batch`, or `solid-js/store`.
- **Comments** are 1-2 lines and say why, not what. No planning metadata and no
  issue ids in source text (Go, SQL, proto, TS, CSS).
- **Tests** are written first and seen RED. They assert behaviour a consumer can
  observe: store results and re-reads, Connect codes, published events, tool
  text, DOM. Never a mock echo. A positive control is exempt from RED-first and
  says so.
- **Go** module `github.com/RigelBuild/compass`, `go 1.26.0`. Run only the
  scoped tests of the slice; project-wide validation is the driver's job.

## Approach

Three moves. Each one reuses machinery that exists:

1. **Remove the dead author-update path** (D1). After append, only `AnswerAsk`
   and the new `CancelAsk` write an ask's message. Both change only
   server-owned state, so an ask is immutable by construction.
2. **Add one server-owned terminal flag, `Ask.cancelled`** (D2). Only the
   asking agent sets it, through a new relay arm (D3). It serializes against
   `AnswerAsk` on the row lock that `AnswerAsk` already takes (D4).
3. **Show it.** WAITING clears, the agent's list view marks the ask withdrawn,
   and the UI locks the block (D5, D6).

No new error sentinel, edge mapping, table, migration, or public RPC.

### D1 — Immutable by construction: remove the author-update path

The three defects live in one path, and nothing in production calls it. A text
search over `go/`, `packages/`, `apps/`, and `docs/` for each identifier finds
references only in the path itself, its tests, the generated `db` package, and
frozen design records. (`gopls` was not available for a references query. The
`compass-go` build after T1's deletion is the real proof.)

| Symbol | File |
|---|---|
| `Comms.CommitAgentUpdate`, `Comms.reconcileUpdateAskIDs`, `askIDMismatchError`, `surplusAskError` | `go/internal/comms/agent_caller.go` |
| `updateBlocksFromWire`, `askFromWireForUpdate` | `go/internal/comms/mapping.go` |
| `Store.UpdateMessageBlocksAsAuthor`, `Store.MessageAskIDs` | `go/internal/store/messages.go` |
| queries `UpdateMessageBlocksAsAuthor`, `GetMessageBlocksAsAuthor` | `go/internal/store/queries/messages.sql` |

This record deletes all of them and their tests (T1). Afterwards the only
writer of an existing message's `blocks` is `updateMessageBlocksExec` (the
`UpdateMessageBlocks` query). Its callers are `AnswerAsk` and, after this
record, `CancelAsk`. Both lock the row and change only server-owned fields.
`AppendMessage` mints the ask, and nothing rewrites its content. Defects 1-3
cannot occur: no path sends new ask content or overwrites server state.

A guard keeps it so. `TestNoNewMessageBlockWriters` (`go/internal/store`) has
two reflection checks:

- Every exported method of `*Store` that takes a block-bearing type
  (`Message`, `MessageBlock`, `Ask`, `AskAnswerBlock`, or a slice or pointer of
  one) must be on an allowlist of exactly `AppendMessage`. Today that matches
  `AppendMessage` and `UpdateMessageBlocksAsAuthor`; read methods only return
  these types.
- Every `db.Querier` method whose params struct has a `Blocks` field must be on
  an allowlist of exactly `InsertMessage` and `UpdateMessageBlocks`.

Both checks fail today on `UpdateMessageBlocksAsAuthor`, so the guard is RED
before T1's deletion. The failure text says that a new block-rewrite path needs
its own design record that defines its ask rule. This is the same
reflection-guard pattern as `TestAskFromWireDropsEveryServerOwnedField`.

Kept: `CommitAgentPost` (a test helper for the post-through-an-agent path, out
of scope), `updateMessageBlocksExec`, and `askQuestionsFromWire` (now used only
by `askFromWire`; its comment drops the update sibling).

If Matt keeps the path, Alternatives A1 holds a ready harden design (OQ-1).

### D2 — The terminal state: `Ask.cancelled`

An additive `bool cancelled = 4` on `compass.v1.Ask`, next to `answered = 3`.
It is server-owned in exactly the way `answered` is. Only `CancelAsk` sets it,
and `askFromWire` already drops it because it maps questions only. The
invariant is **never both true**:

- `marshalBlocks` rejects an ask block with `Answered && Cancelled`
  (`ErrInvalidArgument`).
- `unmarshalBlocks` treats such a row as corrupt (a fail-loud read error, like
  the existing ask totality errors).
- An `ask_answer` snapshot must not be cancelled, on both write
  (`validateAskAnswer`) and read.

Store: `Ask.Cancelled bool` (`go/internal/store/types.go`) and
`storedAsk.Cancelled` with JSON tag `cancelled,omitempty`. Every existing row
decodes to `false`, so no migration is needed. The enum shapes are weighed in
A4 and A5 (OQ-7).

### D3 — Cancel surface: an agent relay arm, author only

Cancel is relay-only, like `set_status`. `CancelAskRequest{ask_id = 1}` and an
empty `CancelAskResponse` live in `agent_gateway.proto`, and arm
`cancel_ask = 13` is added to both oneofs. `Hub.executeCall` dispatches the
arm to `CommsCaller.CancelAskAsAccount(ctx, account, askID)`. `Comms`
implements that method over `Store.CancelAsk` in the `SetStatusAsAccount`
shape: primitive arguments, `errNoActor` on an empty account, and `edgeError`
on a store error. There is no `CommsService` RPC, so there is no auth-gate
entry and no client export.

Why relay-only: the only planned caller is the agent tool. A UI withdraw
control would bring its own authorization rule (OQ-4 b), and the public RPC can
land with it, so its contract is decided once (OQ-2; A2 holds the public
shape).

Why not a field on `RespondToAsk`: respond is the answerer's verb, open to any
participant. DL-028's clause (carried by DL-212) is that the agent's request
oneof cannot express `RespondToAsk`. Cancel is the author's verb and gets its
own arm, so the agent stays structurally unable to answer.

**Who may cancel: the ask's author only** (OQ-4). `Store.CancelAsk` resolves
the ask with `FindAskMessage`, the same participation gate as `AnswerAsk`. It
then checks `msg.AuthorAccountID == actor`. A participant who is not the author
can already see the ask, so a distinct `ErrPermissionDenied` leaks nothing.
`errors.go` documents that sentinel for "unauthorized operations on addressable
rows". A non-participant and an unknown id stay merged as `ErrNotFound`.

Outcomes. Checks run in this order: find, then author, then state. The relay
codes are the in-band `CommsCallError.code` tokens that `commsCallError`
builds from the Connect code.

| Call | Ask state | Result |
|---|---|---|
| `cancel_ask`, author | open | `cancelled=true` written; `MessageUpdated` published; empty `cancel_ask` result |
| `cancel_ask`, author | cancelled | empty `cancel_ask` result; no write, no event (OQ-6) |
| `cancel_ask`, author | answered | `ErrConflict` → `already_exists` ("ask %q is already answered") |
| `cancel_ask`, participant, not author | any | `ErrPermissionDenied` → `permission_denied` |
| `cancel_ask`, non-participant or unknown id | — | `ErrNotFound` → `not_found` |
| `cancel_ask`, empty `ask_id` | — | `ErrInvalidArgument` → `invalid_argument` |
| `RespondToAsk` | cancelled | `ErrConflict` → `AlreadyExists` ("ask %q is cancelled"); no answer message |
| `RespondToAsk` | answered | `ErrConflict`, unchanged |

`ErrConflict`'s doc already names "an operation rejected because its target is
already in a terminal state". Both crossings (cancel after answer, answer after
cancel) are exactly that.

### D4 — Respond vs cancel: exactly one wins

Both `AnswerAsk` and `CancelAsk` run in one `beginTenantTx` (READ COMMITTED)
transaction:

1. `FindAskMessage` locks the row `FOR UPDATE OF m`.
2. The terminal check runs on the locked row, and the state is mutated in place.
3. `updateMessageBlocksExec` writes the row; `AnswerAsk` also inserts the answer
   message.
4. Commit.

The second transaction blocks on the lock. When the first commits, Postgres
re-evaluates the `blocks @> {ask_id}` predicate on the new row version
(EvalPlanQual). The ask_id is still present, so the row matches, and the
second transaction reads the committed state:

- answer first: `CancelAsk` sees `Answered` → `ErrConflict`;
- cancel first: `applyAskAnswer` sees `Cancelled` → `ErrConflict` before any
  answer message is built. So at most one `ask_answer` message exists, and none
  exists for a cancelled ask.

`applyAskAnswer` gains one guard next to the `ask.Answered` guard. Since the
two flags are never both true, the order of the guards does not matter. The
race test copies the start barrier of `TestAnswerAskConcurrentSameAskOneConflict`
(`go/internal/store/messages_test.go`) and runs 20 trials, because the winner
is not deterministic.

### D5 — Fan-out, presence, and the agent side

- **Event.** `CancelAskAsAccount` publishes `MessageUpdated` for the ask's
  message after commit, only when the row changed. It publishes no
  `MessagePosted`: a cancel is not a delivery trigger. The asker is the
  canceller, so no one is owed a delivery.
- **Presence (WAITING).** `AgentHasOpenAsk`
  (`go/internal/store/queries/presence_reads.sql`) also excludes cancelled
  asks. The presence publisher (`go/internal/presence/run.go`, `handleEvent`)
  already recomputes from the store on every `MessageUpdated`, so WAITING clears
  when the agent's last open ask is cancelled. This refines DL-074's "an
  authored `Ask` with `Ask.answered=false`" by citation.
- **The tool result.** `comms_cancel_ask` returns `Withdrew ask <id>; no answer
  will arrive.` on success, first call or repeat. When the answer won the race,
  the call fails in-band with `already_exists`. The tool description says what
  that means: the ask was answered first, and the answer arrives in a later
  turn on the deliver lane.
- **Finding the ask id again.** After a restart, a compaction, or a resume, the
  agent needs the ask id, but the list view prints only the message id today.
  `comms_list_messages` therefore prints the ask id in every ask marker:
  `[ask <fence> id="<ask_id>"] <question>`, and the same for `[answered …]` and
  the new `[withdrawn …]`. A cancelled ask is checked before the per-question
  map, so its empty-questions fallback is `[withdrawn <fence> id="…"]`, never a
  bare `[ask …]`.
- **Revising an ask** is two calls (OQ-3): post the new ask, then withdraw the
  old one. The tool description gives this order. With it, the agent always has
  an open ask, so WAITING does not flap.

### D6 — How a withdrawn ask renders (apps/ui)

- `Ask` (`apps/ui/src/comms-stub.ts`) gains `cancelled: boolean`. `adaptAsk`
  (`apps/ui/src/live/adapt.ts`) maps it read-only, like `answered`.
- `AskBlock` (`apps/ui/src/components/ChannelView.tsx`):
  - `closed()` becomes `submitted() || ask().answered || ask().cancelled`, so
    every option and text input is disabled and the submit row is hidden.
  - The class list becomes `["block-ask", { answered: submitted() ||
    ask().answered, cancelled: ask().cancelled }]`. Today `answered` keys on
    `closed()`; keeping that would mark a cancelled ask as answered in the DOM.
  - A status line shows while cancelled: `<div class="ask-withdrawn"
    role="status">Withdrawn by the asker — no longer taking answers.</div>`.
    While the ask is cancelled, it replaces the `ask-error` span, so a refusal
    caused by the cancel does not show as a raw error. This guards the DOM only;
    the store rules below keep `askError` clear.
- **A user mid-answer when the cancel arrives.** Their picks and typed text are
  local only. The pushed cancelled ask wins:
  - `serverHasNoAnswer` becomes `serverIsOpen = (ask) => !ask.answered &&
    !ask.cancelled`, so `preserveLocalAsks` does not carry staged input over a
    cancelled push.
  - The block re-renders with the server's empty answers, locked and marked
    withdrawn. Staged input is dropped, because showing it would suggest it was
    recorded (OQ-5).
  - `recordAnswer` and `submitAsk` return early on a cancelled ask, as they do
    on an answered one.
- **A respond in flight when the cancel commits** is refused with
  `AlreadyExists`. Both arrival orders converge on "withdrawn, no error":
  - push first, then refusal: the `sendAsk` catch clears the submitted mark.
    Its restage guard adds `!current.cancelled`, so nothing is restaged. The
    catch also skips `setAskErrors` when `current?.cancelled`, so
    `askError(askId)` stays clear. It still calls `onCommsError`, so the refusal
    is never swallowed.
  - refusal first, then push: the ask still reads open, so the catch restages
    and records the error. The cancelled push then replaces the block, and the
    withdrawn line hides the error span. `askError(askId)` keeps the stale
    message in this order; only the DOM hides it.
- **CSS** (`apps/ui/src/app.css`): the `.block-ask.answered` rule's selector
  becomes `.block-ask.answered, .block-ask.cancelled`, so there is one rule, not
  a copy. `.ask-withdrawn` uses `--cx-text-dim`.
- **Out of scope:** the `ASK` row flag (`messageFlags`, `apps/ui/src/comms.ts`).
  It flags answered asks today and stays as is. A UI cancel control is also out
  (OQ-2, OQ-4).

### D7 — Migration and compatibility

- **Stored rows.** No key means `false`, so there is no backfill. The deleted
  path had no production caller, so no stored ask was wiped or re-pointed.
- **Wire.** The change is additive only: field 4, two messages, and arm 13.
  `compass-proto:drift` and `gen-fence` gate the regenerated code.
- **Old UI bundle, new server.** It renders a cancelled ask as open. Its respond
  gets `AlreadyExists` and shows the refusal in the error span. A reload fixes
  it. This is safe.
- **Old agent image, new server.** It renders a cancelled ask as `[ask …]`
  without an id. The agent cannot answer, so this does no harm. It never sends
  `cancel_ask`.
- **New agent, old server.** Arm 13 is an unknown field to the old server, so
  `executeCall` sees no variant and returns `invalid_argument`. The tool fails
  in-band. Ship the server first; that is the normal order.
- **Runner gateway.** `CommsCallRequest` passes through opaquely
  (`go/internal/runner/gateway/gateway.go`), so the gateway does not change.
- **Server rollback (accepted cost).** A binary without `Cancelled` reopens
  cancelled asks. Its decoder drops the `cancelled` key, it accepts a respond to
  the ask, and its next write of that row erases the key. This is accepted
  pre-GA; a rollback across this change is rare and visible.

### Non-goals

- Immutability for other block kinds (text, `ask_answer`) is out. This record
  covers asks only.
- A link from a withdrawn ask to its replacement is out (OQ-3, A3).
- A public cancel RPC and a UI cancel control are out (OQ-2, OQ-4).
- Timeout auto-close of an ask is out. `timed_out` is untouched.

### Known limits

- **A removed agent stays WAITING.** `CancelAsk` finds the ask through
  `FindAskMessage`, which requires channel participation. `AgentHasOpenAsk`
  filters by `author_account_id` only, with no visibility filter. So an agent
  removed from the ask's channel cannot cancel its ask, and stays WAITING. This
  gap predates this record (DL-074 says "in a visible channel"); the record does
  not widen it.
- **A human-authored ask cannot be cancelled.** There is no public RPC (OQ-2).
- **Withdraw-then-post flaps WAITING.** If the agent withdraws before it posts
  the revision, presence publishes WAITING → IDLE → WAITING. The tool
  description steers it to post first.

## Alternatives considered

### A1 — Harden the author-update path (instead of D1)

Keep the path and enforce immutability inside it.

- **Change rule.** The update must hold exactly as many ask blocks as the
  stored row, in the same order, each with the stored `ask_id`. So it cannot
  remove or add an ask. Text blocks are free. The agent-owned content of each
  ask must be field-equal: per question `question_id`, `question`, `header`,
  `allow_multiple`, `recommended`; per option, in order, `id`, `label`,
  `description`, `preview`. Server-owned state (`Answered`, `Cancelled`,
  `ChosenOptionIDs`, `CustomText`, `TimedOut`) is copied from the stored ask,
  never taken from the update. A violation is `ErrInvalidArgument` (OQ-8).
- **Enforcement.** `UpdateMessageBlocksAsAuthor` becomes a locked
  read-modify-write: a new `LockMessageBlocksAsAuthor` query (the
  chain/participating predicate, `FOR UPDATE OF m`), then
  `checkAskUpdate(stored, next)` and `sameAskContent`, then the existing UPDATE
  on the same transaction.
- **Fixes it must carry.**
  - Move the positional ask_id fill and the mismatch and surplus errors from
    `reconcileUpdateAskIDs` into `checkAskUpdate`, under the lock. Then delete
    `MessageAskIDs` and `GetMessageBlocksAsAuthor`. That query checks only
    `EXISTS (SELECT 1 FROM channel_members ...)`, which is not TREE-aware, so it
    refuses a subtree author that the UPDATE allows.
  - The field-classifier test walks `Ask`, `AskQuestion`, and `AskOption`. For
    each compared field it mutates a copy and asserts `sameAskContent` is false.
    For each carried field it asserts `sameAskContent` is true and that
    `checkAskUpdate` copies the stored value.
  - Seed tests with `pendingAsk(...)`, not `askBlockID`, which pre-fills chosen
    ids on an unanswered ask, a state no server can hold. Rework
    `TestUpdateMessageBlocksAsAuthorEditsInPlaceAndReturnsRow`, which adds an
    ask through an update.
- **Variant: by-reference asks.** `checkAskUpdate` replaces each ask block with
  the stored block instead of comparing content. No classifier is needed, and
  server state can never be wiped. A changed ask is ignored or refused.
- **Cost.** It is the largest store slice, and it hardens code that only tests
  call. The change rule is chosen with no consumer to check it against. If a
  real edit path later wants other semantics (for example text-only streaming
  edits), the work is redone.

### A2 — Public `CommsService.CancelAsk` RPC (instead of the relay-only arm)

`rpc CancelAsk(CancelAskRequest) returns (CancelAskResponse)`, with the
messages in `comms.proto`. Add `CommsServiceCancelAskProcedure` to the
`authenticatedOpen` case in `go/internal/auth/admin_gate.go`, and export the
types from `packages/compass-client/src/index.ts`. The relay arm then calls the
same handler, in the `OpenDMAsAccount` pattern. It lets any `PostMessage`
author, human or agent, cancel. The cost is a public contract with no shipped
caller, whose authorization may change when a UI withdraw control lands.

### A3 — Replace flow (instead of withdraw-only)

An optional `replaces_ask_id` on the post, or a
`CancelAskRequest.replacement` that is posted in the same transaction. The
withdrawn line then links to the revision, and WAITING never flaps. A smaller
variant records the replacing ask_id on the cancelled ask. The cost is a new
post-path field and linking rules before any user needs them.

### A4 — `AskState` enum that replaces `answered = 3`

It makes the both-true state unrepresentable, but it removes field 3. Every
reader moves in lock-step: the UI `adaptAsk`, the agent list renderer, the
`AgentHasOpenAsk` jsonpath, and the stored JSONB key, which needs a data
migration.

### A5 — Additive `AskState` enum at field 4, with `answered` kept

No third terminal state is expected. The proto `timed_out` comment makes a
timeout an answer ("the answer was recorded by timeout auto-selection"). So the
both-true check stays one pairwise check. The enum would duplicate `answered`
with a derivation rule that every reader must follow.

## Decisions (ledger rows)

These rows are not claimed yet. After Matt rules, the ids are claimed with
`bun tools/dl-claim` in this record's PR, and that PR adds the
`docs/designs/decisions/agent/DL-NNN.md` files (front matter `id` / `decision` /
`status` / `record`). No later slice claims ids. Each row follows its OQ's
recommendation; a different ruling changes the row.

- **Asks are immutable by construction.** The author block-update path is
  removed. After append, only `AnswerAsk` and `CancelAsk` write an ask's
  message, and they change only server-owned state. A new block-rewrite path
  needs its own record that defines its ask rule. Record:
  `#d1--immutable-by-construction-remove-the-author-update-path`.
- **Ask cancel.** An ask closes without an answer through the server-owned
  terminal flag `Ask.cancelled` (additive, field 4), never true together with
  `answered`. It is set only through the agent relay arm `cancel_ask = 13`
  (no public RPC), and only by the ask's author. A repeat cancel succeeds with no
  change. Cancel after answer and respond after cancel are both
  `AlreadyExists`. Respond and cancel serialize on the message row lock, so
  exactly one wins. There is no replace link; a revision is a new, unlinked ask.
  Record: `#d3--cancel-surface-an-agent-relay-arm-author-only`.
- **Agent cancel tool.** The agent gets `comms_cancel_ask`, and every ask marker
  in `comms_list_messages` prints its ask id. This refreshes DL-212's tool
  count by citation (thirteen tools). DL-028's no-answering clause carries
  forward unchanged: the oneof still cannot express `RespondToAsk`. Record:
  `#d5--fan-out-presence-and-the-agent-side`.
- **WAITING excludes cancelled asks.** This refines DL-074 by citation. Record:
  `#d5--fan-out-presence-and-the-agent-side`.
- **Withdrawn rendering.** The UI renders a cancelled ask as withdrawn and
  locked, and drops any staged local input. Record:
  `#d6--how-a-withdrawn-ask-renders-appsui`.

## Plan

Stack order: T1 → T2 → T3 → T4 → T5, one linear line. T3 and T4 touch disjoint
trees, so they can be built in parallel off T2 and then stacked in that order.
Each slice lands green on its own (Global Constraints). Lane hints name the moon
project whose gate covers the slice.

### T1 — Remove the author-update path; guard against a new one

Lane: `compass-go` (pgtest). Gate: `compass-go:build`, `compass-go:sqlc-drift`,
and `go test -tags pgtest ./internal/store/... ./internal/comms/...`.

- `go/internal/comms/agent_caller.go`: delete `CommitAgentUpdate`,
  `reconcileUpdateAskIDs`, `askIDMismatchError`, and `surplusAskError`. Remove
  the `fmt` import: the two error types' `Error` methods are its only users.
- `go/internal/comms/mapping.go`: delete `updateBlocksFromWire` and
  `askFromWireForUpdate`. The `askQuestionsFromWire` comment drops the
  UPDATE sibling.
- `go/internal/comms/context.go`: the `errServerOwnedBlock` comment drops "the
  agent update path".
- `go/internal/store/messages.go`: delete `UpdateMessageBlocksAsAuthor` and
  `MessageAskIDs`.
- `go/internal/store/queries/messages.sql`: delete the
  `UpdateMessageBlocksAsAuthor` and `GetMessageBlocksAsAuthor` queries.
  Regenerate with `moon run compass-go:sqlc-gen`.
- Surviving comments that name a deleted symbol are updated, for example the
  header of `agent_conversation_pgtest_test.go`.
- Tests that exercise only the deleted path go with it:
  - `go/internal/comms/agent_conversation_pgtest_test.go`: delete the eight
    `TestCommitAgentUpdate*` tests and the helpers `updatedFrame`,
    `askBlockWire`, and `askBlockWireID`. Remove the `strings` import: only the
    deleted ask-id-mismatch, surplus-ask, and foreign-ask tests use it. In
    `TestCommitAgentFramesEmptyAccountFailsClosed`, drop the update assertion,
    discard the now-unused `seeded` result, and rename the test
    `TestCommitAgentPostEmptyAccountFailsClosed`.
  - `go/internal/comms/respond_ask_answer_test.go`: delete
    `TestCommitAgentUpdateRejectsAskAnswerBlock`. Its POST twin
    `TestPostMessageRejectsAskAnswerBlock` stays.
  - `go/internal/store/messages_authored_update_test.go`: delete the file. Move
    `TestAnswerAskStillWorksForANonAuthorMember` to `messages_test.go`, and
    reword its comment to say it pins answering by a non-author member.
  - `go/internal/store/channel_participant_pgtest_test.go`: in
    `TestChannelTreeParticipantReadsAndWrites`, drop the subtree-author update.
    After `ReparentAgent`, assert that `UpdateTopic` by the reparented agent is
    `ErrNotFound`. That query (`ResolveTopicForUpdate`) uses the same
    chain/participating gate, so the reparent coverage stays. In
    `TestChannelTreeOwnerSetSiblingReadWriteDenials`, drop the outsider-message
    insert and its update assertion. The sibling's `UpdateTopic` denial in the
    same test stays.
  - `go/internal/store/message_author_handle_pgtest_test.go`: drop the
    `"author block update"` reader. The `"SearchMessages"` reader searched for
    the text that the update wrote, so it now searches for `"agent note"`.
- New guard, `go/internal/store/block_writers_test.go` (no build tag):
  `TestNoNewMessageBlockWriters`, as D1 describes.

Interfaces:

```go
// Removed (signatures as they are today):
func (c *Comms) CommitAgentUpdate(ctx context.Context, account store.AccountID, updated *compassv1.MessageUpdated) (*compassv1.MessageUpdated, error)
func (c *Comms) reconcileUpdateAskIDs(ctx context.Context, actor store.AccountID, id store.MessageID, blocks []store.MessageBlock) error
func updateBlocksFromWire(blocks []*compassv1.MessageBlock) ([]store.MessageBlock, error)
func askFromWireForUpdate(a *compassv1.Ask) *store.Ask
func (s *Store) UpdateMessageBlocksAsAuthor(ctx context.Context, actor AccountID, id MessageID, blocks []MessageBlock) (Message, error)
func (s *Store) MessageAskIDs(ctx context.Context, actor AccountID, id MessageID) ([]string, error)

// Kept, unchanged: the sole post-append block writer.
func updateMessageBlocksExec(ctx context.Context, dbtx db.DBTX, id MessageID, blocks []MessageBlock) error

// block_writers_test.go
// Allowlists: *Store methods taking Message / MessageBlock / Ask / AskAnswerBlock
// (or a slice or pointer of one) = {AppendMessage}; db.Querier methods whose
// params struct has a Blocks field = {InsertMessage, UpdateMessageBlocks}.
func TestNoNewMessageBlockWriters(t *testing.T)
```

Tests: `TestNoNewMessageBlockWriters` is RED before the deletion (it names
`UpdateMessageBlocksAsAuthor` in both checks) and GREEN after. The reworked
tests above stay green. The build proves that no caller remained.

### T2 — Server cancel: proto, store, comms, relay arm

Lane: `compass-go` with `compass-proto`. This is one landing unit: the proto
regeneration adds a oneof arm and an `Ask` field that the Go guards check, so
the store, comms, and relay changes must land with it. Gate:
`compass-proto:lint`, `compass-proto:drift`, `compass-proto:gen-fence`,
`compass-go:build`, `compass-go:sqlc-drift`, `go test -tags pgtest
./internal/store/... ./internal/comms/... ./internal/runnerhub/...`, plus
`compass-agent:typecheck` and `compass-ui:typecheck`, because the TS gen trees
change.

- Proto:
  - `proto/compass/v1/comms.proto`: add `bool cancelled = 4;` to `Ask`. Extend
    the `RespondToAsk` rpc comment: a cancelled ask is refused with
    ALREADY_EXISTS.
  - `proto/compass/v1/agent_gateway.proto`: add `CancelAskRequest` and
    `CancelAskResponse` after `SetAgentStatusResponse`. Add arm 13 to both
    oneofs, and add `cancel_ask` to the `CommsCallResult` comment's arm list.
  - Regenerate with `moon run compass-proto:gen` (all four trees).
- Store:
  - `go/internal/store/types.go`: add `Ask.Cancelled`.
  - `go/internal/store/blocks.go`: add `storedAsk.Cancelled` and carry it in
    `toStoredAsk` / `fromStoredAsk`. `marshalBlocks` rejects an ask with
    `Answered && Cancelled`. `unmarshalBlocks` returns `store: block %d ask is
    both answered and cancelled` for such a row. `validateAskAnswer` and the
    `ask_answer` read arm reject a cancelled snapshot.
  - `go/internal/store/messages.go`: `applyAskAnswer` returns
    `fmt.Errorf("%w: ask %q is cancelled", ErrConflict, askID)` when
    `ask.Cancelled`. Add `CancelAsk` in the order of D3 and D4:
    `FindAskMessage` lock → author check → state check → set `Cancelled` →
    `updateMessageBlocksExec` → commit. The `updateMessageBlocksExec` doc names
    both callers.
  - `go/internal/store/queries/presence_reads.sql`: the `AgentHasOpenAsk`
    jsonpath becomes `$[*] ? (@.kind == "ask" && (!exists(@.ask.answered) ||
    @.ask.answered == false) && (!exists(@.ask.cancelled) || @.ask.cancelled ==
    false))`. Update the `AgentHasOpenAsk` doc in `presence_reads.go` and
    regenerate sqlc.
- Comms:
  - `go/internal/comms/mapping.go`: `askToWire` sets `Cancelled: a.Cancelled`.
  - `go/internal/comms/agent_caller.go`: add `CancelAskAsAccount`. It returns
    `errNoActor` on an empty account, calls `Store.CancelAsk`, maps errors with
    `edgeError`, and calls `publishMessageUpdated(askMsg)` only when `changed`.
- Relay:
  - `go/internal/runnerhub/hub.go`: add `CancelAskAsAccount` to `CommsCaller`,
    and `cancel_ask` to its `//nolint:interfacebloat` arm list.
  - `go/internal/runnerhub/relay_comms.go`: add the `executeCall` case. Add
    `cancel_ask` to both copies of the "no recognized variant" error string.
  - Test support: `commsCall` (`helpers_test.go`) gains `cancelAsk string`, and
    `fakeCommsCaller` gains `cancelAskErr error`. A `relayCancelAsk` builder
    goes in new `relay_cancel_ask_test.go`, shaped like `relaySetStatus`.

Interfaces:

```proto
// comms.proto
message Ask {
  string ask_id = 1;
  repeated AskQuestion questions = 2;
  bool answered = 3;
  // True once the asker withdrew this ask without an answer. Server-owned: set
  // only by the cancel_ask relay call, never together with answered.
  bool cancelled = 4;
}

// agent_gateway.proto
// Withdraw the calling agent's own pending ask (agent-only, via the relay; no
// public CommsService RPC).
message CancelAskRequest {
  string ask_id = 1;
}
// Empty: the withdraw is acknowledged by a non-error result.
message CancelAskResponse {}
message CommsCallRequest { oneof call { CancelAskRequest cancel_ask = 13; } }
message CommsCallResult { oneof result { CancelAskResponse cancel_ask = 13; } }
```

```go
// store/types.go
type Ask struct {
	AskID     string
	Questions []AskQuestion
	Answered  bool
	// Cancelled is server-owned: set only by CancelAsk, never with Answered.
	Cancelled bool
}

// store/blocks.go
type storedAsk struct {
	AskID     string              `json:"ask_id"`
	Questions []storedAskQuestion `json:"questions,omitempty"`
	Answered  bool                `json:"answered,omitempty"`
	Cancelled bool                `json:"cancelled,omitempty"`
}

// store/messages.go
// CancelAsk withdraws askID for its author. changed is false when the ask was
// already cancelled; the caller publishes only on change.
func (s *Store) CancelAsk(ctx context.Context, actor AccountID, askID string) (askMsg Message, changed bool, err error)

// comms/agent_caller.go
func (c *Comms) CancelAskAsAccount(ctx context.Context, account store.AccountID, askID string) error

// runnerhub/hub.go, CommsCaller
CancelAskAsAccount(ctx context.Context, account store.AccountID, askID string) error

// runnerhub/relay_comms.go, executeCall
case *compassv1internal.CommsCallRequest_CancelAsk:
	if err := h.comms.CancelAskAsAccount(ctx, account, c.CancelAsk.GetAskId()); err != nil {
		return nil, err
	}
	return &compassv1internal.CommsCallResult{
		Result: &compassv1internal.CommsCallResult_CancelAsk{CancelAsk: &compassv1internal.CancelAskResponse{}},
	}, nil

// runnerhub/relay_cancel_ask_test.go
func relayCancelAsk(sessionID, callID, askID string) *compassv1internal.RelayCommsCallRequest
```

Tests (RED first):

- Store, new `go/internal/store/messages_cancel_test.go` (pgtest). Seed asks
  with `pendingAsk(...)`.
  - `TestCancelAskClosesOpenAsk`: `changed=true`. On re-read, `Cancelled=true`
    and `Answered=false`, and no new message is inserted.
  - `TestCancelAskTwiceIsIdempotent`: the second call returns `changed=false`,
    nil, and the same row.
  - `TestCancelAskAfterAnswerConflicts`: `ErrConflict`; the row stays answered.
  - `TestAnswerAskAfterCancelConflicts`: `ErrConflict`. The topic message count
    is unchanged, so no `ask_answer` was inserted.
  - `TestCancelAskByNonAuthorParticipantIsPermissionDenied`, plus
    `TestCancelAskByNonParticipantIsNotFound` (equal to the unknown-id result).
  - `TestCancelAskKeepsSiblingAskOpen`: a message with two asks. Cancel one,
    then answer the other; both succeed.
  - `TestCancelAskRacesAnswerExactlyOneWins`: the start barrier of
    `TestAnswerAskConcurrentSameAskOneConflict`, 20 trials. In each trial,
    exactly one call returns nil and the other returns `ErrConflict`. The final
    ask is answered xor cancelled, and an `ask_answer` message exists if and
    only if it is answered.
- Store, `blocks_test.go`: `TestMarshalBlocksRejectsAnsweredAndCancelled`,
  `TestUnmarshalBlocksAnsweredAndCancelledIsCorrupt`, and
  `TestAskAnswerSnapshotRejectsCancelled`.
- Store, `presence_reads_test.go`: `TestAgentHasOpenAskIgnoresCancelledAsk`.
  The only open ask is cancelled → false; a second open ask → true.
- Comms, `ask_mapping_test.go`: add `"cancelled"` to
  `askOwnership.serverOwned`, and add `if got.Cancelled { t.Error(...) }` beside
  the `got.Answered` check in `TestAskFromWireDropsEveryServerOwnedField`. The
  classification alone does not prove the drop; the named assertion does. Add
  `TestAskCancelledProjectsOutbound`.
- Comms, new `go/internal/comms/ask_cancel_test.go` (pgtest, `newStreamHarness`):
  - `TestCancelAskAsAccountPublishesMessageUpdatedOnce`: the subscriber sees
    one `MessageUpdated` with `ask.cancelled=true` and no `MessagePosted`. Then
    a second cancel, then a text post: the next event is that post's
    `MessagePosted`, which proves the second cancel emitted nothing.
  - `TestCancelAskAsAccountCodes`: empty account → `InvalidArgument`;
    non-author participant → `PermissionDenied`; non-participant →
    `NotFound`; answered → `AlreadyExists`.
  - `TestRespondToAskOnCancelledAskIsAlreadyExists`.
- Relay, `relay_arm_coverage_test.go`: add a `cancel_ask` row to
  `commsArmCases()`, with no seed and no response pair, like `set_status`
  (`CancelAskResponse` has zero fields). Update the `armCase` comment that says
  `set_status` is the only such arm. `requireEveryArmCovered` fails RED until
  the row exists. `TestRelayCommsEveryArmAttributesToBoundAccount` proves
  bound-account attribution and the result arm.

### T3 — Agent: `comms_cancel_ask` and ask ids in list markers

Lane: `compass-agent`. Gate: `compass-agent:typecheck` and `compass-agent:test`.

- `packages/compass-agent/src/compassv1.ts`: in the `agent_gateway_pb` export
  block, export `CancelAskRequest`, `CancelAskRequestSchema`,
  `CancelAskResponse`, and `CancelAskResponseSchema`.
- `packages/compass-agent/src/comms.ts`:
  - add `cancelAskParameters` and the `cancelAsk` tool, placed after `postAsk`
    in the returned array;
  - change "Twelve" to "Thirteen" in the header comment and the
    `createCommsTools` doc;
  - `comms_post_ask`'s description adds: "To revise an ask, post the new one
    first, then withdraw the old one with comms_cancel_ask.";
  - the `comms_list_messages` ask arm adds `id="${attr(askId, fence)}"` to
    every marker. It branches on `cancelled` before the per-question map: each
    question renders as `[withdrawn ${fence} id="…"] ${text}`, and the
    empty-questions fallback renders as `[withdrawn ${fence} id="…"]`.
- `packages/compass-agent/src/render-guard.ts`: add `[withdrawn]` to the
  `flat` comment's marker list.
- Tool count "twelve" → "thirteen": `packages/compass-agent/AGENTS.md`,
  `docs/concepts/tools.md`, and the `packages/compass-agent/src/transport/index.ts`
  header comment ("all twelve native comms tools"). The first two also list
  `comms_cancel_ask`.

Interfaces:

```ts
export const cancelAskParameters = type({
	ask_id: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe(
			'Your ask\'s id, from comms_post_ask or the id="…" on a comms_list_messages ask marker (must not be blank)',
		),
});
// tool: name "comms_cancel_ask", label "Withdraw channel ask", approval "write"
// request: { case: "cancelAsk", value: create(CancelAskRequestSchema, { askId: params.ask_id }) }
// success (result.result.case === "cancelAsk"): `Withdrew ask ${attr(params.ask_id)}; no answer will arrive.`
// otherwise: throw commsFailure(result, "comms_cancel_ask", "cancel_ask")
// list markers: `[ask ${fence} id="${attr(askId, fence)}"] ${text}`
//               `[answered ${fence} id="${attr(askId, fence)}"] ${text}${answer}${how}`
//               `[withdrawn ${fence} id="${attr(askId, fence)}"] ${text}`
```

The tool description says: withdraw your own pending ask so it takes no
answer; a repeat withdraw succeeds; `already_exists` means the ask was answered
first, and that answer arrives in a later turn.

Tests (RED first, `comms.test.ts`):

- The tool-list test names thirteen tools, including `comms_cancel_ask`, and
  still no answering tool.
- The cancel tool sends the `cancelAsk` arm with the given id, and returns the
  success text.
- An `error` result with code `already_exists` throws
  `comms_cancel_ask failed: already_exists: …`.
- The parameter schema rejects a blank `ask_id`.
- The list renderer prints `[ask <fence> id="ask-1"]`,
  `[answered <fence> id="ask-1"]`, and `[withdrawn <fence> id="ask-1"]`. A
  cancelled ask with no renderable question prints `[withdrawn <fence>
  id="ask-1"]`, not `[ask …]`. A malformed ask id renders `(malformed <fence>)`.
- Round trip: an id read from a listed marker, passed to `comms_cancel_ask`,
  is the id the request carries.
- The existing marker expectations in `comms.test.ts` (`[ask ${f}] …`) move to
  the id-bearing form.

### T4 — UI: render withdrawn, drop staged input

Lane: `compass-ui`. Gate: `compass-ui:typecheck`, `compass-ui:test`, and
`compass-ui:stylelint`.

- `apps/ui/src/comms-stub.ts`: add `Ask.cancelled` (doc mirrors `answered`).
  Add `cancelled: false` to the fixture literals, and to every `Ask` literal
  that `tsc` flags in tests.
- `apps/ui/src/live/adapt.ts`: `adaptAsk` maps `cancelled: w.cancelled`.
- `apps/ui/src/store.ts`:
  - `serverHasNoAnswer` → `serverIsOpen`;
  - in the `sendAsk` catch, the restage guard adds `!current.cancelled`, and
    `setAskErrors` is skipped when `current?.cancelled` (`onCommsError` still
    fires);
  - `recordAnswer` and `submitAsk` return early on `ask.cancelled`.
- `apps/ui/src/components/ChannelView.tsx`: apply the D6 changes to
  `AskBlock`: `closed()`, the split `answered` / `cancelled` classes, the
  withdrawn status line, and the error span hidden while cancelled. Update the
  doc comment.
- `apps/ui/src/app.css`: the `.block-ask.answered` selector becomes
  `.block-ask.answered, .block-ask.cancelled`. Add `.ask-withdrawn`.
- `apps/ui/src/live/comms-fake.ts`:
  - `wireAskMessage` gains `cancelled?: boolean`. It throws when `cancelled` is
    combined with `answered` or chosen ids, because no server can hold that
    state.
  - `FakeComms` gains `markAskCancelled(askId)`. After it, `respondToAsk`
    throws `[already_exists] store: conflict: ask "<id>" is cancelled`, after
    the `askHold` and `askFailure` steps, like the answered-once check.

Interfaces:

```ts
export interface Ask {
	askId: string;
	questions: AskQuestion[];
	answered: boolean;
	/** The asker withdrew this ask; server-owned, never true with `answered`. */
	cancelled: boolean;
}
const serverIsOpen = (ask: Ask) => !ask.answered && !ask.cancelled;
// AskBlock: <div class={["block-ask", { answered: submitted() || ask().answered, cancelled: ask().cancelled }]}>
// FakeComms: markAskCancelled: (askId: string) => void;
// wireAskMessage(opts: { …existing; cancelled?: boolean }): WireMessage
```

Tests (RED first):

- `ChannelView.ask.test.tsx`:
  - a cancelled ask shows the withdrawn status, has class `cancelled` and not
    `answered`, and every option and input is disabled with no submit control;
  - push first: a respond is held (`holdNextAskResponse`), a cancelled push
    arrives (`emit`), and the gate rejects. No `ask-error` span renders, and
    the withdrawn line shows;
  - refusal first: `markAskCancelled`, then submit, then the cancelled push. No
    `ask-error` span renders, and the withdrawn line shows.
- `store.live.test.ts`: the user stages picks and text, and then a cancelled
  push arrives. The block holds no staged values, is locked, and `submitAsk`
  sends nothing (`askResponses` stays empty).
- `store.ask-race.test.ts` (store only, no DOM). Its `withLiveStore` gains an
  optional `onCommsError` parameter, the same shape as the `store.live.test.ts`
  helper:
  - push first: a respond is held, a cancelled push arrives, and the gate
    rejects with `[already_exists] … is cancelled`. Nothing is restaged, the
    block holds no staged values, `store.askError("ask-1")` is `undefined`, and
    `onCommsError` received the refusal.
  - refusal first: `markAskCancelled`, then submit. The refusal restages and
    `store.askError("ask-1")` holds the message. Then the cancelled push
    arrives: the block is cancelled with no staged values.
- `adapt.test.ts`: `cancelled` maps through.

### T5 — Spec and ledger

Lane: `compass-eng-docs`.

- `docs/specs/product/compass.md`: add a requirement **"An ask is immutable once
  posted and closes once"** beside "Answering an ask is visibility-scoped and
  serialized". Scenarios: the author withdraws an open ask; a respond after the
  withdraw is refused; a withdraw after an answer is refused; a non-author
  cannot withdraw; a racing respond and withdraw have exactly one winner. The
  `CommsService` message-surface list does not change (no public RPC).
- Ledger: T5 claims no ids. The record PR claimed them (see Decisions); T5's PR
  body cites them. The spec itself carries no DL ids, as today.

Tests: none new. The driver's docs gate covers this slice.

## Tasks

- [ ] T1 — remove `CommitAgentUpdate` … `GetMessageBlocksAsAuthor` and their tests; add `TestNoNewMessageBlockWriters`
- [ ] T2 — server cancel: `Ask.cancelled = 4`, `cancel_ask = 13`, `Store.CancelAsk`, respond guard, WAITING query, `CancelAskAsAccount`, relay arm
- [ ] T3 — agent: `comms_cancel_ask`, ask ids in list markers, `[withdrawn]` marker, tool docs
- [ ] T4 — UI: withdrawn render, staged input dropped, fake `markAskCancelled`
- [ ] T5 — spec requirement, ledger rows

## Open Questions

The design follows each recommendation. Matt rules in the linked decision
issue; the rulings are then folded in here. OQ-1 is load-bearing: it decides
the shape of T1.

- **OQ-1 — Delete the dead author-update path, or harden it?**
  - (a) Delete it (D1). Asks are immutable by construction. A future edit path
    defines its own ask rule in its own record.
  - (b) Harden it (A1): a locked compare of ask content, with server state
    carried from the stored row.
  - (c) Harden it by reference (A1 variant): the stored ask block always
    replaces the update's.

  **Recommendation: (a).** Only tests call the path, and its relay frame is
  removed. (b) and (c) choose an edit rule with no consumer to check it, keep
  the dead path and its tests alive, and add the largest store slice of any
  option.
- **OQ-2 — Cancel surface: relay-only, or also a public RPC?**
  - (a) Relay-only arm `cancel_ask = 13`, like `set_status` (D3).
  - (b) Public `CommsService.CancelAsk` plus the relay arm (A2).

  **Recommendation: (a).** The only planned caller is the agent tool. (b)
  ships a public contract with no caller, and its authorization may change
  when a UI withdraw control lands (OQ-4 b). The accepted cost: a human cannot
  withdraw an ask they posted (Known limits).
- **OQ-3 — Replace flow, or withdraw-only?**
  - (a) Withdraw-only. A revision is a new ask, posted first, then the old one
    is withdrawn.
  - (b) A replace link: `replaces_ask_id` on the post, or a cancel that posts
    the replacement in the same transaction (A3).
  - (c) Record the replacing ask_id on the cancelled ask.

  **Recommendation: (a).** The accepted costs: the withdrawn line does not
  point to the revision, and if the agent withdraws before it posts, WAITING
  flaps WAITING → IDLE → WAITING. The tool description gives the post-first
  order, which avoids the flap.
- **OQ-4 — Who may cancel?**
  - (a) The ask's author only.
  - (b) The author, plus the owning user of an agent author, through a UI
    withdraw control. This clears a stuck WAITING agent without a turn, but the
    agent needs a delivered notice, because it did not cancel.
  - (c) Any channel participant.

  **Recommendation: (a).** It is the smallest surface, and the asker always
  knows the outcome. (b) is a clean follow-on that brings the public RPC with
  it.
- **OQ-5 — Staged input when a cancel lands mid-answer.**
  - (a) Drop it.
  - (b) Keep it read-only and greyed, so the user can copy it into a normal
    message.

  **Recommendation: (a).** Showing picks on a closed ask reads as "recorded".
  (b) costs a second local-only render state.
- **OQ-6 — Second cancel of a cancelled ask.**
  - (a) Success with no write and no event.
  - (b) `already_exists`, like a second respond.

  **Recommendation: (a).** A retried tool call should not turn into a
  model-visible failure. Respond keeps (b) because a second answer carries new
  content; a second cancel carries none.
- **OQ-7 — Terminal-state wire shape.**
  - (a) Additive `bool cancelled = 4`.
  - (b) An `AskState` enum that replaces `answered = 3` (A4).
  - (c) An additive `AskState` enum at field 4, with `answered` kept (A5).

  **Recommendation: (a).** It is additive and needs no data migration, and old
  clients fail safe with `AlreadyExists`. (b) needs a lock-step reader change
  and a migration. (c) duplicates `answered` with no third state in sight.
- **OQ-8 — Error class for a changed ask on update (only if OQ-1 is b or c).**
  - (a) `InvalidArgument`.
  - (b) `FailedPrecondition`.

  **Recommendation: (a).** The rule does not depend on the ask's state, and it
  matches the existing ask_id-mismatch and surplus-ask refusals, which are
  `InvalidArgument`.
