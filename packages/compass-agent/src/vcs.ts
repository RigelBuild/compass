import { isAbsolute, resolve } from "node:path";
import { type } from "@oh-my-pi/omptype/ark";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import {
	create,
	ForgeCallRequestSchema,
	type StackEntryResult,
	StackStatusRequestSchema,
} from "./compassv1";
import { type ForgeBroker, forgeFailure } from "./forge";
import type { CommandRunner } from "./vcs-runner";

export interface JjResult {
	stdout: string;
	stderr: string;
	code: number;
}

export interface JjRunner {
	run(args: readonly string[], cwd: string): Promise<JjResult>;
}

export interface VcsDeps {
	jj: JjRunner;
	// Runs git in the colocated clone: the origin URL, the pre-push hook, and the bundle.
	git: CommandRunner;
	cwd: string;
	forge: ForgeBroker;
	readFile?: (path: string) => Promise<string | undefined>;
	// Overrides the 120 s pre-push limit (OQ-9) in tests.
	hookTimeoutMs?: number;
}

export const JJ_FLAGS = ["--no-pager", "--color=never"] as const;
const LOG_TEMPLATE =
	'json(self) ++ "\\t" ++ self.local_bookmarks().map(|bookmark| bookmark.name()).join(",") ++ "\\t" ++ self.remote_bookmarks().map(|bookmark| bookmark.name() ++ "@" ++ bookmark.remote()).join(",") ++ "\\t" ++ json(self.contained_in("trunk()")) ++ "\\t" ++ json(self.conflict()) ++ "\\t" ++ json(self.empty()) ++ "\\t" ++ json(self.current_working_copy()) ++ "\\t" ++ json(self.mine()) ++ "\\t" ++ self.parents().map(|parent| parent.commit_id() ++ ":" ++ json(parent.contained_in("trunk()")) ++ ":" ++ parent.remote_bookmarks().map(|bookmark| bookmark.name() ++ "@" ++ bookmark.remote()).join(",")).join(";") ++ "\\n"';

type JjParent = {
	commit_id: string;
	trunk: boolean;
	remoteBookmarks: string[];
};
export type JjRow = {
	commit_id?: string;
	change_id?: string;
	description?: string;
	localBookmarks: string[];
	remoteBookmarks: string[];
	trunk: boolean;
	conflict: boolean;
	empty: boolean;
	workingCopy: boolean;
	mine: boolean;
	parents: JjParent[];
};

type JjWorkspace = { name: string; root: string };

const WORKSPACE_TEMPLATE = 'self.name() ++ "\\t" ++ self.root() ++ "\\n"';

function isStringArray(value: unknown): value is string[] {
	return (
		Array.isArray(value) &&
		value.every((item: unknown) => typeof item === "string")
	);
}

function isJjCommitRow(
	value: unknown,
): value is Pick<JjRow, "commit_id" | "change_id" | "description"> {
	return (
		typeof value === "object" &&
		value !== null &&
		("commit_id" in value ? typeof value.commit_id === "string" : true) &&
		("change_id" in value ? typeof value.change_id === "string" : true) &&
		("description" in value ? typeof value.description === "string" : true)
	);
}

function isJjParent(value: unknown): value is JjParent {
	return (
		typeof value === "object" &&
		value !== null &&
		"commit_id" in value &&
		typeof value.commit_id === "string" &&
		"trunk" in value &&
		typeof value.trunk === "boolean" &&
		"remoteBookmarks" in value &&
		isStringArray(value.remoteBookmarks)
	);
}

function isJjRow(value: unknown): value is JjRow {
	return (
		typeof value === "object" &&
		value !== null &&
		isJjCommitRow(value) &&
		"localBookmarks" in value &&
		isStringArray(value.localBookmarks) &&
		"remoteBookmarks" in value &&
		isStringArray(value.remoteBookmarks) &&
		"trunk" in value &&
		typeof value.trunk === "boolean" &&
		"conflict" in value &&
		typeof value.conflict === "boolean" &&
		"empty" in value &&
		typeof value.empty === "boolean" &&
		"workingCopy" in value &&
		typeof value.workingCopy === "boolean" &&
		"mine" in value &&
		typeof value.mine === "boolean" &&
		"parents" in value &&
		Array.isArray(value.parents) &&
		value.parents.every(isJjParent)
	);
}

