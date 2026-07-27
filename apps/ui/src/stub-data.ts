// Dev-only stub data for the Compass ADE UI.
//
// Compass is an Agentic Development Environment: a persistent daemon (compassd)
// with a Tauri shell rendering this web UI, meeting at the compass.v1 gRPC
// contract (docs/specs/product/compass.md). The real board / agent / ACP /
// audit event payloads are not built yet — the daemon today reports liveness and
// a daemon-status stream — so this module hand-fakes a representative fleet so
// the full interface is explorable in `vite dev` with no daemon and no Tauri IPC.
//
// Everything here is a plain in-memory fixture. When the daemon grows the board,
// agent-runtime (ACP over compass.v1, design record compass-0.4), and audit
// streams, this module is deleted and the components read the generated
// @compass/client instead — the shapes below intentionally mirror that eventual
// contract. The data is drawn from a real multi-agent wave so it reads true.

// ── Enums ──────────────────────────────────────────────────────────────────

/**
 * Where a workstream sits in its lifecycle (design D1):
 * Backlog → Todo → Queued → Blocked ⇄ In Progress → In Review → Done. The board
 * shows the active subset (constants `BOARD_LANES`); Backlog + Todo are the
 * pre-active tier surfaced in the Backlog view.
 */
export type WorkstreamState =
	| "backlog"
	| "todo"
	| "queued"
	| "blocked"
	| "in_progress"
	| "in_review"
	| "done";

/** What the running agent process is doing — the dot beside the agent icon
 *  (design D9). A UI projection over the daemon's coarse `AgentSessionState`
 *  (#443) plus event-stream refinements; `waiting`/`done`/`paused` are UI-only
 *  (see agent-state.ts). This is the *process* axis, distinct from a
 *  workstream's `blocked` (the *task* axis). */
export type AgentState =
	| "working"
	| "idle"
	| "waiting"
	| "done"
	| "paused"
	| "stopped"
	| "error"
	| "disconnected";

/** The kind of agent — the moat agents plus leveraged worker agents. */
export type AgentRole = "supervisor" | "warden" | "worker";

/** Priority of a workstream, drives the card accent. */
export type Priority = "urgent" | "high" | "medium" | "low";

/** An issue tracker Compass projects workstream state onto (D2). Linear first;
 *  the shape is tracker-agnostic for Jira/GitHub later. */
export type TrackerKind = "linear" | "jira" | "github";

// ── Board ──────────────────────────────────────────────────────────────────

/** A pull request attached to a workstream, for the right-sidebar PR pane. */
export interface PullRequest {
	number: number;
	title: string;
	state: "draft" | "open" | "merged" | "closed";
	/** CI check-runs, newest per workflow. */
	checks: { name: string; status: "success" | "failure" | "pending" }[];
	/** Review-thread resolution, across all bots + humans. */
	threads: { resolved: number; total: number };
	/** Bot reviews, for the PR pane summary. */
	reviews: { bot: string; verdict: "approved" | "changes" | "commented" }[];
}

/** A commit on a workstream's branch, for the VCS pane's commit history. */
export interface Commit {
	/** Short SHA. */
	sha: string;
	subject: string;
	author: string;
	/** Wall-clock or relative time, matching the feed's `at` style. */
	at: string;
}

/** A single unit of work on the Bridge board: an issue promoted to a workstream. */
export interface Workstream {
	id: string;
	/** The source issue reference (tracker id). */
	issue: string;
	title: string;
	state: WorkstreamState;
	priority: Priority;
	/** The agent id currently on it, or null when unassigned. */
	assignee: string | null;
	/** A one-line summary of the latest activity, for the card. */
	summary: string;
	/** The feature branch, for the VCS pane + branch dropdown. */
	branch: string;
	/** Files-touched / diff size, for a quick sense of scope. */
	changed: { files: number; additions: number; deletions: number };
	/** The attached PR, or null before one is opened. */
	pr: PullRequest | null;
	/** Recent commits on `branch`, newest first, for the VCS pane's commit
	 *  history (design D5's "history = commits menu at the bottom of VCS"). */
	commits?: Commit[];
	/** The linked tracker issue (D2), if any — the projection target. */
	tracker?: TrackerRef;
	/** Set by the archive action (T5) when a Done workstream is cleared; a
	 *  marker, not a delete — the Done view still lists it. */
	archivedAt?: string;
}

