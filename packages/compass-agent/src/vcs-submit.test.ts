import { describe, expect, test } from "bun:test";
import { readFile, writeFile } from "node:fs/promises";
import type { AgentTool, AgentToolResult } from "@oh-my-pi/pi-agent-core";
import {
	create,
	type ForgeCallRequest,
	type ForgeCallResult,
	ForgeCallResultSchema,
	PullRequestSchema,
	StackEntryResultSchema,
	StackEntryStatus,
	StackResultSchema,
} from "./compassv1";
import { ForgeBroker } from "./forge";
import type { JjResult } from "./vcs";
import type { CommandOptions, CommandResult } from "./vcs-runner";
import {
	createSubmitTools,
	HOOK_TIMEOUT_NOTE,
	MAX_SUBMIT_BYTES,
} from "./vcs-submit";

const cwd = "/repo";
const url = "https://github.com/octo/repo.git";
const ok = (stdout = ""): JjResult => ({ stdout, stderr: "", code: 0 });

type Row = {
	id: string;
	bookmarks?: string[];
	remote?: string[];
	parents?: { id: string; trunk?: boolean; remote?: string[] }[];
	description?: string;
	conflict?: boolean;
	empty?: boolean;
};

function line(r: Row): string {
	const parents = r.parents ?? [{ id: "base", trunk: true }];
	return `${JSON.stringify({ commit_id: r.id, change_id: `ch-${r.id}`, description: r.description ?? `feat: ${r.id}\n\nbody of ${r.id}\n` })}\t${(r.bookmarks ?? []).join(",")}\t${(r.remote ?? []).join(",")}\tfalse\t${r.conflict ?? false}\t${r.empty ?? false}\tfalse\ttrue\t${parents.map((p) => `${p.id}:${p.trunk ?? false}:${(p.remote ?? []).join(",")}`).join(";")}\n`;
}

// Two bookmarks, bottom `a` (two commits) under `b`; rows are newest first, as jj prints them.
const stack: Row[] = [
	{ id: "c3", bookmarks: ["b"], parents: [{ id: "c2" }] },
	{ id: "c2", bookmarks: ["a"], parents: [{ id: "c1" }] },
	{ id: "c1", parents: [{ id: "base", trunk: true }] },
];

/** Routes jj calls by subcommand and revset so each test states only what differs. */
class Jj {
	readonly calls: string[][] = [];
	constructor(
		readonly own: Row[] = stack,
		readonly above: Row[] = [],
		readonly forks: Row[] = [],
		readonly untracked = "",
		readonly diverged: Row[] = [],
		readonly trunkBookmarks: string[] = ["main@origin"],
		readonly remoteTarget?: string,
		readonly localTarget?: string,
	) {}
	async run(args: readonly string[]): Promise<JjResult> {
		this.calls.push([...args]);
		const a = args.slice(2);
		if (a[0] === "bookmark" && a[1] === "list") return ok(this.untracked);
		if (a[0] !== "log") return ok();
		const revset = a[3] ?? "";
		if (revset === "trunk()")
			return ok(line({ id: "base", remote: this.trunkBookmarks, parents: [] }));
		if (revset.startsWith("(descendants"))
			return ok(this.forks.map(line).join(""));
		if (revset.startsWith('"b":: ~ "b"'))
			return ok(this.above.map(line).join(""));
		if (revset === "@ | @-") return ok(line({ id: "c3", bookmarks: ["b"] }));
		if (revset.startsWith("remote_bookmarks(exact:")) {
			const name =
				revset.match(/^remote_bookmarks\(exact:"([^"]+)"/)?.[1] ?? "";
			const local = this.own.find((r) => r.bookmarks?.includes(name));
			return this.remoteTarget !== undefined
				? ok(line({ id: this.remoteTarget }))
				: local
					? ok(line({ id: this.localTarget ?? local.id }))
					: ok();
		}
		if (revset.startsWith("bookmarks(exact:")) {
			const name = revset.match(/^bookmarks\(exact:"([^"]+)"\)/)?.[1] ?? "";
			if (revset.includes(" ~ ::remote_bookmarks("))
				return ok(this.diverged.map(line).join(""));
			const local = this.own.find((r) => r.bookmarks?.includes(name));
			return local ? ok(line({ id: local.id })) : ok();
		}
		return ok(this.own.map(line).join(""));
	}
}

