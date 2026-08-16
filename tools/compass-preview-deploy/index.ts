// compass-preview-deploy — the Compass PR-preview deploy tool (SEA-2027).
//
// Record B-preview P1 (orion
// docs/designs/platform/compass-pr-preview/design.md): deploy a compass PR's
// FULL stack to the isolated `preview` env on mattfw, selected by a
// single-holder `preview` GitHub label, served tailnet-gated, with a GitHub
// Deployment as the PR-visible state surface. The workflow
// (.github/workflows/compass-preview-deploy.yml) is a THIN orchestrator; ALL
// real logic lives here (the repo's no-bash-gate posture — design record
// §Global Constraints "Scripts over bash").
//
// CONSTRUCTION/EXECUTION SPLIT (mirrors tools/cx-token-gate,
// tools/design-ledger-gate):
//   * `decide()` — the pure label-lifecycle state machine. No I/O, no exit;
//     it maps an event context to a `Decision`. This is the tested oracle.
//   * `runOnce()` — the execution shell. Dispatches a `Decision` to injected
//     side effects (the GitHub label/comment/Deployments API + the ssh deploy
//     to mattfw), so tests drive it with fakes.
//
// THE SECURITY INVARIANT (why this is the load-bearing bit). The deploy
// credential + tailnet reach must NEVER be exposed to fork-PR-controlled code.
// Two layers enforce it:
//   1. The workflow triggers on `on: pull_request` (NOT `pull_request_target`),
//      so a fork PR's job gets a read-only GITHUB_TOKEN and NO secrets ever —
//      the same guarantee publish-agent-image.yml relies on ("no token or
//      secret is ever exposed to a fork PR").
//   2. Every deploy/label-mutating step is gated on
//      `head.repo.full_name == github.repository` at the YAML layer, AND this
//      tool refuses to deploy a fork (decide() returns `reject-fork`, never
//      `claim`/`redeploy`, for a fork PR). A fork claim is rejected up front:
//      strip the label + post a same-repo-only comment, WITHOUT displacing the
//      incumbent or deploying (design record §P1 label lifecycle).
//
// Inputs (env, set by the workflow):
//   EVENT_ACTION   - labeled | synchronize | unlabeled | closed
//   PR_NUMBER      - the PR this event is about
//   HEAD_REPO      - github.event.pull_request.head.repo.full_name
//   BASE_REPO      - github.repository (the canonical repo)
//   HEAD_SHA       - github.event.pull_request.head.sha (the deploy ref)
//   CHANGED_LABEL  - github.event.label.name (labeled/unlabeled only)
//   PR_LABELS      - comma-joined label names currently on the PR
//   REPO, GH_TOKEN - for the gh CLI (GitHub API)
//   Deploy-reach (see the workflow header): the runner joins the tailnet as an
//   ephemeral tagged node, then this tool ssh's the deploy to mattfw. The
//   deploy target is configured via PREVIEW_SSH_HOST / PREVIEW_SSH_USER /
//   PREVIEW_CHECKOUT / PREVIEW_DOOR_URL / PREVIEW_URL / PREVIEW_ADMIN_ACCOUNT.
// Exit codes:
//   0 - the decision was applied cleanly (incl. a no-op)
//   1 - a deploy/release failed (the Deployment is marked failure)
//   2 - usage / internal error (bad event context, API unreachable)

import { appendFileSync } from "node:fs";
import { $ } from "bun";

/** The single-holder selection label. The env has ONE occupant at a time. */
export const PREVIEW_LABEL = "preview";
/** The GitHub Deployment environment name — the PR-visible state surface. */
export const PREVIEW_ENVIRONMENT = "preview";
/**
 * The marker line opening the fork-rejection + displacement + preview-link
 * payloads. Stable so the result-surfacing lane (SEA-2014) and re-runs can
 * find and update the right comment rather than stacking duplicates.
 */
export const PREVIEW_MARKER = "<!-- compass-preview-deploy -->";

/** The `pull_request` event actions this tool acts on. */
export type EventAction = "labeled" | "synchronize" | "unlabeled" | "closed";