/** The tracker issue a workstream is linked to — the projection target (D2).
 *  Compass state is canonical; this carries the tracker's *native* status. */
export interface TrackerRef {
	kind: TrackerKind;
	/** The tracker's native issue id, e.g. "SEA-1042" (mirrors `issue`). */
	id: string;
	/** The tracker's native status name in the user's org. */
	status: string;
	url: string;
}

/** A user-editable projection between Compass state and a tracker's native
 *  statuses (D2). `toTracker` is total over Compass states; `fromTracker` is
 *  many-to-one (e.g. Linear's Cancelled + Duplicate both read back as Done). */
export interface TrackerStatusMapping {
	kind: TrackerKind;
	/** Compass state → the tracker's status name in this org. */
	toTracker: Record<WorkstreamState, string>;
	/** Tracker status name → Compass state (many-to-one). */
	fromTracker: Record<string, WorkstreamState>;
}

/** The user's tracker wiring (design T11): which tracker, the user's identity
 *  on it, and the Compass↔tracker projection. Edited in the Settings screen. */
export interface TrackerConfig {
	kind: TrackerKind;
	/** The user's tracker handle/identity, for listing their assigned issues. */
	handle: string;
	mapping: TrackerStatusMapping;
}

// ── Agents ───────────────────────────────────────────────────────────────

/** How far a plan step has progressed — mirrors the contract's
 *  AgentPlanEntryStatus (comms.proto AgentPlan, reused from #443). */
export type PlanStepStatus = "pending" | "in_progress" | "completed";

/** One step in an agent's execution plan. */
export interface PlanStep {
	content: string;
	status: PlanStepStatus;
}

/** One selectable answer to an Ask — mirrors the contract's AskOption
 *  (comms.proto): an id, a label, and optional explanatory text shown under the
 *  label. Deliberately carries no permission-outcome semantics — permission
 *  gating is a separate, deferred concern (agents run in containers with
 *  prompts disabled), intentionally absent from the contract (design D5). */
export interface AskOption {
	id: string;
	label: string;
	/** Optional explanatory text shown under the label. */
	description?: string;
}

/** A terminal open next to an agent (dev server, tests, shell). */
export interface Terminal {
	id: string;
	name: string;
	running: boolean;
	/** Fake scrollback, most-recent last. */
	lines: string[];
}

/** Durable comms identity (SubscribeComms · Postgres). The agent-kind arm
 *  gains an additive homeChannelId mirroring ratified 0.6 RT-2
 *  (`../compass-0.6/design.md:1760-1764`); the proto landing of
 *  `home_channel_id` on AgentAccount is the comms-server lane (SEA-1195). */
export interface Account {
	/** Account id, e.g. "acc-cook" — the one id space. */
	id: string;
	/** Unique handle, e.g. "cook". */
	handle: string;
	displayName: string;
	kind: "user" | "agent";
	/** Agent kind: the owning user's account id. */
	ownerUserId?: string;
	/** Agent kind: the agent's home DM (RT-2). */
	homeChannelId?: string;
}

/** The agent's ephemeral lifecycle — SubscribeEvents.AgentSessionStatus.state
 *  (`compass.proto:126-129`), keyed by account id. Absent = created but no
 *  session has run. This is the ONLY agent-object field SubscribeEvents feeds. */
type AgentLifecycle = AgentState;

