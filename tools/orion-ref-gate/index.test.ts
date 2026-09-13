// Unit tests for the orion-ref-gate pure core (index.ts).
//
// These defend the gate's contract: it flags a whole-word reference to the
// private monorepo in a tracked, non-carved-out file, and does NOT flag a
// carve-out, an allowlisted path, or a mere substring (e.g. "orionic"). The
// grep-hit parser is exercised on real `git grep -n` line shapes, including
// text that itself contains colons.

import { describe, expect, test } from "bun:test";
import {
	type Deps,
	findViolations,
	isCarveOut,
	lineHasToken,
	REMEDIATION_DOC,
	runOnce,
} from "./index.ts";

describe("lineHasToken — whole-word, case-insensitive", () => {
	test("matches the bare token", () => {
		expect(lineHasToken("ported from orion's config")).toBe(true);
	});
	test("matches any case", () => {
		expect(lineHasToken("Orion is Woodpecker-driven")).toBe(true);
		expect(lineHasToken("the ORION monorepo")).toBe(true);
	});
	test("matches inside a path citation", () => {
		expect(lineHasToken("`orion tools/publish-agent-config/index.ts:79`")).toBe(
			true,
		);
	});
	test("does NOT match a substring / different word", () => {
		expect(lineHasToken("orionic nebula naming")).toBe(false);
		expect(lineHasToken("categorion")).toBe(false);
	});
	test("no token, no match", () => {
		expect(lineHasToken("the internal monorepo's renovate config")).toBe(false);
	});
	test("the gate's own compound name is not a private-repo reference", () => {
		expect(lineHasToken("orion-ref-gate: 'tools/orion-ref-gate'")).toBe(false);
		expect(lineHasToken("# The orion-ref-gate boundary gate (RIG-2489)")).toBe(
			false,
		);
		expect(lineHasToken('"@compass/orion-ref-gate": ["…"]')).toBe(false);
	});
	test("a real token still matches even alongside the gate's own name", () => {
		expect(lineHasToken("orion-ref-gate scans for orion references")).toBe(
			true,
		);
	});
});

describe("lineHasToken — the former name, matched only in repo-shaped uses", () => {
	test("a path inside the private repo is a reference", () => {
		expect(lineHasToken("See `sealed/ci/pipeline.ts:554` for the job")).toBe(
			true,
		);
	});
	// The space form, not the slash form: citing a file in another repo by
	// `<repo> <path>` is the most natural shape, and every one of these is a
	// line this scrub removed by hand — so the gate has to catch its own
	// regression, not just the form that happened to be written with a slash.
	test("a space-separated path or bare filename citation is a reference", () => {
		expect(
			lineHasToken("Adapted from sealed apps/docs/scripts/deploy.ts"),
		).toBe(true);
		expect(lineHasToken("mirrors `sealed gather.ts:25,203-205`")).toBe(true);
		expect(lineHasToken("ported from sealed flake.nix")).toBe(true);
	});
	// Prose carries slashes too, so the space form anchors on a source-file
	// extension rather than on any slash-bearing token.
	test("slash-bearing English prose is not a reference", () => {
		expect(lineHasToken("values sealed and/or rotated")).toBe(false);
		expect(lineHasToken("rows sealed in transit/at rest")).toBe(false);
	});
	test("a hyphenated repo-noun is a reference", () => {
		expect(lineHasToken("a sealed-monorepo domain")).toBe(true);
		expect(lineHasToken("sealed-repo records")).toBe(true);
		expect(lineHasToken('the "sealed-private" annotation')).toBe(true);
	});
	test("its possessive, docsite host, and repo-noun uses are references", () => {
		expect(lineHasToken("following sealed's shape")).toBe(true);
		expect(lineHasToken("deployed to sealed-docs.rigel.build")).toBe(true);
		expect(lineHasToken("this record lives in the sealed design corpus")).toBe(
			true,
		);
		expect(lineHasToken("per sealed convention a later change ADDS")).toBe(
			true,
		);
	});
	// The word is ordinary English throughout go/: a whole-word scan flags 60
	// tracked lines, every one legitimate. These pin the narrowing that keeps
	// the gate usable.
	test("the English engineering word is not a reference", () => {
		expect(lineHasToken("Frame is a sealed sum type")).toBe(false);
		expect(lineHasToken("the row was sealed under a known key")).toBe(false);
		expect(lineHasToken("an egress-sealed agent holds no server token")).toBe(
			false,
		);
		expect(lineHasToken("gochecksumtype for sealed-interface sum types")).toBe(
			false,
		);
	});
	test("genuinely public names carrying the string are not references", () => {
		expect(lineHasToken("github.com/sealedsecurity/compass/go")).toBe(false);
		expect(lineHasToken("Sealed Security Inc → Rigel AI Software Inc")).toBe(
			false,
		);
		expect(lineHasToken("ci.sealedsecurity.com")).toBe(false);
	});
});