/** Whether a string is one of the handled event actions. */
export function isEventAction(s: string): s is EventAction {
	return (
		s === "labeled" ||
		s === "synchronize" ||
		s === "unlabeled" ||
		s === "closed"
	);
}

/**
 * The event context the decision is made against. Everything the pure state
 * machine needs — the workflow + gh queries populate it; `decide` reads only
 * this, never the environment.
 */
export interface EventContext {
	/** The `pull_request` action that fired the workflow. */
	action: EventAction;
	/** The PR this event is about. */
	prNumber: number;
	/** `github.event.pull_request.head.repo.full_name` (a fork differs). */
	headRepo: string;
	/** `github.repository` — the canonical repo. */
	baseRepo: string;
	/** The deploy ref: the PR head sha. */
	headSha: string;
	/** `github.event.label.name` for labeled/unlabeled; null otherwise. */
	changedLabel: string | null;
	/** Label names currently on THIS PR (from the event payload). */
	prLabels: string[];
	/** Open PR numbers currently carrying the `preview` label (gh query). */
	currentHolders: number[];
	/**
	 * The PR the current ACTIVE preview-environment Deployment tracks, or null
	 * if none is active. This is what makes release race-safe: a displacement
	 * strips the loser's label (firing `unlabeled` on it), but by then the
	 * active deployment already points at the WINNER, so the loser's release is
	 * a no-op and never tears down the incumbent's live env.
	 */
	activePreviewPr: number | null;
}

/** The action the state machine resolves an event to. */
export type Decision =
	/** Nothing to do; `reason` is logged for observability. */
	| { kind: "noop"; reason: string }
	/** A fork claimed the label: strip it + comment, never displace or deploy. */
	| { kind: "reject-fork" }
	/** A same-repo PR claimed the env: displace holders, then deploy. */
	| { kind: "claim"; displaced: number[] }
	/** The current holder pushed: redeploy in place. */
	| { kind: "redeploy" }
	/** The active holder released the env: stop the service, mark inactive. */
	| { kind: "release" };

/**
 * The pure label-lifecycle state machine (same-repo claims only). Given the
 * event context, resolve the action. No I/O; total over EventAction.
 *
 * The invariant it preserves: exactly one PR owns the shared `preview` env at
 * a time, a fork NEVER deploys or displaces, and a stale displacement event
 * never tears down the live incumbent (the `activePreviewPr` guard on release).
 */
export function decide(ctx: EventContext): Decision {
	const sameRepo = ctx.headRepo === ctx.baseRepo;
	switch (ctx.action) {
		case "labeled": {
			// Only the preview label selects; any other label is not ours.
			if (ctx.changedLabel !== PREVIEW_LABEL) {
				return { kind: "noop", reason: `not the ${PREVIEW_LABEL} label` };
			}
			// FORK GUARD: a fork's claim is rejected up front — strip + comment,
			// never displace, never deploy. The incumbent is untouched.
			if (!sameRepo) return { kind: "reject-fork" };
			// Single-holder: displace every OTHER PR currently holding the label.
			const displaced = ctx.currentHolders.filter((n) => n !== ctx.prNumber);
			return { kind: "claim", displaced };
		}
		case "synchronize": {
			// A push only redeploys the CURRENT holder; a synchronize on a
			// non-holder (or any fork) is a no-op — never a silent deploy.
			if (!sameRepo) return { kind: "noop", reason: "fork PR never deploys" };
			if (!ctx.prLabels.includes(PREVIEW_LABEL)) {
				return {
					kind: "noop",
					reason: `PR does not hold the ${PREVIEW_LABEL} label`,
				};
			}
			return { kind: "redeploy" };
		}
		case "unlabeled": {
			if (ctx.changedLabel !== PREVIEW_LABEL) {
				return { kind: "noop", reason: `not the ${PREVIEW_LABEL} label` };
			}
			// Release ONLY if this PR is the active deployment. A displacement
			// strip fires `unlabeled` on the loser AFTER the winner is already
			// active, so the loser's release is a no-op (race-safe).
			if (ctx.activePreviewPr !== ctx.prNumber) {
				return {
					kind: "noop",
					reason: "not the active preview holder (stale/displaced)",
				};
			}
			return { kind: "release" };
		}
		case "closed": {
			// Closing releases only if this PR owned the live env.
			if (ctx.activePreviewPr !== ctx.prNumber) {
				return { kind: "noop", reason: "not the active preview holder" };
			}
			return { kind: "release" };
		}
	}
}

