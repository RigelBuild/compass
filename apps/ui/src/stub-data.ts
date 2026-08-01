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
 * Where an issue sits in its Compass lifecycle (DL-033 + DL-071):
 * Backlog → Todo → Queued → Blocked ⇄ In Progress → In Review → Done, plus a
 * terminal `archived` sink past Done. The board shows the active subset
 * (constants `BOARD_LANES`); Backlog + Todo are the pre-active tier surfaced in
 * the Backlog view; `archived` is off the board, listed only in the Done view's
 * Archived section. Server-authoritative (DL-070): computed and streamed by the
 * server projection, mutated only through the write-path RPC.
 */
export type IssueState =
	| "backlog"
	| "todo"
	| "queued"
	| "blocked"
	| "in_progress"
	| "in_review"
	| "done"
	| "archived";

/** The seven WORKING states — the lifecycle domain the tracker projection and
 *  the Settings mapping editor are total over. `archived` is a terminal sink
 *  that carries no tracker status (DL-071), so it is excluded here. */
export type WorkingIssueState = Exclude<IssueState, "archived">;

/** What the running agent process is doing — the dot beside the agent icon
 *  (design D9). A UI projection over the daemon's coarse `AgentSessionState`
 *  (#443) plus event-stream refinements; `waiting`/`done`/`paused` are UI-only
 *  (see agent-state.ts). This is the *process* axis, distinct from an issue's
 *  `blocked` (the *task* axis). */
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

/** Priority of an issue, drives the card accent. */
export type Priority = "urgent" | "high" | "medium" | "low";

/** An issue tracker Compass projects issue state onto (D2). Linear first;
 *  the shape is tracker-agnostic for Jira/GitHub later. */
export type TrackerKind = "linear" | "jira" | "github";

// ── Board ──────────────────────────────────────────────────────────────────

// The canonical Compass board types — the forge artifact's fields PLUS the
// Compass agent attribution PLUS the Compass machinery, translated from raw
// forge data at server ingestion (DL-069). The raw forge shape is never a wire
// type. Fixture-typed here until `@compass/client` generates them, then the
// import flips to the generated type (the seam). camelCase field names mirror
// the protobuf-es codegen convention; the proto is snake_case.

/** Which forge (and host, for self-hosted instances) an artifact lives on.
 *  Carried on both Issue and PullRequest so multi-forge artifacts never collide
 *  on `repo` alone (DL-071). A tracker-as-forge (Linear) uses its constant
 *  service host. */
export type ForgeProvider = "github" | "gitlab" | "forgejo" | "linear";
export interface ForgeRef {
	provider: ForgeProvider;
	/** "github.com", a self-hosted host like "git.acme.internal", or the
	 *  constant service host for a tracker-as-forge ("linear.app"). */
	host: string;
}

/** The agent attribution parsed from the owner header at ingestion (DL-050,
 *  #995 Decision 2). UNTRUSTED display metadata — never reaches an authz,
 *  routing, or ownership decision, and is never derived into `assignee`. It
 *  carries handles plus a server-set trust bit, no account/session ids. */
export interface AgentAttribution {
	/** The agent handle CLAIMED by the header; not proof. */
	agentHandle: string;
	/** The owning user's handle CLAIMED by the header. */
	ownerHandle: string;
	/** Server-set at ingestion from the forge-login cross-check (#995 OQ-1):
	 *  true only when the artifact's forge author login equals Compass's own
	 *  forge identity. The UI hedges the claim unless verified (DL-068). */
	verified: boolean;
}

/** A PR diffstat (files/additions/deletions), carried on PullRequest — a diff
 *  is a PR fact, not an issue fact (DL-071 correction #4). */
export interface ChangedStats {
	files: number;
	additions: number;
	deletions: number;
}

/** One CI/status check on a PR head — the 6-valued forge `state`, mapped to a
 *  3-valued pip class at the render sites (the shared 6→3 map). */
