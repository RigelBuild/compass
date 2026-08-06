# Compass — Product Design Document

Status: Historical

> Internal. Draft v0.3 — June 2026.

---

## 1. Vision

Compass is an Agentic Development Environment (ADE) that lets developers run multiple AI coding agents in parallel — in full YOLO mode — with a built-in security layer watching everything they do.

The core bet: developers aren't choosing YOLO because they don't care about security. They're choosing it because the productivity gains are real and the existing security solutions add too much friction. Compass gives them both. Agents run unrestricted. Warden watches the action stream and intervenes only when it matters. Most developers will barely notice it's there most of the time — and that's the point.

---

## 2. Problem

### 2.1 The market has moved to YOLO

Every major coding agent now ships with a flag to disable permission dialogs:
`--dangerously-skip-permissions`, `--yolo`, `--auto-approve`. The ADEs built around them (Orca, Superset, Emdash) set these flags by default, and the fastest-growing agent forks are the ones that made friction-free execution the default. The direction is clear: friction-free agent execution has become the baseline expectation.

### 2.2 The security risk is real and growing

Prompt injection sits at the top of OWASP's LLM risk list, and it isn't theoretical: coding agents have already been targeted by injection-class vulnerabilities — supply-chain injection through dependencies, configuration injection through agent settings files, and indirect injection through repository content an agent reads. Every developer running YOLO agents today is one malicious repo, one poisoned dependency, or one crafted error message away from their agent doing something unintended.

### 2.3 Existing security solutions solve the wrong problem

The current security approaches are all either:

- **Permission-based and high-friction** — constant dialogs, whitelist management, OS-level sandboxing that interrupts the agent. Developers reject this for good reason.
- **Enterprise infrastructure** — network proxies, SIEM-level platforms, compliance audit trails. Not developer-facing, not ADE-integrated.
- **Research** — AgentWatcher, AgentSentry, Agent Audit. Not products.

None of them sit at the ADE layer. None of them work *with* YOLO mode rather than against it.

### 2.4 ADEs lack a coherent supervision model

The existing ADEs treat agents as ephemeral task workers. Start a task, agent runs, task ends, context is gone. Developers lose track of what's running, what finished, and what the agent was doing in the first place. There's no persistent workstream identity, no coordinator that understands the full picture, and no separation between "someone who assigns work" and "someone who does work."

---

## 3. Solution: Compass

Compass is a desktop ADE built around three concepts:

1. **Named workstream agents** — persistent, reloadable agents with identity and continuity across sessions, not ephemeral task workers you lose track of.
2. **Dispatcher** — a built-in supervisor agent that schedules work, manages agent queues, coordinates publishing, and translates issue tracker tickets into agent workstreams.
3. **Warden** — an always-on security auditor that watches all agent sessions in real time and intervenes when it detects threats, without touching the normal agent execution flow.

The developer experience: open Compass, connect your issue tracker, run your agents in YOLO mode, let the Dispatcher coordinate the workstreams and Warden handle the threat surface. The security is invisible until it matters.

---

## 4. Core Concepts

### 4.1 Named Workstream Agents

Each agent in Compass has a name, a persona (optional), its own container with a dedicated repo clone (§5.3), a task queue, and persistent history. These are not one-shot task runners — they're workstreams with identity.

- A named agent is created once and reused across many tasks. New tasks get new branches under the same agent — isolated per workstream (§5.3) — not new agents.
- When an agent's context is cleared (session reset), it reloads from its workspace state and current task specification. The name and continuity carry across reloads.
- Agents can be customized: name, system prompt additions, workspace location, which agent CLI they use.
- The user holds a mental model: "Atlas is working on the auth refactor, Scout is cleaning up tests." This is qualitatively different from "agent 7 from Tuesday."

**Persistent vs ephemeral agents.** Most named agents are *persistent* — a fixed name, a stable container and clone, and a continuous history that accumulates across sessions. They're durable team members you build a mental model around. Compass also supports *ephemeral* agents: spun up on demand for a single task or a short stack, assigned a codename from the active theme's pool, and retired when their work merges (container torn down, codename returned to the pool). Ephemeral agents cover one-off fixes and surge capacity when the persistent team is saturated. The distinction is operational, not architectural — both run the same agent runtime; what differs is whether Compass reuses the container and identity across tasks.

**Codename themes.** Agent names come from selectable themes (Norse gods, explorers, constellations, …) shipped with Compass. The user picks a theme, and ephemeral agents draw their codenames from its pool; users can also add their own naming lists. Distinct first letters within a theme keep agents easy to scan at a glance.

### 4.2 Dispatcher

The Dispatcher is the built-in supervisor agent. It is always present in Compass and manages the workstream layer:

- Pulls issues from the project's sources — Linear, GitHub Issues, Jira, or a local store (§4.5) — and proposes assignments to named agents.
- Maintains a conflict map: which agents own which file zones. This is **advisory scheduling** — it steers assignment away from collisions, but agents run unrestricted, so an overlapping edit is surfaced rather than blocked at write time; hard zone enforcement would need a write-time check, a hardening-layer concern (§6.7).
- Manages agent task queues: when an agent finishes, the Dispatcher proposes the next pickup from the backlog.
- Coordinates publishing: working agents signal *ready-for-push* when a branch is ready (§5.4), keeping the Dispatcher's board current; agents publish under their own forge identity (§9), so the Dispatcher tracks and coordinates rather than gating the push.
- Can spin up new named agent sessions from within its own context — though spawning and task-queue changes are user- or policy-gated actions, not unilateral model decisions (§6.7).
- Runs on a configurable model (defaults to a capable mid-tier model, Sonnet-class; user can upgrade).
- The user interacts with the Dispatcher through the Bridge (§4.4) — where they assign work, ask questions, review the conflict map, and get status across all agents.

The Dispatcher does not write code. It coordinates agents that do.

### 4.3 Warden

Warden is the always-on security auditor. It runs as a persistent background agent watching the action streams of all other agents in the session.

