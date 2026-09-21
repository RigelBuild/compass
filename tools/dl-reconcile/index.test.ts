import { describe, expect, test } from "bun:test";
import {
	buildRequestBody,
	countRawLedgerRows,
	parseLedger,
	reconcile,
} from "./index.ts";

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
	test("parses escaped pipes within a valid table run", () => {
		expect(
			parseLedger(
				"| ID | Decision | Status | Record |\n| --- | --- | --- | --- |\n| DL-7 | a \\| b | Active | [r](r.md) |",
			),
		).toEqual([{ id: "DL-7", surface: "designs", ref: "none" }]);
	});

	test("closes a shorter fence with a longer fence", () => {
		expect(
			parseLedger(
				"```\n| ID | Decision | Status | Record |\n| --- | --- | --- | --- |\n| DL-9 | hidden | x | y |\n````\n| ID | Decision | Status | Record |\n| --- | --- | --- | --- |\n| DL-1 | visible | x | y |",
			),
		).toEqual([{ id: "DL-1", surface: "designs", ref: "none" }]);
	});

	test("ignores indented code rows and deeply indented fences", () => {
		const text = [
			"| ID | Decision | Status | Record |",
			"| --- | --- | --- | --- |",
			"| DL-001 | real | x | y |",
			"    | DL-900 | code | x | y |",
			"    ```",
			"    | DL-901 | code | x | y |",
			"    ```",
		].join("\n");
		expect(parseLedger(text)).toEqual([
			{ id: "DL-001", surface: "designs", ref: "none" },
		]);
		expect(countRawLedgerRows(text)).toBe(1);
	});

	test("counts only decision rows in a valid table run", () => {
		expect(
			countRawLedgerRows(
				"| ID | Decision | Status | Record |\n| --- | --- | --- | --- |\n| DL-7 | decision | Active | [r](r.md) |",
			),
		).toBe(1);
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
					"| ID | Decision | Status | Record |",
					"| --- | --- | --- | --- |",
					"| DL-002 | after | x | y |",
				].join("\n"),
			),
		).toEqual([{ id: "DL-002", surface: "designs", ref: "none" }]);
	});
	test("the floor excludes non-DL rows inside a table run", () => {
		const text = [
			"| ID | Decision | Status | Record |",
			"| --- | --- | --- | --- |",
			"| DL-001 | first | x | y |",
			"| note | see above | x | y |",
			"| DL-002 | second | x | y |",
		].join("\n");
		expect(countRawLedgerRows(text)).toBe(2);
	});
	test("rejects unterminated fences", () => {
		expect(() => parseLedger("~~~\n| DL-001 | hidden | x | y |")).toThrow(
			"unterminated",
		);
	});
	test("supports indented tilde fences", () => {
		expect(
			parseLedger(
				"  ~~~\n| DL-999 | hidden | x | y |\n  ~~~\n| ID | Decision | Status | Record |\n| --- | --- | --- | --- |\n| DL-001 | valid | x | y |",
			),
		).toEqual([{ id: "DL-001", surface: "designs", ref: "none" }]);
	});
	test("skips fenced examples and malformed rows", () => {
		const ledger = [
			"```markdown",
			"| DL-999 | example | x | y |",
			"```",
			"| ID | Decision | Status | Record |",
			"| --- | --- | --- | --- |",
			"| DL-001 | valid | x | y |",
			"| DL-002 | partial | x |",
		].join("\n");
		expect(parseLedger(ledger)).toEqual([
			{ id: "DL-001", surface: "designs", ref: "none" },
		]);
	});

	test("treats a fence line carrying an info string as content", () => {
		const ledger = [
			"```markdown",
			"```json",
			"| DL-900 | fenced example | x | y |",
			"```",
			"| ID | Decision | Status | Record |",
			"| --- | --- | --- | --- |",
			"| DL-001 | real | x | y |",
		].join("\n");
		expect(parseLedger(ledger)).toEqual([
			{ id: "DL-001", surface: "designs", ref: "none" },
		]);
	});
});

type LedgerOutcome =
	| { kind: "rows"; count: number }
	| { kind: "throws"; message: string };

function outcome(run: () => number): LedgerOutcome {
	try {
		return { kind: "rows", count: run() };
	} catch (error) {
		return {
			kind: "throws",
			message: error instanceof Error ? error.message : String(error),
		};
	}
}

const ANCHORED_ROW = [
	"| ID | Decision | Status | Record |",
	"| --- | --- | --- | --- |",
	"| DL-001 | real | Active (Matt, 2026-01-01) | [r](r.md) |",
];

