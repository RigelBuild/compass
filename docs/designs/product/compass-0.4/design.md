# Compass v0.4 — Leverage the commodity layers, own the moat

Status: Historical

> Internal design record — July 2026. A strategic-posture revision of the Compass product
> design. The full ADE vision it builds on is the frozen v0.3 record
> ([`../compass.md`](../compass.md)); this record captures only what v0.4 changes
> and why. Living built-behavior is the spec ([`../../../specs/product/compass.md`](../../../specs/product/compass.md)).

## Problem

The v0.3 design implied Compass would build much of its own stack: a bespoke
coordination/messaging layer, its own agent process management, and — read
literally — a path toward its own general coding agent. Three things changed the
calculation:

- **The commodity layers aren't the moat.** Compass's differentiators are the
  security runtime (Warden), per-agent container isolation, and the
  issue→workstream Dispatcher/Bridge. Every hour spent owning a messaging
  protocol or re-implementing an agent is an hour not spent on those.
- **The agent runtime is enormous and already exists.** Oh My Pi (OMP) is a
  mature, feature-rich agent harness (loop + tools + provider/AI layer + memory +
  eval + TUI + ACP). Rebuilding that surface — in any language — is a
  multi-hundred-KLOC reimplementation of a moving target, not a "fork."
- **A coordination bus we'd want already exists.** Cotal (Apache-2.0,
  NATS/JetStream) is further along than a hand-rolled bus on the axes Compass
  needs (role-addressed delivery, hierarchical channels with replay-on-join,
  JWT/ACL identity), and the OMP-side receiver work already exists.

The intent of v0.4: state, as the product's posture, that **Compass builds only
its moat and leverages the commodity layers behind thin seams it controls.**

## Approach

Three moves, each behind a seam Compass owns, sequenced so no single dependency
is load-bearing before it's de-risked:

1. **Coordination → Cotal.** Adopt Cotal as the coordination substrate behind a
   thin interface Compass owns; resolve the trust-model gate (below) before it
   carries anything past the baseline tier.
2. **Worker + Dispatcher agent → OMP over ACP.** OMP is the default/reference
   agent and the runtime the Dispatcher runs on for MVP. BYOA stays the
   compatibility surface. Compass improves OMP in the open (upstream
   contribution), not as a private fork it maintains alone.
3. **seal → the Warden runtime, scoped tight.** seal is the one agent Compass
   builds first-party, scoped to exactly what hosting Warden requires. Growing
   seal into a general worker agent is explicitly deferred.

**Result:** Compass focuses on the wedge — the Bridge, per-agent containers, and
the Dispatcher(OMP)/Warden(seal) agents. Coordination is leveraged (Cotal), the
general agent is leveraged (OMP), and Compass owns exactly the two things that
are the moat: the security runtime and the ADE orchestration.

### Alternatives weighed

- **Rebuild seal as an OMP fork (absorb OMP's features into seal's Rust→WASM
  runtime).** Rejected. OMP's tools freely touch the filesystem, network, and
  clock; seal's entire guarantee is that code *can't* except through signed
  capabilities. Running OMP's tools outside the WASM sandbox forfeits the
  guarantee; porting them inside it is a multi-year rebuild that fights the
  model. The WASM sandbox earns its keep for the *auditor* (Warden), not for
  re-hosting a general agent.
- **Keep owning the coordination layer (a bespoke bus).** Rejected. It's not the
  moat, and a capable Apache-2.0 bus already exists. The residual risk (a v0
  dependency) is handled by the thin seam + the trust-tier gate, not by
  rebuilding.
- **Make OMP just "one BYOA target among many," own no default agent.** Rejected.
  A default agent is needed for the dogfood path, for the Dispatcher runtime, and
  for the one place Warden's gate can be *structural* (OMP's tool-dispatch path)
  rather than hook-config-dependent. OMP is that default; the others stay
  first-class BYOA.

## Decisions

### OMP is the default/reference agent — and what OMP *is*

OMP is an **external open-source project** (`can1357/oh-my-pi`). Compass runs a
**fork** (`mattwilkinsonn/oh-my-pi`) and contributes fixes and features
**upstream** rather than maintaining a divergent private fork; the fork is the
integration point and the staging area for upstream PRs, not a permanent
diverging branch. This is the accurate ownership model — OMP is *leveraged
external OSS with Compass as an active contributor*, **not** an "in-house
harness." The dependency-risk framing (below) depends on stating this correctly.

OMP is the default because it is ACP-native (no wrapping adapter), it carries
Warden's gate structurally in its tool-dispatch path (which the agent cannot
rewrite, unlike a hook configuration), and it is the runtime the Dispatcher and
the first-party security paths run on. The other targeted agents (Claude Code,
Codex, Antigravity, OpenCode, Amp, any ACP agent) remain first-class BYOA
citizens, driven identically over ACP.

**Design-doc changes:** v0.3 §5.1 (BYOA) reframes OMP from "first integration
target" to default/reference agent; §4.2 (Dispatcher) states it runs on OMP over
ACP for MVP. The word "in-house" is dropped wherever it described OMP.

### seal is scoped to hosting Warden

Warden runs on seal, the Rust→WASM agent Compass owns end-to-end, because the
security auditor is the moat. seal is scoped to exactly what hosting Warden
requires — no more. Warden's actuator surface is tiny (`pause_agent` + a
clear/caution/flag gate decision), and its loop needs are ordinary, so the
additions are bounded: the `pause_agent` IPC channel, the tool-call event feed,
the session-scoped taint registry, the synchronous gate-decision channel, and a
pre-inference secret classifier/redactor. The WASM sandbox is justified *here* —
isolating the auditor from the agents it watches and bounding the
credential-bearing payloads it inspects — even though it is not an MVP
requirement for the worker agents.

