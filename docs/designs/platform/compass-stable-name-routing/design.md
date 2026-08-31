# Compass stable-name provider routing (RIG-2845)

Ledger-impact: none (platform surface is ungoverned by the design ledger — no
DECISIONS.md delta).

Status: draft for freeze (red-teamed + folded; narrowed to stable-name
provider routing only — model SELECTION is owned by the frozen RIG-2936
profile record + OMP's built-in `modelRoles`; Matt's freeze-gate rulings
on OQ-1/OQ-2/OQ-3/OQ-4/OQ-5 are all folded — the stable-name registry
is server-side operator config in a versioned Server store behind an
operator-scoped `compass.v1` write RPC, a scope-driven refinement of the
earlier VC'd-content ruling now that the record is routing-only — the
frozen RIG-2936 delivery boundary holds, and the container-listing /
bundle-hash sub-seam (OQ-2) is ruled belt-and-suspenders (gateway
discovery + static cold-boot seed, composed-hash option A): NO
load-bearing open forks remain — the record is freeze-ready).
Composes with the frozen RIG-1715 gateway record
(`docs/designs/platform/compass-server-llm-gateway/design.md`), the frozen
RIG-2936 per-Manager profile record
(`docs/designs/agent/compass-per-agent-overrides/design.md`), the
config-delivery record
(`docs/designs/agent/compass-agent-config-delivery/design.md`, DL-078),
and the config-passthrough record
(`docs/designs/agent/compass-agent-config-passthrough/design.md`);
consumes the RIG-2562 model evaluations (in design in the internal
fleet-tooling repo) as evidence, not as a blocker.

## Problem / Intent

Compass agents today receive their model as a hard-coded concrete
selector: the Runner exports one opaque `COMPASS_MODEL` string per agent
carrying a `provider/id` pair, hard-coupling model choice to a specific
provider the user must hold. There is no stable model vocabulary and no
server-side routing of a name to the backends a caller's credential pool
actually holds. This record defines the layer the gateway record
explicitly scoped out (`compass-server-llm-gateway/design.md:407-413`):
stable Compass model names (e.g. `claude-opus-4-8`) that resolve to a
preferred backend ORDER across the providers a user holds — decoupling
which-model from which-backend-serves-it (credential availability). Model
SELECTION — which model each agent uses — is out of scope: a profile's
`models.manager`/`models.agents` fields carry it (RIG-2936's DL-284
model-stack axis, `compass-per-agent-overrides/design.md:1204-1211`), and
OMP's built-in `modelRoles` map carries the tier defaults. This record
supplies only the stable-name VOCABULARY those profile fields reference and
the upstream ROUTING of those names — narrowing the RIG-2845 scope the
frozen RIG-2936 DL-288 boundary named (taxonomy + routing,
`compass-per-agent-overrides/design.md:469-474`) to its routing half.

## Global Constraints

Inherited from the gateway record's Global Constraints
(`compass-server-llm-gateway/design.md:678-710`), which BIND this record:

- **All-Go server, one ratified Bun exception**: "the LLM gateway is the
  adopted OMP TS auth-gateway and stays TS/Bun for the medium term …
  compass-side gateway code (ingestion, UsageService, token minting, stack
  supervision) is Go under `go/internal/`"
  (`compass-server-llm-gateway/design.md:680-686`). The stable-name resolver
  this record adds is compass-side wiring INSIDE the already-ratified Bun
  gateway boot entrypoint — no new non-Go runtime.
- **Fork changes are seam-shaped and upstreamable**: "compass touches the
  gateway only at injection points (auth verifier, `AuthStorage`
  implementation, usage hook) — never the routing core"
  (`compass-server-llm-gateway/design.md:687-690`). This record adds one
  small seam widening — ratified by Matt at the freeze gate (OQ-1,
  Resolved decisions): the injected resolver boot option goes from
  `resolveModel: (modelId) => Model<Api> | undefined`
  (synchronous, tenant-blind — `forks/oh-my-pi/packages/ai/src/auth-gateway/
  server.ts:55,65`) to `(modelId, identity) => Promise<Model<Api> |
  undefined>`, the direct sibling of the RIG-1715 T3 `authorize(req) →
  identity` boot-option change
  (`compass-server-llm-gateway/design.md:270-281`). The identity the seam
  carries is PER-USER, not merely per-org/tenant: subscription/OAuth
  candidates are ToS-bound to the individual user and resolve against
  the caller's own `owner_user_id`-scoped credential rows; only shared
  org API-key candidates resolve at org scope
  (`compass-server-llm-gateway/design.md:324-329,375-395`). The widening
  stays at the injection point — the routing core is untouched — under
  the ruled v1 hard-down failover (OQ-3, Resolved decisions;
  pre-first-byte/outage failover is the filed v2 follow-up RIG-3029, the
  one path that WOULD touch the core). The earlier draft's "no seam
  change" claim was wrong: the tenant-blind sync signature cannot carry
  per-caller candidate selection, which needs the request identity and an
  async pool query.
- **Registry substrate is server-side operator config** (Matt's
  freeze-gate ruling on OQ-4, a scope-driven refinement of the earlier
  VC'd-content ruling now that the record is routing-only — Resolved
  decisions): the stable-name registry (name → ordered candidate chain +
  listing metadata) lives in a versioned Server store, a sibling in kind
  of the RIG-1715 `gateway_credentials` value store ("a monotonic
  `version` per row supplying the CAS substrate",
  `compass-server-llm-gateway/design.md:324-329`), authored via an
  operator-scoped `compass.v1` write RPC — the same operator-scoped
  pattern as CD-1's `PutAgentConfig` / `GetAgentConfigInfo` /
  `DeleteAgentConfig`
  (`compass-agent-config-delivery/design.md:173-178`) — and read by the
  gateway over the narrow stack-token RPC-to-Server channel its T2
  `AuthStorage` credential store establishes
  (`compass-server-llm-gateway/design.md:348-358`). `compass.v1` stays
  the sole UI↔server door
  (`compass-server-llm-gateway/design.md:691-694`) — an operator RPC on
  it is in-posture; the gateway is not a UI and reads via the
  stack-token channel instead. Agents never author config (RIG-2936
  Layer 1, reinforced by Layer 2, "The agent SELECTS; it never AUTHORS",
  `compass-per-agent-overrides/design.md:494-501`) — the write RPC is
  operator-scoped only; a narrow `compass.v1` read RPC serves the UI
  display surface (OQ-5, Resolved decisions).
