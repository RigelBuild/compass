# Compass dogfood e2e harness — full-stack scenario testing (SEA-1681)

> **Design record.** A reusable full-stack e2e HARNESS for Compass — shared
> stack-bring-up core, a scenario-authoring API of composable primitives, two
> fidelity tiers (backend-only headless, UI-inclusive), and a model-backend
> seam (deterministic vs live) — plus the FIRST scenario set authored on it:
> the five-leg dogfood capstone (bring-up → real agent turn → agent-driven
> spawn → cross-agent messaging → persist+resume) that is the acceptance gate
> for the Dogfood milestone. The design targets the **`RigelBuild/compass`**
> repo; every `go/*`, `proto/*`, `packages/*`, `apps/*`, and `devenv.nix:*`
> citation below is a path in that repo at origin/main HEAD `abdb412c`, not
> this one. It lives in the sealed design corpus because that is where the
> wave's design records freeze.

Status: **Draft** — SEA-1681. Three forks are ruled by Matt (2026-08-05):
model axis (D1), per-PR gate cadence (D2), harness shape (D3); he re-ratifies
at the design-PR gate. Two forks remain open and cross-lane (OQ3 UI tier, OQ4
leg-2 sequencing) — batched in §Open Questions; the record designs against the
recommended arm of each so no task is blocked.

## Problem / Intent

Every e2e proof Compass has today is a subsystem seam test: the in-process
`e2eWire` suite proves spawn/despawn over real per-container sockets but with a
stub engine where "the test plays the agent"
(`go/server/lifecycle_e2e_pgtest_test.go:455` `newE2EWire`, `:641`
`e2eStubRuntime`), the runnerhub integration proves socket-Post → store + bus
(`go/internal/runnerhub/integration_pgtest_test.go:67`), and the merged
`compass-stack` podman test stands up the real server+runner+postgres but
deliberately "creates NO containers — per-agent containers are on-demand via
the ProvisionAgentWorkspace RPC, never called here"
(`go/cmd/compass-stack/integration_podman_test.go:20-28`) and uses
`docker.io/library/alpine:latest` as a stand-in agent image (`:64-68`). Nothing
drives the REAL deployable stack — real server, real runner, a real
`compass-agent` container running a real model turn — end to end, and nothing
composes the already-proven seams into one ordered, repeatable scenario.