// ---------------------------------------------------------------------------
// Pure comment/payload/state builders (exported for unit tests).
// ---------------------------------------------------------------------------

/** The comment posted when a fork PR is refused the env. */
export function forkRejectionComment(): string {
	return (
		`${PREVIEW_MARKER}\n` +
		`### Compass preview — same-repo only\n\n` +
		"The `preview` label deploys a PR's full stack to a shared tailnet-gated " +
		"environment, so it is restricted to branches in this repository. A fork " +
		"PR can never claim the preview env or reach its deploy credentials.\n\n" +
		"The label has been removed. A maintainer can re-push this branch to the " +
		"canonical repo to preview it."
	);
}

/** The sticky comment posted on a PR displaced from the env by a new claimant. */
export function displacementComment(claimantPr: number): string {
	return (
		`${PREVIEW_MARKER}\n` +
		`### Compass preview — env released\n\n` +
		`This PR no longer holds the \`${PREVIEW_LABEL}\` env: PR #${claimantPr} ` +
		"claimed the single shared preview environment. Re-add the " +
		`\`${PREVIEW_LABEL}\` label to reclaim it (which will displace #${claimantPr}).`
	);
}

/**
 * The stable preview-link payload the result-surfacing lane (SEA-2014)
 * consumes. This lane OWNS the Deployment record (the canonical state) and
 * emits this payload as a workflow output; it does NOT render the sticky
 * preview comment (SEA-2014 owns that surface — design record §Review surface).
 */
export interface PreviewLinkPayload {
	prNumber: number;
	environmentUrl: string;
	marker: string;
}

/**
 * The cross-workflow interface SEA-2014 consumes is the GitHub Deployment
 * record (its `environment_url`, set on the success status); GITHUB_OUTPUT does
 * not cross workflows. The `preview_url` + `preview_pr` outputs below (marker
 * `PREVIEW_MARKER`) are a same-run/`::notice::` breadcrumb for local
 * visibility, not the cross-workflow payload.
 */

/** The GitHub deployment_status states this tool drives. */
export type DeploymentState =
	| "in_progress"
	| "success"
	| "failure"
	| "inactive";

// ---------------------------------------------------------------------------
// Execution wiring.
// ---------------------------------------------------------------------------

/** An active preview Deployment: its API id and the PR it tracks. */
export interface ActiveDeployment {
	id: number;
	pr: number;
}

/**
 * The GitHub side effects (label lifecycle + Deployments API). Injected so the
 * dispatch is tested with fakes. Real impls use the `gh` CLI (see main).
 */
export interface GitHubApi {
	/** Remove a label from a PR (best-effort on a fork: read-only token). */
	removeLabel(pr: number, label: string): Promise<void>;
	/** Post a comment on a PR. */
	postComment(pr: number, body: string): Promise<void>;
	/** Create a `preview`-environment Deployment for a PR; returns its id. */
	createDeployment(pr: number, ref: string): Promise<number>;
	/** Set a Deployment's status (+ environment_url on success). */
	setDeploymentStatus(
		deploymentId: number,
		state: DeploymentState,
		environmentUrl?: string,
	): Promise<void>;
	/** The current active preview Deployment, or null. */
	findActiveDeployment(): Promise<ActiveDeployment | null>;
}

/** The inputs a deploy needs: which PR + ref go onto the preview env. */
export interface DeployInput {
	prNumber: number;
	headSha: string;
}

/** The result of a successful deploy: where the preview is reachable. */
export interface DeployResult {
	environmentUrl: string;
}

/**
 * The mattfw-side deploy (checkout → service restart → UI build → serve) and
 * its inverse (release). Injected so the dispatch is tested without ssh. The
 * real impl ssh's a command sequence to mattfw (see main).
 */
export interface Deployer {
	deploy(input: DeployInput): Promise<DeployResult>;
	release(): Promise<void>;
}