/** The composed roster view-model the store assembles at the seam — NEVER a
 *  wire shape. `account` is durable; `lifecycle` is optional (honest for an
 *  unstarted agent); role/model/cwd are UI-only roster config, terminals is
 *  pure fixture (no terminal stream in the MVP). The typed OMP session trace is
 *  NOT here — it is a separate type (`AgentSession`, session-events.ts) read by
 *  account id via `store.agentSession()`, folded and rendered by Compass. */
export interface Agent {
	account: Account;
	lifecycle?: AgentLifecycle;
	/** UI-only roster config. */
	role: AgentRole;
	/** UI-only (the model the OMP SDK is set with). */
	model: string;
	/** UI-only. */
	cwd: string;
	/** Fixture-only. */
	terminals: Terminal[];
}

// ── Left-sidebar organization ────────────────────────────────────────────

/** A user-defined folder grouping agents; folders nest arbitrarily. */
export interface Folder {
	id: string;
	name: string;
	/** Accent color (hex) the user picks. */
	color: string;
	/** A single-glyph icon the user picks. */
	icon: string;
	children: TreeNode[];
}

/** A node in the left-sidebar tree: a folder or a leaf agent reference. */
export type TreeNode =
	| { kind: "folder"; folder: Folder }
	| { kind: "agent"; agentId: string };

// ── Daemon / usage / supervisor ──────────────────────────────────────────

/** Liveness/version the daemon-status header shows (mirrors GetDaemonInfo). */
export interface DaemonInfo {
	version: string;
	apiVersion: string;
	/** true when a real daemon answered; false when this is stub data. */
	live: boolean;
}

/** A provider account's usage, for the bottom usage bar. */
export interface UsageAccount {
	provider: string;
	plan: string;
	tokensUsed: number;
	tokensLimit: number;
	/** Human string until the rate-limit window resets. */
	resetIn: string;
	costToday: number;
}

/** A file/dir in an agent worktree, for the right-sidebar file explorer. */
export interface FileNode {
	name: string;
	kind: "file" | "dir";
	/** Git status, when changed. */
	status?: "modified" | "added" | "deleted" | "untracked";
	children?: FileNode[];
}

// ── Fixture data ───────────────────────────────────────────────────────────

export const STUB_DAEMON: DaemonInfo = {
	version: "0.1.0-dev",
	apiVersion: "compass.v1",
	live: false,
};