SEA-1681 closes that gap, and Matt expanded its scope (direct, 2026-08-05):
the deliverable is not one 5-leg test but a **reusable e2e harness** — test
infrastructure that stands the full stack up once and exposes composable
scenario primitives (create-agent, provision, start-session/drive-turn,
spawn-peer, post-message/@mention, teardown, resume), so that full
end-to-end functionality can be tested with different scenarios, at more than
one fidelity ("may even need to be multiple harnesses, with some including the
full UI client and some not"). The five-leg dogfood capstone — (1) full-stack
bring-up, (2) agent launch + a completed model turn with the transcript
persisted, (3) agent-driven spawn of a second agent, (4) cross-agent
messaging with @mention delivery, (5) teardown + resume from durable state —
becomes the first scenario set authored on the harness and remains the
concrete Dogfood acceptance gate.

Note on ledgers: this record lives in the sealed design corpus
(`docs/designs/platform/`), which the design-ledger-gate governs only for the
**product** corpus (`docs/designs/product/DECISIONS.md`). A platform record
adds no DECISIONS row and declares no ledger delta, so nothing here is
ledger-tracked; the `compass` repo it targets has no design-ledger tooling of
its own.

## Global Constraints

- **Linux-only, rootless podman.** The harness suite is gated exactly like the
  merged compass-stack integration test: `//go:build podman` +
  `podmanUsable()`-guarded skip — "a missing binary or broken rootless setup
  means skip, not fail"
  (`go/cmd/compass-stack/integration_podman_test.go:1,77-81`). The runner runs
  the agent as a baked in-container uid 1000 but no longer requires the HOST
  uid to be 1000: containers launch with
  `--userns=keep-id:uid=<agent-uid>,gid=<agent-gid>`
  (`go/internal/runtime/podman.go:389`, `spec.UID` = `defaultAgentUID` = 1000,
  `go/cmd/compass-runner/main.go:140,167`), which remaps any host uid onto the
  baked uid, and the engine's support for that remap is floor-checked by
  `VerifyUsernsRemapSupport` (`podman.go:415-438`, called from `main.go:97`). The
  once-blocking uid-1000-host requirement (`verifyRunnerUID`) has since been
  lifted (SEA-1691), so the full-stack tier runs on ordinary arbitrary-uid CI
  runners. The interim uid handling for embedded Dogfood is preflight-and-refuse
  (compass-native T4, SEA-1685).
- **AF_UNIX sun_path budget.** `stack.Config.Validate` rejects a `RuntimeDir`
  whose per-container agent-socket tail would overflow the platform sun_path
  cap — on Linux "a RuntimeDir over 38 bytes overflows the cap"
  (`go/internal/stack/config.go:54`, enforcement `:77-81`). The harness MUST
  use the short-root pattern the podman test established (`shortRoot`,
  `integration_podman_test.go:191-203`: a short unique 0700 dir under `/tmp`,
  because "a t.TempDir path would overflow it" `:144-146`).
- **No retries, no sleeps — event-gated determinism.** Every assertion waits on
  a real signal: the comms-bus subscription (`waitMessagePosted` checks Replay
  then Live, "nothing here is a sleep",
  `go/internal/runnerhub/integration_pgtest_test.go:434-439`), store reads, or
  the stack's own readiness/health probes (`go/internal/stack/stack.go:230-233`
  `waitReady` "polls GetServerInfo until the server answers or the budget
  elapses"). Matches the delivery suite's discipline ("never a sleep, never a
  retry", `go/internal/delivery/mention_test.go:8`).
- **Scripts-over-bash.** The harness and its scenarios are Go test code (and,
  for the UI tier, checked-in TS test code) — never shell orchestration. A
  multi-agent, multi-leg scenario with typed assertions cannot be carried by a
  shell script, and the repo's automated-test lane is `go test`
  (`AGENTS.md:34`).
- **Process safety on a shared box.** Teardown is process-based via the
  stack's own `Down` — "SIGTERM to each child's process group … NEVER
  pkill/killall — only the stack's own Down (or the exact PIDs it owns) ever
  stops anything" (`integration_podman_test.go:20-28`); agent containers are
  removed by exact name only (deterministic `NamePrefix + accountID`,
  `go/internal/runner/spec.go:85`).
- **One harness core, tiered consumers.** The stack bring-up, scenario
  primitives, and model seam live in ONE shared core; fidelity tiers
  (backend-only, UI-inclusive) and model modes (deterministic, live) are
  configurations/consumers of that core, never forks of it (Decision D1;
  §Approach A3/A4).
- **Deterministic tier gates PRs; live/UI tiers are on-demand.** Per D1: the
  backend-only + deterministic-model configuration is the per-PR CI gate and
  regression base; live-model and UI-inclusive runs are on-demand/nightly
  (nondeterministic, keys + cost). The two once-blocking feasibility
  prerequisites have since landed — agent-image distribution (SEA-1690, GHCR
  publish) and the host-uid lift (SEA-1691, the userns keep-id remap) — so the
  full-stack deterministic tier now runs as the required per-PR check directly
  on ordinary arbitrary-uid CI runners, with no interim merge-queue/nightly
  staging. This is the subject of Decision D2.
- **Commits** authored as Matt with the seal co-author trailer
  (rule://commit-conventions); the spawning agent ships the PR.

## Approach

One shared harness CORE (stack bring-up + scenario primitives + the model
seam), consumed along three axes:

```text
           ┌──────────────── harness core ────────────────┐
           │ stack.Up/Down (real server+runner+postgres)  │
           │ scenario primitives (Connect clients + store)│
           │ model-backend seam (COMPASS_MODEL)           │
           └──────┬────────────────────────┬──────────────┘
   fidelity axis  │                        │
     backend-only (headless Go suite)   UI-inclusive (real UI client)
     model axis: deterministic = per-PR gate │ live = on-demand smoke
   scenario axis: 5-leg capstone set first; new scenarios compose primitives
```

**Harness shape (Decision D3, Matt): option C — a Go test suite over the
merged compass-stack bring-up (`go/internal/stack`, `stack.Up`/`stack.Stack`).**
The harness is a
`//go:build podman`-tagged Go package that imports `go/internal/stack` and the
generated Connect clients, following the pattern
`go/cmd/compass-stack/integration_podman_test.go` established. Rationale:

- `stack.Up(ctx, cfg, deps)` is an importable Go API that stands up the REAL
  shipped artifact — "private postgres up+reachable → TLS anchor →
  compass-server → poll GetServerInfo readiness → runner token → agent image
  present → compass-runner (token via env). On any step failure the children
  started so far are drained and the lock released, so no half-started stack
  leaks" (`go/internal/stack/stack.go:52-91`). A Ready stack already encodes
  the whole 7-step spawn chain (`integration_podman_test.go:41-49`).
- The podman-gated, `podmanUsable()`-skipped pattern, the short-root sun_path
  discipline, `buildBinariesFromModuleRoot` (compile the three child binaries
  from the tree, `:88-104`), `freePorts` (`:111-129`), and per-test config via
  the same `resolveConfig`/`buildDeps` the CLI uses (`:168-184`,
  `go/cmd/compass-stack/main.go:146,222`) all exist on main and are reused
  verbatim.
- A harness with a scenario-authoring API, typed assertions over store rows
  and bus events, and a model-backend seam is Go-native; shell orchestration
  (option B's shape) cannot carry a scenario API (Global Constraints,
  scripts-over-bash).
- The Matt-ruled model decision (D1) requires a per-PR CI-gating deterministic
  test — that belongs in the `go test` lane next to the existing pgtest lane
  (`.github/workflows/ci.yml:38-44`), not in a shell script.

What the capstone adds over compass-stack's own test: that test stops at
enrollment and never provisions an agent container
(`integration_podman_test.go:20-28`) and pulls alpine as a stand-in because
"the local-only compass-agent:latest … is not pullable, so EnsureImage's real
`podman pull` would fail" (`:30-39`; `go/internal/stack/adapters/image.go:52-65`
"the pull IS the ensure"). This harness is the FIRST thing to drive
Provision → Start → a real agent turn through the real stack — which is why
A1 below needs a real-image path around `EnsureImage`'s pull semantics.

### A1 — Harness core: bring-up over the compass-stack `stack.Up`

A new `go/e2e` (name final at implementation) package with a fixture that:

1. Compiles `compass-postgres`, `compass-server`, `compass-runner` from the
   tree and prepends them to PATH (the `ProcessSupervisor` resolves components
   via `exec.LookPath`, `integration_podman_test.go:83-104`).
2. Builds a `stack.Config` through `resolveConfig` semantics — short-root
   `RuntimeDir`, free fixed ports (`Validate` rejects `:0`,
   `go/internal/stack/config.go:64-72`), private-postgres DSN — and calls
   `stack.Up(ctx, cfg, deps) (*stack.Stack, error)` with real adapters.
3. Registers `Stack.Down` via `t.Cleanup` ("Down stops the stack's children in
   reverse start order and releases the lock", `stack.go:141-151`), so a
   t.Fatal still drains children.
4. Dials the loopback TLS door with the stack's own state-dir anchor
   (`Config.StateDir` holds `tls.crt`/`tls.key`, `config.go:17-19`) and the
   bootstrap-admin token the network door writes 0600 to
   `<StateDir>/admin-token` on startup (`go/server/network_door.go:36-39`
   `adminTokenFile = "admin-token"`; write-before-serve ordering per
   `go/server/serve.go:442-444`), yielding authenticated generated Connect
   clients for `CompassService` + `CommsService`.

**Real agent image, not alpine.** The capstone's `Config.AgentImage` is
`compass-agent:latest` built+loaded into containers-storage by the dogfood-loop
task (`devenv.nix:349-354` `dogfood:agent-image`, opt-in). Because
`EnsureImage` unconditionally `podman pull`s — "no pre-existence check is done
by deliberate choice — the pull IS the ensure" (`adapters/image.go:52-65`) —
and a containers-storage-local image is not pullable, this gap has TWO halves.
The LOCAL half is small and owned here: the harness either (a) pre-loads the
image and satisfies the ensure with a `containers-storage:` ref podman can
resolve, or (b) grows a present-check in the image adapter — scoped as task
H1's red case, with the adapter change preferred (mirror of the podman test's
documented avoidance, `integration_podman_test.go:30-39`). The CI half is NOT
small: the image is a large self-contained NixOS closure built only by that
opt-in task, with no publish/cache pipeline to put it on a CI runner — that
distribution story is the GHCR publish pipeline (SEA-1690), folded into
Decision D2, not an adapter tweak.

### A2 — Scenario-authoring API: composable primitives

The core exposes primitives as methods on the fixture; scenarios are ordered
compositions of primitives plus event-gated assertions. First-class, so a new
scenario is a new function, never new plumbing. The primitives and their
grounded wire contracts:

| Primitive | Wire contract |
| --- | --- |
| `CreateAgent(handle, displayName)` | `CommsService.CreateAgent{handle, display_name, parent_agent_id}` → `{account}` (`proto/compass/v1/comms.proto:526-542`); find-or-create by handle via `ListAccounts` (CreateAgent has no idempotency key) |
| `Provision(accountID, reqID)` | `CompassService.ProvisionAgentWorkspace{agent_account_id, client_request_id, persona}` → `{container_name}` — repo carriage REMOVED (SEA-1527): "spawn/provision no longer clone a repo … the agent self-clones whatever it needs after launch"; tags 2-4 (`remote_url`/`local_path`/`ref`) are reserved (`proto/compass/v1/compass.proto:429-459`). Stable `client_request_id` so a timeout-retry dedups (`:442-447`) |
| `StartSession(container, prompt, resumeID)` | `CompassService.StartAgentSession{container_name, initial_prompt, resume_session_id}` → `{session_id}` (`compass.proto:480-500`); `resume_session_id` set ⇒ the server "reconstructs the stored transcript into a session-JSONL body the Runner materializes into the new container" (`:488-493`) |
| `PostMessage(channel, blocks, reqID)` | `CommsService.PostMessage{channel_id, blocks, topic, client_request_id}` (`comms.proto:663-689`); an `@handle` token inside a text block is the mention surface (`go/internal/delivery/consumer.go:299-308` `mentionRE`) |
| `SubscribeComms()` | `CommsService.SubscribeComms` streaming (`comms.proto:109`) — the event-gated wait source (Replay-then-Live, the `waitMessagePosted` shape, `integration_pgtest_test.go:434-449`) |
| `AwaitSessionSettled(sessionID)` | the leg-2/leg-5 settle wait — the ONE primitive with no grounded wire contract today ("turn settled" has no pinned signal; confirmed with the compass-agent owner during H3). Candidate sources: the PG hot-tail transcript append (`agent_transcripts.go:153`) and — likely better than polling the store — `CompassService.SubscribeAgentSession(SubscribeAgentSessionRequest{session_id}) → stream AgentSessionFrame`, the typed per-session trace stream (`proto/compass/v1/compass.proto:74,413-414`), whose lifecycle frames are a push-based settle source |
| `RemoveWorkspace(container, reqID)` | `CompassService.RemoveAgentWorkspace{container_name, client_request_id}` — "symmetric with ProvisionAgentWorkspace … same idempotency contract" (`compass.proto:466-476`) |
| Store-side asserts | direct `store` reads over the private-postgres DSN: `SessionTranscript(ctx, sessionID)` (`go/internal/store/agent_transcripts.go:215`), `AgentOwner`, message rows — the harness owns the DSN, so the store of record is directly assertable |

The spawn primitive (leg 3) is NOT a harness-called RPC: `AgentGateway.
Lifecycle{Spawn: SpawnPeerRequest{handle, display_name, initial_prompt,
client_request_id}}` is an agent→Runner call over the per-container socket
(`proto/compass/v1/agent_gateway.proto:50-56,111-128`). In the real stack the
AGENT initiates it — so the deterministic mode's canned turn must include a
spawn action (A4), and the harness asserts the RESULT (a fresh peer account
owned by the spawner's owner + a second container), the same properties
`TestSpawnDespawnOverTheWire` pins in-process
(`go/server/lifecycle_e2e_pgtest_test.go:96-160`).

Spawn-leg observability: the harness must be able to tell "the model never
emitted the spawn tool-call" apart from "the tool-call ran and failed in-band"
(a `LifecycleCallError` comes back in-band, never as a transport teardown —
`packages/compass-agent/src/lifecycle.ts:17-21`). No tool-call-introspection
assert is scoped; the distinguishing signal is the session trace itself — the
transcript hot-tail (or the same `SubscribeAgentSession` stream above) carries
the tool-call and its in-band result, and carries neither when the model never
called the tool. H4's red-case diagnosis names which of the two it saw.

### A3 — Fidelity axis: backend-only tier now, UI-inclusive tier gated

**Backend-only (headless).** Everything above: real
server+runner+agent-container, driven via generated Connect clients, asserted
via bus + store. This tier is the per-PR gate and carries the whole 5-leg
scenario set.

**UI-inclusive.** Drives a human-shaped path through the real UI client: post
a message from the client, read a thread reply rendered through the UI. The
UI today is a browser SolidJS app that dials a door from
`VITE_COMPASS_BASE_URL` + `VITE_COMPASS_CALLER_ID` (`apps/ui/src/boot.ts:1-17`
"resolving the live connection from the Vite env";
`apps/ui/.env.development:23`), and the native shell (Wails v3, product record
`docs/designs/product/compass-native-app/design.md`) adds an embedded mode
that itself supervises the stack via the SAME `go/internal/stack` (its §A3:
"the Wails v3 shell … spawns and monitors ONE" stack) — so the harness core
and the native app share the bring-up seam by construction. HOW a test drives
the UI (headless browser against the Vite app pointed at the harness's door
vs. driving the Wails shell vs. the native-client connect path) is genuinely
unsettled and cross-lane (compass-ui / compass-native own those surfaces) —
parked as OQ3, with the UI tier scoped as a follow-on task (H7) gated on that
coordination. The load-bearing design property locked HERE: the UI tier
consumes the same harness core (the stack fixture + its door URL + admin
token) — a UI-tier scenario points a real client at the harness's door rather
than standing up a second stack shape.

### A4 — Model axis: the model-backend seam (Decision D1)

**Decision (Matt, relayed 2026-08-05; re-ratified at the design-PR gate):
BOTH modes over ONE shared harness with a model-backend seam.** (1) A
deterministic full-stack test — real server/runner/agent-container/comms/
spawn/resume with a canned/stub LLM — gates every PR in CI: the regression
base. (2) A live-model smoke — the SAME harness with only the model backend
swapped for a real agent turn — runs on-demand/nightly, never per-PR
(nondeterministic, keys + cost). Deterministic-first with live as a
fast-follow is explicitly acceptable.

The seam is shaped for this at the RUNNER level: the runner forwards a model
selector — `--agent-model` → `COMPASS_MODEL` env on the agent exec
(`go/cmd/compass-runner/main.go:50-53`;
`go/internal/runner/agent_exec.go:80`) — and the agent entrypoint treats it as
"an opaque pattern string for `createAgentSession` to resolve against its own
model registry — the entrypoint deliberately does not parse provider/id
itself, so adding a provider never touches this file"
(`packages/compass-agent/src/cli.ts:122-136` `resolveModelSelector`). But the
STACK does not forward it yet: `runnerSpec` passes only
`--runner-id/--server/--ca/--image/--runtime-dir`
(`go/internal/stack/spec.go:39-47`), and `stack.Config` has no model or
egress field (`go/internal/stack/config.go:16-43`) — so a harness (or the
native shell) driving the stack today can set neither. The model selector
COULD ride parent-env inheritance (`AgentModel: orEnv(*agentModel,
"COMPASS_AGENT_MODEL")`, `go/cmd/compass-runner/main.go:156`; the
ProcessSupervisor appends `os.Environ()` to every child's env,
`go/internal/stack/adapters/process.go:64`), but live mode's egress allowlist
CANNOT — `parseEgress(*egressHosts)` reads the flag only, no env fallback
(`main.go:115`) — so extending `stack.Config{AgentModel, EgressAllow}` +
`runnerSpec` is REQUIRED for live mode, not a nicety. H1 owns that
extension. So:

- **Deterministic mode** = a stub/canned model provider registered in the
  agent's model registry, selected by a `COMPASS_MODEL` value the harness
  sets (via H1's `stack.Config.AgentModel` → `runnerSpec` extension, or
  interim via the `COMPASS_AGENT_MODEL` parent-env path above). The canned
  script must
  produce the turn shapes the scenarios assert on: a settled text turn
  (leg 2), a spawn tool-call (leg 3), a post/@mention (leg 4). The provider
  is only ONE of three compass-agent-lane gaps: the entrypoint also does not
  REGISTER the native lifecycle/comms tools yet (`createAgentSession`
  receives `customTools: mcp.tools` only,
  `packages/compass-agent/src/cli.ts:608,633`; `createLifecycleTools` is
  verbatim "NOT YET WIRED",
  `packages/compass-agent/src/lifecycle.ts:137-141`), and all three native
  tools carry `approval: "write"` with no headless approval mode wired
  (`lifecycle.ts:146,191`; `comms.ts:212`). All three gaps are scoped as
  task H3 — the full agent-lane contract, coordinated with the compass-agent
  owner; the seam itself — an env-selected registry entry — is the settled
  design.
- **Live mode** = the same scenarios with `COMPASS_MODEL` pointing at the
  LiteLLM proxy (per the sibling record's D2: LiteLLM, key via user
  `SetSecret` KIND_PROVIDER), the provider credential materialized as the
  0600 `$HOME/.compass/auth-seed.json` (`packages/compass-agent/src/cli.ts:13-14`),
  and the LiteLLM host added to the runner's `--egress-allow` — an empty host
  set is pure default-deny (`go/internal/runtime/egress.go:29-31`), so
  without it the agent's own firewall drops the provider call. Because
  `--egress-allow` has no env fallback, live mode is hard-gated on H1's
  `stack.Config.EgressAllow` extension (above).

Mode is a harness configuration knob (one field on the fixture options), not a
second suite: identical scenario code, different model backend + gating.

### A5 — The first scenario set: the five-leg capstone

One ordered scenario (state flows leg to leg, the
`TestSpawnDespawnOverTheWire` shared-wire shape,
`lifecycle_e2e_pgtest_test.go:96-101`):

1. **Bring-up.** `stack.Up` → Ready. Assertion: `Stack.Health` Ready
   (`stack.go:153-166`); admin token present 0600. (This is compass-stack's
   own proven gate, re-used as the scenario's precondition.)
2. **Agent + real turn.** `CreateAgent` → `Provision` → `StartSession(prompt)`
   — the dogfood-loop T5 driver sequence (sibling record §6/T5), reused as a
   harness primitive chain, not re-authored. Assertions, event-gated: the
   session's transcript lands in the PG hot-tail
   (`store.SessionTranscript(ctx, sessionID)` non-empty,
   `agent_transcripts.go:215`; entries appended via `AppendTranscriptEntry`
   `:153`), and the turn's settled frames arrive on the board/bus. "Turn
   completed" = the canned (or live) turn's final settled entry is present —
   the exact settle signal is confirmed with the compass-agent owner during H3
   (the dossier's outstanding DM); the harness surface for it is the
   `AwaitSessionSettled` primitive (A2), with `SubscribeAgentSession`'s typed
   frame stream the leading candidate source.
3. **Agent-driven spawn.** The driven turn issues `Lifecycle(Spawn)` from
   INSIDE the container (A2). Assertions: `SpawnPeerResponse` carries a fresh
   `agent_account_id` + `container_name`; the peer's owner is the SPAWNER'S
   owner, never the caller agent or admin (the F2 property,
   `lifecycle_e2e_pgtest_test.go:150-160`); a second real container exists
   (podman inspect by deterministic name, `spec.go:85`).
4. **Cross-agent messaging.** Post into a shared channel with an `@handle`
   mention of the peer. Assertions: `MessagePosted` fans on the comms bus
   (Replay-then-Live wait); the Message row is in the store; the mentioned
   peer's session receives a STEER dispatch while an unmentioned subscriber
   gets a plain DELIVER (`go/internal/delivery/mention_test.go:33,59` pin
   exactly this split; delivery trigger `go/internal/delivery/dispatch.go:14-31`).
5. **Persist + resume.** `RemoveWorkspace` (or stack-down of the container leg)
   → `Provision` fresh → `StartSession(resume_session_id=<leg-2 session>)`.
   Assertions: `ReconstructSessionBody` semantics hold — the resume body is
   rebuilt from the PG hot-tail, checkpoint-first, "a valid loadable session
   file BY CONSTRUCTION" (`go/internal/runnerhub/reconstruct.go:29-41,57`) —
   and the resumed container materializes the session file at the agent's
   resume path (the property `TestStartWithResumeBodyMaterializesSessionFile`
   pins at the host layer, `go/internal/runner/host_test.go:1087`); the
   resumed agent's first turn continues without a fresh-session reset.

New scenarios (multi-peer fan-out, despawn fail-closed at full fidelity,
config push, secrets) compose the same primitives — the scenario set grows
without touching the core.

### A6 — Teardown and idempotence

Two leak surfaces, each with an owner:

- **Stack children**: `Stack.Down` drains runner → server → postgres in
  reverse start order (`stack.go:141-151,278-281`); registered via `t.Cleanup`
  per fixture.
- **Agent containers**: NOTHING removes them on runner shutdown — "every
  container lives until the Runner process ends" refers to socket lifetime
  only (`go/internal/runner/host.go:203-210` `Close` tears down sockets), the
  podman container survives, container names are deterministic
  (`spec.go:85`), and the `client_request_id` dedup is in-memory — so a
  re-run's Provision hits a `podman create` name collision (the sibling
  record's T6 finding). The harness therefore does BOTH: a preflight
  `podman rm -f` of its own deterministic container names before Provision,
  and a `t.Cleanup` sweep of `compass-agent-*` containers it created plus the
  runtime socket dir. Exact-name removal only (process safety).

Idempotence gate: the whole scenario runs twice back-to-back green in one CI
job — the red case for any leaked state.

### A7 — Boundary with the dogfood-loop record (no duplication)

The sibling record (`docs/designs/platform/compass-dogfood-loop/design.md`)
owns the HUMAN dev-loop: devenv wiring (T1-T4, T6), the `compass drive` admin
driver CLI (T5, compass-server lane, **not yet built** — `go/cmd/` on main has
only compass-{gen-cert,mint-runner-token,postgres,runner,server,stack}), and a
shell-smoke over `devenv up` (T7). T5/T7 are single-agent, single-turn. This
record owns the AUTOMATED harness and is strictly broader: legs 3/4/5 (spawn,
cross-agent messaging, resume) exist only here. The leg-2 primitive chain IS
T5's driver sequence (CreateAgent → Provision → StartSession → tail) — this
harness implements that chain as library primitives against the same RPC
contracts T5 specs, so T5's CLI can later wrap the same primitives, but the
harness does NOT wait for T5's CLI to land (the RPC contracts are on main
now; the sequencing note is OQ4-adjacent, resolved in-plan: no dependency).
What leg 2 DOES depend on is a runnable `compass-agent:latest` — SEA-1359's
runtime activation (artifacts merged: `packages/compass-agent/src/cli.ts`,
`agent-image/`, `devenv.nix:281` `--image compass-agent:latest`; final
activation in progress) — flagged in H2's red case, not an open fork.

## Alternatives considered

- **Option B — `devenv up` (SEA-1360) + shell-script orchestration (the T7
  shape).** The dogfood loop's own mechanism: `processes.{compass-server,
  compass-runner}` + `services.postgres` with ordered start and a
  GetServerInfo readiness probe (`devenv.nix:166-290`), the real
  `compass-agent:latest` image via the opt-in `dogfood:agent-image` task
  (`devenv.nix:349-354`). Its genuine strength: it IS leg 1's shipped
  bring-up mechanism, and the sibling record's T7 smoke already rides it. It
  loses to C for an AUTOMATED, scenario-bearing harness: orchestration is
  process-compose/shell, a scenario-authoring API with typed assertions over
  store rows and bus subscriptions cannot be carried by a shell script
  (scripts-over-bash), a per-PR CI gate wants the `go test` lane, and the
  five-leg multi-agent scenario would fight the grain at every assertion.
  `compass-stack` exists precisely as the embeddable Go equivalent of this
  bring-up (`go/cmd/compass-stack/main.go:98-146` resolves the same config
  shape), so C loses nothing B has except the devenv process UI — which the
  human loop keeps via the sibling record. The genuine alternative Matt ruled
  against (Decision D3).
- **Option A — extend the in-process `e2eWire` pgtest suite.** Insufficient by
  construction: its engine is a stub (`e2eStubRuntime`,
  `go/server/lifecycle_e2e_pgtest_test.go:641` — "ExecStreaming spawns a real,
  terminatable child (a shell-stub `podman` exec-ing `sleep`)"), so the test
  process plays the agent over the gateway socket. No real agent image, no
  real container, no model turn — leg 2 is impossible, and legs 3/4/5 would
  prove the stub, not the deployable stack. It remains the right home for
  subsystem seam proofs (it already covers the spawn/despawn wire); the
  harness composes WITH it, not instead of it.
- **A single monolithic 5-leg test (the pre-steer framing).** Rejected by
  Matt's scope ruling: a one-off test leaves every future e2e scenario paying
  full bring-up plumbing again. The harness/scenario split makes the 5-leg
  set the first consumer, not the deliverable.
- **A second, separate UI harness with its own bring-up.** Rejected as the
  default: the native shell already supervises the stack through the same
  `go/internal/stack` seam (product record compass-native-app §A3), so a
  UI-tier scenario can point a real client at the harness core's door. Whether
  the UI tier is a build-tag/tier of one harness or a sibling package SHARING
  the core is parked (OQ3) — but a fork of the bring-up itself is off the
  table.

## Plan

Tasks are lane-tagged: `[harness]` = this record's net-new Go e2e package
(compass repo, `go/`); `[compass-agent]` = the agent-lane work inside the
agent image; `[ci]` = GitHub Actions wiring; `[ui]` = the
UI-inclusive tier (gated, cross-lane). Each task carries a real red-green
cycle (rule://red-green-testing). H1-H6 use harness shape C (Decision D3).

### H1 [harness] — harness core: bring-up fixture over `stack.Up`

New podman-tagged package standing up the real stack per Approach A1: build
child binaries, short-root config, `stack.Up`, `t.Cleanup(Down)`,
authenticated Connect clients over the TLS door using the state-dir anchor +
admin token. Includes the real-image path: `Config.AgentImage` resolves the
locally-built `compass-agent:latest` (adapter present-check or
`containers-storage:` ref — implementer's choice, red case below). H1 ALSO
owns the model/egress plumbing gap (A4): extend `stack.Config` with
`AgentModel` and `EgressAllow` fields and teach `runnerSpec` to forward
`--agent-model`/`--egress-allow` — today it passes only
`--runner-id/--server/--ca/--image/--runtime-dir`
(`go/internal/stack/spec.go:39-47`), and egress has no env fallback, so live
mode is unreachable without this. Cross-lane: the compass-native shell
supervises via the SAME `go/internal/stack` (A3), so this Config surface is
shared — the compass-stack and compass-native owners sign off on the field
shape before it lands.

Interfaces:

- Consumes: `stack.Up(ctx context.Context, cfg stack.Config, deps stack.Deps)
  (*stack.Stack, error)` / `(*Stack).Down(ctx) error` / `(*Stack).Health(ctx)
  (Status, error)` (`go/internal/stack/stack.go:62,145,156`);
  `stack.Config{StateDir, SocketPath, ListenAddr, DatabaseDSN, AgentImage,
  RuntimeDir, Linger}` + `Validate` sun_path budget
  (`go/internal/stack/config.go:16-43,63-83`); the podman test's
  `buildBinariesFromModuleRoot`/`freePorts`/`shortRoot` patterns
  (`go/cmd/compass-stack/integration_podman_test.go:88-203`); admin token at
  `<StateDir>/admin-token` 0600 (`go/server/network_door.go:36-39`).
- Produces: a `Fixture` with `Compass()`/`Comms()` authenticated Connect
  clients, the private-postgres DSN for store-side asserts, and the stack
  handle — the substrate every scenario and both fidelity tiers consume —
  plus the extended `stack.Config{AgentModel, EgressAllow}` + `runnerSpec`
  forwarding of `--agent-model`/`--egress-allow`: a shared seam consumed by
  this harness AND the compass-native shell's embedded supervisor
  (cross-lane Interfaces note for the compass-stack + compass-native owners).

Test cycle: red — no such package on main; additionally `EnsureImage`'s
unconditional `podman pull` (`go/internal/stack/adapters/image.go:52-65`)
fails on the local-only `compass-agent:latest`, and `stack.Config` cannot
carry a model selector or egress allowlist (`config.go:16-43`). Green —
fixture reaches Ready with the REAL agent image configured, both clients
answer an authenticated RPC, a configured `AgentModel`/`EgressAllow` reaches
the runner's flags, and `Down` leaves no child processes.

As-built delta (SEA-1785, #181): H1 shipped a THIRD additive `stack.Config`
field beyond `{AgentModel, EgressAllow}` — `CheckoutDir string`
(`go/internal/stack/config.go:52-59`), forwarded conditionally as
`--checkout-dir` (`spec.go:67-71`, empty value omits the flag, same
zero-value-omit pattern as the other two). The runner's `--checkout-dir`
defaults to `/workspace` with no env fallback (`cmd/compass-runner/main.go`),
but the real `compass-agent:latest` image ships `/workspace` non-writable
(only `$HOME` is agent-owned), so Provision fails with a `mkdir … permission
denied` unless the checkout dir is agent-writable. The fixture sets
`CheckoutDir: "/home/agent/repo"` (`go/e2e/fixture.go:118`,
matching `config_delivery_e2e_test.go`'s precedent). Every leg Provisions, so
this field is load-bearing for H2/H4/H5/H6 too — a scenario that stands up its
own `stack.Config` must set it or Provision fails. Same shared-seam cross-lane
note as `{AgentModel, EgressAllow}`: compass-stack + compass-native consume the
same `stack.Config`.

### H2 [harness] — leg-2 primitives + real-turn scenario

The `CreateAgent → Provision → StartSession` primitive chain (A2 table) and
the leg-2 scenario asserting a completed turn.

Interfaces:

- Consumes: `CommsService.CreateAgent{handle, display_name} → {account}`
  (`proto/compass/v1/comms.proto:526-542`);
  `CompassService.ProvisionAgentWorkspace{agent_account_id,
  client_request_id, persona} → {container_name}`
  (`proto/compass/v1/compass.proto:429-459`);
  `CompassService.StartAgentSession{container_name, initial_prompt} →
  {session_id}` (`compass.proto:480-500`); transcript reads via
  `(*store.Store).SessionTranscript(ctx, sessionID) ([]TranscriptEntryRow,
  error)` (`go/internal/store/agent_transcripts.go:215`).
- Produces: fixture primitives `CreateAgent`/`Provision`/`StartSession` and a
  green "real container runs a turn; transcript persisted to the PG hot-tail"
  scenario — the first time Provision→Start→turn is driven through the real
  stack (compass-stack's own test stops at enrollment,
  `integration_podman_test.go:20-28`).

Test cycle: red — running the scenario against H1's fixture with today's
stack: the turn cannot complete without H3's model backend (and SEA-1359's
final activation); the transcript assert stays empty. Green — with H3 landed,
the scenario passes; the settle signal is the one confirmed with the
compass-agent owner (H3 coordination).

### H3 [compass-agent] — the full agent-lane contract: tool registration, deterministic provider, headless approval

Not "a canned model provider" alone: THREE gaps in the compass-agent lane,
all of which must close before legs 3/4 are buildable —

1. **Native tool REGISTRATION in the entrypoint.** `createAgentSession`
   today receives `customTools: mcp.tools` — MCP tools ONLY
   (`packages/compass-agent/src/cli.ts:608,633`); the native
   `agents_spawn_peer`/`agents_despawn_peer`/`comms_post_message` tools are
   never passed. `createLifecycleTools` says so verbatim: "NOT YET WIRED:
   there is no container entrypoint in this repo, so this has no non-test
   caller. The registration leg is tracked separately (index.ts:13 is the
   seam, beside createCommsTools)"
   (`packages/compass-agent/src/lifecycle.ts:137-141`; `comms.ts:204-206`
   carries the same notice). A canned model emitting `agents_spawn_peer`
   today gets "unknown tool". This is tracked as **SEA-1741** (child of
   SEA-1359), and is pure entrypoint wiring: the transport seam already
   carries both arms (`transport/index.ts:57-58` `comms()`/`lifecycle()`,
   socket-wired `:103-104`), and natives registered at construction survive a
   later `config.tools` control via `agent.ts` `#withNatives` (DL-028;
   SEA-1532) — so no new transport or config work, only constructing the
   brokers in `cli.ts main()` and adding the returned tools alongside
   `customTools: mcp.tools`.
2. **The canned/scripted provider MECHANISM.** A deterministic provider
   selectable via `COMPASS_MODEL`, packaged in the agent image, whose
   scripted turns produce the shapes the scenario set needs: a settled text
   reply (leg 2), a spawn tool-call (leg 3), a post with an `@handle`
   mention (leg 4), a resumable continuation (leg 5). The confirmed
   mechanism (compass-agent owner, 2026-08-05): a `models.yml` custom entry
   whose `baseUrl` points at a stub OpenAI-compatible server the harness
   stands up, returning the canned tool-calls per turn — a test-fixture
   concern, explicitly NOT part of SEA-1741. The injection point is the
   existing opaque-selector seam — no new selector wiring.
3. **Headless APPROVAL semantics.** All three native tools carry
   `approval: "write"` (`lifecycle.ts:146,191`; `comms.ts:212`) and neither
   cli.ts nor agent.ts wires any approval mode. Confirmed (compass-agent
   owner, 2026-08-05): the container runs headless with write-approval tools
   auto-executing (yolo default) — there is no human to approve. This task
   pins that approval policy in the entrypoint.

**RECOMMENDED: fold gap (1) — tool registration (SEA-1741) — and gap (3) —
the headless approval policy — into SEA-1359's final runtime activation (its
own recommended arm, OQ4): ONE image change, not separate respins.** Gap (2),
the canned provider, is a harness-side test fixture (stub `baseUrl`) that
rides H1/H2, not the agent image. H4's green condition depends on all three
landing; without them it is unreachable.