describe("shared line classification", () => {
	// Both counters read one classification pre-pass, so every fence and comment
	// shape must resolve identically for the parser and the floor.
	const cases: {
		name: string;
		lines: string[];
		expected: LedgerOutcome;
	}[] = [
		{
			name: "an unterminated fence",
			lines: ["```", ...ANCHORED_ROW],
			expected: {
				kind: "throws",
				message: "unterminated fenced block in design ledger",
			},
		},
		{
			name: "a DL row inside a closed fence",
			lines: ["```", "| DL-900 | hidden | x | y |", "```", ...ANCHORED_ROW],
			expected: { kind: "rows", count: 1 },
		},
		{
			name: "a fence line carrying an info string",
			lines: [
				"```markdown",
				"```json",
				"| DL-900 | hidden | x | y |",
				"```",
				...ANCHORED_ROW,
			],
			expected: { kind: "rows", count: 1 },
		},
		{
			name: "an unterminated HTML comment",
			lines: ["<!--", ...ANCHORED_ROW],
			expected: {
				kind: "throws",
				message: "unterminated HTML comment in design ledger",
			},
		},
		{
			name: "a commented-out whole table",
			lines: [
				...ANCHORED_ROW,
				"",
				"<!--",
				"| ID | Decision | Status | Record |",
				"| --- | --- | --- | --- |",
				"| DL-905 | parked draft | Active (Matt, 2026-01-01) | [r](r.md) |",
				"-->",
			],
			expected: { kind: "rows", count: 1 },
		},
		{
			name: "a comment closer carrying trailing whitespace",
			lines: ["<!--", "| DL-905 | parked | x | y |", "-->   ", ...ANCHORED_ROW],
			expected: { kind: "rows", count: 1 },
		},
		{
			name: "fence markers inside a comment",
			lines: [
				"<!--",
				"```",
				"| DL-905 | parked | x | y |",
				"-->",
				...ANCHORED_ROW,
			],
			expected: { kind: "rows", count: 1 },
		},
		{
			name: "a comment opener inside a fence",
			lines: [
				"```markdown",
				"<!--",
				"| DL-900 | example | x | y |",
				"```",
				...ANCHORED_ROW,
			],
			expected: { kind: "rows", count: 1 },
		},
		{
			name: "a single-line comment inside a table run",
			lines: [
				...ANCHORED_ROW,
				"<!-- | DL-905 | parked | x | y | -->",
				"| DL-002 | also real | Active (Matt, 2026-01-02) | [r](r.md) |",
			],
			expected: { kind: "rows", count: 2 },
		},
		{
			name: "a multi-line comment inside a table run",
			lines: [
				...ANCHORED_ROW,
				"<!--",
				"| DL-905 | parked | x | y |",
				"-->",
				"| DL-002 | also real | Active (Matt, 2026-01-02) | [r](r.md) |",
			],
			expected: { kind: "rows", count: 2 },
		},
	];
	for (const { name, lines, expected } of cases) {
		test(`${name} resolves the same for both counters`, () => {
			const text = lines.join("\n");
			expect(outcome(() => parseLedger(text).length)).toEqual(expected);
			expect(outcome(() => countRawLedgerRows(text))).toEqual(expected);
		});
	}

	test("a commented-out table contributes no row to the posted body", () => {
		const ledger = [
			...ANCHORED_ROW,
			"",
			"<!--",
			"| ID | Decision | Status | Record |",
			"| --- | --- | --- | --- |",
			"| DL-905 | parked draft | Active (Matt, 2026-01-01) | [r](r.md) |",
			"-->",
		].join("\n");
		expect(buildRequestBody(ledger).landed).toEqual([
			{ id: "DL-001", surface: "designs", ref: "none" },
		]);
	});
});

describe("the ledger table anchor", () => {
	// The floor carries no anchor by design, so a header the parser stops
	// recognizing reads parser-zero against a non-zero floor: a loud mismatch
	// rather than a silently empty snapshot.
	for (const header of [
		"| ID | Decision | Status | Records |",
		"| **ID** | **Decision** | **Status** | **Record** |",
		"| ID | Decision | Record | Status |",
	]) {
		test(`yields no rows under the near-miss header ${header}`, () => {
			const text = [
				header,
				"| --- | --- | --- | --- |",
				"| DL-001 | real | Active (Matt, 2026-01-01) | [r](r.md) |",
			].join("\n");
			expect(parseLedger(text)).toEqual([]);
			expect(countRawLedgerRows(text)).toBe(1);
		});
	}
});

describe("reconcile", () => {
	test("posts the exact request and body", async () => {
		const requests: Request[] = [];
		const body = buildRequestBody(
			"| ID | Decision | Status | Record |\n| --- | --- | --- | --- |\n| DL-001 | one | x | y |\n| DL-001 | two | x | y |",
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
		let requestedTimeout = 0;
		const control = new AbortController();
		await reconcile(buildRequestBody(""), "token", {
			timeoutMs: 0,
			timeoutSignal: (timeoutMs) => {
				requestedTimeout = timeoutMs;
				return control.signal;
			},
			fetchFn: async (_input, requestInit) => {
				signal = requestInit?.signal;
				return new Response(null, { status: 204 });
			},
		});
		expect(requestedTimeout).toBe(30_000);
		expect(signal).toBe(control.signal);
		control.abort();
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

	test("builds the deadline signal from the caller's timeout", async () => {
		let requestedTimeout = 0;
		let signal: AbortSignal | null | undefined;
		const control = new AbortController();
		await reconcile(buildRequestBody(""), "token", {
			timeoutMs: 5,
			timeoutSignal: (timeoutMs) => {
				requestedTimeout = timeoutMs;
				return control.signal;
			},
			fetchFn: async (_input, init) => {
				signal = init?.signal;
				return new Response(null, { status: 204 });
			},
		});
		expect(requestedTimeout).toBe(5);
		expect(signal).toBe(control.signal);
	});

	test("defaults to a real firing deadline when none is injected", async () => {
		let signal: AbortSignal | null | undefined;
		// No timeoutSignal, so this exercises the production AbortSignal.timeout
		// path that actually bounds the request.
		await reconcile(buildRequestBody(""), "token", {
			timeoutMs: 1,
			fetchFn: async (_input, init) => {
				signal = init?.signal;
				return new Response(null, { status: 204 });
			},
		});
		if (!signal?.aborted)
			await new Promise<void>((resolve) =>
				signal?.addEventListener("abort", () => resolve(), { once: true }),
			);
		expect(signal?.aborted).toBe(true);
	});
});