function parseRow(line: string, toolName: string): JjRow {
	const fields = line.split("\t");
	if (fields.length !== 9)
		throw new Error(`${toolName}: jj returned an invalid log row`);
	try {
		const parsed: unknown = JSON.parse(fields[0] ?? "");
		if (!isJjCommitRow(parsed))
			throw new Error(`${toolName}: jj returned an invalid log row`);
		const parents = fields[8]
			? fields[8].split(";").filter(isString).map(parseParent)
			: [];
		const row: unknown = {
			...parsed,
			localBookmarks: fields[1]?.split(",").filter(nonEmpty) ?? [],
			remoteBookmarks: fields[2]?.split(",").filter(nonEmpty) ?? [],
			trunk: fields[3] === "true",
			conflict: fields[4] === "true",
			empty: fields[5] === "true",
			workingCopy: fields[6] === "true",
			mine: fields[7] === "true",
			parents,
		};
		if (!isJjRow(row))
			throw new Error(`${toolName}: jj returned an invalid log row`);
		return row;
	} catch (error) {
		if (error instanceof Error && error.message.startsWith(`${toolName}:`))
			throw error;
		throw new Error(`${toolName}: jj returned invalid JSON`, { cause: error });
	}
}

function parseWorkspaces(output: string, toolName: string): JjWorkspace[] {
	return output
		.split("\n")
		.filter((line) => line.length > 0)
		.map((line) => {
			const separator = line.indexOf("\t");
			if (separator < 1 || separator === line.length - 1)
				throw new Error(`${toolName}: jj returned an invalid workspace list`);
			return {
				name: line.slice(0, separator),
				root: line.slice(separator + 1),
			};
		});
}

async function resolveWorkspace(
	deps: VcsDeps,
	workspace: string | undefined,
	toolName: string,
): Promise<string> {
	const selected = await findWorkspace(deps, workspace, toolName);
	return selected ? resolve(selected.root) : deps.cwd;
}

async function findWorkspace(
	deps: VcsDeps,
	workspace: string | undefined,
	toolName: string,
): Promise<JjWorkspace | undefined> {
	if (!workspace) return undefined;
	const output = await runJj(deps, toolName, [
		...JJ_FLAGS,
		// A stale caller checkout must still list workspaces, so update-stale can run.
		"--ignore-working-copy",
		"workspace",
		"list",
		"-T",
		WORKSPACE_TEMPLATE,
	]);
	const workspaces = parseWorkspaces(output.stdout, toolName);
	const requestedRoot = isAbsolute(workspace)
		? resolve(workspace)
		: workspace.split(/[\\/]+/).includes("..")
			? ""
			: resolve(deps.cwd, workspace);
	const match = workspaces.find(
		(candidate) =>
			candidate.name === workspace ||
			(requestedRoot.length > 0 && resolve(candidate.root) === requestedRoot),
	);
	if (!match)
		throw new Error(
			`${toolName}: workspace ${workspace} is not listed for this repository`,
		);
	return match;
}

export function validateBookmark(
	bookmark: string | undefined,
	toolName: string,
): void {
	if (
		bookmark !== undefined &&
		!/^[A-Za-z0-9][A-Za-z0-9._/@-]*$/.test(bookmark)
	)
		throw new Error(`${toolName}: expected a plain bookmark name`);
}

function parseParent(parent: string): JjParent {
	const [commit_id = "", trunk = "false", remote = ""] = parent.split(":");
	return {
		commit_id,
		trunk: trunk === "true",
		remoteBookmarks: remote.split(",").filter(nonEmpty),
	};
}

function nonEmpty(value: string): boolean {
	return value.length > 0;
}

