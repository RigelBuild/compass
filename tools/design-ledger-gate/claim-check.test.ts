import { describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";
import {
	claimCheckViolations,
	fetchClaims,
	mergeBaseDecisionIds,
	parseHttpDate,
} from "./claim-check.ts";
import { type Changed, type Deps, KEY_LINE, runOnce } from "./index.ts";

const claimed = new Map([["DL-123", "lane" as const]]);
const notClaimed = new Map([["DL-123", "not-lane" as const]]);

function claimDeps(options: {
	fetchFn: (
		input: string | URL | Request,
		init?: RequestInit,
	) => Promise<Response>;
	sleep?: (milliseconds: number) => Promise<void>;
	now?: () => number;
}) {
	return {
		fetchFn: options.fetchFn,
		sleep: options.sleep ?? (async () => {}),
		now: options.now ?? (() => 0),
	};
}

function jsonResponse(
	body: unknown,
	status = 200,
	init?: Omit<ResponseInit, "status">,
) {
	return Response.json(body, { ...init, status });
}

test("claim verdicts allow lane claims and report not-lane claims at the id line", () => {
	const pathById = new Map([["DL-123", "docs/designs/decisions/ui/DL-123.md"]]);
	expect(
		claimCheckViolations(["DL-123"], claimed, "feature/ui", pathById),
	).toEqual([]);
	const [violation] = claimCheckViolations(
		["DL-123"],
		notClaimed,
		"feature/ui",
		pathById,
	);
	expect(violation).toEqual({
		file: "docs/designs/decisions/ui/DL-123.md",
		line: KEY_LINE.id,
		message:
			"DL-123: not claimed for lane feature/ui. Claim a fresh id with `bun tools/dl-claim --ref <RIG-n|none> --lane feature/ui` and rename this decision file and every citation of it.",
	});
});

describe("fetchClaims", () => {
	test("returns the lane verdict and sends the anonymous route request", async () => {
		let requestUrl: string | URL | Request | undefined;
		let requestInit: RequestInit | undefined;
		const verdicts = await fetchClaims(
			"feature/ui",
			["DL-123"],
			claimDeps({
				fetchFn: async (input, init) => {
					requestUrl = input;
					requestInit = init;
					return jsonResponse({ claims: [{ id: "DL-123", state: "lane" }] });
				},
			}),
		);
		expect(verdicts).toEqual(claimed);
		expect(new URL(String(requestUrl)).href).toBe(
			"https://dl.rigel.build/claims?repo=compass&lane=feature%2Fui&ids=DL-123",
		);
		expect(requestInit?.headers).toBeUndefined();
		expect(requestInit?.signal).toBeInstanceOf(AbortSignal);
	});

	test("retries 5xx twice with 1s and 3s sleeps before success", async () => {
		let calls = 0;
		const sleeps: number[] = [];
		const verdicts = await fetchClaims(
			"feature/ui",
			["DL-123"],
			claimDeps({
				fetchFn: async () => {
					calls += 1;
					return calls < 3
						? jsonResponse({}, 503)
						: jsonResponse({ claims: [{ id: "DL-123", state: "lane" }] });
				},
				sleep: async (milliseconds) => {
					sleeps.push(milliseconds);
				},
			}),
		);
		expect(verdicts).toEqual(claimed);
		expect(calls).toBe(3);
		expect(sleeps).toEqual([1000, 3000]);
	});

	test("three 5xx responses fail with the service-unavailable message", async () => {
		let calls = 0;
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						return jsonResponse({}, 503);
					},
					sleep: async () => {},
				}),
			),
		).rejects.toThrow(
			"design-ledger-gate: cannot reach the DL claim service at https://dl.rigel.build (HTTP 503). Re-run the pipeline when it answers. Do not hand-pick an id: an unclaimed id fails.",
		);
		expect(calls).toBe(3);
	});
	test("retries network errors three times with bounded backoff", async () => {
		let calls = 0;
		const sleeps: number[] = [];
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						throw new Error("offline");
					},
					sleep: async (milliseconds) => {
						sleeps.push(milliseconds);
					},
				}),
			),
		).rejects.toThrow("(offline)");
		expect(calls).toBe(3);
		expect(sleeps).toEqual([1000, 3000]);
	});

	test("does not make a request for an empty id list", async () => {
		let calls = 0;
		const verdicts = await fetchClaims(
			"feature/ui",
			[],
			claimDeps({
				fetchFn: async () => {
					calls += 1;
					return jsonResponse({ claims: [] });
				},
			}),
		);
		expect(verdicts.size).toBe(0);
		expect(calls).toBe(0);
	});

	test("retries 429 after Retry-After without spending a network attempt", async () => {
		let calls = 0;
		const sleeps: number[] = [];
		const verdicts = await fetchClaims(
			"feature/ui",
			["DL-123"],
			claimDeps({
				fetchFn: async () => {
					calls += 1;
					return calls === 1
						? jsonResponse({}, 429, { headers: { "retry-after": "30" } })
						: jsonResponse({ claims: [{ id: "DL-123", state: "lane" }] });
				},
				sleep: async (milliseconds) => {
					sleeps.push(milliseconds);
				},
			}),
		);
		expect(verdicts).toEqual(claimed);
		expect(calls).toBe(2);
		expect(sleeps).toEqual([30000]);
	});

	test("429 waits past the 65s run deadline use the rate-limit message", async () => {
		let clock = 0;
		let calls = 0;
		const sleeps: number[] = [];
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						return jsonResponse({}, 429, {
							headers: { "retry-after": calls === 1 ? "60" : "10" },
						});
					},
					sleep: async (milliseconds) => {
						sleeps.push(milliseconds);
						clock += milliseconds;
					},
					now: () => clock,
				}),
			),
		).rejects.toThrow(
			"design-ledger-gate: the DL claim service rate-limited this runner for 65 s. Re-run the pipeline in a minute.",
		);
		expect(calls).toBe(2);
		// The second wait would overrun the deadline, so it fails without sleeping.
		expect(sleeps).toEqual([60_000]);
	});

	test("request time counts against the rate-limit deadline", async () => {
		let clock = 0;
		let calls = 0;
		const sleeps: number[] = [];
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						clock += 9_000;
						return jsonResponse({}, 429, { headers: { "retry-after": "5" } });
					},
					sleep: async (milliseconds) => {
						sleeps.push(milliseconds);
						clock += milliseconds;
					},
					now: () => clock,
				}),
			),
		).rejects.toThrow("rate-limited this runner");
		// Each round costs 14 s; counting sleeps alone would allow 13 rounds.
		expect(calls).toBe(5);
		expect(sleeps).toEqual([5_000, 5_000, 5_000, 5_000]);
	});

	test("parseHttpDate reads all three RFC 9110 forms as UTC", () => {
		const now = Date.UTC(2026, 9, 12);
		const expected = Date.UTC(1994, 10, 6, 8, 49, 37);
		for (const text of [
			"Sun, 06 Nov 1994 08:49:37 GMT",
			"Sunday, 06-Nov-94 08:49:37 GMT",
			"Sun Nov  6 08:49:37 1994",
		]) {
			expect(parseHttpDate(text, now)).toBe(expected);
		}
		// rfc850 two-digit year: within 50 years ahead stays in this century.
		expect(parseHttpDate("Monday, 12-Oct-26 00:00:00 GMT", now)).toBe(now);
	});

	test("parseHttpDate rejects impossible dates and mismatched weekdays", () => {
		const now = Date.UTC(2026, 9, 12);
		for (const text of [
			"Mon, 32 Jan 2025 00:00:00 GMT",
			"Mon, 31 Feb 2025 00:00:00 GMT",
			"Mon, 06 Nov 1994 08:49:37 GMT",
			"Sun, 06 Nov 1994 25:00:00 GMT",
			"Sun, 06-Nov-94 08:49:37 GMT",
			"Sun Nov  6 08:49:37 1994 GMT",
			"2026-10-12",
		]) {
			expect(parseHttpDate(text, now)).toBeNull();
		}
	});

	test("Retry-After accepts zero and an HTTP-date", async () => {
		for (const [header, expected] of [
			["0", 0],
			[new Date(Date.now() + 20_000).toUTCString(), 20_000],
		] as const) {
			let calls = 0;
			const sleeps: number[] = [];
			await fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						return calls === 1
							? jsonResponse({}, 429, { headers: { "retry-after": header } })
							: jsonResponse({ claims: [{ id: "DL-123", state: "lane" }] });
					},
					sleep: async (milliseconds) => {
						sleeps.push(milliseconds);
					},
				}),
			);
			expect(sleeps).toHaveLength(1);
			// toUTCString drops milliseconds, so the date form lands up to 1 s early.
			expect(sleeps[0]).toBeLessThanOrEqual(expected);
			expect(sleeps[0]).toBeGreaterThan(expected - 1_500);
		}
	});

	test("Retry-After that is fractional or malformed falls back to 60 s", async () => {
		for (const header of [
			"1.5",
			"soon",
			"2026-10-12",
			"Mon, 32 Jan 2025 00:00:00 GMT",
		]) {
			let calls = 0;
			const sleeps: number[] = [];
			await fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						return calls === 1
							? jsonResponse({}, 429, { headers: { "retry-after": header } })
							: jsonResponse({ claims: [{ id: "DL-123", state: "lane" }] });
					},
					sleep: async (milliseconds) => {
						sleeps.push(milliseconds);
					},
				}),
			);
			expect(sleeps).toEqual([60_000]);
		}
	});

	test("400 fails immediately", async () => {
		let calls = 0;
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({
					fetchFn: async () => {
						calls += 1;
						return jsonResponse({ error: "invalid_query" }, 400);
					},
				}),
			),
		).rejects.toThrow(
			"cannot reach the DL claim service at https://dl.rigel.build",
		);
		expect(calls).toBe(1);
	});

	test("malformed and incomplete responses fail immediately", async () => {
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({ fetchFn: async () => new Response("not json") }),
			),
		).rejects.toThrow("malformed response");
		await expect(
			fetchClaims(
				"feature/ui",
				["DL-123"],
				claimDeps({ fetchFn: async () => jsonResponse({ claims: [] }) }),
			),
		).rejects.toThrow("missing DL-123");
	});

	test("splits ids into request batches of at most 100", async () => {
		const requested: string[][] = [];
		const ids = Array.from(
			{ length: 101 },
			(_, index) => `DL-${String(index).padStart(3, "0")}`,
		);
		const verdicts = await fetchClaims(
			"feature/ui",
			ids,
			claimDeps({
				fetchFn: async (input) => {
					const url = new URL(String(input));
					const batch = url.searchParams.get("ids")?.split(",") ?? [];
					requested.push(batch);
					return jsonResponse({
						claims: batch.map((id) => ({ id, state: "lane" })),
					});
				},
			}),
		);
		expect(requested.map((batch) => batch.length)).toEqual([100, 1]);
		expect(verdicts.size).toBe(101);
	});
});

