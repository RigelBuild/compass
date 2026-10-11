// Every input of the image build must also schedule this gate. The project
// graph never reaches it from another tree (only task inputs do), so this
// asks real moon which tasks a one-file change affects.

import { describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const GATE = "agent-image-env-gate:check";
const BUILD = "compass-agent-image:build";

interface MoonInput {
	readonly file?: string;
	readonly glob?: string;
}

function moon(args: string[], stdin?: string): string {
	const res = Bun.spawnSync(["moon", ...args], {
		cwd: repoRoot,
		stdin: stdin === undefined ? "ignore" : Buffer.from(stdin),
		stdout: "pipe",
		stderr: "pipe",
	});
	if (res.exitCode !== 0) {
		throw new Error(
			`moon ${args.join(" ")} exited ${res.exitCode}: ${res.stderr.toString()}`,
		);
	}
	return res.stdout.toString();
}

/** Targets moon reports affected when exactly `file` changed. */
function affectedTargets(file: string): string[] {
	const out = JSON.parse(moon(["query", "affected"], `${file}\n`)) as {
		projects?: Record<string, { tasks?: string[] | null }>;
	};
	if (out.projects == null) {
		throw new Error(`moon query affected: no projects key for ${file}`);
	}
	return Object.values(out.projects).flatMap((p) => p.tasks ?? []);
}

/** One existing repo file per build input, so each input is probed for real. */
function sampleFiles(inputs: readonly MoonInput[]): string[] {
	return inputs.map((input) => {
		if (input.file !== undefined) {
			const rel = input.file.replace(/^\//, "");
			if (!existsSync(join(repoRoot, rel))) {
				throw new Error(`build input ${input.file} does not exist`);
			}
			return rel;
		}
		const pattern = (input.glob ?? "").replace(/^\//, "");
		const projectRelative = !input.glob?.startsWith("/");
		const glob = projectRelative ? `agent-image/${pattern}` : pattern;
		for (const hit of new Bun.Glob(glob).scanSync({
			cwd: repoRoot,
			dot: true,
		})) {
			if (!hit.includes("node_modules/")) return hit;
		}
		throw new Error(`build input glob ${input.glob} matches no file`);
	});
}

const buildInputs = (
	JSON.parse(moon(["task", BUILD, "--json"])) as { inputs: MoonInput[] }
).inputs;

describe("agent-image-env-gate is scheduled with the image build", () => {
	for (const file of sampleFiles(buildInputs)) {
		test(`a change to ${file} schedules ${GATE}`, () => {
			const targets = affectedTargets(file);
			// Control: the build must be affected too, or the probe proves nothing.
			expect(targets).toContain(BUILD);
			expect(targets).toContain(GATE);
		});
	}

	test("a change outside the image closure schedules neither", () => {
		const targets = affectedTargets("go/cmd/compass/main.go");
		expect(targets).not.toContain(BUILD);
		expect(targets).not.toContain(GATE);
	});
});
