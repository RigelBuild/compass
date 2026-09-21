import { constants } from "node:fs";
import { access, readdir, stat } from "node:fs/promises";
import { join, resolve } from "node:path";
import { assertCliArtifact } from "./cli-check";

const WORKSPACE_ROOT = resolve(import.meta.dir, "../..");
const SOURCE_ROOT = join(WORKSPACE_ROOT, "go/cmd/compass");
// Unset means this was not run from a devenv task; an empty prefix would make
// the path relative and misreport a config error as a build that never ran.
const stateDir = process.env.DEVENV_STATE;
if (!stateDir) {
	console.error(
		"DEVENV_STATE is unset: run this through `devenv tasks run dogfood:check-cli`, not directly",
	);
	process.exit(2);
}
const BINARY = join(stateDir, "compass/compass");

async function newestSourceMtime(path: string): Promise<number> {
	const entries = await readdir(path, { withFileTypes: true });
	let newest = 0;
	for (const entry of entries) {
		const entryPath = join(path, entry.name);
		if (entry.isDirectory()) {
			newest = Math.max(newest, await newestSourceMtime(entryPath));
		} else if (entry.isFile() && entry.name.endsWith(".go")) {
			newest = Math.max(newest, (await stat(entryPath)).mtimeMs);
		}
	}
	return newest;
}

async function main(): Promise<number> {
	const binaryStat = await stat(BINARY).catch(() => null);
	const binaryExists = binaryStat !== null;
	const binaryExecutable = binaryExists
		? await access(BINARY, constants.X_OK).then(
				() => true,
				() => false,
			)
		: false;
	const runExitCode =
		binaryExists && binaryExecutable
			? await Bun.spawn([BINARY, "--help"], {
					stdout: "ignore",
					stderr: "ignore",
				}).exited
			: null;
	const result = assertCliArtifact({
		binaryExists,
		binaryExecutable,
		binaryMtimeMs: binaryStat?.mtimeMs ?? null,
		newestSourceMtimeMs: await newestSourceMtime(SOURCE_ROOT),
		runExitCode,
	});
	if (!result.ok) {
		console.error(result.message);
		return 1;
	}
	console.log(
		"operator CLI artifact is present, current, executable, and runs",
	);
	return 0;
}

process.exit(await main());