const statusOf = (entry: {
	head: string;
	authored?: boolean;
	number?: number;
	error?: string;
}): ForgeCallResult =>
	create(ForgeCallResultSchema, {
		result: {
			case: "stack",
			value: create(StackResultSchema, {
				entries: [
					create(StackEntryResultSchema, {
						headRef: entry.head,
						authored: entry.authored ?? false,
						error: entry.error
							? { code: "unavailable", message: entry.error }
							: undefined,
						pullRequest: entry.number
							? create(PullRequestSchema, { number: entry.number })
							: undefined,
					}),
				],
			}),
		},
	});

const tracked = (jj: Jj) =>
	jj.calls.filter((args) => args[2] === "bookmark" && args[3] === "track");

class Git {
	readonly calls: {
		args: string[];
		options?: CommandOptions;
		stdin?: string;
	}[] = [];
	constructor(
		readonly hook: CommandResult = { stdout: "", stderr: "", code: 0 },
		readonly bundleBytes = new Uint8Array([1, 2, 3]),
		readonly remoteUrl = url,
		readonly bundleSize?: number,
	) {}
	async run(
		args: readonly string[],
		_cwd: string,
		options?: CommandOptions,
	): Promise<CommandResult> {
		const call: (typeof this.calls)[number] = { args: [...args], options };
		this.calls.push(call);
		if (args[0] === "config")
			return { stdout: `${this.remoteUrl}\n`, stderr: "", code: 0 };
		if (args[0] === "for-each-ref")
			return {
				stdout: `refs/remotes/origin/a ${"f".repeat(40)}\nrefs/remotes/origin/main ${"e".repeat(40)}\n`,
				stderr: "",
				code: 0,
			};
		if (args[0] === "hook") {
			const file = args.find((x) => x.startsWith("--to-stdin="))?.slice(11);
			if (file) call.stdin = await readFile(file, "utf8");
			return this.hook;
		}
		if (args[0] === "bundle") {
			const file = args[2];
			if (file && file !== "-") {
				const content =
					this.bundleSize === undefined
						? this.bundleBytes
						: new Uint8Array(this.bundleSize);
				await writeFile(file, content);
			}
			return { stdout: "", stderr: "", code: 0 };
		}
		return { stdout: "", stderr: "", code: 0 };
	}
}

test("refuses the default branch before stack status, hook, and bundle", async () => {
	const mainRows: Row[] = [
		{ id: "c3", bookmarks: ["main"], parents: [{ id: "c2" }] },
		{ id: "c2", parents: [{ id: "base", trunk: true }] },
	];
	const s = setup({ jj: new Jj(mainRows) });
	await expect(
		exec(s.tool("vcs_submit"), { issues: { main: "#1" } }),
	).rejects.toThrow(
		"vcs_submit: main is the default branch; commit on a bookmark of its own",
	);
	expect(s.transport.requests).toHaveLength(0);
	expect(s.git.calls.map((c) => c.args[0])).toEqual(["config"]);
});

test("vcs_retire refuses fixed default branch names before forge calls", async () => {
	for (const bookmark of ["main", "master", "trunk"]) {
		const s = setup();
		await expect(exec(s.tool("vcs_retire"), { bookmark })).rejects.toThrow(
			`vcs_retire: ${bookmark} is the default branch; commit on a bookmark of its own`,
		);
		expect(s.git.calls).toHaveLength(0);
		expect(s.transport.requests).toHaveLength(0);
	}
});

