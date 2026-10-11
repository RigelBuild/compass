// Every input of the image build must also schedule this gate. The project
// graph never reaches it from another tree (only task inputs do), so this
// asks real moon, with the query ci-matrix runs, what a one-file change affects.

import { describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { dirname, extname, join } from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const BUILD = "compass-agent-image:build";
const GATE = "agent-image-env-gate:check";
// Exists, and sits outside the image closure.
const UNRELATED = "go/go.mod";

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

/** The `project:task` targets a change to exactly `file` affects, per ci-matrix's query. */
function affected(file: string): Set<string> {
	const out = JSON.parse(
		moon(["query", "tasks", "--affected"], `${file}\n`),
	) as { tasks?: Record<string, Record<string, unknown>> | null };
	if (out.tasks == null) {
		throw new Error(`moon query tasks: no tasks key for ${file}`);
	}
	return new Set(
		Object.entries(out.tasks).flatMap(([project, tasks]) =>
			Object.keys(tasks).map((task) => `${project}:${task}`),
		),
	);
}

/**
 * Repo files covering every build input: each file input, and for each glob
 * one file per extension, so an input narrowed to some file types is caught.
 */
function sampleFiles(inputs: readonly MoonInput[]): string[] {
	const samples = new Set<string>();
	for (const input of inputs) {
		if (input.file !== undefined) {
			const rel = input.file.replace(/^\//, "");
			if (!existsSync(join(repoRoot, rel))) {
				throw new Error(`build input ${input.file} does not exist`);
			}
			samples.add(rel);
			continue;
		}
		const raw = input.glob ?? "";
		const glob = raw.startsWith("/") ? raw.slice(1) : `agent-image/${raw}`;
		const byExt = new Map<string, string>();
		const hits = [...new Bun.Glob(glob).scanSync({ cwd: repoRoot, dot: true })]
			.filter((hit) => !hit.includes("node_modules/"))
			.sort();
		for (const hit of hits) {
			if (!byExt.has(extname(hit))) byExt.set(extname(hit), hit);
		}
		if (byExt.size === 0) {
			throw new Error(`build input glob ${raw} matches no file`);
		}
		for (const hit of byExt.values()) samples.add(hit);
	}
	return [...samples].sort();
}

// Each probe spawns moon (~0.2s, slower under the pre-push battery).
const PROBE_TIMEOUT_MS = 60_000;

describe("agent-image-env-gate is scheduled with the image build", () => {
	test(
		"every image-build input file also affects the gate",
		() => {
			const { inputs } = JSON.parse(moon(["task", BUILD, "--json"])) as {
				inputs: MoonInput[];
			};
			const missed: string[] = [];
			for (const file of sampleFiles(inputs)) {
				const targets = affected(file);
				// Control: the build must be affected, or the probe proves nothing.
				expect(`${file} -> ${targets.has(BUILD)}`).toBe(`${file} -> true`);
				if (!targets.has(GATE)) missed.push(file);
			}
			expect(missed).toEqual([]);
		},
		PROBE_TIMEOUT_MS,
	);

	test(
		"a change outside the image closure affects neither",
		() => {
			expect(existsSync(join(repoRoot, UNRELATED))).toBe(true);
			const targets = affected(UNRELATED);
			expect(targets.has(BUILD)).toBe(false);
			expect(targets.has(GATE)).toBe(false);
		},
		PROBE_TIMEOUT_MS,
	);
});
