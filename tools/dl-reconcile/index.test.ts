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
});

describe("reconcile", () => {
	test("posts the exact body, including repo and all rows", async () => {
		const requests: Request[] = [];
		const body = buildRequestBody(
			"| DL-001 | one | x | y |\n| DL-001 | two | x | y |",
		);
		await reconcile(body, "test-token", {
			fetchFn: async (input, init) => {
				requests.push(new Request(input, init));
				return new Response(null, { status: 204 });
			},
		});
		expect(requests).toHaveLength(1);
		expect(await requests[0]?.json()).toEqual({
			repo: "compass",
			landed: [
				{ id: "DL-001", surface: "designs", ref: "none" },
				{ id: "DL-001", surface: "designs", ref: "none" },
			],
		});
	});
});