/** Everything `runOnce` needs. */
export interface Deps {
	ctx: EventContext;
	gh: GitHubApi;
	deployer: Deployer;
	/** Emit a stable workflow output (name=value) for downstream lanes. */
	emitOutput: (name: string, value: string) => void;
	log: (msg: string) => void;
	err: (msg: string) => void;
}

/**
 * Apply the decision. Drives the Deployment record through
 * in_progress → success/failure (create-on-claim) and inactive-on-release, and
 * emits the preview-link payload on success. Returns the process exit code.
 */
export async function runOnce(deps: Deps): Promise<number> {
	const { ctx, gh, deployer, log, err } = deps;
	const decision = decide(ctx);

	switch (decision.kind) {
		case "noop":
			log(`compass-preview-deploy: no-op (${decision.reason}).`);
			return 0;

		case "reject-fork": {
			log(
				`compass-preview-deploy: fork PR #${ctx.prNumber} (${ctx.headRepo}) ` +
					"claimed the preview label — rejecting (no displace, no deploy).",
			);
			// Best-effort on a fork: the `on: pull_request` token is read-only for
			// a fork head, so these WILL 403. The security invariant does NOT depend
			// on them — the deploy simply never runs for a fork — so tolerate a
			// failing label API here (realGitHubApi swallows via .nothrow(); this
			// guard also holds for an injected API that throws) and still exit 0.
			try {
				await gh.removeLabel(ctx.prNumber, PREVIEW_LABEL);
				await gh.postComment(ctx.prNumber, forkRejectionComment());
			} catch (e) {
				err(
					`compass-preview-deploy: fork-reject label/comment failed (ignored): ${
						e instanceof Error ? e.message : String(e)
					}`,
				);
			}
			return 0;
		}

		case "claim": {
			// Displace every other holder FIRST, so exactly one PR owns the env.
			// We strip the loser's label + comment but do NOT explicitly mark its
			// Deployment inactive: GitHub's auto_inactive default flips prior
			// deployments on this environment inactive when the winner's success
			// status posts below (via deployAndRecord).
			for (const loser of decision.displaced) {
				log(`compass-preview-deploy: displacing PR #${loser}.`);
				await gh.removeLabel(loser, PREVIEW_LABEL);
				await gh.postComment(loser, displacementComment(ctx.prNumber));
			}
			return deployAndRecord(deps, "claim");
		}

		case "redeploy":
			return deployAndRecord(deps, "redeploy");

		case "release": {
			log(`compass-preview-deploy: releasing the env (PR #${ctx.prNumber}).`);
			try {
				await deployer.release();
			} catch (e) {
				err(
					`compass-preview-deploy: release failed: ${
						e instanceof Error ? e.message : String(e)
					}`,
				);
				return 1;
			}
			const active = await gh.findActiveDeployment();
			if (active && active.pr === ctx.prNumber) {
				await gh.setDeploymentStatus(active.id, "inactive");
			}
			return 0;
		}
	}
}

/**
 * The deploy half shared by `claim` and `redeploy`: create the Deployment,
 * drive its status, run the mattfw deploy, and emit the link payload. Marks the
 * Deployment `failure` and returns 1 on any deploy error.
 */
async function deployAndRecord(
	deps: Deps,
	origin: "claim" | "redeploy",
): Promise<number> {
	const { ctx, gh, deployer, emitOutput, log, err } = deps;
	log(
		`compass-preview-deploy: ${origin} — deploying PR #${ctx.prNumber} ` +
			`@ ${ctx.headSha} to the preview env.`,
	);
	const deploymentId = await gh.createDeployment(ctx.prNumber, ctx.headSha);
	await gh.setDeploymentStatus(deploymentId, "in_progress");
	try {
		const { environmentUrl } = await deployer.deploy({
			prNumber: ctx.prNumber,
			headSha: ctx.headSha,
		});
		await gh.setDeploymentStatus(deploymentId, "success", environmentUrl);
		// The CROSS-workflow interface SEA-2014 consumes is the GitHub Deployment
		// record (its environment_url, set on success above); these emitted outputs
		// are a same-run/`::notice::` breadcrumb for local visibility, not the
		// cross-workflow payload (GITHUB_OUTPUT does not cross workflows).
		emitOutput("preview_url", environmentUrl);
		emitOutput("preview_pr", String(ctx.prNumber));
		log(`compass-preview-deploy: preview up at ${environmentUrl}`);
		return 0;
	} catch (e) {
		await gh.setDeploymentStatus(deploymentId, "failure");
		err(
			`compass-preview-deploy: deploy failed: ${
				e instanceof Error ? e.message : String(e)
			}`,
		);
		return 1;
	}
}

