import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type } from "@oh-my-pi/omptype/ark";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import {
	create,
	type ForgeCallRequest,
	ForgeCallRequestSchema,
	RetireHeadRequestSchema,
	type StackEntry,
	type StackEntryResult,
	StackEntrySchema,
	StackEntryStatus,
	SubmitStackRequestSchema,
	toBinary,
} from "./compassv1";
import { forgeFailure } from "./forge";
import { attr, flat, ref } from "./render-guard";
import {
	argsForLog,
	composeSections,
	JJ_FLAGS,
	type JjRow,
	originRemote,
	ownCommits,
	parseRows,
	quoteRevset,
	type Remote,
	resolveTip,
	runJj,
	stackStatus,
	syncOwn,
	type VcsDeps,
	validateBookmark,
} from "./vcs";

const HOOK_TIMEOUT_MS = 120_000;
// The gateway refuses a message over 16 MiB; the margin covers framing and relay envelopes.
export const MAX_SUBMIT_BYTES = 15 * 1024 * 1024;
export const HOOK_TIMEOUT_NOTE =
	"pre-push hook timed out after 120 s: find out why and fix the hook in a change of its own";
const NO_ISSUE_HINT =
	"find an issue that fits with forge_list_issues, or create one with forge_create_issue when none does, then submit again";
const ZERO_SHA = "0".repeat(40);

export const vcsSubmitParameters = type({
	"bookmark?": type("string").describe(
		"Bookmark at the tip of the stack; omit to use the one bookmark on @ or @-",
	),
	"issues?": type("Record<string, string>").describe(
		'Bookmark -> issue it works on ("#12", "owner/repo#12", or a Linear key); required for a bookmark with no PR',
	),
	"ready?": type("boolean").describe("Take the tip PR out of draft"),
	"text?": type({
		bookmark: type("string").describe("Bookmark whose PR text to replace"),
		subject: type("string").describe("New PR title, without the issue"),
		sections: type("Record<string, string>").describe(
			"New PR body keyed by the pull request template headings",
		),
	}).describe("Replace one PR's title and body"),
});

export const vcsRetireParameters = type({
	bookmark: type("string").describe(
		"Own bookmark whose PR to close and branch to delete",
	),
});

export const vcsPrStatusParameters = type({
	"bookmark?": type("string").describe(
		"Bookmark at the tip of the stack; omit to use the one bookmark on @ or @-",
	),
});

interface Segment {
	bookmark: string;
	commit: string;
	// The description of the segment's first commit, `roots(<below>..<bookmark>)`.
	first: string;
}

/** Splits the own rows into one segment per bookmark, bottom to top. */
function segments(rows: JjRow[], toolName: string): Segment[] {
	const out: Segment[] = [];
	let first: string | undefined;
	for (const row of [...rows].reverse()) {
		first ??= row.description ?? "";
		const [bookmark] = row.localBookmarks;
		if (bookmark === undefined) continue;
		out.push({ bookmark, commit: row.commit_id ?? "", first });
		first = undefined;
	}
	if (first !== undefined)
		throw new Error(`${toolName}: own commits above the last bookmark`);
	return out;
}

/** The D4 refusals that the own rows alone decide. */
function checkRows(rows: JjRow[], toolName: string): void {
	for (const row of rows) {
		const id = row.commit_id ?? row.change_id ?? "unknown commit";
		if (row.parents.length > 1)
			throw new Error(`${toolName}: ${id} is a merge; stacks must be linear`);
		if ((row.description ?? "").trim().length === 0)
			throw new Error(`${toolName}: ${id} has no description`);
		if (row.localBookmarks.length > 1)
			throw new Error(
				`${toolName}: ${id} carries two bookmarks: ${row.localBookmarks.join(", ")}`,
			);
	}
}