Interfaces:

- Consumes: `--agent-model` → `COMPASS_MODEL`
  (`go/cmd/compass-runner/main.go:50-53`;
  `go/internal/runner/agent_exec.go:80` `spec.Env["COMPASS_MODEL"]`);
  `resolveModelSelector(env) string|undefined` feeding
  `createAgentSession`'s model registry
  (`packages/compass-agent/src/cli.ts:122-136`); the registration seam
  beside `createCommsTools` (`index.ts:13` per the lifecycle.ts docstring)
  and `createAgentSession`'s `customTools` option (`cli.ts:608-633`).
- Produces: a registry-resolvable deterministic provider (exact packaging —
  registry entry vs. local OpenAI-compatible endpoint baked into the image —
  is the compass-agent owner's call, coordinated at implementation) + the
  canned scripts for the four turn shapes + the entrypoint registering the
  native lifecycle+comms tools alongside the MCP tools + a pinned headless
  approval policy for write-approval tools; documented selector value the
  harness sets.

Test cycle: red — `COMPASS_MODEL=<canned>` today resolves nothing and the
session falls back to the SDK default (`cli.ts:128-129`), and a scripted
spawn tool-call gets "unknown tool" (native tools unregistered,
`cli.ts:633`). Green — the agent in a real container completes the scripted
turn, its registered native tools execute headlessly under the pinned
approval policy, with zero network egress (deterministic mode keeps
`--egress-allow` empty — default-deny, `go/internal/runtime/egress.go:29-31`
— which doubles as proof no live model is reachable).

### H4 [harness] — legs 3+4: spawn + cross-agent messaging scenario

Leg 3: the canned turn issues `Lifecycle(Spawn)`; assert fresh peer account
with F2 ownership + second real container. Leg 4: post + `@mention`; assert
bus fan, store row, steer-vs-deliver split.

Interfaces:

- Consumes: `AgentGateway.Lifecycle{Spawn: SpawnPeerRequest} →
  {SpawnPeerResponse{agent_account_id, container_name}}`
  (`proto/compass/v1/agent_gateway.proto:50-56,111-128`) — issued BY the
  agent, asserted by the harness; `CommsService.PostMessage{channel_id,
  blocks, client_request_id}` (`comms.proto:663-689`);
  `CommsService.SubscribeComms` Replay-then-Live waits
  (`go/internal/runnerhub/integration_pgtest_test.go:434-449`); mention
  routing contract: mentioned member → steer, unmentioned subscriber →
  deliver (`go/internal/delivery/mention_test.go:33,59`; `mentionRE`
  `consumer.go:299-308`).
- Produces: `PostMessage`/`SubscribeComms`/`AwaitDelivery` primitives + the
  legs-3/4 scenario green over the real stack.

Test cycle: red — the in-process suite proves these seams with a stub engine
only; over the real stack the composed path is unexercised — and UNREACHABLE
until H3 lands in full: without registration (H3.1) the canned spawn call is
"unknown tool", and without a headless approval policy (H3.3) a
write-approval tool may never execute. Green — gated on H3 (1)+(2)+(3): one
ordered run: spawn observed, second container inspected by exact name
(`go/internal/runner/spec.go:85`), steer reaches the mentioned peer's real
session, deliver reaches the unmentioned one. Red-case diagnosis reads the
session trace (transcript hot-tail / `SubscribeAgentSession`) to tell "model
never emitted the tool-call" from "tool-call failed in-band" (A2 spawn-leg
note).