export function parseRows(stdout: string, toolName: string): JjRow[] {
	return stdout
		.split("\n")
		.filter((line) => line.trim().length > 0)
		.map((line) => parseRow(line, toolName));
}

function validateSections(
	firstCommit: boolean,
	sections: Record<string, string> | undefined,
	headings: string[],
	toolName = "vcs_commit",
): void {
	if (!firstCommit && sections !== undefined)
		throw new Error(
			`${toolName}: sections are only allowed on the first commit of a bookmark`,
		);
	const unknown = Object.keys(sections ?? {}).filter(
		(heading) => !headings.includes(heading),
	);
	if (unknown.length > 0)
		throw new Error(
			`${toolName}: unknown template heading(s): ${unknown.join(", ")}`,
		);
	const missing = firstCommit
		? headings.filter((heading) => sections?.[heading] === undefined)
		: [];
	if (missing.length > 0)
		throw new Error(
			`${toolName}: first commit is missing template heading(s): ${missing.join(", ")}`,
		);
}

/** Checks first-commit sections against the repo PR template and renders them. */
export async function composeSections(
	deps: VcsDeps,
	cwd: string,
	sections: Record<string, string>,
	toolName: string,
): Promise<string> {
	const headings = parseHeadings(await readTemplate(deps, cwd));
	validateSections(true, sections, headings, toolName);
	const authoredWords = Object.values(sections)
		.join(" ")
		.trim()
		.split(/\s+/)
		.filter((word) => word.length > 0).length;
	if (authoredWords < 40 || authoredWords > 300)
		throw new Error(
			`${toolName}: authored sections must contain 40 to 300 words; found ${authoredWords}`,
		);
	return renderSections(sections, headings);
}

export function argsForLog(revset: string): string[] {
	return [...JJ_FLAGS, "log", "--no-graph", "-r", revset, "-T", LOG_TEMPLATE];
}

function failIfJjFailed(toolName: string, result: JjResult): void {
	if (result.code !== 0) {
		const details =
			result.stderr.trim() || result.stdout.trim() || `exit ${result.code}`;
		throw new Error(`${toolName}: ${details}`);
	}
}

export async function runJj(
	deps: VcsDeps,
	toolName: string,
	args: readonly string[],
	cwd = deps.cwd,
): Promise<JjResult> {
	const result = await deps.jj.run(args, cwd);
	failIfJjFailed(toolName, result);
	return result;
}

export async function resolveTip(
	deps: VcsDeps,
	bookmark: string | undefined,
	toolName: string,
	cwd = deps.cwd,
): Promise<string> {
	if (bookmark) return bookmark;
	const result = await runJj(deps, toolName, argsForLog("@ | @-"), cwd);
	const localBookmarks = new Set(
		parseRows(result.stdout, toolName).flatMap((row) => row.localBookmarks),
	);
	if (localBookmarks.size !== 1)
		throw new Error(
			`${toolName}: expected exactly one bookmark across @ and @-`,
		);
	return localBookmarks.values().next().value ?? "";
}

export function ownCommits(tip: string): string {
	return tip === "@"
		? "::@ ~ ::(trunk() | untracked_remote_bookmarks())"
		: `::${quoteRevset(tip)} ~ ::(trunk() | untracked_remote_bookmarks())`;
}

export function quoteRevset(name: string): string {
	return `"${name.replaceAll("\\", "\\\\").replaceAll('"', '\\"')}"`;
}