test("refuses a stack bookmark matching the resolved trunk branch", async () => {
	const own: Row[] = [
		{
			id: "c3",
			bookmarks: ["release"],
			parents: [{ id: "base", trunk: true }],
		},
	];
	const s = setup({ jj: new Jj(own, [], [], "", [], ["release@origin"]) });
	await expect(
		exec(s.tool("vcs_submit"), { issues: { release: "#1" } }),
	).rejects.toThrow(
		"vcs_submit: release is the default branch; commit on a bookmark of its own",
	);
	expect(s.transport.requests).toHaveLength(0);
	expect(s.git.calls.map((c) => c.args[0])).toEqual(["config"]);
});

test("text replacement still requires an issue for its bookmark before any calls", async () => {
	const s = setup();
	await expect(
		exec(s.tool("vcs_submit"), {
			text: { bookmark: "b", subject: "feat: title", sections: {} },
		}),
	).rejects.toThrow(
		"vcs_submit: text replaces b's title and body, so issues must name its issue too",
	);
	expect(s.jj.calls).toHaveLength(0);
	expect(s.git.calls).toHaveLength(0);
	expect(s.transport.requests).toHaveLength(0);
});

test("origin URLs reject query and fragment parts but accept the three GitHub forms", async () => {
	for (const remoteUrl of [
		"https://github.com/o/r?token=x",
		"https://github.com/o/r#x",
	]) {
		const s = setup({ git: new Git(undefined, undefined, remoteUrl) });
		await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow(
			"origin must be a github.com https or ssh remote (owner/name)",
		);
		expect(s.jj.calls).toHaveLength(0);
		expect(s.transport.requests).toHaveLength(0);
	}
	for (const remoteUrl of [
		"https://github.com/o/r.git/",
		"ssh://git@github.com/o/r",
		"git@github.com:o/r.git",
	]) {
		const s = setup({ git: new Git(undefined, undefined, remoteUrl) });
		await exec(s.tool("vcs_submit"), { issues });
		expect(s.transport.requests.at(-1)?.call.case).toBe("submitStack");
	}
});

test("PR-less pushed heads track only when local and remote target commits match", async () => {
	const equal = setup({
		jj: new Jj(stack, [], [], "a\n"),
		results: [statusOf({ head: "a" }), noPrs, submitted],
	});
	await exec(equal.tool("vcs_submit"), { issues });
	expect(tracked(equal.jj)).toHaveLength(1);

	const different = setup({
		jj: new Jj(stack, [], [], "a\n", [], ["main@origin"], "remote-head"),
		results: [statusOf({ head: "a" }), noPrs, submitted],
	});
	await expect(exec(different.tool("vcs_submit"), { issues })).rejects.toThrow(
		"vcs_submit: a@origin has no pull request and differs from your a; rename your bookmark and submit again",
	);
	expect(tracked(different.jj)).toHaveLength(0);

	const absent = setup({
		jj: new Jj(stack, [], [], "ghost\n"),
		results: [statusOf({ head: "ghost" }), noPrs, submitted],
	});
	await exec(absent.tool("vcs_submit"), { issues });
	expect(tracked(absent.jj)).toHaveLength(0);
});

test("base branch selection uses fixed precedence and rejects ambiguous custom names", async () => {
	const ordered = setup({
		jj: new Jj(
			stack,
			[],
			[],
			"",
			[],
			["trunk@origin", "custom@origin", "main@origin", "master@origin"],
		),
	});
	await exec(ordered.tool("vcs_submit"), { issues });
	const request = ordered.transport.requests.at(-1);
	if (request?.call.case !== "submitStack") throw new Error("arm");
	expect(request.call.value.entries[0]?.baseRef).toBe("main");

	const single = setup({
		jj: new Jj(stack, [], [], "", [], ["custom@origin"]),
	});
	await exec(single.tool("vcs_submit"), { issues });
	const singleRequest = single.transport.requests.at(-1);
	if (singleRequest?.call.case !== "submitStack") throw new Error("arm");
	expect(singleRequest.call.value.entries[0]?.baseRef).toBe("custom");

	const ambiguous = setup({
		jj: new Jj(stack, [], [], "", [], ["custom-a@origin", "custom-b@origin"]),
	});
	await expect(exec(ambiguous.tool("vcs_submit"), { issues })).rejects.toThrow(
		"vcs_submit: trunk() carries several origin bookmarks (custom-a, custom-b); cannot tell the default branch",
	);
});

