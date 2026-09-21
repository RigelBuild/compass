import { describe, expect, test } from "bun:test";
import { buildRequestBody, parseLedger, reconcile } from "./index.ts";

describe("parseLedger", () => {
	test("parses every DL row in order, including duplicates", () => {
		const ledger = [
			"| ID | Decision | Status | Record |",
			"| --- | --- | --- | --- |",
			"| DL-001 | first | Active (Matt, 2026-01-01) | [one](one.md) |",
			"| DL-001 | duplicate | Active (Matt, 2026-01-02) | [two](two.md) |",
			"| DL-42 | another | Retired (Matt, 2026-01-03) | [three](three.md) |",
		].join("\n");
		expect(parseLedger(ledger)).toEqual([
			{ id: "DL-001", surface: "designs", ref: "none" },
			{ id: "DL-001", surface: "designs", ref: "none" },
			{ id: "DL-42", surface: "designs", ref: "none" },
		]);
	});
	test("matches outer fence length and marker", () => {
		expect(
			parseLedger(
				[
					"````markdown",
					"| DL-999 | inner | x | y |",
					"```",
					"| DL-001 | valid | x | y |",
					"````",
					"| DL-002 | after | x | y |",
				].join("\n"),
			),
		).toEqual([{ id: "DL-002", surface: "designs", ref: "none" }]);
	});
	test("rejects unterminated fences", () => {
		expect(() => parseLedger("~~~\n| DL-001 | hidden | x | y |")).toThrow(
			"unterminated",
		);
	});
	test("supports indented tilde fences", () => {
		expect(
			parseLedger(
				"  ~~~\n| DL-999 | hidden | x | y |\n  ~~~\n| DL-001 | valid | x | y |",
			),
		).toEqual([{ id: "DL-001", surface: "designs", ref: "none" }]);
	});
	test("skips fenced examples and malformed rows", () => {
		const ledger = [
			"```markdown",
			"| DL-999 | example | x | y |",
			"```",
			"| DL-001 | valid | x | y |",
			"| DL-002 | partial | x |",
		].join("\n");
		expect(parseLedger(ledger)).toEqual([
			{ id: "DL-001", surface: "designs", ref: "none" },
		]);
	});
});

describe("reconcile", () => {
	test("posts the exact request and body", async () => {
		const requests: Request[] = [];
		const body = buildRequestBody(
			"| DL-001 | one | x | y |\n| DL-001 | two | x | y |",
		);
		await reconcile(body, "test-token", {
			fetchFn: async (input, init) => {
				requests.push(new Request(String(input), init));
				return new Response(null, { status: 204 });
			},
		});
		expect(requests).toHaveLength(1);
		const request = requests[0];
		expect(request?.url).toBe("https://dl.rigel.build/reconcile");
		expect(request?.method).toBe("POST");
		expect(request?.headers.get("Authorization")).toBe("Bearer test-token");
		expect(await request?.json()).toEqual({
			repo: "compass",
			landed: [
				{ id: "DL-001", surface: "designs", ref: "none" },
				{ id: "DL-001", surface: "designs", ref: "none" },
			],
		});
	});
	test("rejects redirects and trims the token", async () => {
		let init: RequestInit | undefined;
		await reconcile(buildRequestBody(""), "  token  ", {
			fetchFn: async (_input, requestInit) => {
				init = requestInit;
				return new Response(null, { status: 204 });
			},
		});
		expect(init?.redirect).toBe("error");
		expect(new Headers(init?.headers).get("Authorization")).toBe(
			"Bearer token",
		);
	});
	test("zero timeout uses the default deadline", async () => {
		let signal: AbortSignal | null | undefined;
		await reconcile(buildRequestBody(""), "token", {
			timeoutMs: 0,
			fetchFn: async (_input, requestInit) => {
				signal = requestInit?.signal;
				return new Response(null, { status: 204 });
			},
		});
		expect(signal?.aborted).toBe(false);
	});

	test("rejects a non-2xx response", async () => {
		await expect(
			reconcile(buildRequestBody("| DL-001 | one | x | y |"), "token", {
				fetchFn: async () => new Response(null, { status: 503 }),
			}),
		).rejects.toThrow("HTTP 503");
	});

	test("rejects an empty token without making a request", async () => {
		let called = false;
		await expect(
			reconcile(buildRequestBody(""), "", {
				fetchFn: async () => {
					called = true;
					return new Response(null, { status: 204 });
				},
			}),
		).rejects.toThrow("DL_CLAIM_TOKEN is required");
		expect(called).toBe(false);
	});

	test("aborts the request at the configured deadline", async () => {
		let signal: AbortSignal | null | undefined;
		await reconcile(buildRequestBody(""), "token", {
			timeoutMs: 5,
			fetchFn: async (_input, init) => {
				signal = init?.signal;
				return new Response(null, { status: 204 });
			},
		});
		expect(signal?.aborted).toBe(false);
		// Gate on the abort event itself, so the deadline drives the test.
		await new Promise((resolve) => signal?.addEventListener("abort", resolve));
		expect(signal?.aborted).toBe(true);
	});
});