// ---------------------------------------------------------------------------
// Real I/O impls (gh CLI + ssh to mattfw), wired only under `import.meta.main`.
// ---------------------------------------------------------------------------

/** Read a required env var, or throw a usage error naming it. */
function requireEnv(name: string): string {
	const v = process.env[name];
	if (!v) throw new Error(`${name} is required`);
	return v;
}

/** Split a comma-joined label list into names (empty string → no labels). */
export function parseLabels(raw: string | undefined): string[] {
	if (!raw) return [];
	return raw
		.split(",")
		.map((s) => s.trim())
		.filter((s) => s.length > 0);
}

/** The `payload` field for a preview Deployment: JSON so findActiveDeployment can recover the PR. Pure. */
export function deploymentPayload(pr: number): string {
	return JSON.stringify({ pr });
}

/**
 * Recover the PR a preview Deployment tracks from its `payload`, robust to
 * BOTH shapes GitHub can return: the Deployments API returns `payload` as a
 * JSON STRING (what `gh api -f payload=…` sends), but a caller may hold it
 * already parsed. Returns the numeric `pr`, or undefined for a non-object,
 * malformed JSON, or a missing/non-numeric `pr` — never throws.
 */
export function deploymentPr(payload: unknown): number | undefined {
	let obj: unknown = payload;
	if (typeof payload === "string") {
		try {
			obj = JSON.parse(payload);
		} catch {
			return undefined;
		}
	}
	if (typeof obj !== "object" || obj === null) return undefined;
	const pr = (obj as { pr?: unknown }).pr;
	return typeof pr === "number" ? pr : undefined;
}

/** The gh-CLI-backed GitHubApi. Every call names the repo explicitly. */
function realGitHubApi(repo: string): GitHubApi {
	return {
		removeLabel: async (pr, label) => {
			// -X DELETE the specific label; a 404 (already absent) is not fatal.
			await $`gh api --method DELETE repos/${repo}/issues/${pr}/labels/${label}`
				.nothrow()
				.quiet();
		},
		postComment: async (pr, body) => {
			await $`gh api --method POST repos/${repo}/issues/${pr}/comments -f body=${body}`
				.nothrow()
				.quiet();
		},
		createDeployment: async (pr, ref) => {
			// required_contexts is sent as a raw-JSON empty array `[]` (NOT `[""]`)
			// so the deployment is not left pending on a context literally named "";
			// the PR number rides in payload so findActiveDeployment can recover it.
			// Build the JSON in JS and interpolate the variable: a bun-shell string
			// literal `{"pr":123}` gets its quotes STRIPPED (yielding invalid JSON
			// `{pr:123}`), so the payload must be a single interpolated value. The
			// raw-JSON `required_contexts:=[]` likewise rides as an array element to
			// dodge quote-stripping (mirrors setDeploymentStatus's args array below).
			const payload = deploymentPayload(pr);
			const args = [
				"--method",
				"POST",
				`repos/${repo}/deployments`,
				"-f",
				`ref=${ref}`,
				"-f",
				`environment=${PREVIEW_ENVIRONMENT}`,
				"-F",
				"auto_merge=false",
				"-F",
				"required_contexts:=[]",
				"-f",
				`payload=${payload}`,
			];
			const out = await $`gh api ${args} --jq .id`.text();
			const id = Number.parseInt(out.trim(), 10);
			if (!Number.isFinite(id)) {
				throw new Error(`could not parse deployment id from: ${out}`);
			}
			return id;
		},
		setDeploymentStatus: async (deploymentId, state, environmentUrl) => {
			const args = [
				"--method",
				"POST",
				`repos/${repo}/deployments/${deploymentId}/statuses`,
				"-f",
				`state=${state}`,
				"-f",
				`environment=${PREVIEW_ENVIRONMENT}`,
			];
			if (environmentUrl) args.push("-f", `environment_url=${environmentUrl}`);
			await $`gh api ${args}`.nothrow().quiet();
		},
		findActiveDeployment: async () => {
			// Newest-first preview deployments; the first whose latest status is
			// success or in_progress is the live one. Its payload carries the PR.
			const raw =
				await $`gh api repos/${repo}/deployments?environment=${PREVIEW_ENVIRONMENT}&per_page=30`.json();
			const deployments = raw as Array<{ id: number; payload?: unknown }>;
			for (const d of deployments) {
				const statuses =
					await $`gh api repos/${repo}/deployments/${d.id}/statuses?per_page=1`.json();
				const latest = (statuses as Array<{ state: string }>)[0];
				if (!latest) continue;
				if (latest.state === "success" || latest.state === "in_progress") {
					const pr = deploymentPr(d.payload);
					if (pr !== undefined) return { id: d.id, pr };
				}
			}
			return null;
		},
	};
}