- Active authority, mostly out of the way: agents run freely while Warden observes asynchronously, but it can pause any agent the instant it detects a threat — and it inspects in-line at the two synchronous gates (§6). It is not a passive notifier; pausing is its own decision.
- Narrow actuator surface: Warden has two narrow actuators — `pause_agent`, and the clear/caution/flag decision it returns on a held call at the synchronous gates (§6.5). Neither is web access, filesystem write, or an agent-controllable external call; a deceived Warden's worst case is a wrong gate decision — the false-negative path §6.4 names — not a new capability for an attacker. This describes Warden's *actuator* surface, not model egress: when Warden runs on a hosted model, the content it inspects (flagged tool results, suspicious repo/API/secret payloads) is sent to that model provider for inference, which is itself outbound egress. Deployments where that egress is unacceptable run Warden on a local or on-prem model (§11); secret- or credential-bearing flagged content is the sharp case and is either inspected by a local/on-prem Warden or redacted before any hosted-model call, so the auditor never widens the trust boundary it exists to protect.
- Mostly out of the hot path: Warden does not gate every agent action; it reads the action stream asynchronously. The exception is the two synchronous gates described in §6.

### 4.4 The Bridge

The Bridge is the primary panel in the Compass UI — where the user talks to the Dispatcher and sees the whole board. Its default view is a Kanban board, with one change that reflects how agents (and people) actually work: the named agent is first-class, not pinned to a single card.

A developer rarely works one issue start to finish. They begin on an issue, it expands into new issues, and they jump between them — coding one while another sits in review. Named agents work the same way. The Bridge renders this as a **swimlane grid**: rows are named agents (plus a Dispatcher/unassigned row for the pool), columns are workstream status (Backlog, In Progress, In Review, Merged). A card sits at the intersection of an agent and a status, so an agent's row shows its work spread across columns at once, with its current focus emphasized. A team-glance toggle collapses the rows back into a conventional issue-status board.

Work expands in place: when an agent discovers new work mid-task, it files a child issue (linked to its parent) that appears on the board, and the Dispatcher proposes whether the same agent continues or another picks it up, applying the conflict map. The board updates live as runs progress, decisions surface, and branches move toward merge.

### 4.5 Projects

A project is the top-level unit of organization — a full-screen workspace the user switches into, the way Arc switches spaces (a bottom bar, not a sidebar panel, because changing projects is a real context switch). Each project scopes the agents, directories, and issue sources for one body of work.

- A project can span multiple repos and directories; it is not confined to a single repo. On creation the user picks the directories and repos in scope, and can add more at any time.
- An agent works across **all** of a project's repos at once, not one at a time: when assigned to a project, Compass gives that agent a container holding its own clone of *each* in-scope repo (§5.3) and presents them as one coherent agent workspace, so a single workstream can edit a library in one repo and its consumer in another, with a branch tracked per repo (§5.3, §9).
- A directory that is a git repo gets the full repo experience: clone-per-agent isolation (§5.3), branch and PR linking, and CI status surfaced in-app. A non-repo directory (e.g. an Obsidian notes folder) is in scope too, but it has no branch isolation — Compass mounts it into the agent's container per workstream (snapshotted, or single-writer) so parallel agents don't clobber each other, and applies no repo-specific features (PR, CI) to it.
- Making a repo container-ready is a one-time step: if it has no `devenv` config, the Setup agent (§5.5) authors one, so Compass can build the repo's per-agent container image from it.
- Issue sources are pluggable per project: Linear, GitHub Issues, Jira, or a local SQLite store for ad-hoc tasks the user doesn't want to file in an external tracker. The Bridge (§4.4) reads from whichever sources the project is wired to.

---

## 5. Agent Architecture

### 5.1 Bring Your Own Agent (BYOA)

Compass is Bring Your Own Agent first: developers connect whichever agent they already use, driven over the **Agent Client Protocol (ACP)** — the editor↔agent standard (Zed's [ACP](https://agentclientprotocol.com), the LSP-for-agents) — through the agent's native ACP server or a thin CLI-wrapping adapter (the `claude-code-acp` / `codex-acp` pattern). The agents Compass targets:

- Oh My Pi (OMP) — our in-house harness; the dogfood agent and first integration target
- Claude Code
- OpenAI Codex
- Google Antigravity CLI — the `agy` successor to Gemini CLI; native ACP is still pending (the `agy --acp` request), so it's driven through an adapter until it ships, while Gemini CLI itself already speaks ACP
- OpenCode
- Amp
- Any other agent with an ACP server or adapter

Compass drives each agent **over ACP** — consuming its structured stream (thinking, tool calls, diffs, plans) and rendering Compass's own UI rather than scraping a terminal (§7.4) — and runs it **inside its own container** (§5.3), managing its process and lifecycle. Agents run with YOLO/auto-approve flags set by default; the per-agent container is the structural sandbox that makes YOLO-by-default safe (§5.3, §6.7), and Warden's gate rides the agent's native runtime hooks (§5.2), not ACP's cooperative permission prompt (§6.7).

An agent becomes a Compass agent through a small **provision step**: a script that installs the agent and its ACP server *inside* the container and wires its native Warden hook (§5.2), layered on the repo's devenv image (§5.5). The script runs in the container at provision time — Compass's host process never executes it — so adding or updating an agent is a provision-script change, not a Compass code change. ACP is the single interaction surface; the provision step carries only the install and the security-hook wiring.

Going ACP-only means there is no second terminal-scraping path to maintain — one interaction surface, not two. The remaining distinction is *where the gate lives* (§6.7): a foreign agent's gate rides its configurable native hook, mounted read-only so the agent can't rewrite it (§5.3), whereas **OMP** — our in-house dogfood agent — carries its gate in its runtime's tool-dispatch path, which the agent cannot rewrite at all, making Warden structural rather than hook-config-dependent for it. Both are driven identically over ACP.

### 5.2 Hook Integration

ACP drives the agent, but ACP's permission flow is **cooperative** — an agent only *may* request approval and *should* report tool calls, so a compromised agent simply stops reporting (§6.7). Warden's gate therefore rides each agent's **native runtime hook system**, which fires inside the agent's execution path on every tool call — runtime-enforced, not model-elected — intercepting at the right boundaries:

- **OMP (Oh My Pi)**: tool-call interception via its native hook surface — the dogfood agent and first integration target; the public agents below follow.
- **Claude Code**: pre/post tool-call hooks via the `settings.json` hooks interface. Pre-tool hooks can block execution — this is the mechanism for the synchronous security gates. (CLAUDE.md is loaded as prompt context only and cannot deterministically block a tool call, so it is not a gate surface.)
- **Codex, Antigravity, and other supported agents**: hook integration via each agent's native hook system.

The synchronous gates (§6) need two hook capabilities: a **pre-tool** hook that blocks an action before it runs (the output gate, §6.2) and a **post-tool** hook that holds or replaces a tool result before it reaches the model (the input gate, §6.1). Both are table-stakes for a serious coding agent today and present in every agent Compass targets; an agent missing one can be run and supervised but not fully synchronously gated. Gate coverage tracks hook coverage: where an agent's hooks don't intercept a given tool surface (e.g. web/search calls on some agents), those calls fall through to the async watch (§6.3) rather than the synchronous gate, so the support matrix records which surfaces each agent gates. ACP's `session/request_permission` may surface the same decision to the user as a convenience, but enforcement is always the native hook plus the container floor (§6.7) — never the agent's self-report.

### 5.3 Per-Agent Containers

Each agent runs in **its own container** — a rootless-podman container, built from the project's devenv image (§5.5), that the daemon creates, starts, attaches to, and tears down (§7.1). The container *is* the unit of isolation: a scoped `$HOME` for that agent's credentials, its own process namespace, a default-deny egress allowlist at the container layer (§6.7), the agent's hook configuration mounted **read-only** so it can't disable its own gate (§6.7), and mounts that keep one agent out of another's home and secrets. This is what makes YOLO-by-default safe — a compromised or runaway agent is structurally contained, not merely watched.

**Clone-per-container.** Each agent gets its *own full git clone* of every in-scope repo, created inside the container at startup — not a shared checkout. (A clone, not a host worktree: a worktree's `.git` link assumes a shared on-disk path it can't keep across the container boundary, so each container clones instead.) Clones are node-local, never on a network share. Within the container, each active workstream gets its own **git worktree off that clone** — so an agent keeping one branch in review while coding another has a separate working tree, with its own untracked and generated state, per workstream (the per-workstream isolation §4.1 promises) without a second container.

**Multi-repo workspaces.** Because a project can span several repos (§4.5), the agent's container holds a clone of *each* in-scope repo, presented as one coherent workspace. The agent's branch is named consistently across them, so a workstream that spans repos produces one coordinated ready-for-push (§5.4). A non-repo directory in scope is mounted into the container (snapshotted per workstream, or single-writer) rather than cloned.

**Scoped credentials.** Each container carries only its own credentials, written into the agent's `$HOME` — e.g. a git credential helper in `$HOME/.gitconfig`, never the workspace's `.git/config`. Those credentials belong to the agent's **own forge identity** — a dedicated machine user, not yours (§9) — with no admin rights and branch protection on protected refs: branch protection keeps a push-capable token off `main` (PRs only), and the absence of admin rights keeps it out of repo settings. The host's environment and other agents' secrets never enter an agent's container.

### 5.4 Agent–Dispatcher Channel

Working agents need to talk back to the Dispatcher — to signal that a branch is ready to push, to file a newly discovered issue, to ask a design question, or to update their queue. Compass exposes the Dispatcher to agents as an **MCP server**, configured on each agent's ACP session at setup (ACP carries the client's MCP-server config to the agent). Because the agent runs inside its container, the Dispatcher isn't a host process the agent can launch over stdio: Compass reaches it through a container-local endpoint — a thin in-container shim that relays to the host daemon, or an HTTP/SSE MCP transport the daemon exposes into the container — so the Dispatcher's capabilities appear as native callable tools (`ready_for_push`, `file_issue`, `ask`, `update_queue`). Each call is authenticated to the calling workstream — an agent can signal, file, ask, or update only for itself, never mark another agent's branch ready-for-push or mutate another workstream's queue. `update_queue` is scoped to the agent's own task list — reordering, annotating, or marking its items done — and grants no agent spawning or edit to another workstream's queue; those stay Dispatcher- or user-controlled (§4.2).