export interface Check {
	name: string;
	state:
		| "queued"
		| "in_progress"
		| "success"
		| "failure"
		| "neutral"
		| "cancelled";
	url: string;
	required: boolean;
}

/** The rolled-up CI + status-check state on a PR head, translated at ingestion.
 *  The pips render `checks` (one per check), NOT the roll-up `state`. */
export interface ChecksSummary {
	headSha: string;
	/** The roll-up: "pending" | "success" | "failure". */
	state: "pending" | "success" | "failure";
	checks: Check[];
}

/** One comment within a review thread; `isBot` flags a bot author. */
export interface Comment {
	author: string;
	isBot: boolean;
	body: string;
}

/** A review thread with its comments and resolution — the "N/M threads
 *  resolved" derivation counts these. */
export interface ReviewThread {
	/** The file the thread anchors to; empty for a PR-level thread. */
	path: string;
	resolved: boolean;
	comments: Comment[];
}

/** A submitted review (human or bot). `reviews` is submission-ordered so a
 *  reviewer's CURRENT verdict is its last entry; the bot chips take the
 *  latest-per-author. The canonical `verdict` is the forge's vocabulary; the UI
 *  maps "changes_requested"→"changes" at the chip site. */
export interface Review {
	/** The reviewer's forge account, or a Compass agent handle. */
	author: string;
	isBot: boolean;
	verdict: "approved" | "changes_requested" | "commented";
	/** The review's summary comment (may be empty). */
	body: string;
}

/** A Compass pull request: the forge PR's fields plus the Compass agent
 *  attribution plus this PR's diffstat plus the full review state. */
export interface PullRequest {
	forge: ForgeRef;
	repo: string;
	number: number;
	title: string;
	/** Forge truth: "open" | "closed" | "merged". The badge derives from this
	 *  plus `draft`. */
	forgeState: "open" | "closed" | "merged";
	url: string;
	headRef: string;
	baseRef: string;
	/** Compass agent attribution; unset for a non-Compass author. */
	agent?: AgentAttribution;
	/** The native forge account that opened the PR; always set. */
	forgeAccount: string;
	draft: boolean;
	/** THIS PR's diffstat; unset for no diff. */
	changed?: ChangedStats;
	/** Rolled-up CI state; unset for a PR with no CI. */
	checks?: ChecksSummary;
	reviews: Review[];
	threads: ReviewThread[];
}

/** A commit on an issue's branch, for the VCS pane's commit history. */
export interface Commit {
	/** Short SHA. */
	sha: string;
	subject: string;
	author: string;
	/** Wall-clock or relative time, matching the feed's `at` style. */
	at: string;
}

/** The board unit: a Compass Issue — the forge issue's fields PLUS the Compass
 *  agent attribution PLUS the Compass machinery (DL-069). */
export interface Issue {
	/** Compass-local stable id — the projection/mutation join key and the store
	 *  key; never a display fallback. */
	id: string;

	// ── Forge fields (translated from the raw forge payload at ingestion) ──
	/** Which forge + host — multi-forge disambiguation. */
	forge: ForgeRef;
	/** "<owner>/<name>" on GitHub, project key on Linear. */
	repo: string;
	/** The forge issue number. */
	number: number;
	title: string;
	/** Owner header STRIPPED at ingestion (DL-050). */
	body: string;
	/** Forge truth: "open" | "closed" — NOT the Compass lifecycle. */
	forgeState: "open" | "closed";
	url: string;
	/** Compass agent attribution; unset for a non-Compass (human) author. */
	agent?: AgentAttribution;
	/** The native forge account that authored the artifact; always set. */
	forgeAccount: string;
	labels: string[];

