import { resolve } from "node:path";
import { type } from "@oh-my-pi/omptype/ark";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import type { ForgeBroker } from "./forge";

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
	cwd: string;
	forge: ForgeBroker;
	readFile?: (path: string) => Promise<string | undefined>;
}

const JJ_FLAGS = ["--no-pager", "--color=never"] as const;
const LOG_TEMPLATE =
	'json(self) ++ "\\t" ++ self.local_bookmarks().map(|bookmark| bookmark.name()).join(",") ++ "\\t" ++ self.remote_bookmarks().map(|bookmark| bookmark.name() ++ "@" ++ bookmark.remote()).join(",") ++ "\\t" ++ json(self.contained_in("trunk()")) ++ "\\t" ++ json(self.conflict()) ++ "\\t" ++ json(self.empty()) ++ "\\t" ++ json(self.current_working_copy()) ++ "\\t" ++ json(self.mine()) ++ "\\t" ++ self.parents().map(|parent| parent.commit_id() ++ ":" ++ json(parent.contained_in("trunk()")) ++ ":" ++ parent.remote_bookmarks().map(|bookmark| bookmark.name() ++ "@" ++ bookmark.remote()).join(",")).join(";") ++ "\\n"';

type JjParent = {
	commit_id: string;
	trunk: boolean;
	remoteBookmarks: string[];
};
type JjRow = {
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

function parseRows(stdout: string, toolName: string): JjRow[] {
	return stdout
		.split("\n")
		.filter((line) => line.trim().length > 0)
		.map((line) => parseRow(line, toolName));
}

function validateSections(
	firstCommit: boolean,
	sections: Record<string, string> | undefined,
	headings: string[],
): void {
	if (!firstCommit && sections !== undefined)
		throw new Error(
			"vcs_commit: sections are only allowed on the first commit of a bookmark",
		);
	const unknown = Object.keys(sections ?? {}).filter(
		(heading) => !headings.includes(heading),
	);
	if (unknown.length > 0)
		throw new Error(
			`vcs_commit: unknown template heading(s): ${unknown.join(", ")}`,
		);
	const missing = firstCommit
		? headings.filter((heading) => sections?.[heading] === undefined)
		: [];
	if (missing.length > 0)
		throw new Error(
			`vcs_commit: first commit is missing template heading(s): ${missing.join(", ")}`,
		);
}

function argsForLog(revset: string): string[] {
	return [...JJ_FLAGS, "log", "--no-graph", "-r", revset, "-T", LOG_TEMPLATE];
}

function failIfJjFailed(toolName: string, result: JjResult): void {
	if (result.code !== 0) {
		const details =
			result.stderr.trim() || result.stdout.trim() || `exit ${result.code}`;
		throw new Error(`${toolName}: ${details}`);
	}
}

async function runJj(
	deps: VcsDeps,
	toolName: string,
	args: readonly string[],
	cwd = deps.cwd,
): Promise<JjResult> {
	const result = await deps.jj.run(args, cwd);
	failIfJjFailed(toolName, result);
	return result;
}

export function ownCommits(tip: string): string {
	return `::${tip} ~ ::(trunk() | untracked_remote_bookmarks())`;
}

async function resolveTip(
	deps: VcsDeps,
	bookmark: string | undefined,
	toolName: string,
	cwd = deps.cwd,
): Promise<string> {
	if (bookmark) return bookmark;
	const result = await runJj(deps, toolName, argsForLog("@ | @-"), cwd);
	return resolveTipFromRows(parseRows(result.stdout, toolName), toolName);
}

function resolveTipFromRows(rows: JjRow[], toolName: string): string {
	const localBookmarks = new Set(rows.flatMap((row) => row.localBookmarks));
	if (localBookmarks.size !== 1)
		throw new Error(
			`${toolName}: expected exactly one bookmark across @ and @-`,
		);
	const tip = localBookmarks.values().next().value;
	if (!tip)
		throw new Error(
			`${toolName}: expected exactly one bookmark across @ and @-`,
		);
	return tip;
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
			const cwd = params.workspace
				? resolve(deps.cwd, params.workspace)
				: deps.cwd;
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
			const cwd = params.workspace
				? resolve(deps.cwd, params.workspace)
				: deps.cwd;
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
			const headings =
				firstCommit || sections !== undefined
					? parseHeadings(await readTemplate(deps, cwd))
					: [];
			validateSections(firstCommit, sections, headings);
			const description = params.text ?? renderSections(sections, headings);
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
			switch (params.action) {
				case "add":
					args = [
						...JJ_FLAGS,
						"workspace",
						"add",
						resolve(deps.cwd, params.name),
						...(params.base ? ["-r", params.base] : []),
					];
					break;
				case "forget":
					args = [...JJ_FLAGS, "workspace", "forget", params.name];
					break;
				case "update-stale":
					args = [...JJ_FLAGS, "workspace", "update-stale"];
					break;
			}
			const result = await runJj(deps, "vcs_workspace", args);
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
		execute: async (_toolCallId, params) => {
			await runJj(deps, "vcs_sync", [
				...JJ_FLAGS,
				"git",
				"fetch",
				"--remote",
				"origin",
			]);
			const tip = await resolveTip(deps, params.bookmark, "vcs_sync");
			const own = ownCommits(tip);
			const rowsResult = await runJj(deps, "vcs_sync", argsForLog(own));
			const rows = parseRows(rowsResult.stdout, "vcs_sync");
			if (rows.length === 0)
				return {
					content: [{ type: "text", text: `No own commits for ${tip}.` }],
				};
			const ownCommitIds = new Set(rows.flatMap((row) => row.commit_id ?? []));
			const root = rows.find((row) =>
				row.parents.some((parent) => !ownCommitIds.has(parent.commit_id)),
			);
			if (!root)
				throw new Error("vcs_sync: could not find the own-commit root");
			const foreign = root.parents
				.filter((parent) => !parent.trunk)
				.flatMap((parent) => parent.remoteBookmarks)
				.find((name) => name.endsWith("@origin"));
			const rebaseBase = foreign ?? "trunk()";
			const result = await deps.jj.run(
				[
					...JJ_FLAGS,
					"rebase",
					"-s",
					`roots(${own})`,
					"-o",
					rebaseBase,
					"--skip-emptied",
				],
				deps.cwd,
			);
			const details = `${result.stderr}\n${result.stdout}`;
			if (/conflict/i.test(details))
				throw new Error(`vcs_sync: rebase conflict: ${details.trim()}`);
			failIfJjFailed("vcs_sync", result);
			return {
				content: [{ type: "text", text: `Synced ${tip} onto ${rebaseBase}.` }],
			};
		},
	};

	return [status, commit, workspace, sync];
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
