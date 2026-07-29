/**
 * Entry point for the generator-stamp gate. Owns all the I/O — walking the gen
 * trees and spawning the plugin — so `stamp-gate.ts` stays a pure decision the
 * tests can drive without a filesystem.
 */
import { readFile } from "node:fs/promises";
import { resolve, sep } from "node:path";
import { Glob } from "bun";
import { assertGeneratorStamp, type CommandRunner, report } from "./stamp-gate";

// This file is tools/stamp-gate/index.ts, so `../..` is the workspace root.
const WORKSPACE_ROOT = resolve(import.meta.dir, "../..");

// Both TS gen trees: the public client and the agent's internal one. Both are
// produced by protoc-gen-es and both carry its stamp, so both must agree — a
// glob covering only one would miss exactly the partial-regeneration case this
// gate exists to name.
//
// INVARIANT: every protoc-gen-es out-dir lives under packages/*/src/gen. This
// glob must stay in sync with the `out:` paths in buf.gen*.yaml — a future gen
// tree under another workspace root (apps/*, tools/*) would be silently invisible
// to the gate (a non-empty-but-incomplete source set the fail-closed empty check
// cannot catch), so widen this glob whenever a buf template targets a new root.
const GEN_GLOB = "packages/*/src/gen/**/*_pb.ts";

const runner: CommandRunner = async (argv) => {
	const proc = Bun.spawn(argv, { stdout: "pipe", stderr: "pipe" });
	const [stdout, stderr, code] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
		proc.exited,
	]);
	return { code, stdout, stderr };
};

async function main(): Promise<number> {
	const sources: Array<{ path: string; text: string }> = [];
	for await (const match of new Glob(GEN_GLOB).scan({ cwd: WORKSPACE_ROOT })) {
		const path = match.split(sep).join("/");
		sources.push({
			path,
			text: await readFile(resolve(WORKSPACE_ROOT, match), "utf8"),
		});
	}
	// Sorted so the two paths named in a disagreement are stable across runs:
	// a gate that names a different pair each time reads as flaky.
	sources.sort((a, b) => a.path.localeCompare(b.path));

	const result = await assertGeneratorStamp(runner, sources);
	return report(result);
}

process.exit(await main());