	// ── Compass machinery (Compass-owned; none of this is on the forge) ──
	/** The canonical lifecycle (DL-033 + `archived`), server-authoritative. */
	state: IssueState;
	priority: Priority;
	/** The agent id currently on it, or null when unassigned. */
	assignee: string | null;
	/** A one-line summary of the latest activity, for the card. */
	summary: string;
	/** The working head branch name (may exist before any PR). */
	branch: string;
	/** Every PR opened for this issue, discovery order (newest last); empty
	 *  before the first. The card/Done/PR pane render the primary PR. */
	prs: PullRequest[];
	/** The linked tracker issue (D2), if any — the projection target. */
	tracker?: TrackerRef;
	/** Fixture-side commit-history side-channel for the VCS pane, carved out to
	 *  a future repo/worktree surface (DL-071 §Global constraints); NOT a
	 *  canonical field. */
	commits?: Commit[];
}

/** The tracker issue an issue is linked to — the projection target (D2).
 *  Compass state is canonical; this carries the tracker's *native* status. */
export interface TrackerRef {
	kind: TrackerKind;
	/** The tracker's native issue id, e.g. "SEA-1042". */
	id: string;
	/** The tracker's native status name in the user's org. */
	status: string;
	url: string;
}

/** A user-editable projection between Compass state and a tracker's native
 *  statuses (D2). `toTracker` is total over the seven WORKING states;
 *  `fromTracker` is many-to-one (e.g. Linear's Cancelled + Duplicate both read
 *  back as Done). `archived` carries no tracker status (DL-071), so the domain
 *  is `WorkingIssueState`. */
