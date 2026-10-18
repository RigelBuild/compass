// Reject retired design milestone references in shipped UI source.

import { describe, expect, test } from "bun:test";
import { Glob } from "bun";

const SRC_DIR = import.meta.dir;

// Exclude this gate because it contains its own forbidden strings.
const SELF = "design-citations.test.ts";

type Violation = { file: string; line: number; text: string; idiom: string };

const FORBIDDEN: Array<{ idiom: string; re: RegExp }> = [
	// Match the retired design tag only when its words share a line.
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
				? `Retired-milestone design citations found in shipped source — re-point to the architecture-lineage record (RigelBuild/docs:docs/designs/compass/meta/compass-architecture-lineage/design.md):\n${report}`
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