For a workstream that spans repos (§5.3), the agent pushes each touched repo and signals `ready_for_push` once; the Dispatcher groups the per-repo branches under the one workstream on the board (§9), tracking each repo's push status, rather than showing N unrelated branches. Cross-repo publishes are not atomic — a push can land in one repo while another is still pending or blocked — so the board reflects per-repo state, with all-or-none cross-repo landing left as a future refinement.

### 5.5 Container images: devenv + the Setup agent

A per-agent container needs the *repo's* toolchain — its language runtimes, build tools, and dependencies — not a generic base. Compass gets that from [devenv](https://devenv.sh): the container image is built from a `devenv` environment, so every agent runs in a reproducible, Nix-pinned image with exactly the toolchain the project declares. The agent and its ACP server are layered on top through its provision step (§5.1).

For a project that spans several repos (§4.5), Compass **composes** the in-scope repos' `devenv` environments into one project image — a single container holding the union of their toolchains — so a cross-repo workstream has exactly one well-defined image rather than an ambiguous per-repo choice.

- **The in-scope repos already have `devenv` configs** — Compass uses them directly; the image is the composed `devenv container build` output, nothing else to author.
- **A repo doesn't** — a built-in **Setup agent** makes it container-ready. The Setup agent runs in a generic **bootstrap base image** — it can't run in the repo's own image, since that image is exactly what it's about to create — and, pointed at the repo, inspects the languages, build files, and dependencies, authors a `devenv` config (`devenv.nix` + `devenv.yaml`), and opens it as a change the user reviews and commits (the same ready-for-push flow as any other agent, §9). Once committed, the repo *is* container-ready, reproducibly, for every agent and every teammate.

**Builds are sandboxed.** Building an image from a repo's `devenv` evaluates and runs repo-controlled Nix *before* the agent's container — and the input gate (§6.1) — exist, so the build itself runs in a throwaway sandbox: no ambient credentials, egress limited to the Nix substituter cache. This holds whether the `devenv` config was just authored by the Setup agent or already shipped in an untrusted repo, so a malicious config can't exfiltrate or tamper at build time.

This is the on-ramp: turning an existing repo into one Compass can run agents against is a single Setup-agent pass, not a hand-written Dockerfile. The Setup agent is transient — it runs at onboarding (or when the environment drifts), unlike the always-on Dispatcher (§4.2) and Warden (§4.3).

---

## 6. Security Model

The security model has two sides — input and output — plus an async watch layer that samples everything in between.

### 6.1 Input Gate (synchronous)

**Trigger**: an agent is about to ingest content it has not seen this session — from a new *trust boundary* (a new repo or directory, a new domain, a new external API response) or a new file/response under a known one. Establishing a boundary is the heavyweight event; within a cleared boundary clearance is keyed on content hash (below), so each distinct file is inspected on first read while re-reading identical content is free. Unread files in a cleared repo are *not* pre-trusted — a known boundary lowers prior suspicion, it doesn't waive inspection of content the agent hasn't read yet.

**Mechanism**: on a post-tool hook, Compass holds the tool-call *result* before it flows to the agent. Warden receives the content first, inspects it, and then either clears it (content flows through to the agent), clears it with a caution attached for the dev agent (a "yellow flag" — e.g. "this repo looks messy but I'm letting it through; keep an eye out" — which turns the dev agent into a second line of defence), or flags it (agent is paused, user is notified).

