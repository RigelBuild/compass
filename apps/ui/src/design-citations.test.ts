// Source-hygiene gate: no retired-milestone design citations survive in shipped
// UI source. The early Compass milestone records (v0.3 through v0.8) were
// retired (RIG-2453); their still-true rationale was consolidated into
// `docs/designs/meta/compass-architecture-lineage/design.md` and every
// shipped citation re-pointed there.
//
// A vintage provenance tag naming a retired milestone — `design compass-0.6`,
// `design compass-0.8`, etc. — is now (A) a dead link to a deleted record and
// (B) a stale stamp AGENTS.md forbids on live code ("no superseded-design refs
// in new code"). This test gives the sweep teeth so a re-pointed cite can't
// regress and a new one can't creep in.
//
// The forbidden idiom, with a precise boundary:
//   the vintage tag `design compass-0.<n>` for any retired milestone n in 3–8.
//   The word order is the discriminator: "design compass-0.6" (a vintage
//   provenance stamp) is forbidden; a bare path like "compass-0.6/design.md"
//   is not matched here — no such path should survive either, but this gate
//   targets the stamp form that tags live code with a dead vintage. The
//   architecture-lineage record is the one legitimate design citation.

import { describe, expect, test } from "bun:test";
import { Glob } from "bun";

const SRC_DIR = import.meta.dir;

// This gate file itself necessarily contains the forbidden strings (as the
// regex and this documentation), so it is excluded from its own scan.
const SELF = "design-citations.test.ts";

type Violation = { file: string; line: number; text: string; idiom: string };

const FORBIDDEN: Array<{ idiom: string; re: RegExp }> = [
	// The vintage design tag: the word "design" (optionally "design record")
	// immediately followed by a retired milestone `compass-0.<n>`, n in 3–8.
	// Single-line by design: the two tokens sit on one line in every observed
	// case, and keeping it single-line avoids matching a `compass-0.<n>/…`
	// provenance path (tokens reversed, slash-joined).
	{
		idiom: "design compass-0.<n> (retired-milestone vintage tag)",
		re: /design(\s+record)?\s+compass-0\.[3-8]\b/i,
	},
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

describe("design-citation hygiene (no retired-milestone design refs)", () => {
	test("shipped UI source cites no retired v0.3–v0.8 milestone record", async () => {
		const violations = await scanForViolations();
		const report = violations
			.map((v) => `  ${v.file}:${v.line} [${v.idiom}] ${v.text}`)
			.join("\n");
		expect(
			violations,
			violations.length > 0
				? `Retired-milestone design citations found in shipped source — re-point to the architecture-lineage record (docs/designs/meta/compass-architecture-lineage/design.md):\n${report}`
				: "",
		).toEqual([]);
	});

	test("the architecture-lineage citation is NOT a false positive", async () => {
		// A live cite naming the lineage record MUST survive. This pins that the
		// vintage-tag regex does not match the lineage citation form, so the gate
		// never sweeps away the one legitimate design reference.
		const lineageTag = "design: architecture-lineage";
		const vintageRe = /design(\s+record)?\s+compass-0\.[3-8]\b/i;
		expect(vintageRe.test(lineageTag)).toBe(false);
	});
});
