import { constants } from "node:fs";
import { access, stat } from "node:fs/promises";
import { join, resolve } from "node:path";
import { assertCliArtifact, type CliRunResult } from "./cli-check";

const WORKSPACE_ROOT = resolve(import.meta.dir, "../..");
const GO_ROOT = join(WORKSPACE_ROOT, "go");
// The toolchain's dependency graph is the source of truth for what `go build` links.
const SOURCE_GLOBS = ["./cmd/compass"];
const HELP_TIMEOUT_MS = 5_000;
const stateDir = process.env.DEVENV_STATE;
if (!stateDir) {
	console.error(
		"DEVENV_STATE is unset: run this through `devenv tasks run dogfood:check-cli`, not directly",
	);
	process.exit(2);
}
const BINARY = join(stateDir, "compass/compass");

interface SourceScan {
	readonly newest: number | null;
	readonly error: string | null;
}

/** Ask the toolchain which directories `go build` actually links. */
async function linkedSourceDirs(): Promise<{
	readonly dirs: readonly string[] | null;
	readonly error: string | null;
}> {
	// Name the pipe shape so `stdout`/`stderr` stay readable streams: a bare
	// `Bun.Subprocess` widens them to a union `Response` cannot take.
	let list: Bun.Subprocess<"ignore", "pipe", "pipe">;
	try {
		list = Bun.spawn(
			["go", "list", "-deps", "-f", "{{.Dir}}", ...SOURCE_GLOBS],
			{ cwd: GO_ROOT, stdout: "pipe", stderr: "pipe" },
		);
	} catch {
		return { dirs: null, error: "go list could not be started" };
	}
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(list.stdout).text(),
		new Response(list.stderr).text(),
		list.exited,
	]);
	if (exitCode !== 0) {
		return {
			dirs: null,
			error: `go list failed (exit ${exitCode}): ${stderr.trim()}`,
		};
	}
	// Stdlib and module-cache deps are immutable store paths; go.mod/go.sum are
	// checked separately, so only in-repo sources can go stale here.
	const dirs = stdout
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line.startsWith(`${WORKSPACE_ROOT}/`));
	return dirs.length === 0
		? { dirs: null, error: "go list returned no in-repo source directories" }
		: { dirs, error: null };
}

async function newestSourceMtime(): Promise<SourceScan> {
	const { dirs, error } = await linkedSourceDirs();
	if (dirs === null) return { newest: null, error };
	let newest = 0;
	let files = 0;
	for (const directory of dirs) {
		const glob = new Bun.Glob("*.go");
		for await (const file of glob.scan({ cwd: directory, absolute: true })) {
			if (file.endsWith("_test.go")) continue;
			files += 1;
			newest = Math.max(newest, (await stat(file)).mtimeMs);
		}
	}
	if (files === 0) {
		return { newest: null, error: "linked source set has no buildable files" };
	}
	for (const input of [join(GO_ROOT, "go.mod"), join(GO_ROOT, "go.sum")]) {
		try {
			newest = Math.max(newest, (await stat(input)).mtimeMs);
		} catch {
			return { newest: null, error: `module input is missing: ${input}` };
		}
	}
	return { newest, error: null };
}

async function runHelp(): Promise<CliRunResult> {
	let child: Bun.Subprocess;
	try {
		child = Bun.spawn([BINARY, "--help"], {
			stdout: "ignore",
			stderr: "ignore",
		});
	} catch {
		return { kind: "spawn-failure" };
	}
	// A real abort timer, not a sleep: this task gates `devenv up`, so a CLI
	// that hangs on --help must fail fast instead of wedging local bring-up.
	let timer: Timer | undefined;
	const timeout = new Promise<"timeout">((resolveTimeout) => {
		// biome-ignore lint/style/noRestrictedGlobals: abort timeout, not a fixed sleep
		timer = setTimeout(() => resolveTimeout("timeout"), HELP_TIMEOUT_MS);
	});
	try {
		const outcome = await Promise.race([child.exited, timeout]);
		if (outcome === "timeout") {
			child.kill();
			return { kind: "timeout" };
		}
		return { kind: "exit", code: outcome };
	} finally {
		// Otherwise the pending timer holds the event loop open past a fast exit.
		clearTimeout(timer);
	}
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
	const run = binaryExists && binaryExecutable ? await runHelp() : null;
	const source = await newestSourceMtime();
	const result = assertCliArtifact({
		binaryExists,
		binaryExecutable,
		binaryMtimeMs: binaryStat?.mtimeMs ?? null,
		newestSourceMtimeMs: source.newest,
		sourceError: source.error,
		run,
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
