// The mutation verifier's own failure-detector, tested against REAL bun output
// in both states. The first version of `suiteFailed` matched the word "fail"
// and so returned true for ` 0 fail` — it reported red on a fully green suite
// and would have reported red on a mutated one too. A detector that answers
// the same in both states cannot detect anything, which is exactly the vacuity
// the verifier exists to catch. So it gets pinned here.

import { describe, expect, test } from "bun:test";
import {
	classify,
	isFailing,
	MUTANTS,
	mutate,
	suiteFailed,
} from "./verify-abort-mutants";

/** Verbatim tail of a green `bun test` run on this package. */
const GREEN = `
src/transport/control-source.test.ts:
[compass-agent] control unmapped: control:steer — payload staged (SEA-1310)

 13 pass
 0 fail
 45 expect() calls
Ran 13 tests across 1 file. [3.68s]
`;

/** Verbatim shape of a run with one failing test. */
const RED = `
src/transport/control-source.test.ts:
(fail) a return()'d source does not re-open the subscription [12.00ms]

 12 pass
 1 fail
 44 expect() calls
Ran 13 tests across 1 file. [3.91s]
`;

describe("suiteFailed", () => {
	test("a green run reporting ` 0 fail` is NOT a failure", () => {
		// The original bug: `0 fail` contains "fail", so a word match said red.
		expect(suiteFailed(GREEN)).toBe(false);
	});

	test("a run with one failing test IS a failure", () => {
		expect(suiteFailed(RED)).toBe(true);
	});

	test("green and red are distinguished — the detector actually discriminates", () => {
		// The property that matters: not that each case is right in isolation,
		// but that the two answers DIFFER. This is the assertion that would have
		// caught the original bug, where both returned true.
		expect(suiteFailed(GREEN)).not.toBe(suiteFailed(RED));
	});

	test("a crashed run with no summary line counts as failure", () => {
		// No evidence of success is not evidence of success — a run that died
		// before printing a summary must never be read as green.
		expect(suiteFailed("error: Cannot find module './control-source'")).toBe(
			true,
		);
		expect(suiteFailed("")).toBe(true);
	});

	test("a multi-digit failure count is read as failure", () => {
		expect(suiteFailed(" 0 pass\n 13 fail\n")).toBe(true);
	});
});

describe("mutate", () => {
	test("every mutant's target occurs exactly once in its own real source", async () => {
		// Guards the verifier against silently verifying nothing: if a refactor
		// moves or reshapes a branch, the find string stops matching and this
		// fails loudly, instead of the mutant becoming a no-op that always
		// "passes" because it never changed the source.
		//
		// Reads each mutant's OWN file — the table spans the Control source and
		// the ack cursor beside it — so a mutant pointed at the wrong file fails
		// here rather than reporting a phantom survivor at run time.
		const defaultSrc = new URL(
			"../src/transport/control-source.ts",
			import.meta.url,
		).pathname;
		const texts = new Map<string, string>();
		for (const m of MUTANTS) {
			const file = m.src ?? defaultSrc;
			let src = texts.get(file);
			if (src === undefined) {
				src = await Bun.file(file).text();
				texts.set(file, src);
			}
			expect(() => mutate(src as string, m), m.name).not.toThrow();
			expect(mutate(src as string, m), m.name).not.toBe(src);
		}
	});

	test("a target that is absent throws rather than no-op'ing", () => {
		expect(() =>
			mutate("unrelated source", {
				name: "phantom",
				find: "nonexistent",
				replace: "",
				expect: "",
			}),
		).toThrow(/expected exactly 1 occurrence/);
	});

	test("a target occurring twice throws — ambiguous mutation", () => {
		expect(() =>
			mutate("dup\ndup", {
				name: "ambiguous",
				find: "dup",
				replace: "",
				expect: "",
			}),
		).toThrow(/found 2/);
	});
});

describe("classify", () => {
	const required = {
		name: "b",
		find: "f",
		replace: "r",
		expect: "must be covered",
	} as const;
	const untestable = { ...required, expectedSurvivor: "masked by X" } as const;

	test("a required branch that dies is killed", () => {
		expect(classify(required, true)).toEqual({ kind: "killed", name: "b" });
	});

	test("a required branch that survives is a coverage gap", () => {
		expect(classify(required, false)).toEqual({
			kind: "gap",
			name: "b",
			expect: "must be covered",
		});
	});

	test("an UNTESTABLE branch that survives is the healthy state", () => {
		expect(classify(untestable, false)).toEqual({
			kind: "known-survivor",
			name: "b",
		});
	});

	test("an UNTESTABLE branch that DIES means the entry is stale", () => {
		expect(classify(untestable, true)).toEqual({
			kind: "stale-entry",
			name: "b",
		});
	});

	test("the same outcome classifies differently by expectation — it discriminates", () => {
		expect(classify(required, false).kind).not.toBe(
			classify(untestable, false).kind,
		);
		expect(classify(required, true).kind).not.toBe(
			classify(untestable, true).kind,
		);
	});
});

describe("isFailing", () => {
	test("a documented survivor alone does NOT fail the run", () => {
		expect(isFailing([{ kind: "known-survivor", name: "b" }])).toBe(false);
	});

	test("an all-killed run does not fail", () => {
		expect(isFailing([{ kind: "killed", name: "b" }])).toBe(false);
	});

	test("a real gap fails the run", () => {
		expect(isFailing([{ kind: "gap", name: "b", expect: "x" }])).toBe(true);
	});

	test("a stale UNTESTABLE entry fails the run too", () => {
		expect(isFailing([{ kind: "stale-entry", name: "b" }])).toBe(true);
	});

	test("an empty run does not fail", () => {
		expect(isFailing([])).toBe(false);
	});
});

describe("the MUTANTS table's recorded expectations", () => {
	test("the two UNTESTABLE branches are the abort guards masked by return()'s interrupt", () => {
		// Before the T4 Effect migration only the catch-side guard was masked (by
		// the top-of-loop guard). Once return() interrupts the pump fiber in
		// addition to aborting, the top-of-loop guard is masked too — the interrupt
		// prevents the re-open whether or not the guard is present, and no public
		// seam fires the abort without also interrupting. See each entry's
		// expectedSurvivor for the measured reasoning.
		const marked = MUTANTS.filter((m) => m.expectedSurvivor !== undefined);
		expect(marked.map((m) => m.name)).toEqual([
			"top-of-loop abort guard",
			"catch-side abort guard",
		]);
	});

	test("every UNTESTABLE mark carries its reasoning, not a bare flag", () => {
		for (const m of MUTANTS.filter((x) => x.expectedSurvivor !== undefined)) {
			expect(m.expectedSurvivor?.length ?? 0).toBeGreaterThan(80);
		}
	});
});