const decisionPath = "docs/designs/decisions/ui/DL-123.md";
const recordPath = "docs/designs/ui/record/design.md";
const decisionText = [
	"---",
	"id: DL-123",
	'decision: "Use the stable design."',
	'status: "Active (Matt, 2026-07-22)"',
	"record: ../../ui/record/design.md",
	"---",
].join("\n");
const changedForDecision: Changed = {
	files: [decisionPath],
	body: null,
	headBranch: "feature/ui",
	author: "alice",
	crossRepository: false,
};

function runFixture(
	options: {
		changed?: Changed;
		mainIds?: Set<string>;
		claims?: Deps["claims"];
		mainDecisionIds?: Deps["mainDecisionIds"];
	} = {},
) {
	const errors: string[] = [];
	let mainCalls = 0;
	let claimCalls = 0;
	const deps: Deps = {
		root: "/fake",
		readText: async (_root, path) =>
			path === decisionPath
				? decisionText
				: path === recordPath
					? "# Record\n"
					: null,
		listDesignFiles: async () => [decisionPath, recordPath],
		readRecord: () => ({ headings: ["record"], sizeBytes: 100 }),
		changed: options.changed ?? changedForDecision,
		log: () => {},
		err: (message) => errors.push(message),
		mainDecisionIds:
			options.mainDecisionIds ??
			(async () => {
				mainCalls += 1;
				return options.mainIds ?? new Set<string>();
			}),
		claims:
			options.claims ??
			(async () => {
				claimCalls += 1;
				return claimed;
			}),
	};
	return {
		deps,
		errors,
		mainCalls: () => mainCalls,
		claimCalls: () => claimCalls,
	};
}