export const STUB_AGENTS: Agent[] = [
	{
		account: {
			id: "acc-supervisor",
			handle: "supervisor",
			displayName: "supervisor",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-supervisor",
		},
		lifecycle: "working",
		role: "supervisor",
		model: "claude-opus-4",
		cwd: "~/agents/workspaces/supervisor/sealed",
		terminals: [],
	},
	{
		account: {
			id: "acc-warden",
			handle: "warden",
			displayName: "warden",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-warden",
		},
		lifecycle: "working",
		role: "warden",
		model: "seal-wasm-runtime",
		cwd: "(sandboxed)",
		terminals: [],
	},
	{
		account: {
			id: "acc-cook",
			handle: "cook",
			displayName: "cook",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-cook",
		},
		lifecycle: "working",
		role: "worker",
		model: "claude-opus-4",
		cwd: "~/agents/workspaces/cook/sealed",
		terminals: [
			{
				id: "t-c1",
				name: "vite dev",
				running: true,
				lines: [
					"$ bunx vite",
					"",
					"  VITE v8.1.0  ready in 247 ms",
					"",
					"  ➜  Local:   http://localhost:5173/",
					"  ➜  press h + enter to show help",
				],
			},
			{
				id: "t-c2",
				name: "compass-ui:ci",
				running: false,
				lines: [
					"$ moon run compass-ui:ci",
					"▪▪▪▪ compass-ui:typecheck (970ms)",
					"▪▪▪▪ compass-ui:build (1.2s)",
					"▪▪▪▪ compass-ui:test (no tests)",
					"Tasks: 6 completed",
					"  green — typecheck + build + test",
				],
			},
		],
	},
	{
		account: {
			id: "acc-livingstone",
			handle: "livingstone",
			displayName: "livingstone",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-livingstone",
		},
		lifecycle: "working",
		role: "worker",
		model: "claude-opus-4",
		cwd: "~/agents/workspaces/livingstone/sealed",
		terminals: [
			{
				id: "t-l1",
				name: "cargo test",
				running: true,
				lines: [
					"$ cargo test -p compass-daemon",
					"   Compiling compass-daemon v0.1.0",
					"    Running unittests src/lib.rs",
					"test session::tests::reload_reuses_id ... ok",
					"test acp_session::real_omp_in_container ... RUNNING",
				],
			},
		],
	},
	{
		account: {
			id: "acc-cousteau",
			handle: "cousteau",
			displayName: "cousteau",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-cousteau",
		},
		lifecycle: "waiting",
		role: "worker",
		model: "claude-sonnet-4",
		cwd: "~/agents/workspaces/cousteau/sealed",
		terminals: [
			{
				id: "t-co1",
				name: "pulumi preview",
				running: false,
				lines: [
					"$ pulumi preview --stack cloudflare",
					"     Type                     Name         Plan     Info",
					" ~   cloudflare:Application    investors    error    404",
					"Resources: 38 unchanged",
					"error: Preview failed: 404 unknown_application",
				],
			},
		],
	},
	{
		account: {
			id: "acc-ross",
			handle: "ross",
			displayName: "ross",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-ross",
		},
		lifecycle: "working",
		role: "worker",
		model: "claude-opus-4",
		cwd: "~/agents/workspaces/ross/sealed",
		terminals: [],
	},
	{
		account: {
			id: "acc-shackleton",
			handle: "shackleton",
			displayName: "shackleton",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-shackleton",
		},
		lifecycle: "done",
		role: "worker",
		model: "claude-opus-4",
		cwd: "~/agents/workspaces/shackleton/sealed",
		terminals: [],
	},
	{
		account: {
			id: "acc-erikson",
			handle: "erikson",
			displayName: "erikson",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-erikson",
		},
		lifecycle: "working",
		role: "worker",
		model: "gpt-5-codex",
		cwd: "~/agents/workspaces/erikson/sealed",
		terminals: [
			{
				id: "t-e1",
				name: "moon renovate-preflight:test",
				running: false,
				lines: [
					"$ moon run renovate-preflight:test",
					"✓ diagnoses missing token (12 tests)",
					"✓ actionable platform-unknown message",
					"Tasks: 2 completed",
				],
			},
		],
	},
	{
		account: {
			id: "acc-drake",
			handle: "drake",
			displayName: "drake",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-drake",
		},
		lifecycle: "idle",
		role: "worker",
		model: "claude-opus-4",
		cwd: "~/agents/workspaces/drake/sealed",
		terminals: [],
	},
	{
		account: {
			id: "acc-magellan",
			handle: "magellan",
			displayName: "magellan",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "dm-magellan",
		},
		lifecycle: "working",
		role: "worker",
		model: "claude-sonnet-4",
		cwd: "~/agents/workspaces/magellan/sealed",
		terminals: [],
	},
];