**Rationale**: prompt injection almost always enters through new untrusted content. Content from sources the agent has already worked with in-session is lower risk. "First contact with a new trust boundary" is a well-targeted gate that catches the primary attack vector with minimal false positives.

**Taint registry**: Compass maintains a per-session registry of cleared sources, but clearance is bound to *content*, not just to a source identifier (domain, repo path, API endpoint). Each cleared read is recorded with a content hash and a time-to-live. A later read flows through only if its content hash still matches and the TTL has not expired; if the content changed or the clearance aged out, the gate fires again. This closes the time-of-check/time-of-use gap where a source serves benign content on first contact to earn clearance, then serves an injection on a later read. In the common case a given file or response is gated once on first read — re-reading already-cleared content (matching hash, valid TTL) is free, and only a content change or TTL expiry re-fires it; a known boundary is never blanket-cleared across files the agent hasn't read. A non-deterministic source — a live API whose envelope differs each call — would never re-match a whole-response hash, so its clearance hashes the *prompt-relevant fields* (the issue title/body/comments — the response text the model will actually read), ignoring volatile envelope noise like timestamps or request IDs; the gate re-fires whenever that extracted content changes, so an attacker can't swap injected text under a stable JSON shape within the TTL, and a short TTL bounds the window regardless.

### 6.2 Output Gate (synchronous)

**Trigger**: an agent is about to execute a high-stakes, hard-to-reverse action:

- A publish to a remote — `git push` (or a stack tool such as `gt submit`, where the agent uses one) — the publish is what leaves the machine, while a local `git commit` is not gated here
- A remote merge or merge-queue action — `gh pr merge`, enabling auto-merge, or an admin merge — which ships code past the Compass PR panel just as a push does, and is gated the same way
- A non-VCS remote state change to an external system — a registry publish (`npm publish`), an infrastructure or cluster mutation (`terraform apply`, `kubectl apply`/`delete`) — hard to reverse even though it is neither a push nor a merge; gated as high-stakes once the command is recognized, since a destination being already warmed doesn't make the mutation safe
- First-contact outbound sends — a tool call opening a connection to a destination not yet cleared this session, held by the pre-tool hook and vetted for secret or credential leakage before the call runs; later sends to an already-cleared destination aren't re-gated. This scopes by destination, not payload: exfiltration to an already-warmed host is caught by the async watch (§6.3), and any destination off the container's egress allowlist (§5.3) is unreachable in the first place; payload-level inspection of an allowed destination is part of the hardening layer (§6.7)
- Credential or secrets access *via a tool call* — the container starts scrubbed, holding only the agent's own scoped credentials (§5.3), so there is no inherited host secret to read (§6.7)
- Subprocess spawning outside the workspace

**Mechanism**: same as the input gate, on the pre-tool hook — Compass holds the tool call before it executes, Warden inspects, and clears or flags it. Because the hook fires before execution, a flagged push, fetch, or command never runs. A send or publish names a *target*, not the bytes (`git push` names refs; `curl -F file=@secret` or `scp path host:` names a path), so Compass resolves the outbound payload — the commit-range diff, the file contents, and for a push the ref update plan itself (old/new SHAs plus any `--delete`, `--force`, or mirror flags, which carry little diff but high blast radius) — and passes it to Warden alongside the command; where the payload can't be resolved Warden falls back to command-shape heuristics plus the async watch (§6.3). A command the agent issues via ACP `terminal/create` (§7.4) is caught from the other side: Compass is the executor, so it runs this same output-gate check before launching the command — a structural gate that doesn't depend on the agent's hook firing, holding an ACP-issued `git push`, `curl`, or `kubectl apply` for the Warden pass just as the hook does.

**Rationale**: these are the actions where a compromised agent can do real damage. Holding them for a Warden pass adds negligible latency in the common case (no threat detected) and catches exfiltration and unintended pushes in the uncommon case. For a remote publish specifically, the agent's pre-tool hook on the `git push` (the mechanism above) holds the push for the Warden pass — clear and it proceeds, flag and it's blocked and the agent paused. The push runs under the agent's own scoped forge identity (§9), so the structural backstop on what reaches a protected ref is branch protection, not the hook: even a missed flag lands only on a feature branch behind a required PR review.

**Enforcement boundary**: the gate inspects the *tool call*, so it can block a flagged `git push`, fetch, or `curl` before it runs. What a hook cannot see is egress synthesized inside an already-approved subprocess (a `python` or `node` process that opens its own socket at runtime) — but the container can: the per-agent container's default-deny egress allowlist (§5.3) bounds *every* outbound connection, hook-mediated or not, to a fixed set of destinations, so a subprocess can't reach an arbitrary host even when no hook fires. Hooks cover tool-call-level intent; the container's allowlist is the structural floor beneath them. Inspecting *payloads* to an allowed destination is the remaining hardening-layer concern (a mediating proxy for high-assurance deployments).

### 6.3 Async Watch Layer

Everything not covered by the two gates runs freely. Warden *samples* the action stream — at tool-call boundaries and on a heartbeat (below), not as a continuous tap — and can pause an agent the moment a sample trips a threat mid-session; coverage between samples is best-effort, which is why the irreversible actions live behind the synchronous gates rather than the watch.

**Warden triggers** (async, non-blocking):

- Tool call result boundaries — Warden reads results as they arrive, not the full token stream. For an oversized result (a large test/build log, a generated bundle) Warden inspects the whole payload by windowing rather than reading it in one call, and the agent never receives more than Warden has inspected — any remainder Warden hasn't covered is withheld or summarized for the agent too, so an attacker can't bury instructions in a tail the agent sees but Warden doesn't
- New-looking bash commands (heuristic, tunable)
- Error outputs — a common injection vector
- Periodic heartbeat: Warden samples each agent's recent tool output on a configurable interval — by default on the order of 1,000 tokens per agent every ~2 minutes — to catch anything suspicious between tool-call boundaries

**Token efficiency**: Warden never reads the full token stream of all agents. It reads at tool call boundaries and via the heartbeat. In the common case this keeps token consumption proportional to the number of tool calls, not agent verbosity; an oversized result adds bounded windowed passes, but coverage is never traded for cost — the agent sees only what Warden has inspected.

### 6.4 Warden's Prompt Injection Resistance