function parseHeadings(template: string | undefined): string[] {
	return [...(template?.matchAll(/^## (.+?)\s*$/gm) ?? [])].map(
		(match) => match[1] ?? "",
	);
}

async function readTemplate(
	deps: VcsDeps,
	cwd: string,
): Promise<string | undefined> {
	if (deps.readFile)
		return deps.readFile(resolve(cwd, ".github/PULL_REQUEST_TEMPLATE.md"));
	const file = Bun.file(resolve(cwd, ".github/PULL_REQUEST_TEMPLATE.md"));
	return (await file.exists()) ? file.text() : undefined;
}

function renderSections(
	sections: Record<string, string> | undefined,
	headings: string[],
): string {
	if (!sections) return "";
	return headings
		.filter((heading) => sections[heading] !== undefined)
		.map((heading) => `## ${heading}\n\n${sections[heading]}`)
		.join("\n\n");
}

export const vcsStatusParameters = type({
	"bookmark?": type("string").describe("Bookmark at the tip of the stack"),
	"workspace?": type("string").describe("Workspace name to inspect"),
});

export const vcsCommitParameters = type({
	bookmark: type("string").describe(
		"Bookmark to create or update for this commit",
	),
	subject: type("string").describe("Commit subject"),
	"sections?": type("Record<string, string>").describe(
		"First-commit text keyed by the pull request template headings",
	),
	"text?": type("string").describe("Optional commit description"),
	"workspace?": type("string").describe("Workspace in which to commit"),
});

export const vcsWorkspaceParameters = type({
	action: type("'add' | 'forget' | 'update-stale'").describe(
		"Workspace operation",
	),
	name: type("string").describe("Workspace name or path"),
	"base?": type("string").describe("Revision to use as the new workspace base"),
});

export const vcsSyncParameters = type({
	"bookmark?": type("string").describe(
		"Bookmark to sync; omit to resolve the current tip",
	),
});

export function createVcsTools(deps: VcsDeps): AgentTool[] {
	const status: AgentTool<typeof vcsStatusParameters> = {
		name: "vcs_status",
		label: "Inspect local VCS stack",
		approval: "read",
		description:
			"Show the commits, bookmarks, subjects, conflicts, and empty commits in your stack.",
		parameters: vcsStatusParameters,
		execute: async (_toolCallId, params) => {
			validateBookmark(params.bookmark, "vcs_status");
			const cwd = await resolveWorkspace(deps, params.workspace, "vcs_status");
			const tip = await resolveTip(deps, params.bookmark, "vcs_status", cwd);
			const result = await runJj(
				deps,
				"vcs_status",
				argsForLog(ownCommits(tip)),
				cwd,
			);
			const rows = parseRows(result.stdout, "vcs_status");
			if (rows.length === 0)
				return {
					content: [{ type: "text", text: "No own commits." }],
					useless: true,
				};
			return {
				content: [{ type: "text", text: rows.map(renderRow).join("\n") }],
			};
		},
	};

	const commit: AgentTool<typeof vcsCommitParameters> = {
		name: "vcs_commit",
		label: "Commit local VCS changes",
		approval: "write",
		description:
			"Describe the current working-copy commit, set its bookmark, then start the next commit on that bookmark. The first commit must include every pull request template heading.",
		parameters: vcsCommitParameters,
		execute: async (_toolCallId, params) => {
			validateBookmark(params.bookmark, "vcs_commit");
			const cwd = await resolveWorkspace(deps, params.workspace, "vcs_commit");
			const current = await runJj(
				deps,
				"vcs_commit",
				argsForLog(ownCommits("@")),
				cwd,
			);
			const ownRows = parseRows(current.stdout, "vcs_commit");
			const firstCommit = !ownRows.some((row) =>
				row.localBookmarks.includes(params.bookmark),
			);
			const sections = params.sections;
			let description: string;
			if (firstCommit) {
				if (params.text !== undefined)
					throw new Error(
						"vcs_commit: first commit requires sections; text cannot replace sections",
					);
				if (sections === undefined)
					throw new Error(
						"vcs_commit: first commit requires template sections",
					);
				description = await composeSections(deps, cwd, sections, "vcs_commit");
			} else {
				validateSections(false, sections, []);
				description = params.text ?? renderSections(sections, []);
			}
			await runJj(
				deps,
				"vcs_commit",
				[...JJ_FLAGS, "describe", "-m", params.subject, "-m", description],
				cwd,
			);
			await runJj(
				deps,
				"vcs_commit",
				[...JJ_FLAGS, "bookmark", "set", params.bookmark, "-r", "@"],
				cwd,
			);
			await runJj(
				deps,
				"vcs_commit",
				[...JJ_FLAGS, "new", params.bookmark],
				cwd,
			);
			return {
				content: [
					{
						type: "text",
						text: `Committed ${params.bookmark}: ${params.subject}`,
					},
				],
			};
		},
	};

	const workspace: AgentTool<typeof vcsWorkspaceParameters> = {
		name: "vcs_workspace",
		label: "Manage a subagent workspace",
		approval: "write",
		description: "Add, forget, or update one subagent working copy.",
		parameters: vcsWorkspaceParameters,
		execute: async (_toolCallId, params) => {
			let args: string[];
			let commandCwd = deps.cwd;
			if (params.action === "add") {
				args = [
					...JJ_FLAGS,
					"workspace",
					"add",
					resolve(deps.cwd, params.name),
					...(params.base ? ["-r", params.base] : []),
				];
			} else {
				const selected = await findWorkspace(
					deps,
					params.name,
					"vcs_workspace",
				);
				if (!selected)
					throw new Error("vcs_workspace: workspace name is required");
				commandCwd = resolve(selected.root);
				args =
					params.action === "forget"
						? [...JJ_FLAGS, "workspace", "forget", selected.name]
						: [...JJ_FLAGS, "workspace", "update-stale"];
			}
			const result = await runJj(deps, "vcs_workspace", args, commandCwd);
			return {
				content: [
					{
						type: "text",
						text:
							result.stdout.trim() ||
							`Workspace ${params.action} complete: ${params.name}`,
					},
				],
			};
		},
	};

	const sync: AgentTool<typeof vcsSyncParameters> = {
		name: "vcs_sync",
		label: "Sync local VCS stack",
		approval: "write",
		description:
			"Fetch origin, then rebase your own commits onto the stack base. Reports conflicts without pushing.",
		parameters: vcsSyncParameters,
		execute: async (toolCallId, params) => {
			const synced = await syncOwn(
				deps,
				toolCallId,
				"vcs_sync",
				params.bookmark,
			);
			return {
				content: [
					{
						type: "text",
						text:
							synced.rows.length === 0
								? `No own commits for ${synced.tip}.`
								: `Synced ${synced.tip} onto ${synced.base}.`,
					},
				],
			};
		},
	};

	return [status, commit, workspace, sync];
}

export interface Synced {
	tip: string;
	// The rebase destination: `trunk()` or a foreign `<name>@origin`.
	base: string;
	// The own commits after the rebase, newest first.
	rows: JjRow[];
}

export interface Remote {
	repo: string;
	url: string;
}

/** Reads `owner/name` and the URL of the origin GitHub remote. */
export async function originRemote(
	deps: VcsDeps,
	toolName: string,
): Promise<Remote> {
	// git config gives the URL as configured; jj prints it after insteadOf rewrites.
	const listed = await deps.git.run(
		["config", "--get", "remote.origin.url"],
		deps.cwd,
	);
	const url = listed.code === 0 ? listed.stdout.trim() : "";
	const match = url.match(
		/^(?:https:\/\/github\.com\/|ssh:\/\/git@github\.com\/|git@github\.com:)([^/\s]+)\/([^/\s]+?)(?:\.git)?\/?$/,
	);
	if (!match)
		throw new Error(
			`${toolName}: origin must be a GitHub remote; found ${url || "none"}`,
		);
	return { repo: `${match[1]}/${match[2]}`, url };
}

export async function stackStatus(
	deps: VcsDeps,
	toolCallId: string,
	repo: string,
	heads: string[],
	toolName: string,
): Promise<StackEntryResult[]> {
	const result = await deps.forge.call(
		create(ForgeCallRequestSchema, {
			callId: toolCallId,
			call: {
				case: "stackStatus",
				value: create(StackStatusRequestSchema, { repo, headRefs: heads }),
			},
		}),
	);
	if (result.result.case !== "stack")
		throw forgeFailure(result, toolName, "stack");
	return result.result.value.entries;
}

/** The untracked `origin` bookmarks on the tip's ancestry above trunk. */
export async function untrackedBelow(
	deps: VcsDeps,
	tip: string,
	toolName: string,
): Promise<string[]> {
	const listed = await runJj(deps, toolName, [
		...JJ_FLAGS,
		"bookmark",
		"list",
		"--remote",
		"exact:origin",
		"-r",
		`::${quoteRevset(tip)} ~ ::trunk()`,
		"-T",
		'if(remote && !tracked, name ++ "\\n")',
	]);
	return listed.stdout.split("\n").filter(Boolean);
}

// A Server push leaves the caller's own remote bookmark untracked, which would read as a foreign base.
async function trackAuthored(
	deps: VcsDeps,
	toolCallId: string,
	tip: string,
	toolName: string,
): Promise<void> {
	const untracked = await untrackedBelow(deps, tip, toolName);
	if (untracked.length === 0) return;
	const { repo } = await originRemote(deps, toolName);
	const entries = await stackStatus(
		deps,
		toolCallId,
		repo,
		untracked,
		toolName,
	);
	for (const entry of entries) {
		if (!entry.authored || !entry.pullRequest) continue;
		await runJj(deps, toolName, [
			...JJ_FLAGS,
			"bookmark",
			"track",
			`exact:${entry.headRef}`,
			"--remote",
			"exact:origin",
		]);
	}
}

/** Fetches, tracks own remote bookmarks, then rebases the own commits onto trunk or their foreign base (D4). */
export async function syncOwn(
	deps: VcsDeps,
	toolCallId: string,
	toolName: string,
	bookmark: string | undefined,
): Promise<Synced> {
	validateBookmark(bookmark, toolName);
	await runJj(deps, toolName, [
		...JJ_FLAGS,
		"git",
		"fetch",
		"--remote",
		"origin",
	]);
	const tip = await resolveTip(deps, bookmark, toolName);
	await trackAuthored(deps, toolCallId, tip, toolName);
	const own = ownCommits(tip);
	const rowsResult = await runJj(deps, toolName, argsForLog(own));
	const rows = parseRows(rowsResult.stdout, toolName);
	if (rows.length === 0) return { tip, base: "trunk()", rows };
	const ownCommitIds = new Set(rows.flatMap((row) => row.commit_id ?? []));
	const root = rows.find((row) =>
		row.parents.some((parent) => !ownCommitIds.has(parent.commit_id)),
	);
	if (!root) throw new Error(`${toolName}: could not find the own-commit root`);
	const foreign = root.parents
		.filter((parent) => !parent.trunk)
		.flatMap((parent) => parent.remoteBookmarks)
		.find((name) => name.endsWith("@origin"));
	const base = foreign ?? "trunk()";
	const result = await deps.jj.run(
		[
			...JJ_FLAGS,
			"rebase",
			"-s",
			`roots(${own})`,
			"-o",
			base,
			"--skip-emptied",
		],
		deps.cwd,
	);
	failIfJjFailed(toolName, result);
	const rebased = parseRows(
		(await runJj(deps, toolName, argsForLog(own))).stdout,
		toolName,
	);
	const conflicts = rebased
		.filter((row) => row.conflict)
		.map((row) => row.commit_id ?? row.change_id ?? "unknown commit");
	if (conflicts.length > 0)
		throw new Error(`${toolName}: rebase conflicts: ${conflicts.join(", ")}`);
	return { tip, base, rows: rebased };
}
function renderRow(row: JjRow): string {
	const markers = [row.conflict ? "conflict" : "", row.empty ? "empty" : ""]
		.filter(Boolean)
		.join(", ");
	return [
		row.change_id ?? "unknown-change",
		row.commit_id ?? "unknown-commit",
		[...row.localBookmarks, ...row.remoteBookmarks].join(",") || "-",
		(row.description ?? "").split("\n", 1)[0] || "(no subject)",
		markers || "-",
	].join(" ");
}

function isString(value: string | undefined): value is string {
	return value !== undefined;
}
