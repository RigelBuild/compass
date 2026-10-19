import { describe, expect, test } from "bun:test";
import { join } from "node:path";

// The shipped prompt and skill teach the agent its push path; a stale one sends it to a tool it no longer has.
const config = join(import.meta.dir, "..", "..", "..", "config");
const manager = await Bun.file(
	join(config, "prompts/manager/SYSTEM.md"),
).text();
const skill = await Bun.file(join(config, "skills/jj/SKILL.md")).text();
const setup = await Bun.file(
	join(config, "skills/compass-setup/SKILL.md"),
).text();

describe("shipped VCS guidance", () => {
	for (const [name, text] of [
		["manager prompt", manager],
		["jj skill", skill],
	] as const) {
		test(`${name} never names jj-vine or jj-hp`, () => {
			expect(text).not.toContain("jj-vine");
			expect(text).not.toContain("jj-hp");
		});
	}

	test("the manager prompt names the submit tools and the issue rule", () => {
		for (const tool of [
			"vcs_commit",
			"vcs_submit",
			"vcs_retire",
			"vcs_pr_status",
		])
			expect(manager).toContain(tool);
		expect(manager).toContain("forge_list_issues");
		expect(manager).toContain("forge_create_issue");
	});

	test("the jj skill says a pre-push timeout means fixing the hook", () => {
		expect(skill).toContain("vcs_submit");
		expect(skill).toMatch(/pre-push hook timed out/);
	});

	test("the setup skill installs a pre-push hook where a repo has none", () => {
		expect(setup).toContain("pre-push");
		expect(setup).toContain("vcs_submit");
	});
});