Whether seal ever grows into a general worker agent is a separate, deliberately
deferred decision (v0.3 §14 gains a bullet). Nothing in the MVP depends on it.

**Design-doc changes:** v0.3 §6.5 (Warden Implementation) reframes seal's role
from a passing mention into the explicit scoping above.

### Cotal is the coordination substrate, behind a thin seam

Compass adopts Cotal (Apache-2.0, NATS/JetStream) as the messaging layer for
agent↔Dispatcher signals, cross-agent presence, and replay-on-join — leveraged,
not owned. NATS clustering fits the multi-container (and later multi-host) model;
the Compass daemon can host or embed the broker. Compass couples to Cotal behind
a thin coordination seam it controls, so the bus stays swappable: the wire
contract is documented and the daemon could reimplement it over NATS directly if
it had to. Coordination *logic* (assignment, conflict map, board) stays
Compass's; only the transport is leveraged.

**Trust-tier gate.** Cotal v0 is a trusted-broker model (plaintext to the broker,
no e2e or non-repudiation; signed envelopes reserved for later). Compass's
high-assurance tier (Enterprise/Federal) sits above "trust the broker," so the
trust boundary **cannot** be fully outsourced to a v0 bus for that tier. This is
not disqualifying for the baseline tier — Warden watches agent *behavior*, a
different layer than wire security, and the per-agent container's egress
allowlist bounds the broker connection. Cotal is adopted for baseline
coordination now and gated out of the high-assurance trust path until the gate is
lifted. **Lift criteria + owner: SEA-1113.**

**Design-doc changes:** v0.3 gains a §7.7 (Coordination substrate: Cotal).

### Upstream contribution is a distribution channel

Compass leverages two external OSS projects (OMP, Cotal) and contributes into
both. That is also GTM: the connectors and fixes Compass upstreams earn codebase
knowledge, protocol influence, first-class connectors those teams help maintain,
and exposure to their users. Leveraging the commodity layers instead of
rebuilding them frees the team for the moat while the leveraged projects'
communities become an audience.

**Design-doc changes:** v0.3 §11 (Business Model) gains an upstream-contribution
paragraph.

## Risks

- **OMP release cadence (dependency risk).** Making OMP the default and the
  Dispatcher runtime couples a load-bearing path to an external project's cadence
  and direction. Mitigation: the agent is driven over ACP — a standard, swappable
  interface, so a BYOA agent can stand in — and the upstream-contribution flow
  keeps Compass close to the project rather than dependent on a divergent private
  fork. The coupling is real and stated; the ACP seam is what keeps it from
  becoming lock-in.
- **Cotal v0 trust model.** Trusted-broker, plaintext to the broker, no e2e.
  Tracked and gated by SEA-1113; baseline coordination proceeds, the
  high-assurance tier waits on the lift criteria.

## Plan

The v0.4 content lands as edits to the frozen v0.3 design doc's successor plus the
living spec and this record. Because `docs/designs/<domain>/` records are frozen
once decided, the pivot is captured *here* (a new record) rather than by
rewriting `../compass.md`; the living spec is updated to point at the current
rationale and to reflect the pivot in its forward-looking overview.

- **T1 — This design record.** Problem · Approach · Decisions · Plan, with OMP's
  ownership stated accurately, the seal scoping, the Cotal seam + SEA-1113
  gate, and the two risks. *(This file.)*
- **T2 — Living spec cross-reference + overview.** Point the spec's design-record
  link at the current rationale, and update its forward-looking overview so the
  not-yet-built runtime/Dispatcher/Warden description reflects OMP-over-ACP,
  seal-hosts-Warden, and Cotal coordination — without inventing
  Requirement/Scenario contracts for unbuilt behavior.
- **T3 — Tracking issue.** SEA-1113 filed for the high-assurance Cotal
  trust-model gate; referenced from this record and the spec.

## Tasks

- [x] Write this design record (`docs/designs/product/compass-0.4/design.md`).
- [x] File SEA-1113 for the Cotal high-assurance trust-model gate.
- [x] Update the living spec (`docs/specs/product/compass.md`): design-record
      cross-reference + forward-looking overview reflect OMP-over-ACP,
      seal-scoped-to-Warden, and Cotal coordination.
- [x] markdownlint the new + edited docs clean.

## Global Constraints

- **`docs/designs/<domain>/` records are frozen once decided** (`docs/README.md`,
  `docs/designs/platform/docs-system.md`): capture a change as a new record, never
  by rewriting a decided one. v0.3 `../compass.md` stays as-is.
- **The living spec states only built behavior**, as `### Requirement:` +
  `#### Scenario:` contracts; it defers rationale to the design records. The
  pivot is not built, so it appears in the spec only as the design-record
  pointer and the forward-looking overview prose — no fabricated contracts.
- **No persona names or agent-product names** in this record (it lives in the
  repo; keep planning personas out). Linear issue refs and the Tasks checklist
  are fine — this is an internal design record, not a published ``
  artifact.
- **OMP is external OSS** (`can1357/oh-my-pi`) that Compass forks
  (`mattwilkinsonn/oh-my-pi`) and contributes upstream to — never "in-house."