Warden's primary input is content flagged as potentially malicious — exactly the content a prompt injection attack would use. This creates a natural resistance: the model's entire attention is tuned to "does this look like an attempt to manipulate agent behavior?" Any injection attempt in Warden's input is simultaneously the attack and the evidence.

Even in the unlikely scenario where Warden is partially deceived, its actuator surface has no capabilities an attacker would want to drive (§4.3). The failure modes are not symmetric, though. A false positive (an agent paused unnecessarily) is a friction cost, not a breach. A false negative (a real injection not flagged) is different: the compromised agent retains its full tool surface — filesystem, network, subprocess — and can proceed with exfiltration or a destructive action before an async trigger catches it. Warden lowers the probability and the dwell time of a successful injection; it does not drive the residual risk of a missed one to zero. That residual risk is exactly why the output gate (§6.2) and branch protection on the agent's forge identity (§9) exist as independent backstops on the highest-stakes actions.

### 6.5 Warden Implementation

Warden runs on seal's existing WASM agent loop. The WASM sandbox is appropriate for Warden because:

- Warden has no need for OS-level syscalls — it reads logs and fires a narrow IPC signal
- The sandbox provides isolation from the agents it's watching
- The agent loop handles LLM interaction, tool dispatch, and turn management — all directly reusable

The only new implementation required is:

- The `pause_agent` IPC channel out of the sandbox — suspending the agent *and its active process tree*, so an in-flight `bash`/`python` child can't keep running (or finish deleting or exfiltrating) after a pause. A process that fully detaches — double-forking or `setsid`/`nohup` to reparent under init — leaves that tree, but it cannot leave the agent's container: the container is the process-namespace and cgroup boundary (§5.3), so pausing or killing the container stops the whole tree, detached children included
- The tool call event feed into Warden's context
- The taint registry (session-scoped, in-memory)
- The synchronous gate decision channel — the path by which Warden returns clear / caution / flag for a held tool call, so the hook can proceed, block, or attach the yellow-flag note
- A pre-inference secret classifier/redactor on the hosted-model path, so secret- or credential-bearing payloads are redacted or routed to local inference before any hosted-model call (§4.3)

### 6.6 Defaults and Cost

Warden ships **on by default** — it's the core differentiator, not an opt-in extra — and its state is always visible in the UI. A user can turn it off entirely; an enterprise can tighten it (custom rules, blocked domains, a required local model, or a policy that keeps it on — §11).

Cost is kept proportional to activity. Warden runs on a small, fast model (Haiku-class, or the equivalent for other providers) and spends tokens on the synchronous gate checks plus the async-watch triggers (tool-call boundaries and the heartbeat); a trusted, already-cleared source still avoids an extra input-gate model call (§6.1). For a developer already paying for a coding-agent subscription the added cost should stay modest; the heartbeat interval, sample size, and trigger set are tuning parameters to settle against real sessions (§13).

### 6.7 Threat Model and Limitations

Compass enforces in **three tiers**, and only the bottom two are load-bearing. (1) The per-agent **container** (§5.3) is a structural sandbox that bounds what a compromised worker can reach at all. (2) Warden's gates run on each agent's **native runtime hook** (§5.2): a deterministic, runtime-enforced intercept on the tool-call path that the model can't skip by going quiet and can block before execution. (3) ACP's permission/reporting surface drives the user-facing approve/deny prompt and activity view, but is **cooperative** — the first thing a compromised agent does is stop reporting — so it is UX, **never** a security boundary. The container contains; the native hook gates; ACP only shows. Being explicit about what each does — and where the limits still are:

**What the container contains at baseline.** Several risks that would otherwise need an opt-in hardening tier are handled structurally by the per-agent container:

- **Blast radius.** A fully compromised worker is confined to its own container — its own clone, its own scoped `$HOME`, its own process namespace. It cannot reach another agent's home, secrets, or working tree.
- **Inherited secrets.** The container starts from a scrubbed environment holding only the agent's own scoped credentials (§5.3), so there is no host token, cloud key, or SSH-agent socket to inherit and leak.
- **Egress.** A default-deny allowlist at the container layer bounds *every* outbound connection — hook-mediated or synthesized inside a subprocess — to a fixed set of destinations, so exfiltration to an arbitrary host is blocked even when no hook fires.
- **Detached processes.** A double-forked or `setsid`/`nohup`'d process reparents under the container's init, not the host's, so pausing or killing the container stops the whole tree (§6.5).

**What the container does not solve** — the residual limits Warden and the opt-in hardening tier address:

- **Hook integrity (BYOA agents).** The synchronous gates run through the agent's own hook configuration. Compass mounts that configuration read-only into the container so the agent can't rewrite its own gate, but on the hook-gated path the gate's *logic* still rides the agent's runtime. A first-party agent like **OMP** (§5.1) has no such surface — its gate lives in the runtime's tool-dispatch path, which the agent cannot rewrite.
- **Payload-level egress.** The container's allowlist bounds *which* destinations are reachable, not *what* bytes leave to an allowed one (e.g. crafted data to an allowlisted `github.com`). Inspecting payloads to allowed destinations is a hardening-layer proxy concern.
- **Effect-scoped heuristics.** Triggers keyed on where a command runs or which destination it contacts are heuristics. A `curl … | bash` or fetch-and-exec one-liner runs untrusted bytes inside a subprocess before the input gate inspects a result, so Compass treats pipe-to-interpreter, fetch-exec, and package-install patterns as gated commands; the container bounds their reach, but the gate still can't see inside the subprocess.
- **Warden's outputs are data, not authority.** The yellow-flag note Warden hands a dev agent, and the messages a worker sends the Dispatcher over the MCP channel (§5.4), are untrusted, non-authoritative input — never instructions. A worker's `ready_for_push` is only a notification (§9) and triggers no privileged action; the agent's actual push is still vetted by the output gate (§6.2), so the channel grants nothing the gate doesn't already mediate.
- **Dispatcher as an injection target.** The Dispatcher ingests issue text and worker reports — untrusted content — while owning task queues, assignment, and agent spawning. That content passes the input gate (§6.1) like any other untrusted source, and its high-stakes actuators (spawn, assignment, queue reorder) are user- or policy-gated rather than model-discretion, so a poisoned ticket can reorder its suggestions but can't unilaterally escalate.
- **Persisted history.** A persistent agent's history can outlive the session taint registry, so raw tool outputs cleared once must not silently re-enter a later context. Persistent history keeps trusted summaries and audit metadata, not unvetted raw payloads; any raw content reloaded into the model is re-gated (§6.1).