### H5 [harness] — leg 5: persist + resume scenario

Tear the leg-2 agent's container down (`RemoveAgentWorkspace`), re-provision,
start with `resume_session_id`, assert continuation from durable state.

Interfaces:

- Consumes: `CompassService.RemoveAgentWorkspace{container_name,
  client_request_id}` (`proto/compass/v1/compass.proto:466-476`);
  `StartAgentSessionRequest.resume_session_id` (`compass.proto:488-493`);
  reconstruction semantics `(*Hub).ReconstructSessionBody(ctx, sessionID)
  ([]byte, error)` — PG hot-tail, checkpoint-first, loadable by construction
  (`go/internal/runnerhub/reconstruct.go:29-41,57`); host-layer
  materialization property (`go/internal/runner/host_test.go:1087`
  `TestStartWithResumeBodyMaterializesSessionFile`).
- Produces: `Remove`/`Resume` primitives + the leg-5 scenario: a NEW container
  resumes the leg-2 session and its first resumed turn sees prior transcript
  state (canned script asserts on carried context deterministically).

Test cycle: red — resume across a REAL container boundary is unexercised on
main (host_test pins it with fakes). Green — the resumed session's transcript
continues the same `session_id` lineage and the resumed turn completes.

### H6 [harness] — teardown + idempotence

