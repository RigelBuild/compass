# Remove dead `initial_prompt` from the agent-session start path

Status: Active
Tracking: SEA-1959. Ruled by Matt as a separate cutover shipping ahead of the
runner deliver-leg build (2026-08-12).

> **Design record.** All file+line citations below are paths in
> `RigelBuild/compass` as read in this working copy on 2026-08-11; line
> numbers drift as code evolves. Every snippet was read and quoted verbatim
> from the named file in this pass.

## Problem / Intent

`StartAgentSessionRequest.initial_prompt` and `SpawnAgentRequest.initial_prompt`
(and the internal `SpawnPeerRequest.initial_prompt` that threads into them) are
accepted on the wire but **never delivered to the agent** — dead plumbing.
There is no working server-originated turn-driver in its place *yet*: the
intended replacement is a message posted to the agent's home channel (the
SEA-1569 delivery path), but that leg is not wired end to end today (see "The
real turn-driver" below) and is being built as an immediately-following work
item. Removing the dead field removes a misleading API surface that promises a
first turn it never delivers — independent of when the replacement lands.

### Evidence: the field is accepted…

The public proto declares it as a live contract
(`proto/compass/v1/compass.proto:592-594`):

```proto
// Optional initial prompt to send once the session is ready. Empty = start
// idle, awaiting a later prompt.
string initial_prompt = 2;
```

and again on the composite spawn (`proto/compass/v1/compass.proto:616-618`):

```proto
// Optional initial prompt sent once the session is ready. Empty = start idle,
// awaiting a later prompt (same contract as StartAgentSessionRequest).
string initial_prompt = 2;
```

The internal gateway spawn threads it along
(`proto/compass/v1/agent_gateway.proto:170`):

```proto
string initial_prompt = 3;       // threaded to StartAgentSessionRequest.initial_prompt
```

The server relays it verbatim: `SpawnAgent`'s `runSpawn` copies it onto the
Start request (`go/server/spawn.go:166-169`):

```go
startResp, err := s.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{
  ContainerName: container,
  InitialPrompt: msg.GetInitialPrompt(),
}))
```

as does the agent-initiated spawn chain (`go/server/lifecycle.go:323-326`):

```go
startResp, err := l.hub.Start(ctx, "", &compassv1.StartAgentSessionRequest{
  ContainerName: container,
  InitialPrompt: req.GetInitialPrompt(),
})
```

and the hub relays the whole request to the Runner unchanged
(`go/internal/runnerhub/commands.go:68-72`):

```go
func (h *Hub) Start(ctx context.Context, requestID string, req *compassv1.StartAgentSessionRequest) (*compassv1.StartAgentSessionResponse, error) {
  result, _, err := h.relay(ctx, req.GetContainerName(), &compassv1internal.SessionsResponse{
    RequestId: orNewRequestID(requestID),
    Command:   &compassv1internal.SessionsResponse_Start{Start: req},
  })
```

(the resume sibling `go/internal/runnerhub/resume_start.go:10-11` states "The
public start request is relayed VERBATIM").

### …and then dropped

The Runner's Start arm hands the request to the host and never touches the
prompt (`go/internal/runner/dispatch.go:360-361`):

```go
case *compassv1internal.SessionsResponse_Start:
  sessionID, err := d.host.Start(ctx, c.Start, cmd.GetResumeBody().GetSessionBody())
```

`agentHost.Start` (`go/internal/runner/host.go:284`) execs the agent and binds
the control subscription, but sends no prompt control op. The exec-time
configuration surface, `AgentEnv` (`go/internal/runner/agent_exec.go:45-68`),
carries `UID`/`HomeDir`/`Workdir`/`Model`/`Persona`/`Role`/`ResumeSessionFile`
— **no prompt field** — and its doc comment enumerates exactly what the agent
CLI reads (`agent_exec.go:36-42`: `HOME`, `COMPASS_WORKDIR`, `COMPASS_MODEL`,
`COMPASS_PERSONA`, `COMPASS_ROLE`, `COMPASS_RESUME_SESSION_FILE`).

The control op that *could* deliver a prompt exists but has zero production
callers: `PromptControl` (`go/internal/gen/compass/v1/agent.pb.go:589`) and the
gateway's `controlProducer.Send`/`SendIfLive`
(`go/internal/runner/gateway/control.go:241`) are referenced outside their
definitions only in `_test.go` files (verified by grep across `go/`: the only
non-generated, non-test hits are the definitions in `control.go` themselves).

### The real turn-driver

A posted message fans out to subscribed agents and is delivered as a control op
(`go/internal/delivery/dispatch.go:31` `onMessagePosted` → `:97` `fanOut` →
`SubscribedAgents` → `DeliverControl`). The agent account is seeded
always-subscribed to its home channel at creation
(`go/internal/store/accounts.go:194-196`):

```go
"INSERT INTO channel_members (channel_id, account_id, subscribed) VALUES ($1, $2, FALSE), ($1, $3, TRUE)",
```

A message posted before the session exists is swept in on session start
(`go/internal/delivery/settle.go:37-48` `OnSessionStarted`), and the agent
starts a turn on an idle deliver (`packages/compass-agent/src/agent.ts:83-85`,
"an idle deliver starts a turn at once"). This is the intended replacement for
the dead `initial_prompt`.

**Status of this path today (load-bearing caveat).** The home-channel deliver
leg is NOT yet functional end to end: the Runner's dispatcher has no
`SessionsResponse_DeliverControl` arm — a deliver op falls through to the
default `errorResult("unrecognized session command variant")`
(`go/internal/runner/dispatch.go:449`; the switch handles Start at `:360` and
the other session commands, none a deliver) — and the gateway parks
`AgentControl_Deliver` as an empty shell that `representable` rejects before it
can be sent (`go/internal/runner/gateway/control.go:190-215`,
`errEmptyControlVariant` `:65-70`). So no server-originated control op drives a
live agent turn yet. Building that deliver leg is a separate,
immediately-following work item (ruled by Matt 2026-08-12; the SEA-1728 e2e
real-turn coverage stacks on it). This record removes the dead `initial_prompt`
field on its own; the replacement turn-driver it names becomes functional when
the deliver leg lands.

## Approach

**Option A — clean cutover: delete the field and `reserved` its number and
name**, in all three messages at once:

- `StartAgentSessionRequest`: drop field 2, add `reserved 2; reserved
  "initial_prompt";` (`proto/compass/v1/compass.proto:589-601`).
- `SpawnAgentRequest`: drop field 2, same reservation
  (`proto/compass/v1/compass.proto:611-626`).
- `SpawnPeerRequest`: drop field 3, add `reserved 3; reserved
  "initial_prompt";` (`proto/compass/v1/agent_gateway.proto:170`).

No in-repo production caller sends a non-empty value except test fixtures and
the agent's spawn tool (both cleaned in the same cutover), so the clean cut is
safe: there is no live pinned client to break. Reserving the number/name
prevents any future field from silently decoding stale bytes.

`buf breaking` is a non-issue here: the breaking gate was removed pre-dogfood
(`buf.yaml:37` and `proto/moon.yml:165` both read "buf breaking gate removed
pre-dogfood (SEA-1922); RE-ADD AT GA / first pinned client (tracked:
SEA-1951)"). There is no `breaking` task in `proto/moon.yml` (its tasks are
lint/gen/drift/gen-fence/ci, `:25-169`) and no `FIELD_NO_DELETE` `ignore_only`
list in `buf.yaml` (its only `ignore_only` block is for lint rules, `:28-36`).
So the removal needs no exemption and satisfies no breaking check — `reserved`
is pure forward-compat hygiene, kept so the freed field numbers cannot be
silently reused when the gate is re-added at GA.

Caller cleanup sweeps every producer/consumer of the generated field (Go, the
agent's TS spawn tool, e2e fixtures, tests) in the same PR, so the tree never
holds a reference to a removed symbol. This removal does not depend on a
replacement being live: it deletes dead surface. The home-channel post +
start-sweep path above is the *intended* driver, becoming functional when the
deliver leg lands (see "The real turn-driver").

## Alternatives considered

### Option B — deprecate-and-ignore (rejected)

Keep the field on the wire, mark it `[deprecated = true]`, and have the server
ignore (or reject) a non-empty value. Why it loses:

- It leaves the exact misleading surface this record exists to remove: a
  client can still set `initial_prompt` and the proto comment ("Optional
  initial prompt to send once the session is ready",
  `proto/compass/v1/compass.proto:592-593`) keeps promising delivery that
  never happens. Deprecation annotations do not stop generated setters from
  existing or being called.
- With no live pinned client and the `buf breaking` gate removed pre-dogfood
  (`buf.yaml:37`), the soft path buys compatibility with clients that do not
  exist. The clean cut is the cheaper, honest option.
- Server-side rejection of a deprecated field is *more* code than deletion —
  a validation arm plus a test pinning it — to protect a dead path.

### Do nothing (rejected)

The field is harmless at runtime but actively misleads: the e2e fixture
(`go/e2e/agent_ops.go:50-52`, "brings the agent … online … with an initial
prompt") and every `InitialPrompt: "go"` test literal encode the false belief
that the prompt seeds a turn. The recent SEA-1728 e2e work had to discover the
hard way that the prompt is never delivered (and that the home-channel deliver
leg meant to replace it is itself not yet wired); the next reader will too,
until the field is gone.

## Global Constraints

- Go module `github.com/RigelBuild/compass/go`, go 1.25/1.26.
- VCS is jj + jj-vine (`skill://jj`): bookmark-per-PR, `jj-vine submit` only,
  review fixes as additive commits. Conventional Commits subject with the
  `Co-authored-by: Matt Wilkinson` trailer (`rule://commit-conventions`).
- Buf discipline: field removals MUST `reserved` both the field number and the
  name in the same edit (forward-compat hygiene). There is no `buf breaking`
  gate to satisfy — it was removed pre-dogfood (`buf.yaml:37`,
  `proto/moon.yml:165`; re-added at GA per SEA-1951) — so no `FIELD_NO_DELETE`
  exemption is needed.
- Regen ordering is proto → codegen → callers: edit the `.proto`, run
  `moon run proto:gen` (three buf lanes — public, agent-TS, internal-Go;
  `proto/moon.yml:51-72`), then fix Go/TS compile errors. The `drift` and
  `gen-fence` CI tasks (`proto/moon.yml:87-121,122-176`) gate a schema edit
  committed without its regenerated trees, so proto edit + regen + caller
  cleanup land in one PR — the tree must compile at every commit.
- No behavioral replacement ships in this cutover (D3): the record removes dead
  surface only.

## Plan

### T1 — proto edit

Delete the three `initial_prompt` fields and reserve their numbers/names.

- `proto/compass/v1/compass.proto`: in `StartAgentSessionRequest` (`:589-601`)
  remove `string initial_prompt = 2;` and its comment; add
  `reserved 2; reserved "initial_prompt";`. Same in `SpawnAgentRequest`
  (`:611-626`), which also uses field 2.
- `proto/compass/v1/agent_gateway.proto`: in `SpawnPeerRequest` (`:170`)
  remove `string initial_prompt = 3;`; add `reserved 3; reserved
  "initial_prompt";`.

No `buf.yaml` edit: the `buf breaking` gate was removed pre-dogfood
(`buf.yaml:37`), so there is no `FIELD_NO_DELETE` list to extend and no breaking
check to pass.

Interfaces: consumes nothing; produces the edited schema. Verified by
`moon run proto:lint` passing (there is no `proto:breaking` task).

### T2 — regenerate all codegen lanes

Run `moon run proto:gen` (`proto/moon.yml:51-86`) and commit the regenerated
trees: `go/gen/compass/v1/compass.pb.go` (drops
`StartAgentSessionRequest.InitialPrompt` at `:2767` /
`GetInitialPrompt` `:2815-2820`, and `SpawnAgentRequest.InitialPrompt`
`:2885` / `:2934-2939`), `go/internal/gen/compass/v1/agent_gateway.pb.go`
(drops `SpawnPeerRequest.InitialPrompt` `:595` / `GetInitialPrompt`
`:645-650`), `packages/compass-client/src/gen` and
`packages/compass-agent/src/gen` (drop the `initialPrompt` TS fields,
`compass_pb.ts:1143,1206`, `agent_gateway_pb.ts:299`).

Interfaces: consumes T1's schema; produces the four checked-in gen trees. The
tree does NOT compile until T3/T4 land — T1–T5 are one atomic PR, ordered
commits within it or a single commit.

### T3 — Go caller cleanup (server + fixtures + tests)

Remove every Go reference to the deleted field:

- `go/server/spawn.go:168`: drop `InitialPrompt: msg.GetInitialPrompt(),` from
  the `StartAgentSessionRequest` literal in `runSpawn`.
- `go/server/lifecycle.go:325`: drop `InitialPrompt: req.GetInitialPrompt(),`
  from the `hub.Start` call in the spawn chain.
- `go/e2e/agent_ops.go`: change `StartSession(ctx, containerName,
  initialPrompt string)` (`:52`) to `StartSession(ctx, containerName string)`
  and `Resume(ctx, containerName, resumeSessionID, initialPrompt string)`
  (`:72`) to drop the param; delete the `InitialPrompt:` struct fields
  (`:57,77`); update the doc comments (`:50-51` currently says "with an
  initial prompt"). Update every e2e callsite of both.
- Test sweep — delete `InitialPrompt: "go"` / `"do the thing"` literals:
  `go/internal/runner/gateway/lifecycle_test.go:65`,
  `go/server/lifecycle_e2e_pgtest_test.go:120,337`,
  `go/server/lifecycle_pgtest_test.go:79,130`,
  `go/server/service_spawn_pgtest_test.go:41`.
- `go/e2e/legthreefour_test.go:59-62`: the leg-3/4 spawn tool-call is built as a
  JSON literal `{"handle":%q,"display_name":%q,"initial_prompt":%q}`. Drop the
  `initial_prompt` key and its `fmt.Sprintf` arg. It is a Go string literal, so
  it does not break `go build`; and `spawnParameters` (arktype, default
  `onUndeclaredKey: "ignore"` — no `reject` configured,
  `packages/compass-agent/src/lifecycle.ts:79-93`) would silently drop an
  undeclared `initial_prompt` at runtime rather than reject it — so an
  un-cleaned literal is harmless but stale. Remove it for hygiene and to keep
  the tree free of references to the removed field.

Out of scope (verified): `apps/ui` `SpawnSpec.initialPrompt` is local
optimistic-UI state only — it is never mapped onto a `SpawnAgentRequest` /
`StartAgentSessionRequest` wire call — so it is intentionally NOT a T3/T4 caller
and stays untouched.

Interfaces: consumes T2's regenerated Go types; produces a compiling `go/`
tree. Verified by `go build ./...` and `go test ./...` (unit lanes) plus the
pgtest lanes.

### T4 — agent spawn-tool cleanup (TypeScript)

- `packages/compass-agent/src/lifecycle.ts`: remove the `"initial_prompt?"`
  tool-schema entry (`:90-92`, "Initial prompt to seed the new peer's first
  turn") and the `initialPrompt: params.initial_prompt ?? "",` mapping
  (`:159`). This edits the agent's spawn tool surface — the schema the model
  sees — so the tool description no longer advertises a seed prompt (see
  D3: the advertised behavior is already a no-op today).
- `packages/compass-agent/src/lifecycle.test.ts`: drop the
  `initial_prompt: "do the thing"` input and the
  `expect(spawn.initialPrompt).toBe("do the thing")` /
  `expect(req.call.value.initialPrompt).toBe("")` assertions
  (`:179,190,207`).

Interfaces: consumes T2's regenerated TS types; produces a passing
`compass-agent` test suite (`bun test` in the package, or its moon task).

### T5 — record freeze hygiene

Fold Matt's answers to the load-bearing forks into this record as decisions,
now the Resolved decisions D1–D3 above (per `skill://design`, no load-bearing OQ
survives the merge), then the driver
submits the design PR; the implementation PR (T1–T4) follows against the
frozen record.

Interfaces: consumes Matt's rulings; produces the frozen record.

## Tasks

- [ ] T1: proto — delete + reserve `initial_prompt` in
      `StartAgentSessionRequest`, `SpawnAgentRequest`, `SpawnPeerRequest`
      (no `buf.yaml` edit — breaking gate removed pre-dogfood).
- [ ] T2: regen — `moon run proto:gen`, commit all four gen trees.
- [ ] T3: Go sweep — `server/spawn.go`, `server/lifecycle.go`,
      `e2e/agent_ops.go` signatures + callsites, five `_test.go` files
      (incl. `e2e/legthreefour_test.go` spawn JSON literal).
- [ ] T4: TS sweep — `compass-agent` `lifecycle.ts` tool schema + mapping,
      `lifecycle.test.ts`.
- [x] T5: fold OQ rulings into this record; freeze (decisions D1–D3 below).

## Resolved decisions

All three were load-bearing forks; Matt ruled each (2026-08-11), folded here as
the contract the implementation PR executes against (frozen on merge of this
record).

### D1 — Removal mechanism: clean cut (A)

**Ruled: A — delete the fields and `reserved` the numbers/names.** Safe without
any breaking-gate ceremony: the `buf breaking` gate was removed pre-dogfood
(`buf.yaml:37`), there is no live pinned client, and no in-repo caller depends
on the behavior (it has none). The deprecate-and-ignore path (B) would leave the
misleading surface in place — the entire point of the removal — while being
*more* code (a server-side validation arm) to protect a dead path. See
Alternatives considered. `reserved` is kept as forward-compat hygiene for when
the `buf breaking` gate is re-added at GA.

### D2 — Scope: all three messages in one cutover

**Ruled: yes — `StartAgentSessionRequest`, `SpawnAgentRequest`, and
`SpawnPeerRequest` all in one cutover.** They share the dead-plumbing fate: spawn
threads `initial_prompt` to Start (`go/server/spawn.go:168`,
`go/server/lifecycle.go:325`, `agent_gateway.proto:170` "threaded to
StartAgentSessionRequest.initial_prompt"), and Start drops it. A partial removal
would leave the spawn fields dangling with nowhere to thread and force a second
buf-breaking PR for no benefit.

### D3 — No replacement seed mechanism

**Ruled: no replacement — the seed is purely a comms message.** A spawner that
wants its new peer to take a first turn posts an instruction to the peer's home
channel (seeded always-subscribed at `go/internal/store/accounts.go:194-196`),
which is the intended driver via the start-sweep delivery path
(`go/internal/delivery/settle.go:37-48`; see §"The real turn-driver") — the
agent's normal messaging workflow, needing no code specific to seeding. That
delivery leg is itself not yet wired end to end (a separate,
immediately-following work item); removing `initial_prompt` stays a pure
dead-code removal regardless of when it lands.

Consequence to note in the follow-up: the agent's spawn tool today advertises
`initial_prompt` as "Initial prompt to seed the new peer's first turn"
(`packages/compass-agent/src/lifecycle.ts:90-92`), but since the field is dead a
freshly spawned peer already gets no seed turn from it — an **existing latent
bug**, not a regression this removal introduces. T4 removes the misleading tool
surface; the driver files a follow-up issue flagging that a spawner must post to
the peer's home channel to drive its first turn.