This is consistent with §2.3: the container sandbox is *transparent* — the agent runs YOLO inside it, with no permission dialogs or whitelist management facing the developer — so the friction-free baseline holds, now with a structural floor under it. The opt-in hardening tier (§11) adds what the baseline container doesn't: a payload-inspecting egress proxy, stricter egress policy, a required local Warden model, and longer audit retention for high-assurance (Enterprise/Federal) deployments.

---

## 7. Tech Stack

### 7.1 Architecture: daemon, shell, UI

Compass is a persistent local **daemon** with a thin desktop **shell** that renders the **UI**; the two communicate over a single typed contract (§7.2).

- **Compass daemon (Rust)** — the backend. It owns everything privileged: the per-agent container lifecycle (rootless podman; images built from each repo's devenv environment, §5.5), agent process management, PTYs, the Warden security layer, hook interception, the taint registry, SSH session management, and issue-tracker/VCS integration. It is long-lived and outlives any UI session — which is what makes the always-on Dispatcher (§4.2) and Warden (§4.3) possible. It runs each agent in its own container and **drives it as an ACP client** — newline-delimited JSON-RPC over stdio across the container boundary (§5.1) — running Warden's gate on the agent's native runtime hooks (§5.2) and attaching the Dispatcher to each agent's ACP session as an MCP server (§5.4); and it exposes one gRPC contract, `compass.v1`, upward to whatever renders the UI, over a platform-appropriate local transport (a Unix socket on macOS/Linux; see §7.3 for Windows).
- **Desktop shell (Tauri)** — a near-empty shell. It launches and supervises the daemon, bridges the system webview to the daemon over its local transport (§7.3), and provides the native-app surface: window, tray, OS notifications (e.g. a Warden pause, §6.5), deep links, and a signed installer with auto-update. It holds no backend logic — the daemon does — so there is no privileged surface in the shell for a compromised agent to subvert.
- **UI (web)** — the panels: the Bridge swimlane board (§4.4), agent status, audit log, diff viewer, PR review, and CI status, rendered in the webview as a `compass.v1` client. In development the same UI runs in a plain browser against a loopback Connect/gRPC-Web endpoint for fast iteration; the shipped path is the shell over the daemon's local transport, so no localhost TCP port is exposed on macOS or native Linux — Windows is the exception (a token-guarded loopback into WSL2, §7.3).

### 7.2 The UI/backend contract (`compass.v1`)

The contract is the seam that keeps the UI swappable (§7.5), so it is defined as the single, sole, and owned door between any UI and the daemon:

- **Singular** — one gRPC service: typed request/response commands plus a server-streaming event channel for board, agent, and audit updates, served over Connect so the same contract reaches native clients (gRPC over the local transport) and the browser (gRPC-Web) alike. The daemon translates each agent's **ACP session updates** — tool calls, diffs, plans, permission prompts, terminal output — into the `compass.v1` event surface, assigning each a monotonic sequence number for gap-free resubscribe (ACP's `session/update` carries none), so the UI renders structured agent activity without ever speaking ACP directly.
- **Sole** — the contract lives in one crate — the schema plus the per-language clients generated from it (TypeScript for the web UI, Rust for the GPUI app, §7.5) — and a generated client is the only sanctioned way to reach the daemon; raw gRPC stubs are never called directly. The crate depends on no UI, transport, or shell code, and the shell's webview-to-socket bridge is pure transport with no command of its own, so logic cannot accrete there.
- **Owned** — the generated clients are checked in and CI-verified against the schema (drift fails the build); breaking-change detection gates incompatible contract edits; the contract crate is review-owned; and the frontend lint config bans raw socket or stub access outside the generated client, so "just one untyped call" cannot land.

The point is that the boundary is enforced by the compiler and CI, not by convention — it holds even with many agents building the app in parallel.

### 7.3 Desktop shell: Tauri (not Electron)

Tauri over Electron follows from the thin shell: a bundled Node backend — Electron's main draw — is dead weight when the daemon is the backend, while Tauri's lean system-webview footprint (megabytes, not ~100MB), Rust alignment with the rest of the stack, and hardened capability-scoped IPC all fit. The cost is the system webview's per-platform variance — WebKitGTK on Linux is the known weak point, and Linux is a priority target (§13) — so the web UI stays conservative (no bleeding-edge CSS or GPU features) while it is webview-hosted. **Cross-platform**: the daemon runs natively on macOS and Linux and, on Windows, under WSL2 (see Windows deployment, below), while the Tauri shell is native on all three (Windows via WebView2). macOS and Linux are the day-one targets; Windows support rides this same stack rather than the native path (§7.5), with its remaining terminal/PTY, SSH, and WSL-bridge validation tracked as an open question (§13). Critical for users who run agents natively on Linux boxes, not just SSH from a Mac.

**Windows deployment.** The least-resistance path is the Tauri shell running natively on Windows (WebView2) with the daemon in **WSL2**, where it keeps its native Linux process/PTY/agent environment and sits next to the agents themselves — where developers running CLI agents on Windows already work. One consequence: AF_UNIX sockets do not cross the WSL2 VM boundary, so on this path the UI↔daemon hop uses a token-guarded loopback port (WSL localhost forwarding) or a named-pipe relay rather than the Unix socket; macOS and native Linux keep the socket-only, no-TCP posture (§7.1).

### 7.4 Rendering: native UI from ACP, xterm.js for command output

Agents are driven over ACP, so Compass renders each agent's activity — thinking, tool calls, diffs, plans — as **native UI** built from the structured event stream, not by embedding or scraping a terminal. This is the point of going ACP-first: the agent surface is Compass's to design (the swimlane board §4.4, diff cards, plan timelines), not a reskin of any one agent's TUI. **xterm.js is retained for command-execution terminals**: when an agent runs a command — ACP's `terminal/create`, which Compass executes inside the agent's container, gating it first via the output gate (§6.2) since Compass is the executor, and streams back — its live output renders in an xterm.js pane, alongside a raw-shell escape hatch for direct container access. xterm.js is the same well-understood, performant approach used across the ADE field, and it avoids the uncharted territory of libghostty in a webview (no existing integration, unstable API, GPU-surface incompatibility); libghostty — or the native terminal of the GPUI path (§7.5) — is a potential future upgrade if terminal performance becomes a documented user pain point.

### 7.5 Native path: GPUI