async function checkAbove(
	deps: VcsDeps,
	tip: string,
	toolName: string,
): Promise<void> {
	const above = parseRows(
		(
			await runJj(
				deps,
				toolName,
				// Untracked remote heads above the tip are other agents' PRs, not this agent's unpublished work.
				argsForLog(
					`${quoteRevset(tip)}:: ~ ${quoteRevset(tip)} ~ ::untracked_remote_bookmarks()`,
				),
			)
		).stdout,
		toolName,
	);
	// The empty, undescribed `@` that vcs_commit leaves on the tip is not work.
	const unpublished = above.filter(
		(row) => !row.empty || (row.description ?? "").trim().length > 0,
	);
	if (unpublished.length > 0)
		throw new Error(
			`${toolName}: commits above ${tip} are not in the stack: ${unpublished.map((row) => row.commit_id).join(", ")}; move the bookmark or commit them first`,
		);
	const forks = parseRows(
		(
			await runJj(
				deps,
				toolName,
				argsForLog(
					`(descendants(${ownCommits(tip)}) & bookmarks()) ~ ::${quoteRevset(tip)}`,
				),
			)
		).stdout,
		toolName,
	);
	if (forks.length > 0)
		throw new Error(
			`${toolName}: the stack forks at ${forks.flatMap((row) => row.localBookmarks).join(", ")}; stacks must be one line`,
		);
}

/** The branch a line's bottom PR targets: the trunk branch or the foreign one. */
async function baseBranch(
	deps: VcsDeps,
	base: string,
	toolName: string,
): Promise<string> {
	if (base !== "trunk()") return base.replace(/@origin$/, "");
	const rows = parseRows(
		(await runJj(deps, toolName, argsForLog("trunk()"))).stdout,
		toolName,
	);
	const branch = rows[0]?.remoteBookmarks
		.find((name) => name.endsWith("@origin"))
		?.replace(/@origin$/, "");
	if (!branch) throw new Error(`${toolName}: trunk() has no origin bookmark`);
	return branch;
}

function splitDescription(description: string): {
	subject: string;
	rest: string;
} {
	const [subject = "", ...rest] = description.trim().split("\n");
	return { subject: subject.trim(), rest: rest.join("\n").trim() };
}

function withIssue(
	subject: string,
	body: string,
	issue: string | undefined,
): { title: string; body: string } {
	if (!issue) return { title: subject, body };
	return {
		title: `${subject} (${issue})`,
		body: body ? `${body}\n\nRefs ${issue}` : `Refs ${issue}`,
	};
}