/**
 * The ssh-to-mattfw Deployer. The runner has already joined the tailnet as an
 * ephemeral tagged node (the workflow's tailscale step); this ssh's the deploy
 * sequence to mattfw's `compass-preview` user, whose checkout + service +
 * doors Record A provisioned. The command is composed here (TS), not as shell
 * logic in YAML (the no-bash-gate); values ride in env, never interpolated
 * into the remote command line.
 */
function realDeployer(cfg: {
	sshHost: string;
	sshUser: string;
	checkout: string;
	doorUrl: string;
	previewUrl: string;
	adminAccount: string;
}): Deployer {
	// The remote deploy sequence, run in one ssh session as the preview user.
	// It: pins the PR ref, restarts the service (Record A: the unit wraps
	// `devenv up`; "may be torn down/redeployed freely"), waits for readiness,
	// mints a reviewer bearer ON preview via IssueToken against the preview
	// admin account, builds the PR's UI against preview's TLS door, and serves
	// the dist at the root over `tailscale serve` (tailnet HTTPS; NEVER funnel).
	const deployScript = [
		"set -euo pipefail",
		'cd "$PREVIEW_CHECKOUT"',
		'git fetch --force origin "$HEAD_SHA"',
		'git checkout --force --detach "$HEAD_SHA"',
		"systemctl --user restart compass-preview.service",
		// Readiness: poll the preview TLS door's GetServerInfo before minting.
		'for i in $(seq 1 60); do if curl -fsS --max-time 3 "$PREVIEW_DOOR_URL" >/dev/null 2>&1; then break; fi; sleep 2; done',
		// Mint the reviewer bearer ON preview (IssueToken against the admin
		// account). Admin-scoped is acceptable — the env is disposable and holds
		// no `main` authority (design record §"Preview mints its own creds").
		'TOKEN="$(compass token issue --account-id "$PREVIEW_ADMIN_ACCOUNT")"',
		'VITE_COMPASS_BASE_URL="$PREVIEW_DOOR_URL" VITE_COMPASS_TOKEN="$TOKEN" moon run compass-ui:build',
		// Serve the built dist at the root over tailnet HTTPS. NEVER funnel.
		'tailscale serve --bg --https=443 "$PREVIEW_CHECKOUT/apps/ui/dist"',
	].join("\n");

	const releaseScript = [
		"set -euo pipefail",
		// Take the served path down and stop the service (cheapest release; a
		// claim redeploys from scratch — design record §P1 label lifecycle).
		"tailscale serve --https=443 off || true",
		"systemctl --user stop compass-preview.service || true",
	].join("\n");

	const target = `${cfg.sshUser}@${cfg.sshHost}`;
	return {
		deploy: async ({ headSha }) => {
			// stdin is fed via `< ${Buffer}` redirection (Bun's $.stdin is a
			// stream property, not a setter); the deploy config rides as remote
			// `env VAR=…` args, each interpolation a single escaped argument.
			const res =
				await $`ssh -o StrictHostKeyChecking=accept-new ${target} env PREVIEW_CHECKOUT=${cfg.checkout} PREVIEW_DOOR_URL=${cfg.doorUrl} PREVIEW_ADMIN_ACCOUNT=${cfg.adminAccount} HEAD_SHA=${headSha} bash -s < ${Buffer.from(deployScript)}`.nothrow();
			if (res.exitCode !== 0) {
				throw new Error(
					`remote deploy exited ${res.exitCode}: ${res.stderr.toString()}`,
				);
			}
			return { environmentUrl: cfg.previewUrl };
		},
		release: async () => {
			const res =
				await $`ssh -o StrictHostKeyChecking=accept-new ${target} bash -s < ${Buffer.from(releaseScript)}`.nothrow();
			if (res.exitCode !== 0) {
				throw new Error(
					`remote release exited ${res.exitCode}: ${res.stderr.toString()}`,
				);
			}
		},
	};
}