- **Agents never hold upstream provider credentials** post-T5
  (`compass-server-llm-gateway/design.md:695-696`); the stable-name
  surface therefore NEVER materializes provider keys agent-side — only
  model selectors.
- **Don't fork the SDK settings schema**: the passthrough record's ruling —
  a curated Compass-owned settings schema "creates a **second schema** that
  must chase the SDK's `SETTINGS_SCHEMA` (5k+ lines,
  `settings-schema.ts:383-5450`) on every fork bump — a permanent maintenance
  tax" (`compass-agent-config-passthrough/design.md:593-596`). The
  stable-name registry is Compass-owned schema in a Compass-owned Server
  store; the profile fields that reference it render through RIG-2936's
  existing seams (the session `modelPattern`, `task.agentModelOverrides` —
  the RIG-2936 render), never a parallel settings schema.
- Markdownlint-clean record; Conventional Commits;
  `Co-authored-by: Matt Wilkinson <matt@rigel.build>` on the record commit
  (driver-owned).
- Ledger: platform is ungoverned → `Ledger-impact: none`; no DECISIONS.md
  delta ships with this record.

## Approach

### (a) Stable model names over direct backend routing (the core fork)

**Decision: stable-name routing.** Compass exposes stable model names
(`claude-opus-4-8`, `gpt-5-5`, `gemini-3-1-pro`, …) as the ONLY vocabulary
profile model fields speak. Each stable name maps to an ordered
backend-candidate chain — e.g. for `claude-opus-4-8`:

1. `anthropic` via own subscription (OAuth),
2. `anthropic` via own API key,
3. alternates the user holds (`openrouter/anthropic/claude-opus-4.8`,
   `amazon-bedrock/...`), in a per-name declared order.

Rationale, grounded in the current routing:

- **The status quo hard-couples model choice to provider inventory.**
  Today `COMPASS_MODEL` carries a concrete `provider/id` selector (e.g.
  tests pin `"anthropic/claude-opus-4-5"`, `cli.test.ts:955`), so one
  model config only works for users holding exactly that provider. A
  stable name makes one model config valid across users holding different
  providers — the gateway record's own framing: "how a stable model name
  (e.g. `claude-opus-4-8`) maps to a preferred BACKEND order across the
  providers a user holds (own subscription, own API key, then alternates
  like OpenRouter or Bedrock)"
  (`compass-server-llm-gateway/design.md:408-410`).
- **The resolution point already exists and is caller-scoped.** Post-T5,
  every agent model call egresses through the gateway (`transport:
  "pi-native"` routes "every model under this provider … via the
  auth-gateway's `POST /v1/pi/stream` endpoint instead of the per-provider
  SDK", `models-config-schema-bundle.ts:284-287`), and the gateway resolves
  the request's `modelId` through the injected resolver boot option
  (`resolveModel`, `forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:55`,
  called on both foreign-wire and pi-native paths at `server.ts:377,569`). The
  compass boot entrypoint (gateway record T1) supplies this function — stable
  names are implemented by REPLACING that lookup with a resolver backed by
  the Server-held stable-name registry store (§(b); the gateway reads it
  over the stack-token RPC-to-Server channel — OQ-4, Resolved decisions).
  This is NOT zero-fork: the seam is widened to carry the caller identity
  and to be async (OQ-1, ratified — Global Constraints), because
  per-caller candidate selection needs both — but the widening stays at
  the injection point, not the routing core.
- **Per-caller resolution needs an identity, and only the gateway has
  one — at PER-USER granularity.** Backend order depends on which
  credentials THIS caller's pool holds (own-then-shared,
  `compass-server-llm-gateway/design.md:375-395`), and the own half is
  per-user by construction: subscription/OAuth credentials are ToS-bound
  to the individual user and live as `owner_user_id`-scoped rows, so the
  own-subscription and own-API-key candidates of a chain resolve per
  USER; only a shared org/company API key resolves at org scope
  (`compass-server-llm-gateway/design.md:324-329`). Own (per-user
  OAuth/subscription, then own key) before shared (org API key) —
  RIG-1715's stage-2 order verbatim. The agent-side registry can't know
  any of this post-T5 (agents hold only a gateway
  bearer); the gateway's per-agent token→caller-identity mapping
  (RIG-1715 T3) can.