Preflight exact-name `podman rm -f` + cleanup sweep per Approach A6; the
double-run gate.

Interfaces:

- Consumes: deterministic container names (`NamePrefix + accountID`,
  `go/internal/runner/spec.go:85`); socket-only runner shutdown semantics
  (`go/internal/runner/host.go:203-210`); `Stack.Down` reverse-order drain
  (`go/internal/stack/stack.go:141-151`).
- Produces: a harness whose full scenario set runs twice back-to-back green
  in one job; no leaked containers, processes, or short-root state.

Test cycle: red — two consecutive runs without cleanup: the second Provision
hits the podman name collision (in-memory `client_request_id` dedup is gone
after restart — the sibling record's T6 finding). Green — double-run passes.

### H7 [ui] — UI-inclusive tier (gated follow-on; cross-lane)

A scenario that drives a human-shaped path through the real UI client against
the H1 fixture's door: post from the client, observe the agent's reply
rendered. Gated on OQ3 (drive shape) and coordination with compass-ui /
compass-native; lands as a fast-follow unless Matt rules it into the first
increment (OQ3).

Interfaces:

- Consumes: the H1 fixture's door URL + admin/caller identity; the UI's env
  connection contract `VITE_COMPASS_BASE_URL` + `VITE_COMPASS_CALLER_ID`
  (`apps/ui/src/boot.ts:1-17`; `apps/ui/.env.development:23`); the native
  shell's stack seam (product record compass-native-app §A3 — same
  `go/internal/stack`).