The web-in-Tauri stack is the validation vehicle, not the destination. The differentiated bet is a native, GPU-accelerated Rust UI built on GPUI — the same class of stack as Zed — giving Compass a native/performance edge over the uniformly web-based ADE field and a single Rust line from the UI through the daemon to the seal runtime and Warden. It is deferred rather than adopted now because GPUI is pre-1.0 (frequent breaking changes, documentation still in progress) and its Windows support is early, whereas Tauri ships all three desktop platforms today. The `compass.v1` contract is what makes the eventual move tractable: a GPUI app is simply another client of the same daemon over the same local transport — just as the standalone seal TUI and Compass's own native surfaces are two clients of one seal runtime. Going native is therefore a reskin against a stable contract, not a rearchitecture; the daemon, the contract, and the validated UX all survive the swap.

### 7.6 SSH

Agents can run on remote machines over SSH. The daemon manages SSH connections; each agent's container runs on the remote host (the same per-agent model, §5.3). The agent's ACP server runs in that remote container, and Compass **tunnels its ACP stdio stream over the SSH channel** — its own transport, so remote agents never wait on ACP's draft HTTP/WebSocket transport; ACP is transport-agnostic and rides whatever bidirectional channel Compass provides. Command-terminal output (§7.4) streams back the same way. Warden receives tool-call events from remote sessions via the same channel, with the Compass agent-side component running on the remote machine and relaying events. The channel is bidirectional: a `pause_agent` signal (§6.5) travels back over the same link to the remote agent-side component, which stops the remote container's process tree — so Warden's pause capability covers remote sessions too, subject to the trust assumption below.

**Event integrity over SSH.** Warden's view of a remote session is only as trustworthy as the event stream relayed to it. SSH provides confidentiality and integrity for that stream *in transit*, but the remote agent-side relay itself sits inside the trusted computing base: a fully compromised remote host could forge or suppress the events Warden inspects. This touches both layers — the async watch can be blinded, and because the same relay backs the synchronous gates' hold-and-proceed step, the gates' *blocking* guarantee also weakens on a compromised host. For v0 this is a documented limitation: local execution is the fully trusted path, and remote SSH carries this residual trust assumption. A signed, sequence-numbered event channel detects *in-transit* tampering, but not suppression at the source — a compromised host can drop events and keep the sequence consistent — so true remote integrity needs the relay attested or moved outside the host's trust boundary. That deeper integrity model is tracked as hardening for high-assurance (Enterprise/Federal) remote execution.

---

## 8. Workflow: Linear → Workstream → PR → Merge

The end-to-end flow Compass is optimised for:

1. **Issue intake**: the Dispatcher reads open issues from Linear (filtered by team, priority, assignee). User selects issues for the current session.
2. **Assignment**: the Dispatcher proposes which named agent should take each issue, applying the conflict map to avoid zone collisions. User approves or adjusts.
3. **Workstream start**: the Dispatcher writes the per-agent task spec and hands off; Compass provisions the agent's container and clone (§5.3). The agent picks up, creates a branch, starts work in YOLO mode. Warden begins watching the session.
4. **Build and test**: agent works freely. Warden watches. Input/output gates fire on first contact with new sources and on irreversible actions.
5. **Ready for review**: when work is complete and local checks pass, the agent pushes its branch under the agent forge identity (§9) and signals *ready-for-push* to the Dispatcher (§5.4) for the board. Compass opens and surfaces the PR and its diff in the review panel; when Warden is enabled, it vets the push on the way out (§6.2).
6. **Iteration**: reviewer leaves comments. The Dispatcher hands the feedback back to the agent as a structured re-load prompt. Agent addresses feedback with new commits on top (no squashing of pushed commits).
7. **Merge**: user merges from the Compass PR panel. Merge detection handles queue-based merges where the source PR ends up `CLOSED` with no merge commit on it (e.g. a squash-via-draft-PR merge queue) — confirming via the merge-activity trail or trunk history rather than the PR's merged-state alone. The Dispatcher then updates the workstream state and proposes the next pickup from the backlog.

---

## 9. VCS

### Version control

Agents use **plain git** inside their containers. Each agent has its own full clone; a new workstream is a git branch in its own worktree off that clone (§5.3), and the agent commits, branches, and **pushes with ordinary git**, authenticating as a dedicated agent forge user (Publishing, below). There is no per-agent "VCS mode" to configure — the container's clone *is* the isolation, so Compass manages no host-side worktree layer and drives no jj working copy on the agent's behalf.

**Your review tool is your choice.** Because agents publish ordinary git branches and PRs, you review and land them however you already work — plain git, [jj](https://jj-vcs.github.io/jj/), or a stacking tool like [Graphite](https://graphite.dev) — locally, against the same refs. Compass never requires any of them: jj and Graphite are conveniences you opt into, not dependencies imposed on your team. Compass surfaces the same abstractions regardless — branch, diff, PR, CI status — so the rest of the product works identically whatever you review with. Merge detection (§8) is stack-aware where you use Graphite, but never depends on it.

### Publishing

Agents push their own branches — Compass does not route the publish through a supervisor. What keeps that safe is **where the push authenticates from**, not an in-process gate:

**Agent forge identity (recommended setup).** Run agents under a dedicated forge account — a `*-agent` machine user — separate from your own. Commits keep *your* name and email (authorship is independent of the push identity, so history still reads as your work), but the agent account holds only **scoped** access: membership on the in-scope repos, **branch protection (or rulesets) on every publish-sensitive branch** — `main`, release and deploy branches — requiring a reviewed PR with no direct pushes, **tag rulesets** that block the agent from creating, updating, or deleting protected tags, and no admin rights. A compromised or runaway agent is then confined to the same review boundary as any outside contributor — it can push feature branches and open PRs, but it cannot land on a protected ref, change repo settings, or act with your privileges. This is the structural control, enforced at the forge, holding whether or not any Compass component sits in the path.

**Ready-for-push is a signal, not a gate.** An agent still emits *ready-for-push* over the agent↔Dispatcher channel (§5.4) so the Dispatcher and Bridge keep the board current and coordinate across agents — but it's a notification: the agent doesn't wait on the Dispatcher to publish, and the Dispatcher doesn't execute the push.

**Warden still inspects pushes.** The **agent's pre-tool hook on the `git push`** (the output gate, §6.2 — an in-process intercept Compass controls, not git's `pre-push` hook, which `--no-verify` would skip) holds the push for a Warden pass: clear and it proceeds, flag and it's blocked and the agent paused. That is the behavioral layer; **branch protection is the structural backstop** beneath it, so a push Warden mis-clears — or one made while Warden is turned off (§6.6) — still reaches only a feature branch behind a required review, never a protected ref.

