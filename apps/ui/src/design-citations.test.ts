// Source-hygiene gate: no superseded compass-0.6 / channel-first design
// citations survive in shipped UI source (design record
// `docs/designs/product/compass-0.7-channel-workspace/design.md`).
//
// The 0.7 reshape moved the shell from the compass-0.6 "channel-first / channel
// as the primary surface" prototype (§T7) BACK to a board-primary shell with
// comms folded into the workspace (record §1-59). Comments, section headers, and
// fixture prose that still cite the superseded model either (A) document the
// OPPOSITE of what ships — actively misleading the next maintainer on the core
// architecture — or (B) tag new code with the stale compass-0.6 vintage, which
// AGENTS.md forbids ("no superseded-design refs in new code"). A one-off grep in
// a PR body caught the symbol names but missed the strings, and 17 refs shipped;
// this test gives the sweep teeth so it can't regress.
//
// Two forbidden idioms, each with a precise boundary:
//   1. `channel-first` / `channel-primary` — the superseded architecture names.
//      Zero legitimate uses: the shell is board-primary; a channel is a surface
//      WITHIN it, never "channel-primary".
//   2. the vintage tag `design compass-0.6` — a stale provenance stamp on new
//      code. This deliberately does NOT match the one LEGITIMATE compass-0.6
//      reference: `stub-data.ts`'s RT-2 provenance pointer
//      `../compass-0.6/design.md:1760-1764` (the 0.7 record itself cites 0.6 for
//      RT-2, record §118,330 — ratified in 0.6, carried forward, not
//      re-ratified). The word order is the discriminator: "design compass-0.6"
//      (vintage tag) vs "compass-0.6/design.md" (a path to the record).

import { describe, expect, test } from "bun:test";
import { Glob } from "bun";

const SRC_DIR = import.meta.dir;

// This gate file itself necessarily contains the forbidden strings (as the
// regexes and this documentation), so it is excluded from its own scan.
const SELF = "design-citations.test.ts";

type Violation = { file: string; line: number; text: string; idiom: string };

const FORBIDDEN: Array<{ idiom: string; re: RegExp }> = [
	// The superseded architecture names. Hyphenated, case-insensitive so a
	// sentence-initial "Channel-first" in fixture prose is caught too.
	{ idiom: "channel-first / channel-primary", re: /channel-(first|primary)/i },
	// The vintage design tag: the word "design" (or "design record") immediately
	// followed by "compass-0.6". `[\s\S]` is not used — the two tokens sit on one
	// line in every observed case, and keeping it single-line avoids matching the
	// `compass-0.6/design.md` provenance path (which has the tokens reversed and
	// slash-joined).
	{ idiom: "design compass-0.6 (vintage tag)", re: /design\s+compass-0\.6\b/i },
];

async function scanForViolations(): Promise<Violation[]> {
	const glob = new Glob("**/*.{ts,tsx}");
	const violations: Violation[] = [];
	for await (const rel of glob.scan({ cwd: SRC_DIR, onlyFiles: true })) {
		if (rel.endsWith(SELF)) continue;
		const text = await Bun.file(`${SRC_DIR}/${rel}`).text();
		const lines = text.split("\n");
		lines.forEach((lineText, i) => {
			for (const { idiom, re } of FORBIDDEN) {
				if (re.test(lineText)) {
					violations.push({
						file: rel,
						line: i + 1,
						text: lineText.trim(),
						idiom,
					});
				}
			}
		});
	}
	return violations;
}

describe("design-citation hygiene (no superseded compass-0.6 / channel-first refs)", () => {
	test("shipped UI source cites only compass-0.7 (or the RT-2 provenance path)", async () => {
		const violations = await scanForViolations();
		const report = violations
			.map((v) => `  ${v.file}:${v.line} [${v.idiom}] ${v.text}`)
			.join("\n");
		expect(
			violations,
			violations.length > 0
				? `Superseded design citations found in shipped source — reconcile to board-primary / compass-0.7:\n${report}`
				: "",
		).toEqual([]);
	});

	test("the legitimate RT-2 provenance pointer is NOT a false positive", async () => {
		// stub-data.ts's `../compass-0.6/design.md:1760-1764` is the true RT-2
		// provenance and MUST survive. This pins that the vintage-tag regex does
		// not match the path form, so a future tightening can't sweep it away.
		const provenancePath = "../compass-0.6/design.md:1760-1764";
		const vintageRe = /design\s+compass-0\.6\b/i;
		expect(vintageRe.test(provenancePath)).toBe(false);
	});
});