export const STUB_WORKSTREAMS: Workstream[] = [
	{
		id: "ws-1022",
		issue: "SEA-1022",
		title: "Tauri desktop shell — window + daemon spawn/attach",
		state: "in_review",
		priority: "high",
		assignee: "acc-cook",
		summary: "Bridge transport + daemon lifecycle landed; in bot review.",
		branch: "cook-1022-tauri-shell",
		changed: { files: 24, additions: 1180, deletions: 96 },
		pr: {
			number: 453,
			title: "feat(compass): explorable Bridge dev UI on stub data",
			state: "open",
			checks: [
				{ name: "CI (pr)", status: "success" },
				{ name: "compass-ui:ci", status: "success" },
				{ name: "root:lint", status: "success" },
			],
			threads: { resolved: 12, total: 12 },
			reviews: [
				{ bot: "greptile", verdict: "approved" },
				{ bot: "cubic", verdict: "approved" },
				{ bot: "CodeRabbit", verdict: "commented" },
			],
		},
		tracker: {
			kind: "linear",
			id: "SEA-1022",
			status: "In Review",
			url: "https://linear.app/sealed/issue/SEA-1022",
		},
		commits: [
			{
				sha: "91722da2",
				subject: "fix(compass): Tauri window + daemon spawn/attach",
				author: "cook",
				at: "12:08",
			},
			{
				sha: "3f1c0a44",
				subject: "feat(compass): explorable Bridge dev UI on stub data",
				author: "cook",
				at: "11:20",
			},
			{
				sha: "b6d2e159",
				subject: "chore(compass): scaffold apps/ui — vite + solid",
				author: "cook",
				at: "10:05",
			},
		],
	},
	{
		id: "ws-965",
		issue: "SEA-965",
		title: "compass-client — gRPC-Web transport polish + reconnect",
		state: "in_progress",
		priority: "medium",
		assignee: "acc-cook",
		summary: "Resubscribe cursor + backoff; wiring to the dev endpoint.",
		branch: "cook-965-client-transport",
		changed: { files: 6, additions: 214, deletions: 40 },
		pr: null,
		tracker: {
			kind: "linear",
			id: "SEA-965",
			status: "In Progress",
			url: "https://linear.app/sealed/issue/SEA-965",
		},
		commits: [
			{
				sha: "a2e7c110",
				subject: "feat(compass): resubscribe cursor + backoff",
				author: "cook",
				at: "11:40",
			},
			{
				sha: "77b90d3e",
				subject: "wip(compass): gRPC-Web reconnect skeleton",
				author: "cook",
				at: "11:02",
			},
		],
	},
	{
		id: "ws-1023",
		issue: "SEA-1023",
		title: "Agent process management + ACP session over compass.v1",
		state: "in_progress",
		priority: "high",
		assignee: "acc-livingstone",
		summary: "Fixing 5 P1s from the compass-owner seam review on #443.",
		branch: "livingstone-1023-acp-session",
		changed: { files: 18, additions: 921, deletions: 63 },
		pr: {
			number: 443,
			title: "feat(compass): agent process management + ACP over compass.v1",
			state: "open",
			checks: [
				{ name: "compass-daemon:ci", status: "success" },
				{ name: "compass-proto:ci", status: "success" },
				{ name: "CI (pr)", status: "pending" },
			],
			threads: { resolved: 1, total: 6 },
			reviews: [
				{ bot: "greptile", verdict: "changes" },
				{ bot: "CodeRabbit", verdict: "commented" },
			],
		},
		tracker: {
			kind: "linear",
			id: "SEA-1023",
			status: "In Progress",
			url: "https://linear.app/sealed/issue/SEA-1023",
		},
		commits: [
			{
				sha: "5d8b1e73",
				subject: "fix(compass): remove errored session from live map",
				author: "livingstone",
				at: "12:10",
			},
			{
				sha: "c4419f0a",
				subject: "feat(compass): ACP session over compass.v1",
				author: "livingstone",
				at: "11:44",
			},
		],
	},
	{
		id: "ws-864",
		issue: "SEA-864",
		title: "Cloudflare Pulumi — restore the investors Access gate",
		state: "blocked",
		priority: "urgent",
		assignee: "acc-cousteau",
		summary: "Access app deleted out-of-band; recreate + `up` pending Matt.",
		branch: "cousteau-864-cf-investors-gate",
		changed: { files: 3, additions: 142, deletions: 18 },
		pr: {
			number: 444,
			title: "fix(pulumi): recreate investors Access gate",
			state: "draft",
			checks: [{ name: "pulumi-preview-cloudflare", status: "failure" }],
			threads: { resolved: 0, total: 2 },
			reviews: [],
		},
		tracker: {
			kind: "linear",
			id: "SEA-864",
			status: "Blocked",
			url: "https://linear.app/sealed/issue/SEA-864",
		},
	},
	{
		id: "ws-1085",
		issue: "SEA-1085",
		title: "Per-boot instance epoch forces resync for stale cursors",
		state: "in_review",
		priority: "medium",
		assignee: "acc-ross",
		summary: "Lockfile refreshed after restack; CI re-running.",
		branch: "ross-1085-instance-epoch",
		changed: { files: 9, additions: 410, deletions: 32 },
		pr: {
			number: 332,
			title: "feat(compass): per-boot instance epoch",
			state: "open",
			checks: [
				{ name: "compass-daemon:ci", status: "success" },
				{ name: "CI (pr)", status: "success" },
			],
			threads: { resolved: 3, total: 3 },
			reviews: [
				{ bot: "greptile", verdict: "approved" },
				{ bot: "cubic", verdict: "approved" },
				{ bot: "CodeRabbit", verdict: "approved" },
			],
		},
		tracker: {
			kind: "linear",
			id: "SEA-1085",
			status: "In Review",
			url: "https://linear.app/sealed/issue/SEA-1085",
		},
	},
	{
		id: "ws-847",
		issue: "SEA-847",
		title: "renovate-preflight — fail the cron fast with a token diagnosis",
		state: "in_review",
		priority: "low",
		assignee: "acc-erikson",
		summary: "Findings fixed; re-run pending on the saturated fleet.",
		branch: "erikson-847-renovate-preflight",
		changed: { files: 7, additions: 302, deletions: 11 },
		pr: {
			number: 400,
			title: "feat(ci): renovate-preflight token diagnosis",
			state: "open",
			checks: [
				{ name: "root:lint", status: "success" },
				{ name: "seal:test", status: "pending" },
				{ name: "CI (pr)", status: "pending" },
			],
			threads: { resolved: 5, total: 5 },
			reviews: [
				{ bot: "greptile", verdict: "approved" },
				{ bot: "cubic", verdict: "approved" },
			],
		},
		tracker: {
			kind: "linear",
			id: "SEA-847",
			status: "In Review",
			url: "https://linear.app/sealed/issue/SEA-847",
		},
	},
	{
		id: "ws-888",
		issue: "SEA-888",
		title: "Pulumi GCP + GitHub providers — bootstrap the prod stack",
		state: "in_progress",
		priority: "medium",
		assignee: "acc-magellan",
		summary: "Rebased onto clean main; one ESC provisioning gate each.",
		branch: "magellan-888-pulumi-providers",
		changed: { files: 12, additions: 560, deletions: 44 },
		pr: {
			number: 180,
			title: "feat(pulumi): GCP + GitHub provider stacks",
			state: "open",
			checks: [
				{ name: "root:lint", status: "success" },
				{ name: "pulumi-preview-gcp", status: "failure" },
			],
			threads: { resolved: 4, total: 4 },
			reviews: [{ bot: "greptile", verdict: "approved" }],
		},
		tracker: {
			kind: "linear",
			id: "SEA-888",
			status: "In Progress",
			url: "https://linear.app/sealed/issue/SEA-888",
		},
	},
	{
		id: "ws-1128",
		issue: "SEA-1128",
		title: "root:lint gate — actually run biome, fail on drift",
		state: "queued",
		priority: "high",
		assignee: "acc-drake",
		summary: "Design frozen; cache-key invalidation is the core fix.",
		branch: "drake-1128-rootlint-gate",
		changed: { files: 0, additions: 0, deletions: 0 },
		pr: null,
		tracker: {
			kind: "linear",
			id: "SEA-1128",
			status: "Todo",
			url: "https://linear.app/sealed/issue/SEA-1128",
		},
	},
	{
		id: "ws-1145",
		issue: "SEA-1145",
		title: "seal e2e — de-flake the rpc budget asserts (virtual time)",
		state: "done",
		priority: "high",
		assignee: "acc-shackleton",
		summary: "Merged (d45d9160); seal:test deterministic fleet-wide.",
		branch: "shackleton-1145-seal-deflake",
		changed: { files: 5, additions: 88, deletions: 74 },
		pr: {
			number: 436,
			title: "fix(seal): virtual-time rpc budgets",
			state: "merged",
			checks: [{ name: "CI (pr)", status: "success" }],
			threads: { resolved: 2, total: 2 },
			reviews: [{ bot: "CodeRabbit", verdict: "approved" }],
		},
		tracker: {
			kind: "linear",
			id: "SEA-1145",
			status: "Done",
			url: "https://linear.app/sealed/issue/SEA-1145",
		},
	},
	{
		id: "ws-1130",
		issue: "SEA-1130",
		title: "Cotal connector — stop subagents joining the mesh",
		state: "done",
		priority: "medium",
		assignee: "acc-shackleton",
		summary: "Gates mesh-join on an interactive session; rebuilt live.",
		branch: "shackleton-1130-cotal-connector",
		changed: { files: 4, additions: 61, deletions: 29 },
		pr: {
			number: 228,
			title: "fix(cotal): gate mesh-join on hasUI",
			state: "merged",
			checks: [{ name: "CI", status: "success" }],
			threads: { resolved: 0, total: 0 },
			reviews: [{ bot: "CodeRabbit", verdict: "approved" }],
		},
		tracker: {
			kind: "linear",
			id: "SEA-1130",
			status: "Done",
			url: "https://linear.app/sealed/issue/SEA-1130",
		},
	},
	{
		id: "ws-1146",
		issue: "SEA-1146",
		title: "Bucketer design — pooled review-token routing",
		state: "backlog",
		priority: "low",
		assignee: null,
		summary: "Design not yet dispatched; awaiting a free worker.",
		branch: "—",
		changed: { files: 0, additions: 0, deletions: 0 },
		pr: null,
		tracker: {
			kind: "linear",
			id: "SEA-1146",
			status: "Backlog",
			url: "https://linear.app/sealed/issue/SEA-1146",
		},
	},
];

