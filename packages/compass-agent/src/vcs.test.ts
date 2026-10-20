import { describe, expect, test } from "bun:test";
import type { AgentTool, AgentToolResult } from "@oh-my-pi/pi-agent-core";
import {
	create,
	type ForgeCallRequest,
	type ForgeCallResult,
	ForgeCallResultSchema,
	PullRequestSchema,
	StackEntryResultSchema,
	StackResultSchema,
} from "./compassv1";
import { ForgeBroker } from "./forge";
import {
	createVcsTools,
	type JjResult,
	type JjRunner,
	ownCommits,
} from "./vcs";

class FakeJj implements JjRunner {
	readonly calls: { args: readonly string[]; cwd: string }[] = [];
	readonly results: JjResult[];

	constructor(...results: JjResult[]) {
		this.results = results;
	}

	async run(args: readonly string[], cwd: string): Promise<JjResult> {
		this.calls.push({ args, cwd });
		return this.results.shift() ?? { stdout: "", stderr: "", code: 0 };
	}
}

// A stale caller checkout: listing fails unless it skips the working copy.
class StaleJj extends FakeJj {
	override async run(args: readonly string[], cwd: string): Promise<JjResult> {
		this.calls.push({ args, cwd });
		if (!args.includes("list")) return ok();
		return args.includes("--ignore-working-copy")
			? ok(workspaceList)
			: failed("Error: The working copy is stale", 1);
	}
}

const forge = new ForgeBroker({
	async forge() {
		throw new Error("unexpected forge call");
	},
});
const unusedGit = {
	async run(): Promise<never> {
		throw new Error("unexpected git call");
	},
};
const flags = ["--no-pager", "--color=never"] as const;
const cwd = "/repo";
const workspaceRoot = `${cwd}/workspaces/agent`;
const workspaceTemplate = 'self.name() ++ "\\t" ++ self.root() ++ "\\n"';
const logTemplate =
	'json(self) ++ "\\t" ++ self.local_bookmarks().map(|bookmark| bookmark.name()).join(",") ++ "\\t" ++ self.remote_bookmarks().map(|bookmark| bookmark.name() ++ "@" ++ bookmark.remote()).join(",") ++ "\\t" ++ json(self.contained_in("trunk()")) ++ "\\t" ++ json(self.conflict()) ++ "\\t" ++ json(self.empty()) ++ "\\t" ++ json(self.current_working_copy()) ++ "\\t" ++ json(self.mine()) ++ "\\t" ++ self.parents().map(|parent| parent.commit_id() ++ ":" ++ json(parent.contained_in("trunk()")) ++ ":" ++ parent.remote_bookmarks().map(|bookmark| bookmark.name() ++ "@" ++ bookmark.remote()).join(",")).join(";") ++ "\\n"';
const template = `<!-- Write 40 to 300 whitespace-delimited words in the authored sections. -->
## Summary
## Verification
## Risks
## Compatibility
## Documentation
`;

const workspaceList = `default\t${cwd}\nagent\t${workspaceRoot}\n`;
const ok = (stdout = ""): JjResult => ({ stdout, stderr: "", code: 0 });
const failed = (stderr: string, code: number): JjResult => ({
	stdout: "",
	stderr,
	code,
});

function tool(tools: AgentTool[], name: string): AgentTool {
	const found = tools.find((candidate) => candidate.name === name);
	if (!found) throw new Error(`no such tool: ${name}`);
	return found;
}

const exec = (
	candidate: AgentTool,
	params: Record<string, unknown>,
): Promise<AgentToolResult> =>
	candidate.execute.call(candidate, "call-1", params);

function row({
	commitId = "own-1",
	localBookmarks = [],
	remoteBookmarks = [],
	trunk = false,
	conflict = false,
	empty = false,
	workingCopy = true,
	parents = ["base-1"],
	parentDetails = parents.map((parent) => ({
		commitId: parent,
		trunk: true,
		remoteBookmarks: [] as string[],
	})),
	description = "feat: add feature",
}: {
	commitId?: string;
	localBookmarks?: string[];
	remoteBookmarks?: string[];
	trunk?: boolean;
	conflict?: boolean;
	empty?: boolean;
	workingCopy?: boolean;
	parents?: string[];
	parentDetails?: {
		commitId: string;
		trunk: boolean;
		remoteBookmarks: string[];
	}[];
	description?: string;
} = {}): string {
	return `${JSON.stringify({ commit_id: commitId, parents, change_id: `change-${commitId}`, description })}\t${localBookmarks.join(",")}\t${remoteBookmarks.join(",")}\t${trunk}\t${conflict}\t${empty}\t${workingCopy}\ttrue\t${parentDetails.map((parent) => `${parent.commitId}:${parent.trunk}:${parent.remoteBookmarks.join(",")}`).join(";")}\n`;
}

