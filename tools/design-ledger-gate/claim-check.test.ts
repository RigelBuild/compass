import { describe, expect, test } from "bun:test";
import { claimCheckViolations, fetchClaims } from "./claim-check.ts";
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

	test("429 waits exceeding the shared 65s budget use the rate-limit message", async () => {
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
					},
				}),
			),
		).rejects.toThrow(
			"design-ledger-gate: the DL claim service rate-limited this runner for 65 s. Re-run the pipeline in a minute.",
		);
		expect(calls).toBe(2);
		// The second wait would overrun the budget, so it fails without sleeping.
		expect(sleeps).toEqual([60_000]);
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