**Deferred:** a Dispatcher-executed, gated or timed publish path — queuing pushes for manual approval or a drain window — is **not** in the MVP. It may return as an opt-in policy for teams wanting centrally-timed or centrally-gated publishing; the per-agent forge account plus branch protection covers the solo and small-team case without it.

---

## 10. Naming

| Name | Role |
| --- | --- |
| **Compass** | The ADE — the product |
| **Dispatcher** | Built-in supervisor agent — schedules work, manages workstreams, and coordinates publishing |
| **Warden** | Built-in security auditor — watches all agent sessions, runs the gates, can pause any agent |
| **Setup agent** | Built-in onboarding agent — authors a repo's `devenv` config so Compass can build per-agent container images (§5.5) |
| **Bridge** | The primary UI panel — the board where the user talks to the Dispatcher |

Agent names (Dispatcher, Warden, and the named workstream agents) are customizable; the defaults ship as Dispatcher and Warden, with workstream codenames drawn from a selectable theme (§4.1).

---

## 11. Business Model

Compass is OSS. Getting traction in the ADE market requires it — Orca, Emdash, and Agent Deck are all free and open source, and developer tools that aren't free don't get adopted. The OSS ADE drives developer adoption; the enterprise layer is what generates revenue.

### Compass OSS (free)

The full ADE: the Dispatcher, Warden with a capable small model (Haiku-class), per-agent container isolation with devenv-built images, the Setup agent, all agent integrations, SSH, PR review, CI status. Everything a developer needs to run parallel agents safely.

### Compass Team / Enterprise (paid, per-seat)

Features that individual developers don't need but security-conscious companies do:

- **Centralized Warden audit logs** — org-wide view of what every developer's agents did, what was flagged, and when, surfaced in a web dashboard. For local/on-prem deployments the log sink is self-hosted and payloads are redacted/minimized before upload, so the audit path doesn't reintroduce the egress on-prem Warden exists to avoid.
- **Compliance export** — structured export of agent action logs for audit purposes. SOC 2, HIPAA, internal compliance requirements.
- **Org-level policy management** — custom Warden rules, blocked domain lists, org-approved taint registries pushed to all developer instances, and a policy that keeps Warden on (or records/blocks work while it's off) so managed audit and compliance coverage has no gaps.
- **Shared taint registry** — team-wide cleared sources and blocked domains, rather than per-session per-developer. Shared entries carry the same content-hash + TTL binding as the per-session registry (§6.1), so a team clearance still re-gates on a content change or expiry; org admins own the registry's contents and its update policy.
- **Upgraded Warden model** — Opus-class model for higher-stakes environments where false negative cost is high.
- **Local / on-prem Warden** — run Warden on a self-hosted or on-prem model so the content it inspects never leaves your infrastructure (no model-provider egress). The required posture wherever audited content cannot be sent to a third-party model.
- **Network egress proxy** — payload-level inspection of outbound traffic, including egress to already-allowed destinations and connections opened inside subprocesses, layered over the baseline container's default-deny destination allowlist (§5.3). The hardening-layer control §6.2 and §6.7 reference.
- **SSO and access management**.

### Compass Federal (custom pricing)

The security-first story maps directly onto federal procurement requirements. An ADE with a built-in agent auditor that produces a structured action trail is genuinely novel in the federal tooling space. These are target capabilities on the Federal roadmap, not current certifications:

- FIPS-compliant deployment (planned)
- CMMC alignment (planned)
- Post-quantum considerations, building on existing Sealed Federal work
- On-premises deployment, including local Warden
- Dedicated support

### Go-to-Market Motion

Developers adopt Compass OSS because it's the best way to run parallel agents. Warden surfaces in incident postmortems — "it caught X before the agent pushed it." Security teams at those companies hear about it. Security teams pay for the enterprise audit layer.

The two buyers are different: developers want productivity, their employers want oversight. Compass sells productivity to developers and security to their employers. These are not in tension — the same product serves both.

---

## 12. Positioning

**For**: developers who already run Claude Code, Codex, or similar CLI agents and want to run more of them, faster, without giving up the productivity of YOLO mode.

**Against**: every other ADE in the market offers the same thing — parallel agents, worktree isolation, YOLO mode. None of them have a security layer. Compass is the only ADE where you can run YOLO agents and know something is watching.

**Not for**: enterprise security teams wanting compliance dashboards as a standalone product. Compass is developer-first; the enterprise layer sits on top of a tool developers already chose, not a tool pushed down from security teams.

---

## 13. Open Questions

- **Warden model selection**: what model runs Warden by default? Needs to be fast (low heartbeat latency) and capable (catch subtle injection). A capable small model (Haiku-class) is likely the right default, with user override.
- **Taint registry persistence**: should cleared sources (content-hash + TTL entries) persist across sessions, or reset each session? Per-session is safer; persistent reduces friction for known-good repos. Probably user-configurable.
- **Dispatcher model**: what model runs the Dispatcher? Mid-tier default (Sonnet-class), user-upgradeable for complex multi-agent coordination.
- **Repo-scoped conflict map**: multi-repo workspaces are first-class (§4.5, §5.3); what remains open is the Dispatcher's conflict-map granularity across repos — per-repo file zones vs. cross-repo zone tracking — and how it scopes advice when a workstream spans codebases.
- **Windows support**: the deployment model is the Tauri shell on Windows with the daemon in WSL2 (§7.3). Remaining open: validating Windows terminal/PTY and SSH behaviour, and the WSL-bridge transport. Linux is higher priority than Windows for the initial release, and the native (GPUI) path has a later Windows timeline (§7.5).
- **Pause UX**: when Warden pauses an agent, what does the developer see? Needs to be informative (here's what was flagged, here's why) without being alarming. The audit log panel is the right surface for this.
- **False positive rate**: a Warden that cries wolf constantly is worse than no Warden. The heartbeat interval, trigger heuristics, and model prompt all need tuning against real agent sessions before shipping.
- **Warden token budget**: the heartbeat interval, per-agent sample size, and trigger set determine Warden's always-on token overhead. They need tuning against real sessions so the cost stays acceptable for a developer already on a paid agent subscription.
- **Setup-agent coverage**: how much of a repo's `devenv` config can the Setup agent infer (languages, build tools, services) versus prompt for? Polyglot repos, monorepos, and toolchains that aren't Nix-friendly are the hard cases (§5.5).

---

## 14. Planned, Designed Later

- **Real-time human collaboration in the ADE.** Bringing human-to-human and human-to-agent collaboration directly into Compass — rather than routing it through a separate tool like Slack (the "@agent do X" pattern) — is a planned direction. It is not a v0 feature and won't be built first; its design is deferred until after this document lands.