const tipRows = (
	current: string[] = ["topic"],
	previous: string[] = [],
): string =>
	row({ commitId: "tip", localBookmarks: current }) +
	row({
		commitId: "base-1",
		parents: [],
		localBookmarks: previous,
		workingCopy: false,
	});
const ownStackRows = (parent: {
	trunk: boolean;
	remoteBookmarks: string[];
}): string =>
	row({
		commitId: "own-2",
		parents: ["own-1"],
		parentDetails: [{ commitId: "own-1", trunk: true, remoteBookmarks: [] }],
	}) +
	row({
		commitId: "own-1",
		parents: ["base-1"],
		parentDetails: [{ commitId: "base-1", ...parent }],
	});
const deps = (
	jj: FakeJj,
	options: {
		root?: string;
		templateText?: string | undefined;
		workspaceRows?: string;
		readFile?: (path: string) => Promise<string | undefined>;
	} = {},
) => ({
	jj,
	git: unusedGit,
	cwd: options.root ?? cwd,
	forge,
	readFile:
		options.readFile ??
		(async (_path: string) => options.templateText ?? template),
});
const logArgs = (revset: string): string[] => [
	...flags,
	"log",
	"--no-graph",
	"-r",
	revset,
	"-T",
	logTemplate,
];
const workspaceListArgs = (): string[] => [
	...flags,
	"--ignore-working-copy",
	"workspace",
	"list",
	"-T",
	workspaceTemplate,
];
const call = (args: string[], at = cwd) => ({ args, cwd: at });

const listWorkspaces = () => call(workspaceListArgs());
const ownRevset = '::"topic" ~ ::(trunk() | untracked_remote_bookmarks())';
const currentOwnRevset = "::@ ~ ::(trunk() | untracked_remote_bookmarks())";
const words = (count: number): string =>
	Array.from({ length: count }, (_, index) => `word${index + 1}`).join(" ");
const sectionsFor = (count: number): Record<string, string> => ({
	Summary: words(count - 4),
	Verification: `word${count - 3}`,
	Risks: `word${count - 2}`,
	Compatibility: `word${count - 1}`,
	Documentation: `word${count}`,
});
const renderedSections = (sections: Record<string, string>): string =>
	Object.entries(sections)
		.map(([heading, body]) => `## ${heading}\n\n${body}`)
		.join("\n\n");
const rebaseArgs = (base: string): string[] => [
	...flags,
	"rebase",
	"-s",
	`roots(${ownRevset})`,
	"-o",
	base,
	"--skip-emptied",
];
const untrackedArgs = (tip: string): string[] => [
	...flags,
	"bookmark",
	"list",
	"--remote",
	"exact:origin",
	"-r",
	`::${tip} ~ ::trunk()`,
	"-T",
	'if(remote && !tracked, name ++ "\\n")',
];

const conflictRows =
	row({ commitId: "own-2", conflict: true }) +
	row({
		commitId: "own-1",
		conflict: true,
		description: "fix: conflicting base",
	});