**Composition with pool resolution (compose, don't fork).** Resolution is a
strict two-stage fold, each stage owned by the record that defined it:

```text
stable name ──(RIG-2845: per-name ordered candidate list)──▶ [(provider, upstreamModelId), …]
each candidate ──(RIG-1715: pool membership + precedence)──▶ credential | ∅
first candidate with a non-empty pool result wins
```

Stage 2 is RIG-1715's fold verbatim — own-before-shared across sources,
OAuth-before-API-key within a provider ("Own-before-shared … riding OMP's
existing within-provider type precedence unchanged — a deliberate OAuth/login
credential wins over a stored API key", `compass-server-llm-gateway/
design.md:383-387`). RIG-2845 never ACQUIRES or mints credentials and owns no
precedence — it only orders PROVIDERS; the own-sub-then-own-key prefix of
Matt's example order falls out of stage 2 automatically when the chain lists
the native provider first, and the chain's remaining entries order the
alternates (OpenRouter before Bedrock, etc.). It does, however, read one
credential-availability bit per candidate: a side-effect-free dry-run peek
("would this provider yield a usable credential for this caller") to choose
the candidate. That is a NEW read surface on the T2 pool resolver (named in
P1), distinct from the routing core's later stateful acquire
(`storage.getApiKey(model.provider, sessionId)`, `server.ts:419`); the peek
neither acquires nor sets session stickiness.

Failover trigger (ruled — OQ-3, Resolved decisions): a candidate is
skipped on credential absence and on marked usage-limit — both
observable in the dry-run peek before the upstream call (the gateway
already derives session identity and calls `markUsageLimitReached` on
gateway-mediated requests — coding-agent
`CHANGELOG.md:4507`). Matt's ruling is v1 hard-down on a provider
outage; a pre-first-byte upstream failure (connect error / immediate
5xx / 429-without-usage-limit) is NOT seam-shaped — the widened resolver
returns one model before the upstream call, so it would need a
routing-core loop or a compass-side wrapper — and is deferred to the
filed v2 follow-up RIG-3029, together with mid-stream error
failover. Stage-1
candidate choice is DETERMINISTIC (a pure function of the ordered chain plus
current pool marks), so absent a mark change the same candidate is chosen every
request and prompt-cache continuity holds without the seam carrying `sessionId`
(derived downstream at `server.ts:410`/`:578`).

Direct routing survives as an escape hatch: a `provider/id` selector that is
not a known stable name passes through to the existing exact-match lookup
unchanged, so tests' canned providers (`go/e2e/fixture.go:385-389`) and
power-user pins keep working.

### (b) Config surface: the server-side registry store, materialized through existing seams

The stable-name registry (name → ordered candidate chain + listing
metadata) is **server-side operator config**: a versioned Server store
authored via an operator-scoped `compass.v1` write RPC — Matt's
freeze-gate ruling on OQ-4, a scope-driven refinement of the earlier
VC'd-content ruling now that the record is routing-only (Resolved
decisions). Three layers:

1. **Authoring + publish (operator RPC → Server store).** The
   stable-name→candidate registry (name → ordered `(provider, model_id)`
   chain + listing metadata) is a versioned Server store (schema + write
   validation in P2), a sibling in kind of the RIG-1715
   `gateway_credentials` value store ("a monotonic `version` per row
   supplying the CAS substrate",
   `compass-server-llm-gateway/design.md:324-329`), written through an
   operator-scoped `compass.v1` RPC — the same operator-scoped pattern
   as CD-1's `PutAgentConfig` / `GetAgentConfigInfo` /
   `DeleteAgentConfig`
   (`compass-agent-config-delivery/design.md:173-178`) — editable via
   operator tooling, no config-repo PR + CI publish required. A
   profile's `models.manager`/`models.agents` fields (RIG-2936's frozen
   schema, `profiles/<name>/profile.yml` in the fleet config bundle,
   `compass-per-agent-overrides/design.md:177-218`) name models in this
   record's stable-name vocabulary. Agents never author any of it: "The
   agent SELECTS; it never AUTHORS" (RIG-2936 Layer 1, reinforced by
   Layer 2, `compass-per-agent-overrides/design.md:494-501`) — the write
   RPC is operator-scoped, exactly like CD-1's.
2. **Selection delivery (RIG-2936-owned — do not rebuild).** How a chosen
   profile's model fields reach a session is frozen in RIG-2936 T6:
   `models.manager` → the session `modelPattern` ("the profile is the sole
   model source for a Compass-provisioned session, superseding the
   Runner-global `COMPASS_MODEL`") and `models.agents` →
   `task.agentModelOverrides` on the settings overlay
   (`compass-per-agent-overrides/design.md:890-900`; DL-284). The boundary
   is explicit in DL-288: RIG-2936 "delivers per-Manager profile SELECTION +
   propagation; RIG-2845 owns the role taxonomy and routing policy the
   profile's model fields reference"
   (`compass-per-agent-overrides/design.md:469-474`) — of which this
   record, post-narrowing, retains only the routing half: delivery of the
   profile fields that REFERENCE this vocabulary is RIG-2936's, and model
   selection itself is the profile surface's; this record ships the
   stable-name VOCABULARY + registry + routing, never delivery.
3. **Gateway resolution (Bun entrypoint, compass-side).** The compass
   boot entrypoint's `resolveModel` implementation reads the registry
   from the Server over the same narrow stack-token transport + auth its
   T2 `AuthStorage` credential channel establishes ("RECOMMENDED:
   RPC-to-Server-for-creds … a narrow, stack-token-authenticated Server
   surface", `compass-server-llm-gateway/design.md:348-358`) — a narrow
   registry-read method alongside the credential reads, cached in-memory
   keyed on the registry `version` and re-read on version change — and
   the caller's pool to pick the winning candidate, returning a concrete
   `Model<Api>` to the untouched routing core. `listModels`
   (`server.ts:66-67`) lists the stable names so `/v1/models` shows the
   Compass vocabulary — and `/v1/models` is also the container's
   PRIMARY listing source: in gateway mode the container discovers the
   stable names from it live (the container-listing ruling — OQ-2,
   Resolved decisions; mechanics in P1).

This satisfies the schema-fork constraint by construction: this record
adds no settings-schema content of its own — the stable-name registry is
a Compass-owned Server store with a Compass-owned schema; any fleet
settings values that reference stable names ride the SDK's own keys
(e.g. `modelRoles`, `settings-schema.ts:564`) through the existing
RIG-2936/passthrough surfaces.

### (c) Shipped registry defaults + docs deliverable

This record owns the day-1 stable-name registry entries (SEEDED into the
Server registry store via the P2 operator write RPC) + a docs page
"recommended model per role per provider you hold"; the `default`
profile's own model selections are RIG-2936's default-profile content,
not this record's. The RIG-2562 model evaluations (per-role
external-first composite scoring, designed in the internal fleet-tooling
repo) supply the EVIDENCE that picks the recommended values. Until its
numbers land, recommendations are seeded from the fleet's current
practice (the same practice OMP's `priority.json:24-48` `slow` chain
encodes: codex-tier first, then opus-tier). Consuming RIG-2562 output is
a registry write via the same operator RPC — a data update, never a
schema or code change.

The fleet model-selection spec (RIG-2573/DL-025, `docs/specs/platform/
model-selection.md`) is **absent in this repo** — `docs/specs/` contains only
`product/` and `brand/` (verified by glob this session); it lives in the
internal fleet-tooling repo. Defaults MUST cite its optimization target
conceptually when the docs deliverable is written.

## Alternatives considered

### Direct backend routing (status quo) — rejected

Keep `COMPASS_MODEL` carrying concrete `provider/id` selectors and let each
agent's registry resolve them. Rejected: (a) one model config cannot serve
users with different provider inventories — the selector bakes in the
backend; (b) post-T5 the agent registry cannot even see which providers the
caller holds (agents carry only a gateway bearer,
`compass-server-llm-gateway/design.md:695-696`), so agent-side fallback
across backends is structurally impossible; (c) the OMP-side priority-chain
machinery (`priority.json`, `rolePriorityDefaults`) is pattern-matching over
locally-visible models — the wrong layer once visibility moved server-side.
Survives only as the pass-through escape hatch for unknown names.

### Stable names resolved agent-side (registry aliases) — rejected

Ship stable names as `models.yml` aliases via the passthrough CP-4 channel
(`ModelsConfigFile`, `models-config.ts:105`), with the container resolving the
backend. Rejected FOR RESOLUTION: fallback order depends on the caller's
credential pool, which post-T5 exists only gateway-side, and it would fork
alias semantics per container instead of one authoritative mapping. Note the
distinction the drafting missed: the stable name STILL must appear
agent-side as a LISTED registry entry (an id with `Api`/context/cost
metadata) or the container refuses to boot (the container-listing
ruling — OQ-2, Resolved decisions) — listing ≠ resolution. So the CP-4
channel carries the static stable-name seed LISTINGS (Server-generated
from the one authoritative registry store), the cold-boot fallback
beside live gateway `/v1/models` discovery, while resolution stays
gateway-side.

## Plan

Ordering: P2 → P1 → P4; P5 (docs/defaults) parallel after P2. P3 is
dissolved — model delivery is RIG-2936 T6's (frozen); its residue (the
stable-name vocabulary + the container-registry listing) is carried by
P1/P2.
P1 depends on RIG-1715 T2/T3 (pool resolver + tenant identity) having
landed; the resolver-seam widening (OQ-1) and the registry read path
(OQ-4) are ruled (Resolved decisions), as is the container listing
(OQ-2, Resolved decisions — gateway discovery primary + static
cold-boot seed fallback). The static seed must land with or before any
profile naming a stable name, so a profile-named stable name resolves
in-container even on a cold first boot.

### P1 — Gateway resolver over the Server registry store

Owner: the Bun gateway entrypoint (compass-side, within the ratified
exception); compass-server only for the stack-token registry read
surface (the store + operator write RPC are P2's).

The stable-name registry is a versioned Server store (schema + operator
write RPC in P2): per name, `display_name`, an ordered `candidates`
array of `{provider, model_id}`, and `metadata` carrying the listing
shape (context window, cost, `Api` type) taken from the primary
candidate — per the container-listing ruling (OQ-2, Resolved
decisions), since candidates map to different upstream models
with different windows. The registry is deliberately NOT a `models.yml`
member: `models.yml` is the SDK-schema'd registry the container consumes
(`ModelsConfigFile`, `models-config.ts:105`), and stapling Compass-owned
candidate-chain keys onto it would fork the SDK schema — the exact tax
the Global Constraints forbid. The container-facing static `models.yml`
stable-name seed LISTINGS are instead GENERATED by the Server from the
registry store — the cold-boot-fallback half of the OQ-2 ruling; the
PRIMARY listing path is live gateway `/v1/models` discovery (see the
container-registry materialization below).

Implement the compass resolver in the gateway boot entrypoint (the
RIG-1715 T1 entrypoint file): look up the request's `modelId` in the
registry read from the Server over the stack-token RPC-to-Server channel
(the OQ-4 ruling — a narrow registry-read method alongside the T2
credential reads, `compass-server-llm-gateway/design.md:348-358`), held
as an in-memory ref keyed on the registry `version` and re-read on
version change; for each candidate in order, dry-run-peek the T2 pool
for a usable (present, non-usage-limited) credential for `provider`;
return the first candidate materialized as a concrete `Model<Api>`;
unknown names fall through to the existing exact-match model lookup.
Extend `listModels` to emit stable names.

**This rides the ratified resolver-seam widening (OQ-1, Resolved
decisions).** The current boot option is
`ModelResolver = (modelId: string) => Model<Api> | undefined`
(`forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:55`) — synchronous
and tenant-blind, invoked at `server.ts:377`/`:569` before any credential
work. Candidate selection needs the caller tenant (per-request) and an async
pool query (`peekApiKey` is `async`,
`forks/oh-my-pi/packages/ai/src/auth-storage.ts:5122`).
P1 therefore widens the seam to
`(modelId, identity) => Promise<Model<Api> | undefined>`, the exact sibling
of the RIG-1715 T3 `authorize(req) → identity` boot-option change (gateway
`design.md:270-281`) — a small, upstreamable widening, NOT a routing-core
edit; the identity carries the caller USER, not merely a tenant/org id
(own per-user OAuth/subscription rows before the shared org key,
`compass-server-llm-gateway/design.md:324-329,375-395` — Global
Constraints). The candidate dry-run peek is a NEW read on the T2
resolver: a
side-effect-free "would this provider yield a usable credential for this
caller" that does NOT acquire or set stickiness (distinct from the routing
core's later stateful `storage.getApiKey(model.provider, sessionId)` at
`server.ts:419`). Stage-1 candidate choice is DETERMINISTIC, not a held cache:
it is a pure function of the ordered chain plus the current pool marks, so
absent a mark change the same candidate is chosen on every request of a
conversation and prompt-cache continuity is preserved without the seam
carrying `sessionId` (which is derived downstream at `server.ts:410`/`:578`,
after the seam has returned — the ruled OQ-1 sub-point). A mark changes
mid-conversation
only on a usage-limit event, exactly when re-resolution is wanted.

**Failover trigger (ruled — OQ-3):** skip a candidate on credential
absence and marked usage-limit — both observable in the dry-run peek
before the upstream call, so both are seam-executable. Matt's ruling is
**v1 hard-down on a provider outage** (no pre-first-byte retry): the
widened resolver returns one model before the upstream call happens in
`completeSimple` (`server.ts:462`/`:645`) or `streamSimple`
(`server.ts:499`/`:674`), so pre-first-byte failover would need a
routing-core loop or a compass-side upstream wrapper — the filed v2
follow-up RIG-3029, together with mid-stream failover.

**Container-registry materialization (ruled — OQ-2, Resolved
decisions): belt-and-suspenders.** The container learns the gateway's
stable names by TWO mechanisms, one primary and one fallback:

- **Primary — live gateway discovery.** In gateway mode the container
  DISCOVERS the stable names from the gateway's `/v1/models` (served by
  the P1-extended `listModels`,
  `forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:66-67`). The
  machinery already exists in the OMP registry: `ModelRegistry`'s
  gateway mode ignores the local `models.yml` for routing overrides
  ("Gateway mode: ignore local `models.yml` entirely … A broker-backed
  gateway serves only bundled + broker-discovered catalog metadata",
  `forks/oh-my-pi/packages/coding-agent/src/config/model-registry.ts:851-856`);
  discovery is SQLite-cached in `models.db`
  (`model-registry.ts:868`) under the default `online-if-uncached`
  refresh strategy (`model-registry.ts:882`) — a warm boot with a
  fresh cached row never hits the network; the fetch is bounded by a
  hard 15s timeout (`RUNTIME_DYNAMIC_MODEL_FETCH_TIMEOUT_MS`,
  `model-registry.ts:53-58`); and background-refresh errors are
  swallowed to a warning (`model-registry.ts:888-904`), so a failed
  fetch never bricks a warm boot — it falls back to cache + bundled
  entries. Discovery keeps warm boots fast + offline-tolerant and
  picks up registry changes live, without a bundle re-publish.
- **Fallback — the static Server-generated seed, closing the cold-boot
  window.** The refuse-to-boot belt fires only for a PINNED, unresolved
  pattern (`packages/compass-agent/src/cli.ts:991-1004`), and a profile
  pins the stable name — so the one brick window is a COLD first boot
  (empty discovery cache) whose `/v1/models` fetch also fails within
  15s. The Server therefore ALSO generates one gateway-provider
  (`transport: "pi-native"`) `models.yml` entry per stable name from
  its registry store (metadata from the registry entry), delivered
  into the fleet `models.yml` the bundle machinery already delivers
  (DL-126). The seed is a robustness FLOOR, not the live source —
  discovery is live; the seed guarantees a pinned stable name still
  resolves on a cold, gateway-unreachable first boot. How the seed
  enters the content-addressed bundle is ruled with it (option A,
  composed hash — Resolved decisions). Ordering: the seed lands with
  or before any profile naming a stable name (the RIG-2936 T6 render).

Interfaces:

- Consumes: the WIDENED resolver boot option
  `(modelId, identity) => Promise<Model<Api> | undefined>` +
  `listModels?: () => Iterable<Model<Api>>`
  (`forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:55,65-67`,
  widened per the ratified OQ-1); a NEW side-effect-free dry-run peek on
  the T2 pool resolver (own-then-shared,
  `compass-server-llm-gateway/design.md:375-395`); per-agent
  token→caller identity (RIG-1715 T3, per-user per OQ-1); the P2
  registry store via the
  stack-token registry-read method (OQ-4 ruling).
- Produces: `StableNameResolver` (TS, entrypoint-local):
  `resolve(callerUser: UserId, modelId: string): Promise<Model<Api> |
  undefined>`; a registry loader in the gateway entrypoint (reads the
  Server store over the stack-token channel, holds an in-memory ref
  keyed on the registry `version`); fork-harness tests.

Test cycle: red — a request for `claude-opus-4-8` 404s today
("Unknown model", `server.ts:379`); green — it resolves to anthropic-OAuth
for a caller holding a subscription, to `openrouter/...` for a caller
holding only an OpenRouter key, in the fork test harness pattern of
`auth-gateway-model-list.test.ts`.

### P2 — Server registry store, operator write RPC, write validation

Owner: compass-server (store + `compass.v1` RPCs + stack-token
registry-read method).

No config-bundle member, no three-layer member admission, no CI-publish
coupling — those were the mechanics of the earlier VC'd-substrate
ruling, refined at the freeze gate to server-side operator config
(Resolved decisions). P2 instead delivers:

- **The registry store schema.** A versioned Server store holding the
  stable-name registry: per name, `display_name`, ordered `candidates`
  (`{provider, model_id}`), `metadata` (context window, cost, `Api`
  type). Store discipline mirrors its sibling `gateway_credentials`: "a
  monotonic `version` per row supplying the CAS substrate"
  (`compass-server-llm-gateway/design.md:324-329`) — writes are CAS'd on
  the current version so a racing operator write is never clobbered, and
  the registry `version` keys the gateway resolver's in-memory ref (P1).
- **The operator write RPC.** A new operator-scoped `compass.v1` write
  surface (`PutModelRegistry`-style put/get/delete), the same
  operator-scoped pattern as CD-1's `PutAgentConfig` /
  `GetAgentConfigInfo` / `DeleteAgentConfig`
  (`compass-agent-config-delivery/design.md:173-178`). Operator-scoped
  only: agents never author (RIG-2936 Layer 1,
  `compass-per-agent-overrides/design.md:494-501`).
- **Write validation, failing closed** (the door-lint posture carried
  over from the VC'd shape, now at the RPC boundary — mirroring RIG-2936
  T1's superset-key + `models.agents`-key lints,
  `compass-per-agent-overrides/design.md:704-724`): schema shape;
  candidate `provider`/`model_id` shape; a registry delete that would
  orphan a published profile `models.*` reference fails closed. The
  bundle-door profile lint (every profile `models.*` value is a known
  stable name or an explicit `provider/id` escape-hatch selector — those
  always contain a `/`) checks against the Server registry store rather
  than a same-bundle member. Cross-family and model-choice judgment
  calls stay ADVISORY at operator review, matching RIG-2936's posture
  (`compass-per-agent-overrides/design.md:721-724`).
- **The gateway read surface.** The narrow stack-token registry-read
  method on the RPC-to-Server channel (the OQ-4 ruling,
  `compass-server-llm-gateway/design.md:348-358`): returns the current
  `(version, registry)`, plus a cheap version-only check so the
  gateway's in-memory ref re-reads only on change.

Interfaces:

- Consumes: the compass store layer + the CD-1 operator-RPC precedent
  (`PutAgentConfig` family,
  `compass-agent-config-delivery/design.md:173-178`); the
  `gateway_credentials` versioned/CAS store precedent
  (`compass-server-llm-gateway/design.md:324-329`); the stack-token
  RPC-to-Server channel (`compass-server-llm-gateway/design.md:348-358`);
  the gateway record's store-discipline constraint (in-memory reference +
  `pgtest`, DL-174 pyramid,
  `compass-server-llm-gateway/design.md:704-706`).
- Produces: the registry store schema + migration; the operator-scoped
  write RPC with fail-closed validation; the stack-token registry-read
  method; an in-memory reference store + pgtest suite (CAS write
  discipline; malformed schema and an orphaning delete each rejected; a
  version bump observed by the read surface).

### P3 — dissolved: delivery is RIG-2936 T6's; the vocabulary residue moves to P1/P2

The prior P3 — Runner resolution of a policy store into `COMPASS_MODEL` +
merged `modelRoles` — is SUPERSEDED by the frozen RIG-2936 T6 render: the
entrypoint resolves `COMPASS_PROFILE` against the mounted
`profiles/<name>/profile.yml` and renders `models.manager` → the session
`modelPattern` (the sole model source for a Compass-provisioned session,
superseding the Runner-global `COMPASS_MODEL`) and `models.agents` →
`task.agentModelOverrides`
(`compass-per-agent-overrides/design.md:890-900`; `COMPASS_MODEL` retained
as pre-existing infra, no longer a model fallback, `:866-869`). This record
does not re-specify any of that delivery.

What remains RIG-2845's, and where it now lives:

- The stable-name VOCABULARY the profile's model fields speak
  (the DL-288 boundary, `compass-per-agent-overrides/design.md:469-474`) —
  §(a), §(b), P1/P2.
- The container-registry stable-name LISTING (ruled — OQ-2, Resolved
  decisions: gateway `/v1/models` discovery primary + static
  Server-generated seed fallback): a profile-named stable name must
  resolve in-container or the boot belt fires
  (`packages/compass-agent/src/cli.ts:991-1004`); the static seed must
  land with or before any profile naming a stable name.
- Thinking-level encoding needs no new mechanism: a `:<level>` suffix rides
  the shared selector grammar (split on the LAST colon,
  effort-ladder-validated — the RIG-2936 shared anchor,
  `compass-per-agent-overrides/design.md:257-261`), so a profile value like
  `claude-opus-4-8:high` flows through the render unchanged.

### P4 — UI display surface (read-only)

Owner: compass-obs (UI lane).

Registry edits go through the operator-scoped `compass.v1` write RPC
(P2) — there is no UI write surface in v1. What the UI shows is the
PUBLISHED stable-name REGISTRY: the stable names + each name's declared
candidate chain, read for display only (profiles and their configured
models are RIG-2936's display surface), via the ruled OQ-5 read path — a
narrow `compass.v1` READ RPC serving the Server registry store (Resolved
decisions; the store IS the published state, so no
authored-vs-published divergence exists). The EFFECTIVE backend (which
candidate the caller's pool currently picks) is deliberately NOT shown
in v1: it depends on live pool state — credential presence +
usage-limit marks — which post-RIG-1715 lives only inside the TS
gateway's per-tenant `AuthStorage`
(`compass-server-llm-gateway/design.md:287-296`), not in the Go server
that answers `compass.v1`. Surfacing it would need a new Server→gateway
pool-state read surface no record designs; if Matt wants the live
display it is a follow-up with that surface as a named, owned interface.

Interfaces:

- Consumes: the Server registry store via the narrow `compass.v1` read
  RPC (the OQ-5 ruling; a read-only sibling of P2's operator RPCs).
- Produces: a read-only stable-name registry display view.

### P5 — Shipped defaults + docs deliverable

Owner: compass-obs.

Seed the day-1 stable-name registry entries covering the gateway
record's day-1
providers ("anthropic (OAuth + api_key), openai/openai-codex
(OAuth + api_key), google (api_key)",
`compass-server-llm-gateway/design.md:719-720`) plus OpenRouter/Bedrock
alternate candidates — written into the Server registry store via P2's
operator RPC.
Write the docs page "recommended model per role per provider you hold",
forward-referencing the RIG-2562 model evaluations as the evidence source
and citing the fleet model-selection spec's optimization target (the spec
lives in the internal fleet-tooling repo — cite conceptually, do not link
the private repo from this public one).

Interfaces:

- Consumes: RIG-2562 per-role recommendations (when published); the P2
  registry schema + operator write RPC.
- Produces: seeded registry defaults in the Server store + `docs/` page;
  a data-refresh runbook line (updating defaults is an operator registry
  write via P2's RPC, not a release).

## Tasks

- [ ] P1 — Gateway resolver + registry read (Owner: Bun entrypoint +
      compass-server read surface) — resolver-seam widening (async +
      per-user identity, no `sessionId`; OQ-1 ratified), registry read
      over the stack-token RPC-to-Server channel with an in-memory
      version-keyed ref (OQ-4 ruling), pool dry-run peek, hard-down
      failover on absence + usage-limit (OQ-3 ruling; pre-first-byte
      failover is RIG-3029), unknown-name pass-through, container
      listing per the OQ-2 ruling (gateway `/v1/models` discovery
      primary + static Server-generated `models.yml` seed fallback),
      fork-harness green.
- [ ] P2 — Server registry store + operator write RPC (Owner:
      compass-server) — versioned/CAS store schema (sibling of
      `gateway_credentials`), operator-scoped `compass.v1` write RPC
      (the CD-1 `PutAgentConfig` pattern), fail-closed write validation,
      stack-token registry-read method; in-memory reference + pgtest.
- [ ] P3 — dissolved: model delivery is RIG-2936 T6's (`modelPattern` +
      `task.agentModelOverrides`); residue (vocabulary + container
      listing) carried by P1/P2.
- [ ] P4 — UI read-only registry display (Owner: compass-obs) — reads
      the Server registry store via the narrow `compass.v1` read RPC
      (OQ-5 ruling); editing rides the operator write RPC (no UI write
      surface, no live effective-backend display in v1).
- [ ] P5 — Defaults + docs (Owner: compass-obs) — day-1 stable-name
      registry set seeded via the operator RPC, recommended-models docs
      page, RIG-2562 refresh path (operator registry write).

## Open Questions

1. **(Non-load-bearing) Stable-name namespace.** Bare names
   (`claude-opus-4-8`, Matt's example) vs prefixed (`compass/...`).
   **Recommendation:** bare, matching the gateway-record example
   verbatim; the unknown-name pass-through disambiguates collisions with
   concrete `provider/id` selectors since those always contain a `/`.
2. **(Non-load-bearing) Interim defaults before RIG-2562.** Seed from
   fleet-current practice (P5) vs wait for eval numbers.
   **Recommendation:** seed now; RIG-2562 output is a data update by
   construction.
3. **(Non-load-bearing) Model-selection spec location.**
   `docs/specs/platform/model-selection.md` does not exist in the
   compass repo (verified: `docs/specs/` holds only `product/` +
   `brand/`); it lives in the internal fleet-tooling repo
   (RIG-2573/DL-025). **Recommendation:** cite its optimization target
   conceptually in P5's docs page; do not link the private repo from
   this public one.

## Resolved decisions

- **(OQ-4) Registry substrate — server-side operator config (Matt's
  freeze-gate ruling, refining the earlier VC'd ruling).** The earlier
  config-authority ruling — registry content VC'd in the fleet config
  bundle, CI-published — was made when this record still carried the
  full taxonomy + policy scope, where the registry sat beside RIG-2936's
  profile content as genuinely fleet-authored configuration. Narrowed to
  routing-only, the registry is deployment/operator config — the same
  surface class as the provider credentials the operator already sets
  WITH the Server, not in a config repo. A scope-driven refinement, not
  a reversal of intent: the operator remains the sole author and agents
  still never author. The ruled substrate: a versioned Server store,
  sibling in kind of RIG-1715's `gateway_credentials` ("a monotonic
  `version` per row supplying the CAS substrate",
  `compass-server-llm-gateway/design.md:324-329`), authored via an
  operator-scoped `compass.v1` write RPC (the CD-1 `PutAgentConfig` /
  `GetAgentConfigInfo` / `DeleteAgentConfig` pattern,
  `compass-agent-config-delivery/design.md:173-178`) and read by the
  gateway over the stack-token RPC-to-Server channel
  (`compass-server-llm-gateway/design.md:348-358`). The fleet config
  bundle and its CD-1 `compass config put` publish path ("the repo is
  the *authoring* source, `put` is the *publish* step",
  `compass-agent-config-delivery/design.md:208-215`) are unchanged for
  the content that stays in it — profiles; only the routing registry
  moves server-side.
- **(OQ-1) Resolver-seam widening — ratified, with per-user identity.**
  The injected resolver boot option widens from the synchronous,
  tenant-blind `ModelResolver = (modelId: string) => Model<Api> |
  undefined` (`forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:55`,
  invoked at `server.ts:377`/`:569`) to
  `(modelId, identity) => Promise<Model<Api> | undefined>` — async
  because the pool query is (`peekApiKey` is `async`,
  `forks/oh-my-pi/packages/ai/src/auth-storage.ts:5122`),
  identity-carrying because candidate selection is per-caller; the exact
  sibling of the RIG-1715 T3 `authorize(req) → identity` boot-option
  change (`compass-server-llm-gateway/design.md:270-281`). The identity
  carries PER-USER granularity, not merely tenant/org:
  subscription/OAuth candidates are ToS-bound to the individual user and
  resolve against `owner_user_id`-scoped rows; only shared org API-key
  candidates resolve at org scope
  (`compass-server-llm-gateway/design.md:324-329`) — own (per-user
  OAuth/subscription) before shared (org API key), RIG-1715's stage-2
  order (`compass-server-llm-gateway/design.md:375-395`). Sub-point
  ruled with it: no `sessionId` in the signature — `sessionId` is
  derived DOWNSTREAM at `server.ts:410`/`:578`, and stage-1 choice is
  deterministic (a pure function of chain + pool marks), so prompt-cache
  continuity holds without a sticky map.
- **(OQ-3) Failover — v1 hard-down; pre-first-byte is RIG-3029.** v1
  fails over only on credential absence + marked usage-limit — both
  observable in the dry-run peek, fully seam-executable at zero
  routing-core cost; a provider outage pins callers of a stable name to
  the hard-down candidate for its duration. Pre-first-byte failover
  (connect error / immediate 5xx / 429-without-usage-limit) — which
  would need a routing-core retry loop or a compass-side upstream
  wrapper, because the upstream call happens after the seam returns, in
  `completeSimple` (`server.ts:462`/`:645`) or `streamSimple`
  (`server.ts:499`/`:674`) — is the filed v2 follow-up **RIG-3029**,
  together with mid-stream failover. The "transient provider-down" mark
  is out of v1 entirely (nothing sets it).
- **(OQ-2) Container listing — belt-and-suspenders: gateway discovery +
  static cold-boot seed (Matt's freeze-gate ruling).** The container
  learns the gateway's stable names BOTH ways. Primary: in gateway
  mode the container DISCOVERS them live from the gateway's
  `/v1/models` (served by `listModels`,
  `forks/oh-my-pi/packages/ai/src/auth-gateway/server.ts:66-67`) via
  OMP's existing gateway-mode registry (`ignoreLocalModelConfig`,
  `forks/oh-my-pi/packages/coding-agent/src/config/model-registry.ts:851-856`),
  SQLite-cached in `models.db` (`model-registry.ts:868`) with the
  `online-if-uncached` default strategy (`model-registry.ts:882`), a
  15s-bounded fetch (`model-registry.ts:53-58`), and
  background-refresh errors swallowed to a warning
  (`model-registry.ts:888-904`) — warm boots stay fast +
  offline-tolerant and registry changes are picked up live. Fallback:
  the Server ALSO generates a static `models.yml` seed entry per
  stable name from its registry store, delivered in the fleet bundle,
  closing the one cold-first-boot window (empty cache + failed
  discovery fetch) where a pinned stable name cannot resolve and the
  boot belt fires (`packages/compass-agent/src/cli.ts:991-1004`); the
  seed is the cold-start floor, not the live source. The bundle-hash
  sub-seam is ruled with it — option (A): the Server composes the
  effective bundle as operator-authored content + the Server-generated
  `models.yml` overlay, and the effective version hashes the COMPOSED
  canonical content — a registry write mints a new effective version
  and rides the existing CD-1 delivery/Reload machinery unchanged (no
  second delivery mechanism), preserving CD-1's no-op idempotency ("a
  **canonical content hash of the decompressed content**"; "a no-op
  re-push of identical config cannot mint a new version",
  `compass-agent-config-delivery/design.md:187-197`): same authored
  bundle + same registry ⇒ same hash.
- **(OQ-5) UI display read path — a narrow `compass.v1` READ RPC over
  the Server registry store.** With the registry in a Server store
  (OQ-4), the earlier (A)-vs-config-repo fork is moot — the store IS the
  published state, so no authored-vs-published divergence exists to
  choose between; the read RPC serves the store's current version, keeps
  `compass.v1` the sole UI↔server door
  (`compass-server-llm-gateway/design.md:691-694`), and stays
  display-only.
- **Write authority — an operator write path now exists; agents still
  never author.** The pre-reconciliation draft's split ("agents MAY
  write role policy / may NOT write stable-name chains") stays moot
  under the OQ-4 ruling, but the reasoning shifts: it is no longer "no
  write path exists at all" — the operator-scoped P2 `compass.v1` RPC IS
  a write path — it is that no AGENT write path exists to split
  authority over ("The agent SELECTS; it never AUTHORS" — RIG-2936
  Layer 1, reinforced by Layer 2,
  `compass-per-agent-overrides/design.md:494-501`). The blast-radius
  concern the split addressed (one chain edit redirecting every
  referencing profile) is carried by P2's fail-closed write validation +
  operator review.
- **Delivery seam — RIG-2936-owned.** Per-Manager profile selection +
  propagation (`models.manager` → the session `modelPattern`;
  `models.agents` → `task.agentModelOverrides`) is frozen in RIG-2936
  (DL-283/DL-284/DL-288,
  `compass-per-agent-overrides/design.md:890-900,469-474`); this record
  supplies the vocabulary those fields reference and never re-specifies
  delivery. The Runner-global `COMPASS_MODEL` is superseded as the model
  source for Compass-provisioned sessions (`:866-869`).
- **Policy scope shape (the pre-reconciliation draft's OQ-5) — moot.**
  The registry store is a fleet-wide singleton, matching the CD-1
  bundle's fleet-wide-singleton posture
  (`compass-agent-config-delivery/design.md:187-197`); per-agent
  variation is profile SELECTION (RIG-2936), and per-user/org scoping
  arrives, if ever, via DL-078's named post-MVP persona/role-keyed
  seam — no speculative org column, because there are no columns.