/** Runs the repo's own pre-push hook with the stdin a real push gives it (D4, OQ-9). */
async function runPrePush(
	deps: VcsDeps,
	remote: Remote,
	segs: Segment[],
	toolName: string,
): Promise<boolean> {
	const listed = await deps.git.run(
		[
			"for-each-ref",
			"--format=%(refname) %(objectname)",
			"refs/remotes/origin/",
		],
		deps.cwd,
	);
	if (listed.code !== 0)
		throw new Error(`${toolName}: git for-each-ref: ${listed.stderr.trim()}`);
	const remoteSha = new Map(
		listed.stdout
			.split("\n")
			.map((line) => line.split(" "))
			.filter((parts) => parts.length === 2)
			.map(([name = "", sha = ""]) => [
				name.replace(/^refs\/remotes\/origin\//, ""),
				sha,
			]),
	);
	const stdin = segs
		.map(
			(seg) =>
				`refs/heads/${seg.bookmark} ${seg.commit} refs/heads/${seg.bookmark} ${remoteSha.get(seg.bookmark) ?? ZERO_SHA}`,
		)
		.join("\n");
	const dir = await mkdtemp(join(tmpdir(), "vcs-pre-push-"));
	try {
		const file = join(dir, "stdin");
		await writeFile(file, `${stdin}\n`);
		const hook = await deps.git.run(
			[
				"hook",
				"run",
				"--ignore-missing",
				`--to-stdin=${file}`,
				"pre-push",
				"--",
				"origin",
				remote.url,
			],
			deps.cwd,
			{ timeoutMs: deps.hookTimeoutMs ?? HOOK_TIMEOUT_MS },
		);
		if (hook.timedOut) return true;
		if (hook.code !== 0) {
			const output = (hook.stderr.trim() || hook.stdout.trim()).slice(-4000);
			throw new Error(
				`${toolName}: pre-push hook failed (exit ${hook.code}); fix it and submit again:\n${output}`,
			);
		}
		return false;
	} finally {
		await rm(dir, { recursive: true, force: true });
	}
}

async function bundle(
	deps: VcsDeps,
	segs: Segment[],
	baseCommit: string,
	toolName: string,
): Promise<Uint8Array> {
	const result = await deps.git.run(
		[
			"bundle",
			"create",
			"-",
			...segs.map((seg) => `refs/heads/${seg.bookmark}`),
			`^${baseCommit}`,
		],
		deps.cwd,
		{ binary: true },
	);
	if (result.code !== 0 || !result.bytes)
		throw new Error(`${toolName}: git bundle: ${result.stderr.trim()}`);
	return result.bytes;
}

function renderEntry(entry: StackEntryResult): string {
	const pr = entry.pullRequest;
	const head = attr(entry.headRef);
	if (entry.status === StackEntryStatus.SKIPPED)
		return `${head}: skipped (an entry below failed)`;
	if (entry.error)
		return `${head}: failed: ${attr(entry.error.code)}: ${flat(entry.error.message).slice(0, 500)}`;
	if (!pr) return `${head}: no pull request`;
	const parts = [
		`${head}: #${pr.number}`,
		pr.forgeState,
		pr.draft ? "draft" : "ready",
		pr.checks ? `checks ${attr(pr.checks.state)}` : "",
		entry.created ? "created" : "",
		entry.authored ? "" : "not authored by you",
	].filter(Boolean);
	return `${parts.join(", ")} ${ref(pr.url)}`;
}

type SubmitParams = typeof vcsSubmitParameters.infer;

/** Refuses new PRs with no issue: the Server would refuse each one after the push. */
async function requireIssues(
	deps: VcsDeps,
	toolCallId: string,
	repo: string,
	segs: Segment[],
	issues: SubmitParams["issues"],
	toolName: string,
): Promise<void> {
	const known = await stackStatus(
		deps,
		toolCallId,
		repo,
		segs.map((seg) => seg.bookmark),
		toolName,
	);
	const lookupFailed = known.filter((e) => e.error);
	if (lookupFailed.length > 0)
		throw new Error(
			`${toolName}: could not read the stack's PRs:\n${lookupFailed.map(renderEntry).join("\n")}`,
		);
	const hasPr = new Set(
		known.filter((e) => e.pullRequest).map((e) => e.headRef),
	);
	const missing = segs.filter(
		(seg) => !hasPr.has(seg.bookmark) && !issues?.[seg.bookmark],
	);
	if (missing.length > 0)
		throw new Error(
			`${toolName}: ${missing.map((seg) => seg.bookmark).join(", ")} has no pull request and no issue; ${NO_ISSUE_HINT}`,
		);
}

/** One entry per bookmark, bottom to top, with D3 title and body. */
async function buildEntries(
	deps: VcsDeps,
	repo: string,
	segs: Segment[],
	params: SubmitParams,
	toolName: string,
): Promise<StackEntry[]> {
	const entries: StackEntry[] = [];
	for (const seg of segs) {
		const given = params.issues?.[seg.bookmark];
		// A bare `#12` names an issue in the PR's own repo.
		const issue = given && /^#\d+$/.test(given) ? `${repo}${given}` : given;
		const text =
			params.text?.bookmark === seg.bookmark ? params.text : undefined;
		const source = text
			? {
					subject: text.subject,
					rest: await composeSections(deps, deps.cwd, text.sections, toolName),
				}
			: splitDescription(seg.first);
		const titled = withIssue(source.subject, source.rest, issue);
		entries.push(
			create(StackEntrySchema, {
				headRef: seg.bookmark,
				title: titled.title,
				body: titled.body,
				issueRef: issue ?? "",
				replaceText: text !== undefined,
			}),
		);
	}
	return entries;
}

export function createSubmitTools(deps: VcsDeps): AgentTool[] {
	const submit: AgentTool<typeof vcsSubmitParameters> = {
		name: "vcs_submit",
		label: "Submit stack as pull requests",
		approval: "write",
		description: `Sync, check, run the repo's pre-push hook, then push your stack through the Server and create or update one draft PR per bookmark. Every new PR needs an issue in "issues": ${NO_ISSUE_HINT}. PR text comes from each bookmark's first commit and is kept until "text" replaces it.`,
		parameters: vcsSubmitParameters,
		execute: async (toolCallId, params) => {
			const name = "vcs_submit";
			validateBookmark(params.text?.bookmark, name);
			const remote = await originRemote(deps, name);
			const synced = await syncOwn(deps, toolCallId, name, params.bookmark);
			const root = synced.rows[synced.rows.length - 1];
			const baseCommit = root?.parents[0]?.commit_id;
			if (!baseCommit)
				throw new Error(
					`${name}: ${synced.tip} is in trunk; nothing to submit`,
				);
			checkRows(synced.rows, name);
			await checkAbove(deps, synced.tip, name);
			const segs = segments(synced.rows, name);
			const textBookmark = params.text?.bookmark;
			if (textBookmark && !segs.some((seg) => seg.bookmark === textBookmark))
				throw new Error(
					`${name}: text names ${textBookmark}, which is not in the stack`,
				);
			await requireIssues(
				deps,
				toolCallId,
				remote.repo,
				segs,
				params.issues,
				name,
			);
			const entries = await buildEntries(deps, remote.repo, segs, params, name);
			const bottom = entries[0];
			if (bottom) bottom.baseRef = await baseBranch(deps, synced.base, name);
			const timedOut = await runPrePush(deps, remote, segs, name);
			const req: ForgeCallRequest = create(ForgeCallRequestSchema, {
				callId: toolCallId,
				call: {
					case: "submitStack",
					value: create(SubmitStackRequestSchema, {
						repo: remote.repo,
						entries,
						readyHeadRef: params.ready ? synced.tip : "",
						bundle: await bundle(deps, segs, baseCommit, name),
					}),
				},
				clientRequestId: deps.forge.idempotencyKey(toolCallId),
			});
			const size = toBinary(ForgeCallRequestSchema, req).length;
			if (size > MAX_SUBMIT_BYTES)
				throw new Error(
					`${name}: the submit is ${size} bytes, over the ${MAX_SUBMIT_BYTES}-byte limit; split the stack or drop large files`,
				);
			const result = await deps.forge.call(req);
			if (result.result.case !== "stack")
				throw forgeFailure(result, name, "stack");
			const lines = result.result.value.entries.map(renderEntry);
			if (timedOut) lines.push(HOOK_TIMEOUT_NOTE);
			const text = lines.join("\n");
			if (
				result.result.value.entries.some(
					(e) => e.status !== StackEntryStatus.DONE,
				)
			)
				throw new Error(`${name}: not every entry was submitted:\n${text}`);
			return { content: [{ type: "text", text }] };
		},
	};

	const retire: AgentTool<typeof vcsRetireParameters> = {
		name: "vcs_retire",
		label: "Retire a bookmark and its PR",
		approval: "write",
		description:
			"Close your PR for one bookmark, delete its remote branch through the Server, then delete the local bookmark. Safe to retry.",
		parameters: vcsRetireParameters,
		execute: async (toolCallId, params) => {
			const name = "vcs_retire";
			validateBookmark(params.bookmark, name);
			const remote = await originRemote(deps, name);
			const result = await deps.forge.call(
				create(ForgeCallRequestSchema, {
					callId: toolCallId,
					call: {
						case: "retireHead",
						value: create(RetireHeadRequestSchema, {
							repo: remote.repo,
							headRef: params.bookmark,
						}),
					},
				}),
			);
			if (result.result.case !== "stack")
				throw forgeFailure(result, name, "stack");
			await runJj(deps, name, [
				...JJ_FLAGS,
				"bookmark",
				"delete",
				`exact:${params.bookmark}`,
			]);
			// Fetch only this branch: a full fetch would bring in other own heads untracked, and only sync tracks them.
			await runJj(deps, name, [
				...JJ_FLAGS,
				"git",
				"fetch",
				"--remote",
				"origin",
				"--branch",
				`exact:${params.bookmark}`,
			]);
			return {
				content: [
					{
						type: "text",
						text: result.result.value.entries.map(renderEntry).join("\n"),
					},
				],
			};
		},
	};

	const prStatus: AgentTool<typeof vcsPrStatusParameters> = {
		name: "vcs_pr_status",
		label: "Show stack PR status",
		approval: "read",
		description:
			"Show the PR, state, draft, checks, and authorship for each bookmark in your stack, bottom to top.",
		parameters: vcsPrStatusParameters,
		execute: async (toolCallId, params) => {
			const name = "vcs_pr_status";
			validateBookmark(params.bookmark, name);
			const remote = await originRemote(deps, name);
			const tip = await resolveTip(deps, params.bookmark, name);
			const rows = parseRows(
				(await runJj(deps, name, argsForLog(ownCommits(tip)))).stdout,
				name,
			);
			const heads = [...rows].reverse().flatMap((row) => row.localBookmarks);
			if (heads.length === 0)
				return {
					content: [{ type: "text", text: `No own bookmarks for ${tip}.` }],
					useless: true,
				};
			const entries = await stackStatus(
				deps,
				toolCallId,
				remote.repo,
				heads,
				name,
			);
			return {
				content: [{ type: "text", text: entries.map(renderEntry).join("\n") }],
			};
		},
	};

	return [submit, retire, prStatus];
}