test("bundle larger than the limit is rejected from its file size", async () => {
	const s = setup({
		git: new Git(
			undefined,
			new Uint8Array([1, 2, 3]),
			url,
			MAX_SUBMIT_BYTES + 1,
		),
	});
	await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow(
		`vcs_submit: the submit is ${MAX_SUBMIT_BYTES + 1} bytes, over the ${MAX_SUBMIT_BYTES}-byte limit; split the stack or drop large files`,
	);
	expect(s.transport.requests.map((r) => r.call.case)).toEqual(["stackStatus"]);
});

function stackResult(
	entries: {
		head: string;
		number?: number;
		status?: StackEntryStatus;
		error?: string;
	}[],
): ForgeCallResult {
	return create(ForgeCallResultSchema, {
		result: {
			case: "stack",
			value: create(StackResultSchema, {
				entries: entries.map((e) =>
					create(StackEntryResultSchema, {
						headRef: e.head,
						status: e.status ?? StackEntryStatus.DONE,
						authored: true,
						error: e.error
							? { code: "unavailable", message: e.error }
							: undefined,
						pullRequest: e.number
							? create(PullRequestSchema, {
									number: e.number,
									forgeState: "open",
									draft: true,
									url: `https://github.com/octo/repo/pull/${e.number}`,
								})
							: undefined,
					}),
				),
			}),
		},
	});
}

class Transport {
	readonly requests: ForgeCallRequest[] = [];
	constructor(readonly results: ForgeCallResult[]) {}
	async forge(req: ForgeCallRequest): Promise<ForgeCallResult> {
		this.requests.push(req);
		const next = this.results.shift();
		if (!next) throw new Error("unexpected forge call");
		return next;
	}
}

const noPrs = stackResult([{ head: "a" }, { head: "b" }]);
const submitted = stackResult([
	{ head: "a", number: 11 },
	{ head: "b", number: 12 },
]);
function setup(
	opts: {
		jj?: Jj;
		git?: Git;
		results?: ForgeCallResult[];
		hookTimeoutMs?: number;
	} = {},
) {
	const jj = opts.jj ?? new Jj();
	const git = opts.git ?? new Git();
	const transport = new Transport(opts.results ?? [noPrs, submitted]);
	const forge = new ForgeBroker(transport);
	const tools = createSubmitTools({
		jj,
		git,
		cwd,
		forge,
		hookTimeoutMs: opts.hookTimeoutMs,
		readFile: async () =>
			"## Summary\n## Verification\n## Risks\n## Compatibility\n## Documentation\n",
	});
	const tool = (name: string): AgentTool => {
		const found = tools.find((t) => t.name === name);
		if (!found) throw new Error(name);
		return found;
	};
	return { jj, git, transport, forge, tool };
}
const exec = (t: AgentTool, params: Record<string, unknown>) =>
	t.execute.call(t, "call-1", params) as Promise<AgentToolResult>;
const text = (r: AgentToolResult) =>
	r.content.map((c) => (c.type === "text" ? c.text : "")).join("");
const issues = { a: "#1", b: "RIG-2" };