- Produces: one UI-tier scenario proving the human-shaped path end to end;
  the tier mechanism (headless browser vs Wails shell) per the OQ3 ruling.

Test cycle: red — no automated path drives the UI against a live stack today
(UI tests run against fakes, `apps/ui/src/live/comms-fake.ts`). Green — the
UI-tier scenario passes against the harness fixture.

### H8 [ci] — GitHub Actions wiring and gating (Decision D2)

Wire the deterministic backend-only tier into the repo's ACTUAL CI — GitHub
Actions (`.github/workflows/ci.yml`; the repo has no Woodpecker config, so any
Woodpecker migration is out of scope for this record). Per Decision D2 the
Dogfood end state — which this task now implements — is a required per-PR check
running the full-stack deterministic tier on an ORDINARY arbitrary-uid
ubuntu-latest runner. Two once-blocking prerequisites have landed: SEA-1690
published `compass-agent` to GHCR, and the uid-1000 requirement was lifted by
the userns keep-id remap (`go/internal/runtime/podman.go`'s
`--userns=keep-id:uid=%d,gid=%d`, `:389`, floor-checked by
`VerifyUsernsRemapSupport`, `:415-438` / `go/cmd/compass-runner/main.go:97` —
`verifyRunnerUID` no longer exists), so the runner no longer has to be uid 1000
and no interim merge-queue/nightly staging is needed. The check runs on every
PR directly, never a skip-configured required check (which would pass vacuously
green).