// The current user's own tracker-assigned issues, for the Backlog view (D3) —
// the human's personal queue, shown alongside the fleet's board. Distinct from
// STUB_WORKSTREAMS (the agents' work): these are unassigned to any agent and
// carry the tracker's native status. Read through the TrackerSeam (tracker.ts).
export const STUB_ASSIGNED_ISSUES: Workstream[] = [
	{
		id: "ws-1201",
		issue: "SEA-1201",
		title: "Warden audit-log retention policy — design",
		state: "todo",
		priority: "high",
		assignee: null,
		summary: "Assigned to you in Linear; not yet dispatched to an agent.",
		branch: "—",
		changed: { files: 0, additions: 0, deletions: 0 },
		pr: null,
		tracker: {
			kind: "linear",
			id: "SEA-1201",
			status: "Todo",
			url: "https://linear.app/sealed/issue/SEA-1201",
		},
	},
	{
		id: "ws-1180",
		issue: "SEA-1180",
		title: "Compass daemon — graceful shutdown on SIGTERM",
		state: "backlog",
		priority: "medium",
		assignee: null,
		summary: "In your Linear backlog; needs triage before dispatch.",
		branch: "—",
		changed: { files: 0, additions: 0, deletions: 0 },
		pr: null,
		tracker: {
			kind: "linear",
			id: "SEA-1180",
			status: "Backlog",
			url: "https://linear.app/sealed/issue/SEA-1180",
		},
	},
];

