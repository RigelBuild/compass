import { describe, expect, test } from "bun:test";
import { buildClaimBody, claim, formatClaimed, parseArgs } from "./index.ts";

const args = { ref: "RIG-42", lane: "feature/design", count: 1 };
const body = buildClaimBody(args);
const claimed = [{ id: "DL-377", date: "2026-09-29" }];

describe("parseArgs", () => {
	test("accepts separated and equals flag values", () => {
		expect(
			parseArgs(["--ref", "RIG-42", "--lane", "feature/a", "--count", "2"]),
		).toEqual({
			ref: "RIG-42",
			lane: "feature/a",
			count: 2,
		});
		expect(
			parseArgs(["--ref=none", "--lane=feature/b", "--repo=compass"]),
		).toEqual({
			ref: "none",
			lane: "feature/b",
			count: 1,
		});
	});

	test("requires a valid ref and non-empty lane", () => {
		for (const argv of [
			["--lane", "x"],
			["--ref", "RIG-", "--lane", "x"],
			["--ref", "rig-12", "--lane", "x"],
			["--ref", "RIG-1", "--lane", "  "],
		]) {
			expect(() => parseArgs(argv)).toThrow();
		}
	});

	test("accepts only counts from one through ten", () => {
		for (const count of ["0", "11", "1.5"]) {
			expect(() =>
				parseArgs(["--ref", "none", "--lane", "x", "--count", count]),
			).toThrow();
		}
	});

	test("rejects other repos and unknown flags", () => {
		expect(() =>
			parseArgs(["--ref", "none", "--lane", "x", "--repo", "other"]),
		).toThrow("only the compass partition");
		expect(() => parseArgs(["--ref", "none", "--lane", "x", "--wat"])).toThrow(
			"Usage:",
		);
	});
});

describe("buildClaimBody", () => {
	test("pins the Compass designs partition", () => {
		expect(body).toEqual({
			repo: "compass",
			surface: "designs",
			ref: "RIG-42",
			lane: "feature/design",
			count: 1,
		});
	});
});

describe("claim", () => {
	test("posts the exact request with trimmed bearer token", async () => {
		let request: Request | undefined;
		await claim(body, "  test-token  ", {
			fetchFn: async (input, init) => {
				request = new Request(String(input), init);
				return Response.json({ ids: claimed });
			},
		});
		expect(request?.url).toBe("https://dl.rigel.build/claim");
		expect(request?.method).toBe("POST");
		expect(request?.headers.get("Authorization")).toBe("Bearer test-token");
		expect(request?.headers.get("Content-Type")).toBe("application/json");
		expect(request?.redirect).toBe("error");
		expect(await request?.json()).toEqual(body);
	});

	test("rejects blank token without calling fetch", async () => {
		let called = false;
		await expect(
			claim(body, "  ", {
				fetchFn: async () => {
					called = true;
					return Response.json({ ids: claimed });
				},
			}),
		).rejects.toThrow("DL_CLAIM_TOKEN is required");
		expect(called).toBe(false);
	});

	test("returns validated ids", async () => {
		await expect(
			claim(body, "token", {
				fetchFn: async () => Response.json({ ids: claimed }),
			}),
		).resolves.toEqual(claimed);
	});

	test("names service errors and hydration hint", async () => {
		await expect(
			claim(body, "token", {
				fetchFn: async () =>
					Response.json({ error: "not_hydrated" }, { status: 503 }),
			}),
		).rejects.toThrow(
			"HTTP 503 (not_hydrated). The reconcile workflow has not hydrated",
		);
		await expect(
			claim(body, "token", {
				fetchFn: async () =>
					Response.json({ error: "unauthorized" }, { status: 401 }),
			}),
		).rejects.toThrow("HTTP 401 (unauthorized). Token missing or wrong.");
		await expect(
			claim(body, "token", {
				fetchFn: async () => new Response("oops", { status: 502 }),
			}),
		).rejects.toThrow("HTTP 502");
	});

	test("rejects wrong count and malformed ids", async () => {
		for (const [payload, reason] of [
			[{}, "requested number of ids"],
			[{ ids: [] }, "requested number of ids"],
			[{ ids: [...claimed, ...claimed] }, "requested number of ids"],
			[{ ids: [{ id: "DL-37", date: "2026-09-29" }] }, "malformed id"],
			[{ ids: [{ id: "DL-377", date: "20260929" }] }, "malformed id"],
		] as const) {
			await expect(
				claim(body, "token", {
					fetchFn: async () => Response.json(payload),
				}),
			).rejects.toThrow(reason);
		}
	});

	test("warns ids may be consumed only when the counter could have advanced", async () => {
		const consumed = "Ids may already be consumed";
		const failWith = (fetchFn: () => Promise<Response>) =>
			claim(body, "token", { fetchFn }).then(
				() => "",
				(error: Error) => error.message,
			);
		for (const fetchFn of [
			async () => new Response("oops", { status: 502 }),
			async () => Response.json({ ids: [...claimed, ...claimed] }),
			async () => new Response("not json"),
			async () => {
				throw new DOMException("The operation timed out.", "TimeoutError");
			},
		]) {
			expect(await failWith(fetchFn)).toContain(consumed);
		}
		for (const status of [400, 401, 429]) {
			expect(
				await failWith(async () => Response.json({ error: "x" }, { status })),
			).not.toContain(consumed);
		}
		expect(
			await failWith(async () =>
				Response.json({ error: "not_hydrated" }, { status: 503 }),
			),
		).not.toContain(consumed);
	});

	test("names the ids a rejected 200 body carried", async () => {
		await expect(
			claim(body, "token", {
				fetchFn: async () =>
					Response.json({
						ids: [claimed[0], { id: "DL-378", date: "bad" }],
					}),
			}),
		).rejects.toThrow("Returned: DL-377, DL-378.");
	});

	test("forwards the timeout deadline to the request", async () => {
		let timeout = 0;
		let signal: AbortSignal | null | undefined;
		const controller = new AbortController();
		await claim(body, "token", {
			timeoutMs: 12_345,
			timeoutSignal: (timeoutMs) => {
				timeout = timeoutMs;
				return controller.signal;
			},
			fetchFn: async (_input, init) => {
				signal = init?.signal;
				return Response.json({ ids: claimed });
			},
		});
		expect(timeout).toBe(12_345);
		expect(signal).toBe(controller.signal);
		controller.abort();
	});
});

describe("formatClaimed", () => {
	test("formats one line per id", () => {
		expect(
			formatClaimed([
				{ id: "DL-377", date: "2026-09-29" },
				{ id: "DL-378", date: "2026-09-29" },
			]),
		).toBe("DL-377 (claimed 2026-09-29)\nDL-378 (claimed 2026-09-29)");
	});
});
