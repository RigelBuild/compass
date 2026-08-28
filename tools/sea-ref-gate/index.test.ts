// Unit tests for the sea-ref-gate pure core (index.ts).
//
// These defend the gate's contract: it flags an uppercase-numeric `SEA-<n>`
// issue ref in a tracked, non-carved-out file, and does NOT flag a carve-out, a
// lowercase branch slug, a `SEA-NNN` placeholder, or an allowlisted path. The
// grep-hit parser is exercised on real `git grep -n` line shapes, including
// text that itself contains colons.

import { describe, expect, test } from "bun:test";
import {
	type Deps,
	findViolations,
	isCarveOut,
	lineHasToken,
	runOnce,
} from "./index.ts";

describe("lineHasToken — uppercase-numeric SEA-NNN only", () => {
	test("matches a bare issue ref", () => {
		expect(lineHasToken("Refs SEA-1512 in this record")).toBe(true);
	});
	test("matches inside parens / punctuation", () => {
		expect(lineHasToken("proven (SEA-965).")).toBe(true);
	});
	test("does NOT match a lowercase branch-slug artifact", () => {
		expect(lineHasToken("on `compass-sea-1243-t0-harness-drop`")).toBe(false);
	});
	test("does NOT match a SEA-nnn / SEA-NNN / SEA-N placeholder", () => {
		expect(lineHasToken("file a SEA-NNN follow-up")).toBe(false);
		expect(lineHasToken("issue refs (Linear SEA-nnn)")).toBe(false);
		expect(lineHasToken("Split from SEA-N")).toBe(false);
	});
	test("does NOT match an embedded (non-word-boundary) token", () => {
		expect(lineHasToken("NOSEA-1 and xSEA-2")).toBe(false);
	});
	test("no token, no match", () => {
		expect(lineHasToken("the retired team key")).toBe(false);
	});
});

describe("isCarveOut", () => {
	test("forks/ subtree is carved out", () => {
		expect(isCarveOut("forks/devenv/src/x.nix")).toBe(true);
	});
	test("first-party forks/README.md is NOT carved out", () => {
		expect(isCarveOut("forks/README.md")).toBe(false);
	});
	test("the gate's own source is carved out", () => {
		expect(isCarveOut("tools/sea-ref-gate/index.ts")).toBe(true);
	});
	test("generated eng-docs copies are carved out", () => {
		expect(isCarveOut("apps/eng-docs/src/content/docs/x.md")).toBe(true);
		expect(isCarveOut("apps/eng-docs/dist/index.html")).toBe(true);
	});
	test("the generated bun.lock is carved out", () => {
		expect(isCarveOut("bun.lock")).toBe(true);
	});
	test("a canonical docs/ record is NOT carved out", () => {
		expect(
			isCarveOut("docs/designs/product/compass-board-view/design.md"),
		).toBe(false);
	});
});

describe("findViolations", () => {
	test("flags a real reference in a scanned file", () => {
		const v = findViolations([
			"docs/designs/product/compass-board-view/design.md:12:tracked as SEA-1512 here",
		]);
		expect(v).toHaveLength(1);
		expect(v[0]).toEqual({
			file: "docs/designs/product/compass-board-view/design.md",
			line: 12,
			text: "tracked as SEA-1512 here",
		});
	});
	test("does NOT flag a carved-out file even when it carries the token", () => {
		expect(
			findViolations([
				"forks/devenv/README.md:3:upstream mentions SEA-1 here",
				"tools/sea-ref-gate/index.ts:10:the token SEA-1512",
			]),
		).toHaveLength(0);
	});
	test("does NOT flag a lowercase slug or a placeholder", () => {
		expect(
			findViolations([
				"docs/x.md:5:on compass-sea-1243-branch",
				"go/.golangci.yml:65:issue refs (Linear SEA-nnn)",
			]),
		).toHaveLength(0);
	});
	test("parses text containing colons (line/col-like suffixes)", () => {
		const v = findViolations([
			"docs/x.md:42:see `SEA-1787 fixture.go:67` for the backend",
		]);
		expect(v).toHaveLength(1);
		const [first] = v;
		expect(first).toEqual({
			file: "docs/x.md",
			line: 42,
			text: "see `SEA-1787 fixture.go:67` for the backend",
		});
	});
	test("skips blank and malformed hits", () => {
		expect(
			findViolations(["", "no-colons-here", "path-only:notanumber"]),
		).toHaveLength(0);
	});
	test("collects multiple references", () => {
		const v = findViolations([
			"a.md:1:SEA-1 one",
			"b.md:2:SEA-2 two",
			"forks/x:3:SEA-3 carved",
		]);
		expect(v).toHaveLength(2);
	});
});

describe("runOnce", () => {
	function deps(hits: string[] | Error, out: string[], errs: string[]): Deps {
		return {
			grep: async () => {
				if (hits instanceof Error) throw hits;
				return hits;
			},
			log: (m) => out.push(m),
			err: (m) => errs.push(m),
		};
	}

	test("returns 0 and logs clean when there are no references", async () => {
		const out: string[] = [];
		const errs: string[] = [];
		const code = await runOnce(deps([], out, errs));
		expect(code).toBe(0);
		expect(out.join("\n")).toContain("clean");
		expect(errs).toHaveLength(0);
	});

	test("returns 1 and prints each reference when the boundary is crossed", async () => {
		const out: string[] = [];
		const errs: string[] = [];
		const code = await runOnce(
			deps(["docs/x.md:7:tracked as SEA-1512"], out, errs),
		);
		expect(code).toBe(1);
		const e = errs.join("\n");
		expect(e).toContain("docs/x.md:7");
		expect(e).toContain("RIG-<n>");
	});

	test("returns 2 on a scan error (fail closed)", async () => {
		const out: string[] = [];
		const errs: string[] = [];
		const code = await runOnce(deps(new Error("not a git tree"), out, errs));
		expect(code).toBe(2);
		expect(errs.join("\n")).toContain("cannot scan");
	});

	test("a carved-out hit does not trip the gate", async () => {
		const out: string[] = [];
		const errs: string[] = [];
		const code = await runOnce(
			deps(["forks/devenv/x.nix:1:SEA-1 upstream"], out, errs),
		);
		expect(code).toBe(0);
	});
});
