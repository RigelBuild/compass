import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import type { Type } from "@oh-my-pi/omptype/ark";
import {
	vcsCommitParameters,
	vcsStatusParameters,
	vcsSyncParameters,
	vcsWorkspaceParameters,
} from "./vcs";
import {
	vcsPrStatusParameters,
	vcsRetireParameters,
	vcsSubmitParameters,
} from "./vcs-submit";

interface JsonSchema {
	properties?: Record<string, JsonSchema>;
}

const schemas: Record<string, JsonSchema> = Object.fromEntries(
	(
		[
			["vcs_status", vcsStatusParameters],
			["vcs_commit", vcsCommitParameters],
			["vcs_workspace", vcsWorkspaceParameters],
			["vcs_sync", vcsSyncParameters],
			["vcs_submit", vcsSubmitParameters],
			["vcs_retire", vcsRetireParameters],
			["vcs_pr_status", vcsPrStatusParameters],
		] as [string, Type<object>][]
	).map(([name, s]) => [
		name,
		s.toJsonSchema({ fallback: (ctx) => ctx.base }) as JsonSchema,
	]),
);

// Field names in `{ a, b: { c } }` or `{ a: "x", b: <y> }`, each with its nested names.
function fieldsOf(body: string): [string, string[]][] {
	const out: [string, string[]][] = [];
	const re = /([a-z_]+)(?:\s*:\s*(\{[^}]*\}|"[^"]*"|<[^>]*>|[^,}]+))?/g;
	for (const m of body.matchAll(re)) {
		const nested = m[2]?.startsWith("{")
			? [...m[2].slice(1, -1).matchAll(/([a-z_]+)/g)].map((n) => n[1] ?? "")
			: [];
		out.push([m[1] ?? "", nested]);
	}
	return out;
}

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
		test(`${name} never sends the agent to jj-vine or jj-hp`, () => {
			expect(text).not.toContain("jj-vine submit");
			expect(text).not.toContain("jj-vine status");
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

	// The timeout fails open (the record's OQ-9); guidance that says it stops the submit would stall every agent on a slow hook.
	test("the jj skill says a pre-push timeout lets the submit go on and the hook gets fixed", () => {
		expect(skill).toMatch(/after 120 s is stopped and the submit\s+goes on/);
		expect(skill).toMatch(/fix it in a change of its own/);
		expect(manager).toMatch(/fix\s+the hook in a change of its own/);
	});

	test("the setup skill installs a pre-push hook where a repo has none", () => {
		expect(setup).toContain("pre-push");
		expect(setup).toContain("vcs_submit");
	});

	// The doc's call shapes are what the agent copies; a renamed or invented field makes every such call fail.
	test("every documented vcs_* call uses the tool's real fields", () => {
		const calls = [
			...`${manager}\n${skill}`.matchAll(
				/\b(vcs_[a-z_]+) \{ ((?:[^{}]|\{[^{}]*\})*) \}/g,
			),
		];
		expect(calls.length).toBeGreaterThan(10);
		for (const [, tool = "", body = ""] of calls) {
			const props = schemas[tool]?.properties;
			expect(props, tool).toBeDefined();
			for (const [field, nested] of fieldsOf(body)) {
				expect(Object.keys(props ?? {}), `${tool}.${field}`).toContain(field);
				for (const inner of nested)
					expect(
						Object.keys(props?.[field]?.properties ?? {}),
						`${tool}.${field}.${inner}`,
					).toContain(inner);
			}
		}
	});

	test("the older-PR move copies the line instead of renaming in place", () => {
		const move = skill.slice(skill.indexOf("## Moving an older PR"));
		expect(move).toContain("jj duplicate");
		expect(move).toContain("jj bookmark forget");
		expect(move).not.toContain("bookmark rename");
	});
});