// The user-defined left-sidebar organization: nested folders with color + icon,
// grouping the worker agents. The moat agents are not tree leaves: the
// supervisor is baked into its own pinned pane, and the warden lives in the
// right sidebar's fleet tabs (constants.ts RIGHT_SIDEBAR_TAB_BY_ID) — the
// always-on agent conversations belong there, not in the folder tree.
export const STUB_TREE: TreeNode[] = [
	{
		kind: "folder",
		folder: {
			id: "f-compass",
			name: "Compass",
			color: "#58a6ff",
			icon: "◇",
			children: [
				{
					kind: "folder",
					folder: {
						id: "f-compass-ui",
						name: "UI",
						color: "#79c0ff",
						icon: "▤",
						children: [{ kind: "agent", agentId: "acc-cook" }],
					},
				},
				{
					kind: "folder",
					folder: {
						id: "f-compass-runtime",
						name: "Runtime",
						color: "#a5d6ff",
						icon: "⚙",
						children: [{ kind: "agent", agentId: "acc-livingstone" }],
					},
				},
			],
		},
	},
	{
		kind: "folder",
		folder: {
			id: "f-infra",
			name: "Infrastructure",
			color: "#d29922",
			icon: "☁",
			children: [
				{ kind: "agent", agentId: "acc-cousteau" },
				{ kind: "agent", agentId: "acc-magellan" },
			],
		},
	},
	{
		kind: "folder",
		folder: {
			id: "f-ci",
			name: "CI & Build",
			color: "#3fb950",
			icon: "⬢",
			children: [
				{ kind: "agent", agentId: "acc-erikson" },
				{ kind: "agent", agentId: "acc-drake" },
				{ kind: "agent", agentId: "acc-shackleton" },
			],
		},
	},
	{ kind: "agent", agentId: "acc-ross" },
];