/** Build the event context from the environment + gh queries. */
async function contextFromEnv(gh: GitHubApi): Promise<EventContext> {
	const actionRaw = requireEnv("EVENT_ACTION");
	if (!isEventAction(actionRaw)) {
		throw new Error(`unsupported EVENT_ACTION: ${actionRaw}`);
	}
	const prNumber = Number.parseInt(requireEnv("PR_NUMBER"), 10);
	if (!Number.isFinite(prNumber)) throw new Error("PR_NUMBER is not a number");
	const baseRepo = requireEnv("BASE_REPO");
	const repo = process.env.REPO ?? baseRepo;

	// Current holders (for a claim's displacement set): open PRs with the label.
	let currentHolders: number[] = [];
	if (actionRaw === "labeled") {
		const raw =
			await $`gh pr list --repo ${repo} --label ${PREVIEW_LABEL} --state open --json number --jq [.[].number]`
				.nothrow()
				.text();
		try {
			currentHolders = (JSON.parse(raw.trim() || "[]") as number[]) ?? [];
		} catch {
			currentHolders = [];
		}
	}

	const active = await gh.findActiveDeployment();

	return {
		action: actionRaw,
		prNumber,
		headRepo: process.env.HEAD_REPO ?? "",
		baseRepo,
		headSha: process.env.HEAD_SHA ?? "",
		changedLabel: process.env.CHANGED_LABEL || null,
		prLabels: parseLabels(process.env.PR_LABELS),
		currentHolders,
		activePreviewPr: active ? active.pr : null,
	};
}

if (import.meta.main) {
	const repo = requireEnv("BASE_REPO");
	const gh = realGitHubApi(process.env.REPO ?? repo);
	const deployer = realDeployer({
		sshHost: process.env.PREVIEW_SSH_HOST ?? "mattfw",
		sshUser: process.env.PREVIEW_SSH_USER ?? "compass-preview",
		checkout: process.env.PREVIEW_CHECKOUT ?? "~/compass-envs/preview",
		doorUrl: process.env.PREVIEW_DOOR_URL ?? "https://mattfw:50161",
		previewUrl: process.env.PREVIEW_URL ?? "https://mattfw/",
		adminAccount: process.env.PREVIEW_ADMIN_ACCOUNT ?? "preview-admin",
	});

	let ctx: EventContext;
	try {
		ctx = await contextFromEnv(gh);
	} catch (e) {
		console.error(
			`compass-preview-deploy: ${e instanceof Error ? e.message : String(e)}`,
		);
		process.exit(2);
	}

	const outputFile = process.env.GITHUB_OUTPUT;
	process.exit(
		await runOnce({
			ctx,
			gh,
			deployer,
			emitOutput: (name, value) => {
				// GITHUB_OUTPUT is append-only (multiple outputs across calls); a
				// truncating write would clobber the earlier one. Append via the
				// node fs binding — Bun.write has no append mode.
				if (outputFile) appendFileSync(outputFile, `${name}=${value}\n`);
				console.log(`::notice::${name}=${value}`);
			},
			log: (msg) => console.log(msg),
			err: (msg) => console.error(msg),
		}),
	);
}