describe("runOnce claim leg", () => {
	test("accepts a new id claimed for the PR head branch", async () => {
		const { deps, errors, claimCalls } = runFixture();
		expect(await runOnce(deps)).toBe(0);
		expect(errors).toEqual([]);
		expect(claimCalls()).toBe(1);
	});

	test("reports an unclaimed new id with its lane", async () => {
		const { deps, errors } = runFixture({ claims: async () => notClaimed });
		expect(await runOnce(deps)).toBe(1);
		expect(errors.join("\n")).toContain(
			"DL-123: not claimed for lane feature/ui",
		);
	});

	test("does not fetch claims when changed decision file is malformed", async () => {
		const { deps, claimCalls } = runFixture();
		deps.readText = async (_root, path) =>
			path === decisionPath
				? "not a valid decision"
				: path === recordPath
					? "# Record\n"
					: null;
		expect(await runOnce(deps)).toBe(1);
		expect(claimCalls()).toBe(0);
	});

	test("does not fetch claims when there are no changed decision ids", async () => {
		const { deps, mainCalls, claimCalls } = runFixture({
			changed: { ...changedForDecision, files: [] },
		});
		expect(await runOnce(deps)).toBe(0);
		expect(mainCalls()).toBe(0);
		expect(claimCalls()).toBe(0);
	});
	test("returns exit 2 when origin main cannot be listed", async () => {
		const { deps, errors } = runFixture({
			mainDecisionIds: async () => {
				throw new Error(
					"design-ledger-gate: cannot list decision ids from origin/main; origin/main must resolve in this checkout.",
				);
			},
		});
		expect(await runOnce(deps)).toBe(2);
		expect(errors).toContain(
			"design-ledger-gate: cannot list decision ids from origin/main; origin/main must resolve in this checkout.",
		);
	});
	test("skips claim checking for a bot-pinned exempt branch", async () => {
		const { deps, mainCalls, claimCalls } = runFixture({
			changed: {
				...changedForDecision,
				headBranch: "renovate/refresh",
				author: "app/rigelbuild-renovate",
			},
		});
		expect(await runOnce(deps)).toBe(0);
		expect(mainCalls()).toBe(0);
		expect(claimCalls()).toBe(0);
	});

	test("rejects a new decision from a fork without calling the claims service", async () => {
		const { deps, errors, claimCalls } = runFixture({
			changed: { ...changedForDecision, crossRepository: true },
		});
		expect(await runOnce(deps)).toBe(1);
		expect(errors.join("\n")).toContain(
			"open the pull request from a branch in this repository",
		);
		expect(claimCalls()).toBe(0);
	});

	test("returns exit 2 when the claim service fails", async () => {
		const { deps, errors } = runFixture({
			claims: async () => {
				throw new Error(
					"design-ledger-gate: cannot reach the DL claim service at https://dl.rigel.build (HTTP 503). Re-run the pipeline when it answers. Do not hand-pick an id: an unclaimed id fails.",
				);
			},
		});
		expect(await runOnce(deps)).toBe(2);
		expect(errors).toContain(
			"design-ledger-gate: cannot reach the DL claim service at https://dl.rigel.build (HTTP 503). Re-run the pipeline when it answers. Do not hand-pick an id: an unclaimed id fails.",
		);
	});
});

