/**
 * Entry point for the generator-stamp gate. Owns all the I/O — walking the gen
 * trees and spawning the plugin — so `stamp-gate.ts` stays a pure decision the
 * tests can drive without a filesystem.
 */
import { readFile } from "node:fs/promises";
import { resolve, sep } from "node:path";
import { Glob } from "bun";
import { assertGeneratorStamp, type CommandRunner, report } from "./stamp-gate";

// Resolve the workspace root.
const WORKSPACE_ROOT = resolve(import.meta.dir, "../..");

// Scan both protoc-gen-es trees and keep this glob aligned with buf outputs.
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