export const STUB_USAGE: UsageAccount[] = [
	{
		provider: "Claude",
		plan: "Max 20×",
		tokensUsed: 118_400_000,
		tokensLimit: 220_000_000,
		resetIn: "2h 14m",
		costToday: 0,
	},
	{
		provider: "Codex",
		plan: "Pro",
		tokensUsed: 4_120_000,
		tokensLimit: 30_000_000,
		resetIn: "5h 02m",
		costToday: 0,
	},
];

/** The worktree file tree per branch, keyed by workstream id, for the file
 *  explorer. Only a representative slice — enough to show status decoration. */
export const STUB_FILES: Record<string, FileNode[]> = {
	"ws-1022": [
		{
			name: "oss/compass/apps/ui",
			kind: "dir",
			children: [
				{
					name: "src",
					kind: "dir",
					children: [
						{ name: "App.tsx", kind: "file", status: "modified" },
						{ name: "app.css", kind: "file", status: "modified" },
						{ name: "store.ts", kind: "file", status: "added" },
						{ name: "stub-data.ts", kind: "file", status: "modified" },
						{ name: "components", kind: "dir", status: "added" },
					],
				},
				{ name: "package.json", kind: "file" },
			],
		},
	],
	"ws-965": [
		{
			name: "oss/compass/packages/compass-client",
			kind: "dir",
			children: [
				{
					name: "src",
					kind: "dir",
					children: [
						{ name: "transport.ts", kind: "file", status: "modified" },
						{ name: "reconnect.ts", kind: "file", status: "added" },
					],
				},
			],
		},
	],
	"ws-1023": [
		{
			name: "oss/compass/crates/compass-daemon",
			kind: "dir",
			children: [
				{
					name: "src",
					kind: "dir",
					children: [
						{ name: "session.rs", kind: "file", status: "modified" },
						{ name: "acp_session.rs", kind: "file", status: "modified" },
						{ name: "serve.rs", kind: "file", status: "modified" },
						{ name: "translate.rs", kind: "file", status: "added" },
					],
				},
			],
		},
	],
};