describe("local VCS tools", () => {
	test("quotes plain bookmark names in the D4 own-commit revset", () => {
		expect(ownCommits("topic")).toBe(ownRevset);
	});

	test("status resolves the tip from current and parent commits with exact calls", async () => {
		const jj = new FakeJj(ok(tipRows([], ["topic"])), ok(row()));
		const status = tool(createVcsTools(deps(jj)), "vcs_status");
		await exec(status, {});
		expect(jj.calls).toEqual([
			call(logArgs("@ | @-")),
			call(logArgs(ownRevset)),
		]);
	});

	test("status uses a listed workspace root and exact calls", async () => {
		const jj = new FakeJj(ok(workspaceList), ok(tipRows()), ok(row()));
		const status = tool(createVcsTools(deps(jj)), "vcs_status");
		await exec(status, { workspace: workspaceRoot });
		expect(jj.calls).toEqual([
			listWorkspaces(),
			call(logArgs("@ | @-"), workspaceRoot),
			call(logArgs(ownRevset), workspaceRoot),
		]);
	});

	test("rejects unlisted relative, parent, and absolute workspaces", async () => {
		for (const workspace of ["missing", "../outside", "/tmp/not-listed"]) {
			const jj = new FakeJj(ok(workspaceList));
			const status = tool(createVcsTools(deps(jj)), "vcs_status");
			await expect(exec(status, { workspace })).rejects.toThrow(
				/workspace.*not listed/i,
			);
			expect(jj.calls).toEqual([listWorkspaces()]);
		}
	});

	test("refuses bookmark expressions before invoking jj", async () => {
		const jj = new FakeJj();
		const status = tool(createVcsTools(deps(jj)), "vcs_status");
		await expect(exec(status, { bookmark: "topic | main" })).rejects.toThrow(
			/plain bookmark/i,
		);
		expect(jj.calls).toEqual([]);
	});

	test("commit fails when a first commit omits template headings", async () => {
		const jj = new FakeJj(ok(row()));
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await expect(
			exec(commit, {
				bookmark: "topic",
				subject: "feat: add feature",
				sections: {},
			}),
		).rejects.toThrow(/heading/i);
		expect(jj.calls).toEqual([call(logArgs(currentOwnRevset))]);
	});

	test("commit uses exact args and cwd for all first-commit commands", async () => {
		const sections = sectionsFor(40);
		const jj = new FakeJj(ok(row()), ok(), ok(), ok());
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await exec(commit, {
			bookmark: "topic",
			subject: "feat: add feature",
			sections,
		});
		expect(jj.calls).toEqual([
			call(logArgs(currentOwnRevset)),
			call([
				...flags,
				"describe",
				"-m",
				"feat: add feature",
				"-m",
				renderedSections(sections),
			]),
			call([...flags, "bookmark", "set", "topic", "-r", "@"]),
			call([...flags, "new", "topic"]),
		]);
	});

	test("commit resolves a selected workspace before reading its template and writing", async () => {
		const sections = sectionsFor(40);
		const jj = new FakeJj(ok(workspaceList), ok(row()), ok(), ok(), ok());
		const templatePaths: string[] = [];
		const commit = tool(
			createVcsTools(
				deps(jj, {
					readFile: async (path) => {
						templatePaths.push(path);
						return template;
					},
				}),
			),
			"vcs_commit",
		);
		await exec(commit, {
			bookmark: "topic",
			subject: "feat: add feature",
			sections,
			workspace: "agent",
		});
		expect(jj.calls).toEqual([
			listWorkspaces(),
			call(logArgs(currentOwnRevset), workspaceRoot),
			call(
				[
					...flags,
					"describe",
					"-m",
					"feat: add feature",
					"-m",
					renderedSections(sections),
				],
				workspaceRoot,
			),
			call([...flags, "bookmark", "set", "topic", "-r", "@"], workspaceRoot),
			call([...flags, "new", "topic"], workspaceRoot),
		]);
		expect(templatePaths).toEqual([
			`${workspaceRoot}/.github/PULL_REQUEST_TEMPLATE.md`,
		]);
	});

	test("rejects 39 and 301 first-commit words and accepts 40", async () => {
		for (const count of [39, 301]) {
			const jj = new FakeJj(ok(row()));
			const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
			await expect(
				exec(commit, {
					bookmark: "topic",
					subject: "feat: add feature",
					sections: sectionsFor(count),
				}),
			).rejects.toThrow(/word/i);
			expect(jj.calls).toEqual([call(logArgs(currentOwnRevset))]);
		}
		const sections = sectionsFor(40);
		const jj = new FakeJj(ok(row()), ok(), ok(), ok());
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await exec(commit, {
			bookmark: "topic",
			subject: "feat: add feature",
			sections,
		});
		expect(jj.calls).toEqual([
			call(logArgs(currentOwnRevset)),
			call([
				...flags,
				"describe",
				"-m",
				"feat: add feature",
				"-m",
				renderedSections(sections),
			]),
			call([...flags, "bookmark", "set", "topic", "-r", "@"]),
			call([...flags, "new", "topic"]),
		]);
	});

	test("first commit text cannot replace its sections", async () => {
		const jj = new FakeJj(ok(row()));
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await expect(
			exec(commit, {
				bookmark: "topic",
				subject: "feat: add feature",
				text: words(40),
			}),
		).rejects.toThrow(/sections/i);
		expect(jj.calls).toEqual([call(logArgs(currentOwnRevset))]);
	});

	test("later commit text is accepted and sections are refused", async () => {
		const jj = new FakeJj(
			ok(row({ localBookmarks: ["topic"] })),
			ok(),
			ok(),
			ok(),
		);
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await exec(commit, {
			bookmark: "topic",
			subject: "fix: follow-up",
			text: "A later commit description.",
		});
		expect(jj.calls).toEqual([
			call(logArgs(currentOwnRevset)),
			call([
				...flags,
				"describe",
				"-m",
				"fix: follow-up",
				"-m",
				"A later commit description.",
			]),
			call([...flags, "bookmark", "set", "topic", "-r", "@"]),
			call([...flags, "new", "topic"]),
		]);

		const rejected = new FakeJj(ok(row({ localBookmarks: ["topic"] })));
		const rejectedCommit = tool(createVcsTools(deps(rejected)), "vcs_commit");
		await expect(
			exec(rejectedCommit, {
				bookmark: "topic",
				subject: "fix: follow-up",
				sections: { Summary: "Not allowed." },
			}),
		).rejects.toThrow(/only.*first commit/i);
		expect(rejected.calls).toEqual([call(logArgs(currentOwnRevset))]);
	});

	test("workspace add, forget, and update-stale use exact args and cwd", async () => {
		const addRunner = new FakeJj(ok());
		const add = tool(createVcsTools(deps(addRunner)), "vcs_workspace");
		await exec(add, {
			action: "add",
			name: "workspaces/new",
			base: "main@origin",
		});
		expect(addRunner.calls).toEqual([
			call([
				...flags,
				"workspace",
				"add",
				`${cwd}/workspaces/new`,
				"-r",
				"main@origin",
			]),
		]);

		const forgetRunner = new FakeJj(ok(workspaceList), ok());
		const forget = tool(createVcsTools(deps(forgetRunner)), "vcs_workspace");
		await exec(forget, { action: "forget", name: "agent" });
		expect(forgetRunner.calls).toEqual([
			listWorkspaces(),
			call([...flags, "workspace", "forget", "agent"], workspaceRoot),
		]);

		const staleRunner = new FakeJj(ok(workspaceList), ok());
		const stale = tool(createVcsTools(deps(staleRunner)), "vcs_workspace");
		await exec(stale, { action: "update-stale", name: workspaceRoot });
		expect(staleRunner.calls).toEqual([
			listWorkspaces(),
			call([...flags, "workspace", "update-stale"], workspaceRoot),
		]);
	});

	test("sync with an explicit bookmark fetches before rebase and reads conflicts afterward", async () => {
		const stack = ownStackRows({
			trunk: false,
			remoteBookmarks: ["other@origin"],
		});
		const jj = new FakeJj(ok(), ok(), ok(stack), ok(), ok(stack));
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await exec(sync, { bookmark: "topic" });
		expect(jj.calls).toEqual([
			call([...flags, "git", "fetch", "--remote", "origin"]),
			call(untrackedArgs('"topic"')),
			call(logArgs(ownRevset)),
			call(rebaseArgs("other@origin")),
			call(logArgs(ownRevset)),
		]);
	});

	test("sync rejects a revset bookmark and uses the default tip when omitted", async () => {
		const rejected = new FakeJj();
		const rejectedSync = tool(createVcsTools(deps(rejected)), "vcs_sync");
		await expect(
			exec(rejectedSync, { bookmark: "topic | main" }),
		).rejects.toThrow(/plain bookmark/i);
		expect(rejected.calls).toEqual([]);

		const stack = ownStackRows({
			trunk: true,
			remoteBookmarks: ["main@origin"],
		});
		const jj = new FakeJj(
			ok(),
			ok(tipRows()),
			ok(),
			ok(stack),
			ok(),
			ok(stack),
		);
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await exec(sync, {});
		expect(jj.calls).toEqual([
			call([...flags, "git", "fetch", "--remote", "origin"]),
			call(logArgs("@ | @-")),
			call(untrackedArgs('"topic"')),
			call(logArgs(ownRevset)),
			call(rebaseArgs("trunk()")),
			call(logArgs(ownRevset)),
		]);
	});

	test("sync reports each conflicted commit from post-rebase structured rows", async () => {
		const stack = ownStackRows({
			trunk: true,
			remoteBookmarks: ["main@origin"],
		});
		const jj = new FakeJj(ok(), ok(), ok(stack), ok(), ok(conflictRows));
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await expect(exec(sync, { bookmark: "topic" })).rejects.toThrow(
			/vcs_sync: rebase conflicts: own-2, own-1/i,
		);
		expect(jj.calls).toEqual([
			call([...flags, "git", "fetch", "--remote", "origin"]),
			call(untrackedArgs('"topic"')),
			call(logArgs(ownRevset)),
			call(rebaseArgs("trunk()")),
			call(logArgs(ownRevset)),
		]);
	});

	test("sync keeps reporting nonzero rebase exits", async () => {
		const stack = ownStackRows({ trunk: true, remoteBookmarks: [] });
		const jj = new FakeJj(ok(), ok(), ok(stack), failed("rebase failed", 1));
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await expect(exec(sync, { bookmark: "topic" })).rejects.toThrow(
			"vcs_sync: rebase failed",
		);
		expect(jj.calls).toEqual([
			call([...flags, "git", "fetch", "--remote", "origin"]),
			call(untrackedArgs('"topic"')),
			call(logArgs(ownRevset)),
			call(rebaseArgs("trunk()")),
		]);
	});

	test("sync fails a nonzero rebase whose output mentions a conflict", async () => {
		const stack = ownStackRows({ trunk: true, remoteBookmarks: [] });
		const jj = new FakeJj(
			ok(),
			ok(),
			ok(stack),
			failed("Error: cannot rebase: conflict marker limit exceeded", 1),
			ok(stack),
		);
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await expect(exec(sync, { bookmark: "topic" })).rejects.toThrow(
			"vcs_sync: Error: cannot rebase",
		);
	});

	test("sync tracks an untracked remote bookmark only when the caller authored its PR", async () => {
		const stack = ownStackRows({ trunk: true, remoteBookmarks: [] });
		// fetch, untracked list, divergence check (empty), track, rows, rebase, rows.
		const jj = new FakeJj(
			ok(),
			ok("topic\nother\n"),
			ok(),
			ok(),
			ok(stack),
			ok(),
			ok(stack),
		);
		const transport = {
			requests: [] as ForgeCallRequest[],
			async forge(req: ForgeCallRequest): Promise<ForgeCallResult> {
				this.requests.push(req);
				return create(ForgeCallResultSchema, {
					result: {
						case: "stack",
						value: create(StackResultSchema, {
							entries: [
								create(StackEntryResultSchema, {
									headRef: "topic",
									authored: true,
									pullRequest: create(PullRequestSchema, { number: 7 }),
								}),
								create(StackEntryResultSchema, {
									headRef: "other",
									authored: false,
									pullRequest: create(PullRequestSchema, { number: 8 }),
								}),
							],
						}),
					},
				});
			},
		};
		const git = {
			async run() {
				return {
					stdout: "git@github.com:octo/repo.git\n",
					stderr: "",
					code: 0,
				};
			},
		};
		const sync = tool(
			createVcsTools({ ...deps(jj), git, forge: new ForgeBroker(transport) }),
			"vcs_sync",
		);
		await exec(sync, { bookmark: "topic" });
		const req = transport.requests[0];
		if (req?.call.case !== "stackStatus") throw new Error("arm");
		expect(req.call.value.repo).toBe("octo/repo");
		expect(req.call.value.headRefs).toEqual(["topic", "other"]);
		const remote = 'remote_bookmarks(exact:"topic", remote=exact:"origin")';
		expect(jj.calls.slice(0, 4)).toEqual([
			call([...flags, "git", "fetch", "--remote", "origin"]),
			call(untrackedArgs('"topic"')),
			call(logArgs(`bookmarks(exact:"topic") ~ ::${remote} ~ ${remote}::`)),
			call([
				...flags,
				"bookmark",
				"track",
				"exact:topic",
				"--remote",
				"exact:origin",
			]),
		]);
		expect(jj.calls[4]).toEqual(call(logArgs(ownRevset)));
	});

	test("update-stale lists workspaces without snapshotting a stale caller", async () => {
		const jj = new StaleJj();
		await exec(tool(createVcsTools(deps(jj)), "vcs_workspace"), {
			action: "update-stale",
			name: "agent",
		});
		expect(jj.calls.at(-1)).toEqual({
			args: [...flags, "workspace", "update-stale"],
			cwd: workspaceRoot,
		});
	});
});
