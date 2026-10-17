// Pins the matching semantics of the checked-in boundary configs; the shared
// package only validates their shape.
import { describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import {
	findViolations,
	loadRefGateConfig,
	type RefGateConfig,
} from "@rigelbuild/ref-gate";

const here = import.meta.dir;
const repoRoot = join(here, "../..");
const privateName = loadRefGateConfig(join(here, "private-name.json"));
const sea = loadRefGateConfig(join(here, "sea.json"));

const hit = (file: string, text: string): string => `${file}\0${1}\0${text}`;
const flags = (cfg: RefGateConfig, text: string, file = "docs/x.md"): boolean =>
	findViolations(cfg, [hit(file, text)]).length > 0;

describe("private-name config", () => {
	test.each([
		"ported from orion's config",
		"the ORION monorepo",
		"`orion tools/publish-agent-config/index.ts:79`",
		"orion-ref-gate scans for orion references",
		"See `sealed/ci/pipeline.ts:554` for the job",
		"mirrors `sealed gather.ts:25,203-205`",
		"ported from sealed flake.nix",
		"a sealed-monorepo domain",
		"deployed to sealed-docs.rigel.build",
		"following sealed's shape",
		"this record lives in the sealed design corpus",
		"per sealed convention a later change ADDS",
	])("flags %p", (text) => {
		expect(flags(privateName, text)).toBe(true);
	});

	test.each([
		"orionic nebula naming",
		"categorion",
		"orion-ref-gate: 'tools/orion-ref-gate'",
		"values sealed and/or rotated",
		"Frame is a sealed sum type",
		"an egress-sealed agent holds no server token",
		"github.com/sealedsecurity/compass/go",
		"ci.sealedsecurity.com",
	])("does not flag %p", (text) => {
		expect(flags(privateName, text)).toBe(false);
	});

	test.each([
		"bun.lock",
		"tools/ref-gates/private-name.json",
		"apps/eng-docs/src/content/docs/x.md",
		"apps/eng-docs/dist/x/index.html",
	])("carves out %p", (file) => {
		expect(flags(privateName, "from orion", file)).toBe(false);
	});

	test("its remediation doc exists", () => {
		const doc = privateName.remediationDoc;
		expect(doc).toBeDefined();
		expect(existsSync(join(repoRoot, doc ?? ""))).toBe(true);
	});
});

describe("sea config", () => {
	test.each(["tracked as SEA-1512 here", "(SEA-42)"])("flags %p", (text) => {
		expect(flags(sea, text)).toBe(true);
	});

	test.each([
		"branch sea-1512-fix",
		"a SEA-nnn placeholder",
		"SEA-NNN",
		"XSEA-12",
	])("does not flag %p", (text) => {
		expect(flags(sea, text)).toBe(false);
	});

	test.each(["bun.lock", "CHANGELOG.md", "tools/ref-gates/sea.json"])(
		"carves out %p",
		(file) => {
			expect(flags(sea, "SEA-1512", file)).toBe(false);
		},
	);
});

// Moon reports these tasks affected only through workspace-root inputs.
test("both gate tasks take every workspace file as input", () => {
	const moon = readFileSync(join(here, "moon.yml"), "utf8");
	for (const task of ["sea", "private-name"]) {
		const block = new RegExp(
			`\\n  ${task}:\\n[\\s\\S]*?inputs: \\[([^\\]]*)\\]`,
		).exec(moon);
		expect(block?.[1]).toContain("'/**/*'");
	}
});