describe("vcs_submit", () => {
	test("sends one entry per bookmark bottom to top with first-commit text, issue, base, bundle, and key", async () => {
		const s = setup();
		const out = await exec(s.tool("vcs_submit"), { issues, ready: true });
		expect(text(out)).toContain("a: #11");
		const req = s.transport.requests[1];
		if (req?.call.case !== "submitStack") throw new Error("arm");
		expect(req.clientRequestId).toBe(s.forge.idempotencyKey("call-1"));
		const v = req.call.value;
		expect(v.repo).toBe("octo/repo");
		expect(v.readyHeadRef).toBe("b");
		expect(v.bundle).toEqual(new Uint8Array([1, 2, 3]));
		expect(
			v.entries.map((e) => [e.headRef, e.baseRef, e.title, e.body, e.issueRef]),
		).toEqual([
			[
				"a",
				"main",
				"feat: c1 (octo/repo#1)",
				"body of c1\n\nRefs octo/repo#1",
				"octo/repo#1",
			],
			["b", "", "feat: c3 (RIG-2)", "body of c3\n\nRefs RIG-2", "RIG-2"],
		]);
		const bundleCall = s.git.calls.find((c) => c.args[0] === "bundle");
		expect(bundleCall?.args.slice(0, 2)).toEqual(["bundle", "create"]);
		expect(bundleCall?.args[2]).toEndWith("/bundle");
		expect(bundleCall?.args.slice(3)).toEqual([
			"refs/heads/a",
			"refs/heads/b",
			"^base",
		]);
	});

	test("a second submit tracks the caller's own pushed bookmark before it reads the stack", async () => {
		const authoredA = create(ForgeCallResultSchema, {
			result: {
				case: "stack",
				value: create(StackResultSchema, {
					entries: [
						create(StackEntryResultSchema, {
							headRef: "a",
							authored: true,
							pullRequest: create(PullRequestSchema, { number: 11 }),
						}),
					],
				}),
			},
		});
		const s = setup({
			jj: new Jj(stack, [], [], "a\n"),
			results: [authoredA, noPrs, submitted],
		});
		await exec(s.tool("vcs_submit"), { issues });
		const track = s.jj.calls.findIndex(
			(c) => c[2] === "bookmark" && c[3] === "track",
		);
		const firstLog = s.jj.calls.findIndex(
			(c) => c[2] === "log" && c[5]?.startsWith("::"),
		);
		expect(s.jj.calls[track]?.slice(2)).toEqual([
			"bookmark",
			"track",
			"exact:a",
			"--remote",
			"exact:origin",
		]);
		expect(track).toBeLessThan(firstLog);
		const req = s.transport.requests[2];
		if (req?.call.case !== "submitStack") throw new Error("arm");
		expect(req.call.value.entries[0]?.baseRef).toBe("main");
	});

	test("a pushed own head whose PR create failed is tracked; another agent's PR head is not", async () => {
		const own = setup({
			jj: new Jj(stack, [], [], "a\n"),
			results: [statusOf({ head: "a" }), noPrs, submitted],
		});
		await exec(own.tool("vcs_submit"), { issues });
		expect(tracked(own.jj)).toHaveLength(1);

		const foreign = setup({
			jj: new Jj(stack, [], [], "a\n"),
			results: [statusOf({ head: "a", number: 9 }), noPrs, submitted],
		});
		await exec(foreign.tool("vcs_submit"), { issues });
		expect(tracked(foreign.jj)).toHaveLength(0);
	});

	test("a diverged own head refuses before tracking, naming the recovery", async () => {
		const s = setup({
			jj: new Jj(stack, [], [], "a\n", [{ id: "c2", bookmarks: ["a"] }]),
			results: [statusOf({ head: "a", authored: true, number: 11 })],
		});
		await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow(
			/a and a@origin have diverged; run jj bookmark set a -r a@origin/,
		);
		expect(tracked(s.jj)).toHaveLength(0);
		expect(s.transport.requests).toHaveLength(1);
	});

	test("a stack_status entry error stops sync before any rebase", async () => {
		const s = setup({
			jj: new Jj(stack, [], [], "a\n"),
			results: [statusOf({ head: "a", error: "provider down" })],
		});
		await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow(
			"vcs_submit: stack_status for a: provider down",
		);
		expect(s.jj.calls.some((c) => c[2] === "rebase")).toBe(false);
	});

	test("the above-tip check excludes other agents' remote heads", async () => {
		const s = setup();
		await exec(s.tool("vcs_submit"), { issues });
		const above = s.jj.calls.find(
			(c) => c[2] === "log" && c[5]?.startsWith('"b":: ~ "b"'),
		);
		expect(above?.[5]).toBe('"b":: ~ "b" ~ ::untracked_remote_bookmarks()');
	});

	test("the hook gets origin, the URL, and one stdin line per own bookmark", async () => {
		const s = setup();
		await exec(s.tool("vcs_submit"), { issues });
		const hook = s.git.calls.find((c) => c.args[0] === "hook");
		expect(hook?.args.slice(0, 3)).toEqual(["hook", "run", "--ignore-missing"]);
		expect(hook?.args.slice(4)).toEqual(["pre-push", "--", "origin", url]);
		expect(hook?.options?.timeoutMs).toBe(120_000);
		expect(hook?.stdin).toBe(
			`refs/heads/a c2 refs/heads/a ${"f".repeat(40)}\nrefs/heads/b c3 refs/heads/b ${"0".repeat(40)}\n`,
		);
	});

	test("a failing hook refuses before the submit", async () => {
		const s = setup({
			git: new Git({ stdout: "", stderr: "lint: 3 errors", code: 1 }),
		});
		await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow(
			"lint: 3 errors",
		);
		expect(s.transport.requests.map((r) => r.call.case)).toEqual([
			"stackStatus",
		]);
	});

	test("a hook past the limit is stopped, the submit runs, and the result says to fix the hook", async () => {
		const s = setup({
			git: new Git({ stdout: "", stderr: "", code: -1, timedOut: true }),
			hookTimeoutMs: 5,
		});
		const out = await exec(s.tool("vcs_submit"), { issues });
		expect(
			s.git.calls.find((c) => c.args[0] === "hook")?.options?.timeoutMs,
		).toBe(5);
		expect(s.transport.requests[1]?.call.case).toBe("submitStack");
		expect(text(out)).toContain(HOOK_TIMEOUT_NOTE);
	});

	test("a bookmark with no PR and no issue refuses, naming the issue tools", async () => {
		const s = setup();
		const err = exec(s.tool("vcs_submit"), { issues: { a: "#1" } });
		await expect(err).rejects.toThrow("forge_list_issues");
		await expect(err).rejects.toThrow("forge_create_issue");
		expect(s.transport.requests.map((r) => r.call.case)).toEqual([
			"stackStatus",
		]);
	});

	test("an existing PR needs no issue and keeps its text unless text replaces it", async () => {
		const s = setup({
			results: [
				stackResult([
					{ head: "a", number: 11 },
					{ head: "b", number: 12 },
				]),
				submitted,
			],
		});
		await exec(s.tool("vcs_submit"), {
			issues: { b: "RIG-2" },
			text: {
				bookmark: "b",
				subject: "feat: new title",
				sections: {
					Summary: Array.from({ length: 40 }, () => "w").join(" "),
					Verification: "passed",
					Risks: "none",
					Compatibility: "unchanged",
					Documentation: "none",
				},
			},
		});
		const req = s.transport.requests[1];
		if (req?.call.case !== "submitStack") throw new Error("arm");
		expect(req.call.value.entries.map((e) => e.replaceText)).toEqual([
			false,
			true,
		]);
		expect(req.call.value.entries[1]?.title).toBe("feat: new title (RIG-2)");
		expect(req.call.value.entries[1]?.body).toStartWith("## Summary\n\nw w");
		expect(req.call.value.entries[1]?.body).toContain("Refs RIG-2");
	});

	const refusals: [string, Jj][] = [
		[
			"a conflict",
			new Jj([{ ...stack[0], conflict: true } as Row, ...stack.slice(1)]),
		],
		[
			"a merge",
			new Jj([
				{ id: "c3", bookmarks: ["b"], parents: [{ id: "c2" }, { id: "x" }] },
				...stack.slice(1),
			]),
		],
		[
			"an empty description",
			new Jj([{ ...stack[0], description: "" } as Row, ...stack.slice(1)]),
		],
		[
			"two bookmarks on one commit",
			new Jj([
				{ ...stack[0], bookmarks: ["b", "b2"] } as Row,
				...stack.slice(1),
			]),
		],
		["unpublished commits above the tip", new Jj(stack, [{ id: "c4" }])],
		["a fork", new Jj(stack, [], [{ id: "c9", bookmarks: ["side"] }])],
	];
	for (const [name, jj] of refusals)
		test(`refuses ${name} before any broker call`, async () => {
			const s = setup({ jj });
			await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow();
			expect(s.transport.requests).toHaveLength(0);
			expect(s.git.calls.map((c) => c.args[0])).toEqual(["config"]);
		});

	test("the empty @ that vcs_commit leaves above the tip is allowed", async () => {
		const s = setup({
			jj: new Jj(stack, [{ id: "wc", empty: true, description: "" }]),
		});
		await exec(s.tool("vcs_submit"), { issues });
		expect(s.transport.requests[1]?.call.case).toBe("submitStack");
	});

	test("refuses a tip already in trunk", async () => {
		const s = setup({ jj: new Jj([]) });
		await expect(exec(s.tool("vcs_submit"), { issues })).rejects.toThrow(
			"trunk",
		);
		expect(s.transport.requests).toHaveLength(0);
	});

	test("a request over the limit refuses with no submit call; one just under is sent", async () => {
		// The rest of the request is small, so a bundle 1 KiB under the cap stays under it.
		const over = setup({
			git: new Git(undefined, new Uint8Array(MAX_SUBMIT_BYTES)),
		});
		await expect(exec(over.tool("vcs_submit"), { issues })).rejects.toThrow(
			"limit",
		);
		expect(over.transport.requests.map((r) => r.call.case)).toEqual([
			"stackStatus",
		]);
		const under = setup({
			git: new Git(undefined, new Uint8Array(MAX_SUBMIT_BYTES - 1024)),
		});
		await exec(under.tool("vcs_submit"), { issues });
		expect(under.transport.requests[1]?.call.case).toBe("submitStack");
	});

	test("a failed entry throws, naming it and the skipped ones", async () => {
		const s = setup({
			results: [
				noPrs,
				stackResult([
					{ head: "a", error: "rate limited", status: StackEntryStatus.FAILED },
					{ head: "b", status: StackEntryStatus.SKIPPED },
				]),
			],
		});
		const err = exec(s.tool("vcs_submit"), { issues });
		await expect(err).rejects.toThrow("a: failed: unavailable: rate limited");
		await expect(err).rejects.toThrow("b: skipped");
	});
});