The image the gate tests comes from one of two sources, decided per-PR by
whether the PR changes the image's inputs (`.github/workflows/ci.yml`'s
`compass-agent-image` moon `--affected` detection): an image-input-changing PR
builds+loads the image from THIS tree into local containers-storage (`nix run
path:../forks/devenv#devenv -- container copy agent` from `agent-image/`, the
ref the fixture resolves), so the gate proves the image the PR produces; any
other PR (and every push to main, where main's own publish already rebuilt it)
pulls the published `ghcr.io/rigelbuild/compass-agent:latest`. `:latest` is kept
mutable deliberately (Matt-ruled always-fresh), not digest-pinned. Either way
the fixture's `EnsureImage` present-checks local containers-storage and does not
pull at test time, so the seed step above is what satisfies it.

The e2e harness additionally needs a postgres toolchain on PATH: its private
postgres (`go/cmd/compass-postgres/main.go`) shells out to `initdb`/`postgres`/
`createdb` via `exec.LookPath`. In CI those binaries come from the devenv
`packages` list (`devenv.nix`), which gate-tools carries onto PATH; in the dev
shell they come from `services.postgres`. Without them on PATH the full-stack
bring-up cannot stand up its database and the suite fails rather than runs.

Interfaces:

- Consumes: `go test -tags podman ./go/e2e/...` (mirroring the pgtest step
  shape, `.github/workflows/ci.yml`'s Real-Postgres suites); H3's documented
  `COMPASS_MODEL` selector; the published `compass-agent:latest` image (SEA-1690)
  OR — on an image-input-changing PR — the image built+loaded from the tree via
  `container copy agent`; the postgres toolchain (`initdb`/`postgres`/`createdb`)
  carried onto PATH from the devenv `packages` list (`devenv.nix`); live-mode
  secrets (LiteLLM key) injected only in the nightly workflow, never per-PR.
- Produces: a required per-PR full-stack e2e check on an ordinary arbitrary-uid
  ubuntu-latest runner, seeding the agent image per-PR (built-from-tree when the
  PR changes image inputs, else pulled `:latest`); plus an on-demand/nightly
  live-mode job. Documented runner prereqs: Linux, rootless podman,
  subuid/subgid, and the postgres binaries on PATH (via devenv `packages` in CI,
  `services.postgres` in the dev shell).

Test cycle: red — no CI job compiles the podman-tagged suite (build tags keep
it out of `go test ./...`), so the deterministic tier never runs and the gate is
vacuously green; a skip-configured required check would pass vacuously the same
way. Green — the full-stack tier runs as the required per-PR check on
ubuntu-latest, seeding the agent image per-PR, with the assert-ran guard making
a podman-unavailable skip loud rather than silently green; the nightly live run
reports without gating.

## Tasks

- [ ] H1 [harness] harness core: bring-up fixture over `stack.Up`, real agent image, authenticated clients (red-green + clean-Down smoke)
- [ ] H2 [harness] leg-2 primitives (CreateAgent → Provision → StartSession) + real-turn scenario with transcript assert (red-green)
- [ ] H3 [compass-agent] full agent-lane contract: native tool registration in the entrypoint + deterministic provider behind the `COMPASS_MODEL` seam + headless approval policy; canned scripts for all four turn shapes (red-green + zero-egress proof; recommended folded into SEA-1359's activation)
- [ ] H4 [harness] legs 3+4 scenario: agent-driven spawn (F2 ownership + second container) and cross-agent @mention (steer/deliver split) (red-green; green gated on H3 1-3, i.e. SEA-1741 tool registration + the canned provider)
- [ ] H5 [harness] leg-5 scenario: remove → re-provision → resume across a real container boundary (red-green)
- [ ] H6 [harness] teardown + idempotence: exact-name preflight/cleanup; double-run gate (red-green via second-run provision)
- [ ] H7 [ui] UI-inclusive tier scenario — gated on OQ3 + compass-ui/compass-native coordination (fast-follow unless ruled otherwise)
- [ ] H8 [ci] GitHub Actions wiring (D2): a required per-PR full-stack e2e check (`go test -tags podman ./e2e/...`) on ordinary arbitrary-uid ubuntu-latest — SEA-1690 (GHCR image) + SEA-1691 (host-uid lift) both landed, so no interim staging; seeds the agent image per-PR (built-from-tree when the PR changes image inputs, else pull `:latest`); postgres toolchain on PATH via devenv `packages`; live tier on-demand/nightly

## Decisions

1. **D1 — Model axis (Decision, Matt, relayed 2026-08-05; re-ratified at the
   design-PR gate): BOTH modes, ONE shared harness, a model-backend seam.**
   A deterministic full-stack test (real server/runner/agent-container/comms/
   spawn/resume, canned/stub LLM) gates every PR in CI as the
   regression base; a live-model smoke is the SAME harness with only the
   model backend swapped, run on-demand/nightly, never per-PR
   (nondeterministic, keys + cost). Deterministic-first with live as a
   fast-follow is explicitly acceptable. Folded through Approach A4 and
   tasks H3/H8.
2. **D2 — Per-PR gate cadence (Decision, Matt, 2026-08-05).** The full-stack
   deterministic tier is the per-PR gate; the feasibility constraints in its way
   are FIXED, not worked around. Agent-image distribution rides SEA-1690
   (publish `compass-agent` to GHCR), which is needed regardless and becomes
   part of getting this e2e test into CI. The runner's original
   uid-1000-on-the-HOST requirement was a known limitation that "can't be
   required long term"; lifting it to arbitrary host uids (SEA-1691, the userns
   keep-id remap) lets the gate run on ordinary CI runners. Both once-blocking
   prerequisites have since landed, so the full-stack deterministic tier now
   runs as the required per-PR check directly on ordinary arbitrary-uid runners
   — no interim merge-queue/nightly staging. (The interim uid handling for
   embedded Dogfood remains the preflight-and-refuse of compass-native T4,
   SEA-1685.)
   Supersedes the drafted OQ5 arms: neither a bespoke uid-1000 runner ([A]) nor
   a permanent nightly-only fallback ([B]) — fix the constraints, gate per-PR.
   Folded through Global Constraints, Approach A1, and task H8.
   Later-record pointer (added by citation, 2026-08-18; the freeze rule adds,
   never rewrites): D2's no-secrets, deterministic per-PR cadence governs THIS
   dogfood tier and is unchanged. A separate secret-bearing forge live-oracle
   step joins the per-PR gate as its own decision (DL-210,
   [forge integration testing](../../product/compass-forge-integration-testing/design.md));
   it does not modify this tier.
3. **D3 — Harness shape (Decision, Matt, 2026-08-05): option C.** A
   `//go:build podman` Go test suite over the merged compass-stack bring-up
   (`stack.Up`/`stack.Stack`, `go/internal/stack`), not
   option B (`devenv up` + script orchestration). A scenario-authoring API with
   typed store/bus assertions and a `go test` per-PR gate are Go-native; a
   shell script cannot carry either. Option B remains leg 1's own shipped
   mechanism for the HUMAN dev loop (sibling record), recorded in
   §Alternatives. Folded through Approach (head) and the Plan.

## Open Questions

Batched for Matt at the design-PR gate; the record designs against the
recommended arm of each so no task is blocked on a ruling.

OQ numbering is preserved from the pre-ruling draft so cross-references stay
stable: OQ1/OQ2 were resolved into Decisions D1-D3 and OQ5 was superseded by
D2 (see §Decisions); OQ3/OQ4 remain open.

1. **OQ3 — UI-inclusive tier: drive shape and packaging.** How a test drives
   the real UI: (a) a headless browser driving the Vite app pointed at the
   harness door via `VITE_COMPASS_BASE_URL`/`VITE_COMPASS_CALLER_ID`
   (`apps/ui/src/boot.ts:1-17`), (b) driving the Wails v3 native shell
   (embedded or native-client mode, product record compass-native-app), or
   (c) the native-client connect path against the harness's door. Coupled:
   one harness with tiers vs multiple harnesses sharing the core —
   **recommended: a shared core with tiered harness packages** (a bring-up
   fork is off the table, §Alternatives). Also: does the UI tier land in the
   first increment or as a fast-follow (recommended: fast-follow, H7)?
   Cross-lane coordination point with compass-ui and compass-native; their
   owners should weigh in before the ruling. Sequencing (coordinator, not a
   fork): this is parked-by-dependency, not dropped — compass-ui #1075 (the UX
   foundation rewrite) is itself parked pending a brand/product-site freeze, and
   the UI-shell shape OQ3 forks on (Wails native vs headless Vite vs
   native-client) is exactly what #1075 settles. So OQ3 is a recommended
   fast-follow (H7) off the capstone critical path; rule it once #1075 unparks
   and the shell is finalized, not against a moving target.
2. **OQ4 — Leg-2 activation dependency (SEA-1359).** Leg 2 needs a runnable
   `compass-agent:latest` doing a real (canned or live) turn. The artifacts
   are on main (`packages/compass-agent/src/cli.ts`, `agent-image/`,
   `devenv.nix:281` runner `--image compass-agent:latest`) but SEA-1359's
   final runtime activation is In Progress. Is the capstone's H2/H3 sequenced
   strictly after SEA-1359 closes, or may H3's deterministic backend land as
   part of the activation itself (one image change instead of two)?
   **Recommended: coordinate H3 into the SEA-1359 owner's lane as a single
   image change.** Load-bearing for sequencing only — the design is identical
   either way.

Non-load-bearing deferrals (explicitly NOT open questions): the harness
package's final name/location under `go/`; the exact canned-provider
packaging inside the agent image (H3, compass-agent owner's call within the
settled `COMPASS_MODEL` seam); the exact turn-settle signal the leg-2 assert
waits on (confirmed with the compass-agent owner during H3 — candidates are
the PG hot-tail transcript append, the session-status transition, and the
`SubscribeAgentSession` frame stream, `compass.proto:74` — surfaced as the
`AwaitSessionSettled` primitive either way); whether `EnsureImage` grows a
present-check vs the harness using a resolvable `containers-storage:` ref
(H1, implementer's choice — the CI-distribution half of the image story is
Decision D2 via SEA-1690, not deferrable).
