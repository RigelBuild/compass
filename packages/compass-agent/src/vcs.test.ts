import { describe, expect, test } from "bun:test";
import type { AgentTool, AgentToolResult } from "@oh-my-pi/pi-agent-core";
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

const forge = new ForgeBroker({
	async forge() {
		throw new Error("unexpected forge call");
	},
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

const flags = ["--no-pager", "--color=never"] as const;
const cwd = "/repo";
type RowOptions = {
	commitId?: string;
	localBookmarks?: string[];
	parents?: string[];
	parentDetails?: {
		commitId: string;
		trunk: boolean;
		remoteBookmarks: string[];
	}[];
	workingCopy?: boolean;
	parentTrunk?: boolean;
	parentRemoteBookmarks?: string[];
};
const row = ({
	commitId = "own-1",
	localBookmarks = [],
	workingCopy = true,
	parents = ["base-1"],
	parentTrunk = true,
	parentRemoteBookmarks = [],
	parentDetails = parents.map((commitId) => ({
		commitId,
		trunk: parentTrunk,
		remoteBookmarks: parentRemoteBookmarks,
	})),
}: RowOptions = {}): string =>
	`${JSON.stringify({ commit_id: commitId, parents, change_id: "change-1", description: "feat: add feature" })}\t${localBookmarks.join(",")}\t\tfalse\tfalse\tfalse\t${workingCopy}\ttrue\t${parentDetails.map(({ commitId, trunk, remoteBookmarks }) => `${commitId}:${trunk}:${remoteBookmarks.join(",")}`).join(";")}\n`;
const tipRows = (current: string[] = [], previous: string[] = []): string =>
	row({ localBookmarks: current }) +
	row({
		commitId: "base-1",
		parents: [],
		localBookmarks: previous,
		workingCopy: false,
	});
const template =
	"## Summary\n\n## Verification\n\n## Risks\n\n## Compatibility\n\n## Documentation\n";
const deps = (
	jj: FakeJj,
	templateText: string | undefined = template,
	root = cwd,
	readFile = async (_path: string) => templateText,
) => ({
	jj,
	cwd: root,
	forge,
	readFile,
});
const ok = (stdout = ""): JjResult => ({ stdout, stderr: "", code: 0 });

describe("local VCS tools", () => {
	test("builds the D4 own-commit revset", () => {
		expect(ownCommits("topic")).toBe(
			"::topic ~ ::(trunk() | untracked_remote_bookmarks())",
		);
	});

	test("status resolves one local bookmark from current and parent commits", async () => {
		const jj = new FakeJj(ok(tipRows([], ["topic"])), ok(row()));
		const status = tool(createVcsTools(deps(jj)), "vcs_status");
		await exec(status, {});
		expect(jj.calls[0]?.args[5]).toBe("@ | @-");
		expect(jj.calls[1]?.args[5]).toBe(ownCommits("topic"));
	});

	test("refuses multiple local bookmarks from current and parent commits", async () => {
		const jj = new FakeJj(ok(tipRows(["current"], ["parent"])));
		const status = tool(createVcsTools(deps(jj)), "vcs_status");
		await expect(exec(status, {})).rejects.toThrow(/exactly one bookmark/i);
	});

	test("detects first commit by missing bookmark and requires template headings", async () => {
		const jj = new FakeJj(ok(row()));
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await expect(
			exec(commit, { bookmark: "topic", subject: "feat: add feature" }),
		).rejects.toThrow(/heading/i);
		expect(jj.calls).toHaveLength(1);
	});

	test("reads headings from repository template and describes section text", async () => {
		const jj = new FakeJj(ok(row()), ok(), ok(), ok());
		const commit = tool(
			createVcsTools(deps(jj, "## Overview\n## Test Plan\n")),
			"vcs_commit",
		);
		await exec(commit, {
			bookmark: "topic",
			subject: "feat: add feature",
			sections: { Overview: "Adds it.", "Test Plan": "Tests pass." },
		});
		expect(jj.calls[1]?.args[6]).toContain("## Overview");
		expect(jj.calls[1]?.args[6]).toContain("## Test Plan");
	});
	test("reads template headings from the selected workspace root", async () => {
		const jj = new FakeJj(ok(row()), ok(), ok(), ok());
		const templatePaths: string[] = [];
		const workspace = "workspaces/agent";
		const workspaceRoot = `${cwd}/${workspace}`;
		const commit = tool(
			createVcsTools(
				deps(jj, undefined, cwd, async (path) => {
					templatePaths.push(path);
					return "## Overview\n## Test Plan\n";
				}),
			),
			"vcs_commit",
		);
		await exec(commit, {
			bookmark: "topic",
			subject: "feat: add feature",
			sections: { Overview: "Adds it.", "Test Plan": "Tests pass." },
			workspace,
		});
		expect(templatePaths).toEqual([
			`${workspaceRoot}/.github/PULL_REQUEST_TEMPLATE.md`,
		]);
		expect(jj.calls[0]?.cwd).toBe(workspaceRoot);
		expect(jj.calls[1]?.args[6]).toContain("## Overview");
		expect(jj.calls[1]?.args[6]).toContain("## Test Plan");
	});

	test("rejects unknown section headings", async () => {
		const jj = new FakeJj(ok(row()));
		const commit = tool(
			createVcsTools(deps(jj, "## Overview\n")),
			"vcs_commit",
		);
		await expect(
			exec(commit, {
				bookmark: "topic",
				subject: "feat: add feature",
				sections: { Overview: "Adds it.", Unknown: "Reject me." },
			}),
		).rejects.toThrow(/unknown.*heading/i);
		expect(jj.calls).toHaveLength(1);
	});
	test("does not require first-commit sections when bookmark is on an own row", async () => {
		const jj = new FakeJj(
			ok(
				row({
					commitId: "own-2",
					parentDetails: [
						{ commitId: "own-1", trunk: false, remoteBookmarks: [] },
					],
				}) + row({ commitId: "own-1", localBookmarks: ["topic"] }),
			),
			ok(),
			ok(),
		);
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await exec(commit, { bookmark: "topic", subject: "fix: continue feature" });
		expect(jj.calls[1]?.args).toEqual([
			...flags,
			"describe",
			"-m",
			"fix: continue feature",
			"-m",
			"",
		]);
		expect(jj.calls[3]?.args).toEqual([...flags, "new", "topic"]);
	});

	test("rejects sections on later commits for an existing bookmark", async () => {
		const jj = new FakeJj(ok(row({ localBookmarks: ["topic"] })));
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");
		await expect(
			exec(commit, {
				bookmark: "topic",
				subject: "feat: next change",
				sections: { Summary: "Reject me." },
			}),
		).rejects.toThrow(/only.*first commit/i);
		expect(jj.calls).toHaveLength(1);
	});

	test("allows first commit without headings when template is absent", async () => {
		const jj = new FakeJj(ok(row()), ok(), ok(), ok());
		const commit = tool(createVcsTools(deps(jj, "")), "vcs_commit");
		await exec(commit, { bookmark: "topic", subject: "feat: add feature" });
		expect(jj.calls.at(-1)?.args).toEqual([...flags, "new", "topic"]);
	});
	test("describes, bookmarks, then starts the next commit at that bookmark", async () => {
		const jj = new FakeJj(ok(row()), ok(), ok(), ok());
		const commit = tool(createVcsTools(deps(jj)), "vcs_commit");

		await exec(commit, {
			bookmark: "topic",
			subject: "feat: add feature",
			sections: {
				Summary: "Adds the feature.",
				Verification: "bun test passed.",
				Risks: "None: no runtime behavior changed.",
				Compatibility: "Compatible: no API changed.",
				Documentation: "None: no public docs changed.",
			},
		});

		expect(jj.calls.slice(0, 4).map(({ args }) => args[2])).toEqual([
			"log",
			"describe",
			"bookmark",
			"new",
		]);
		expect(jj.calls[0]?.args[5]).toBe(ownCommits("@"));
		expect(jj.calls[0]?.args[6]).toBe("-T");
		expect(jj.calls[0]?.args[7]).toContain("json(self)");
		expect(jj.calls[1]?.args).toEqual([
			...flags,
			"describe",
			"-m",
			"feat: add feature",
			"-m",
			expect.stringContaining("## Summary"),
		]);
	});
	test("uses D4's explicit bookmark as sync tip", async () => {
		const jj = new FakeJj(
			ok(),
			ok(
				row({
					commitId: "own-1",
					parentRemoteBookmarks: ["other@origin"],
					parentTrunk: false,
				}),
			),
			ok(row({ commitId: "own-2", parents: ["own-1"] })),
			ok(),
		);
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await exec(sync, { bookmark: "topic" });
	});

	test("does not select a trunk remote bookmark as the sync base", async () => {
		const jj = new FakeJj(
			ok(),
			ok(
				row({
					commitId: "own-1",
					parentRemoteBookmarks: ["main@origin"],
					parentTrunk: true,
				}),
			),
			ok(row({ commitId: "own-2", parents: ["own-1"] })),
			ok(),
		);
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await exec(sync, { bookmark: "topic" });
		expect(jj.calls[2]?.args[6]).toBe("trunk()");
	});

	test("resolves sync tip from current and parent commits", async () => {
		const jj = new FakeJj(ok(), ok(tipRows(["topic"])), ok(row()), ok());
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await exec(sync, {});
		expect(jj.calls[1]?.args[5]).toBe("@ | @-");
		expect(jj.calls[2]?.args[5]).toBe(ownCommits("topic"));
	});

	test("fetches before rebasing own commits onto a foreign remote base", async () => {
		const jj = new FakeJj(
			ok(),
			ok(
				row({
					commitId: "own-1",
					parentRemoteBookmarks: ["other@origin"],
					parentTrunk: false,
				}),
			),
			ok(row({ commitId: "own-2", parents: ["own-1"] })),
			ok(),
		);
		const sync = tool(createVcsTools(deps(jj)), "vcs_sync");
		await exec(sync, { bookmark: "topic" });
		expect(jj.calls.map(({ args }) => args[2])).toEqual([
			"git",
			"log",
			"rebase",
		]);
		expect(jj.calls[2]?.args[6]).toBe("other@origin");
	});
});