describe("vcs_retire", () => {
	test("deletes the bookmark and fetches only after retire_head succeeds", async () => {
		const s = setup({ results: [stackResult([{ head: "a", number: 11 }])] });
		await exec(s.tool("vcs_retire"), { bookmark: "a" });
		const req = s.transport.requests[0];
		if (req?.call.case !== "retireHead") throw new Error("arm");
		expect(req.call.value).toMatchObject({ repo: "octo/repo", headRef: "a" });
		expect(s.jj.calls.slice(-2).map((c) => c.slice(2))).toEqual([
			["bookmark", "delete", "exact:a"],
			["git", "fetch", "--remote", "origin", "--branch", "exact:a"],
		]);
	});

	test("changes no local state when retire_head fails", async () => {
		const s = setup({
			results: [
				create(ForgeCallResultSchema, {
					result: {
						case: "error",
						value: { code: "permission_denied", message: "not yours" },
					},
				}),
			],
		});
		await expect(exec(s.tool("vcs_retire"), { bookmark: "a" })).rejects.toThrow(
			"not yours",
		);
		expect(s.jj.calls).toHaveLength(0);
	});
});

describe("vcs_pr_status", () => {
	test("asks stack_status for own bookmarks bottom to top", async () => {
		const s = setup({ results: [submitted] });
		const out = await exec(s.tool("vcs_pr_status"), {});
		const req = s.transport.requests[0];
		if (req?.call.case !== "stackStatus") throw new Error("arm");
		expect(req.call.value.headRefs).toEqual(["a", "b"]);
		expect(text(out)).toContain("b: #12, open, draft");
	});
});