describe("isCarveOut", () => {
	test("the gate's own source is carved out", () => {
		expect(isCarveOut("tools/orion-ref-gate/index.ts")).toBe(true);
	});
	test("generated eng-docs copies are carved out", () => {
		expect(
			isCarveOut("apps/eng-docs/src/content/docs/designs/platform/x.md"),
		).toBe(true);
		expect(isCarveOut("apps/eng-docs/dist/designs/platform/x/index.html")).toBe(
			true,
		);
	});
	test("the generated bun.lock is carved out", () => {
		expect(isCarveOut("bun.lock")).toBe(true);
	});
	test("a canonical docs/ record is NOT carved out", () => {
		expect(isCarveOut("docs/designs/platform/compass-drop-proto.md")).toBe(
			false,
		);
	});
});

describe("findViolations", () => {
	test("flags a real reference in a scanned file", () => {
		const v = findViolations([
			"docs/designs/platform/compass-drop-proto.md:138:proven in orion, and its manifest",
		]);
		expect(v).toHaveLength(1);
		expect(v[0]).toEqual({
			file: "docs/designs/platform/compass-drop-proto.md",
			line: 138,
			text: "proven in orion, and its manifest",
		});
	});
	test("does NOT flag a carved-out file even when it carries the token", () => {
		expect(
			findViolations([
				"apps/eng-docs/dist/x/index.html:3:generated copy mentions orion",
				"tools/orion-ref-gate/index.ts:10:the private token orion",
			]),
		).toHaveLength(0);
	});
	test("does NOT flag a line where the token is only a substring", () => {
		expect(
			findViolations(["docs/designs/platform/x.md:5:orionic nebula"]),
		).toHaveLength(0);
	});
	test("parses text containing colons (line/col-like suffixes)", () => {
		const v = findViolations([
			"docs/x.md:42:see `orion ci/pipeline.ts:601-602` for the secret",
		]);
		expect(v).toHaveLength(1);
		const [first] = v;
		expect(first).toEqual({
			file: "docs/x.md",
			line: 42,
			text: "see `orion ci/pipeline.ts:601-602` for the secret",
		});
	});
	test("skips blank and malformed hits", () => {
		expect(
			findViolations(["", "no-colons-here", "path-only:notanumber"]),
		).toHaveLength(0);
	});
	test("collects multiple references", () => {
		const v = findViolations([
			"a.md:1:orion one",
			"b.md:2:orion two",
			"bun.lock:3:orion carved",
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
			deps(["docs/x.md:7:ported from orion"], out, errs),
		);
		expect(code).toBe(1);
		const e = errs.join("\n");
		expect(e).toContain("docs/x.md:7");
		expect(e).toContain("the managed service");
		expect(e).toContain(REMEDIATION_DOC);
	});

	test("the doc cited in the failure hint exists", async () => {
		// Resolved from this file, not the cwd, so the assertion holds under any
		// invocation. Without it the hint can silently die in a docs move —
		// the defect this pointer was repaired for.
		const abs = new URL(`../../${REMEDIATION_DOC}`, import.meta.url);
		expect(await Bun.file(abs).exists()).toBe(true);
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
			deps(["apps/eng-docs/dist/x/index.html:1:orion generated"], out, errs),
		);
		expect(code).toBe(0);
	});
});