export interface TrackerStatusMapping {
	kind: TrackerKind;
	/** Compass working state → the tracker's status name in this org. */
	toTracker: Record<WorkingIssueState, string>;
	/** Tracker status name → Compass working state (many-to-one). */
	fromTracker: Record<string, WorkingIssueState>;
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

// ── Canonical-shape fixture helpers ──────────────────────────────────────────
// Every board item is forge-backed (DL-071 §Global constraints). The agents'
// work lives in the sealed monorepo on GitHub; the attribution owner is the
// human "matt". These builders keep the seeded rows compact and honest.

const GITHUB: ForgeRef = { provider: "github", host: "github.com" };
const SEALED_REPO = "sealedsecurity/sealed";

/** Agent attribution parsed from the owner header (DL-050) — a claim, hedged
 *  in the UI unless `verified`. */
function attrib(handle: string, verified = true): AgentAttribution {
	return { agentHandle: handle, ownerHandle: "matt", verified };
}

/** A single 6-valued CI check on a PR head. */
function chk(name: string, state: Check["state"]): Check {
	return { name, state, url: "", required: false };
}

/** Roll a per-check list up into a ChecksSummary — failure dominates, then
 *  pending, else success — mirroring the server's ingestion roll-up. */
function checksOf(headSha: string, runs: Check[]): ChecksSummary {
	const state: ChecksSummary["state"] = runs.some(
		(c) => c.state === "failure" || c.state === "cancelled",
	)
		? "failure"
		: runs.some(
					(c) =>
						c.state === "queued" ||
						c.state === "in_progress" ||
						c.state === "neutral",
				)
			? "pending"
			: "success";
	return { headSha, state, checks: runs };
}

/** A bot review with the canonical (forge) verdict vocabulary. */
function botReview(author: string, verdict: Review["verdict"]): Review {
	return { author, isBot: true, verdict, body: "" };
}

/** `total` review threads, `resolved` of them resolved — the "N/M threads
 *  resolved" derivation counts these entries. */
function threadsOf(total: number, resolved: number): ReviewThread[] {
	return Array.from({ length: total }, (_, i) => ({
		path: "",
		resolved: i < resolved,
		comments: [],
	}));
}

export const STUB_ISSUES: Issue[] = [
	{
		id: "ws-1022",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1022,
		title: "Tauri desktop shell — window + daemon spawn/attach",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/1022",
		agent: attrib("cook"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "in_review",
		priority: "high",
		assignee: "acc-cook",
		summary: "Bridge transport + daemon lifecycle landed; in bot review.",
		branch: "cook-1022-tauri-shell",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 453,
				title: "feat(compass): explorable Bridge dev UI on stub data",
				forgeState: "open",
				url: "https://github.com/sealedsecurity/sealed/pull/453",
				headRef: "cook-1022-tauri-shell",
				baseRef: "main",
				agent: attrib("cook"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 24, additions: 1180, deletions: 96 },
				checks: checksOf("91722da2", [
					chk("CI (pr)", "success"),
					chk("compass-ui:ci", "success"),
					chk("root:lint", "success"),
				]),
				reviews: [
					botReview("greptile", "approved"),
					botReview("cubic", "approved"),
					botReview("CodeRabbit", "commented"),
				],
				threads: threadsOf(12, 12),
			},
		],
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
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 965,
		title: "compass-client — gRPC-Web transport polish + reconnect",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/965",
		agent: attrib("cook"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "in_progress",
		priority: "medium",
		assignee: "acc-cook",
		summary: "Resubscribe cursor + backoff; wiring to the dev endpoint.",
		branch: "cook-965-client-transport",
		prs: [],
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
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1023,
		title: "Agent process management + ACP session over compass.v1",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/1023",
		agent: attrib("livingstone"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "in_progress",
		priority: "high",
		assignee: "acc-livingstone",
		summary: "Fixing 5 P1s from the compass-owner seam review on #443.",
		branch: "livingstone-1023-acp-session",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 443,
				title: "feat(compass): agent process management + ACP over compass.v1",
				forgeState: "open",
				url: "https://github.com/sealedsecurity/sealed/pull/443",
				headRef: "livingstone-1023-acp-session",
				baseRef: "main",
				agent: attrib("livingstone"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 18, additions: 921, deletions: 63 },
				checks: checksOf("5d8b1e73", [
					chk("compass-daemon:ci", "success"),
					chk("compass-proto:ci", "success"),
					chk("CI (pr)", "in_progress"),
				]),
				reviews: [
					botReview("greptile", "changes_requested"),
					botReview("CodeRabbit", "commented"),
				],
				threads: threadsOf(6, 1),
			},
		],
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
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 864,
		title: "Cloudflare Pulumi — restore the investors Access gate",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/864",
		agent: attrib("cousteau", false),
		forgeAccount: "cousteau-dev",
		labels: [],
		state: "blocked",
		priority: "urgent",
		assignee: "acc-cousteau",
		summary: "Access app deleted out-of-band; recreate + `up` pending Matt.",
		branch: "cousteau-864-cf-investors-gate",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 444,
				title: "fix(pulumi): recreate investors Access gate",
				forgeState: "open",
				url: "https://github.com/sealedsecurity/sealed/pull/444",
				headRef: "cousteau-864-cf-investors-gate",
				baseRef: "main",
				agent: attrib("cousteau", false),
				forgeAccount: "cousteau-dev",
				draft: true,
				changed: { files: 3, additions: 142, deletions: 18 },
				checks: checksOf("d1a4f207", [
					chk("pulumi-preview-cloudflare", "failure"),
				]),
				reviews: [],
				threads: threadsOf(2, 0),
			},
		],
		tracker: {
			kind: "linear",
			id: "SEA-864",
			status: "Blocked",
			url: "https://linear.app/sealed/issue/SEA-864",
		},
	},
	{
		id: "ws-1085",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1085,
		title: "Per-boot instance epoch forces resync for stale cursors",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/1085",
		agent: attrib("ross"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "in_review",
		priority: "medium",
		assignee: "acc-ross",
		summary: "Lockfile refreshed after restack; CI re-running.",
		branch: "ross-1085-instance-epoch",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 332,
				title: "feat(compass): per-boot instance epoch",
				forgeState: "open",
				url: "https://github.com/sealedsecurity/sealed/pull/332",
				headRef: "ross-1085-instance-epoch",
				baseRef: "main",
				agent: attrib("ross"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 9, additions: 410, deletions: 32 },
				checks: checksOf("bb31c700", [
					chk("compass-daemon:ci", "success"),
					chk("CI (pr)", "success"),
				]),
				reviews: [
					botReview("greptile", "approved"),
					botReview("cubic", "approved"),
					botReview("CodeRabbit", "approved"),
				],
				threads: threadsOf(3, 3),
			},
		],
		tracker: {
			kind: "linear",
			id: "SEA-1085",
			status: "In Review",
			url: "https://linear.app/sealed/issue/SEA-1085",
		},
	},
	{
		id: "ws-847",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 847,
		title: "renovate-preflight — fail the cron fast with a token diagnosis",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/847",
		agent: attrib("erikson"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "in_review",
		priority: "low",
		assignee: "acc-erikson",
		summary: "Findings fixed; re-run pending on the saturated fleet.",
		branch: "erikson-847-renovate-preflight",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 400,
				title: "feat(ci): renovate-preflight token diagnosis",
				forgeState: "open",
				url: "https://github.com/sealedsecurity/sealed/pull/400",
				headRef: "erikson-847-renovate-preflight",
				baseRef: "main",
				agent: attrib("erikson"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 7, additions: 302, deletions: 11 },
				checks: checksOf("7c02a9de", [
					chk("root:lint", "success"),
					chk("seal:test", "in_progress"),
					chk("CI (pr)", "in_progress"),
				]),
				reviews: [
					botReview("greptile", "approved"),
					botReview("cubic", "approved"),
				],
				threads: threadsOf(5, 5),
			},
		],
		tracker: {
			kind: "linear",
			id: "SEA-847",
			status: "In Review",
			url: "https://linear.app/sealed/issue/SEA-847",
		},
	},
	{
		id: "ws-888",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 888,
		title: "Pulumi GCP + GitHub providers — bootstrap the prod stack",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/888",
		agent: attrib("magellan"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "in_progress",
		priority: "medium",
		assignee: "acc-magellan",
		summary: "Rebased onto clean main; one ESC provisioning gate each.",
		branch: "magellan-888-pulumi-providers",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 180,
				title: "feat(pulumi): GCP + GitHub provider stacks",
				forgeState: "open",
				url: "https://github.com/sealedsecurity/sealed/pull/180",
				headRef: "magellan-888-pulumi-providers",
				baseRef: "main",
				agent: attrib("magellan"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 12, additions: 560, deletions: 44 },
				checks: checksOf("2fa10b8c", [
					chk("root:lint", "success"),
					chk("pulumi-preview-gcp", "failure"),
				]),
				reviews: [botReview("greptile", "approved")],
				threads: threadsOf(4, 4),
			},
		],
		tracker: {
			kind: "linear",
			id: "SEA-888",
			status: "In Progress",
			url: "https://linear.app/sealed/issue/SEA-888",
		},
	},
	{
		id: "ws-1128",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1128,
		title: "root:lint gate — actually run biome, fail on drift",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/1128",
		agent: attrib("drake"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "queued",
		priority: "high",
		assignee: "acc-drake",
		summary: "Design frozen; cache-key invalidation is the core fix.",
		branch: "drake-1128-rootlint-gate",
		prs: [],
		tracker: {
			kind: "linear",
			id: "SEA-1128",
			status: "Todo",
			url: "https://linear.app/sealed/issue/SEA-1128",
		},
	},
	{
		id: "ws-1145",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1145,
		title: "seal e2e — de-flake the rpc budget asserts (virtual time)",
		body: "",
		forgeState: "closed",
		url: "https://github.com/sealedsecurity/sealed/issues/1145",
		agent: attrib("shackleton"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "done",
		priority: "high",
		assignee: "acc-shackleton",
		summary: "Merged (d45d9160); seal:test deterministic fleet-wide.",
		branch: "shackleton-1145-seal-deflake",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 436,
				title: "fix(seal): virtual-time rpc budgets",
				forgeState: "merged",
				url: "https://github.com/sealedsecurity/sealed/pull/436",
				headRef: "shackleton-1145-seal-deflake",
				baseRef: "main",
				agent: attrib("shackleton"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 5, additions: 88, deletions: 74 },
				checks: checksOf("d45d9160", [chk("CI (pr)", "success")]),
				reviews: [botReview("CodeRabbit", "approved")],
				threads: threadsOf(2, 2),
			},
		],
		tracker: {
			kind: "linear",
			id: "SEA-1145",
			status: "Done",
			url: "https://linear.app/sealed/issue/SEA-1145",
		},
	},
	{
		id: "ws-1130",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1130,
		title: "Cotal connector — stop subagents joining the mesh",
		body: "",
		forgeState: "closed",
		url: "https://github.com/sealedsecurity/sealed/issues/1130",
		agent: attrib("shackleton"),
		forgeAccount: "compass-bot",
		labels: [],
		state: "done",
		priority: "medium",
		assignee: "acc-shackleton",
		summary: "Gates mesh-join on an interactive session; rebuilt live.",
		branch: "shackleton-1130-cotal-connector",
		prs: [
			{
				forge: GITHUB,
				repo: SEALED_REPO,
				number: 228,
				title: "fix(cotal): gate mesh-join on hasUI",
				forgeState: "merged",
				url: "https://github.com/sealedsecurity/sealed/pull/228",
				headRef: "shackleton-1130-cotal-connector",
				baseRef: "main",
				agent: attrib("shackleton"),
				forgeAccount: "compass-bot",
				draft: false,
				changed: { files: 4, additions: 61, deletions: 29 },
				checks: checksOf("e0b7712a", [chk("CI", "success")]),
				reviews: [botReview("CodeRabbit", "approved")],
				threads: threadsOf(0, 0),
			},
		],
		tracker: {
			kind: "linear",
			id: "SEA-1130",
			status: "Done",
			url: "https://linear.app/sealed/issue/SEA-1130",
		},
	},
	{
		id: "ws-1146",
		forge: GITHUB,
		repo: SEALED_REPO,
		number: 1146,
		title: "Bucketer design — pooled review-token routing",
		body: "",
		forgeState: "open",
		url: "https://github.com/sealedsecurity/sealed/issues/1146",
		forgeAccount: "matt",
		labels: [],
		state: "backlog",
		priority: "low",
		assignee: null,
		summary: "Design not yet dispatched; awaiting a free worker.",
		branch: "—",
		prs: [],
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
// STUB_ISSUES (the agents' work): these are unassigned to any agent and carry
// the tracker's native status. Linear-origin (DL-051's issues-only forge), so
// their forge is Linear with the project key as `repo`. Read through the
// TrackerSeam (tracker.ts).
const LINEAR_FORGE: ForgeRef = { provider: "linear", host: "linear.app" };
export const STUB_ASSIGNED_ISSUES: Issue[] = [
	{
		id: "ws-1201",
		forge: LINEAR_FORGE,
		repo: "SEA",
		number: 1201,
		title: "Warden audit-log retention policy — design",
		body: "",
		forgeState: "open",
		url: "https://linear.app/sealed/issue/SEA-1201",
		forgeAccount: "matt",
		labels: [],
		state: "todo",
		priority: "high",
		assignee: null,
		summary: "Assigned to you in Linear; not yet dispatched to an agent.",
		branch: "—",
		prs: [],
		tracker: {
			kind: "linear",
			id: "SEA-1201",
			status: "Todo",
			url: "https://linear.app/sealed/issue/SEA-1201",
		},
	},
	{
		id: "ws-1180",
		forge: LINEAR_FORGE,
		repo: "SEA",
		number: 1180,
		title: "Compass daemon — graceful shutdown on SIGTERM",
		body: "",
		forgeState: "open",
		url: "https://linear.app/sealed/issue/SEA-1180",
		forgeAccount: "matt",
		labels: [],
		state: "backlog",
		priority: "medium",
		assignee: null,
		summary: "In your Linear backlog; needs triage before dispatch.",
		branch: "—",
		prs: [],
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

/** The worktree file tree per branch, keyed by Compass issue id, for the file
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