describe("mergeBaseDecisionIds", () => {
	test("lists the merge ref's base, not a main that moved after it", async () => {
		const repo = await mkdtemp(join(tmpdir(), "ledger-merge-base-"));
		try {
			// Identity per call: CI runners have no global git user.
			const git = (args: string[]) =>
				$`git -c user.name=t -c user.email=t@t ${args}`.cwd(repo).quiet();
			const decisions = join(repo, "docs/designs/decisions/ui");
			const addDecision = async (id: string) => {
				await mkdir(decisions, { recursive: true });
				await writeFile(join(decisions, `${id}.md`), `# ${id}\n`);
				await git(["add", "-A"]);
				await git(["commit", "-q", "-m", id]);
			};
			await git(["init", "-q", "-b", "main"]);
			await addDecision("DL-001");
			await git(["switch", "-q", "-c", "pr"]);
			await addDecision("DL-003");
			await git(["switch", "-q", "main"]);
			await addDecision("DL-002");
			// The merge ref GitHub builds: main at DL-002 merged with the PR.
			await git(["switch", "-q", "--detach", "main"]);
			await git(["merge", "--no-ff", "-q", "pr", "-m", "merge"]);
			const mergeRef = (await git(["rev-parse", "HEAD"]).text()).trim();
			// main then moves on and lands the PR's id for itself.
			await git(["switch", "-q", "main"]);
			await addDecision("DL-003");
			await git(["update-ref", "refs/remotes/origin/main", "main"]);
			await git(["checkout", "-q", mergeRef]);

			expect([...(await mergeBaseDecisionIds(repo))].sort()).toEqual([
				"DL-001",
				"DL-002",
			]);

			// A plain local branch checkout: its fork point with main.
			await git(["checkout", "-q", "pr"]);
			expect([...(await mergeBaseDecisionIds(repo))]).toEqual(["DL-001"]);
		} finally {
			await rm(repo, { recursive: true, force: true });
		}
	});

	test("fails when origin/main does not resolve", async () => {
		const repo = await mkdtemp(join(tmpdir(), "ledger-no-main-"));
		try {
			await $`git init -q -b main`.cwd(repo).quiet();
			await expect(mergeBaseDecisionIds(repo)).rejects.toThrow(
				"origin/main must resolve",
			);
		} finally {
			await rm(repo, { recursive: true, force: true });
		}
	});
});
