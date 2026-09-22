import { constants } from "node:fs";
import { access, lstat, stat } from "node:fs/promises";
import { join, resolve } from "node:path";
import {
	assertCliArtifact,
	type CliRunResult,
	FIELD_SEP,
	parseListedInputs,
} from "./cli-check";

const WORKSPACE_ROOT = resolve(import.meta.dir, "../..");
const GO_ROOT = join(WORKSPACE_ROOT, "go");
// The toolchain's own input list is the source of truth for what `go build`
// compiles: globbing *.go misses go:embed assets and includes build-tag files.
const SOURCE_GLOBS = ["./cmd/compass"];
// `go list` templates do not interpret \t, so separate fields with a literal
// token no path or filename can contain. Every kind `go build` compiles or
// links is listed: the repo has only Go and embeds today, but a future .s or
// .syso must not slip past a gate that claims the exact build closure.
const INPUT_FIELDS = [
	"GoFiles",
	"CgoFiles",
	"EmbedFiles",
	"CFiles",
	"CXXFiles",
	"MFiles",
	"FFiles",
	"SFiles",
	"SysoFiles",
	"HFiles",
	"SwigFiles",
	"SwigCXXFiles",
] as const;
const LIST_FORMAT = `{{.Dir}}${INPUT_FIELDS.map(
	(field) => `${FIELD_SEP}{{range .${field}}}{{.}} {{end}}`,
).join("")}`;
const LIST_TIMEOUT_MS = 30_000;
const HELP_TIMEOUT_MS = 5_000;
const KILL_GRACE_MS = 2_000;
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

/** Ask the toolchain for the exact files `go build` reads, embeds included. */
async function linkedInputs(): Promise<{
	readonly files: readonly string[] | null;
	readonly error: string | null;
}> {
	// Name the pipe shape so `stdout`/`stderr` stay readable streams: a bare
	// `Bun.Subprocess` widens them to a union `Response` cannot take.
	let list: Bun.Subprocess<"ignore", "pipe", "pipe">;
	try {
		list = Bun.spawn(
			["go", "list", "-deps", "-f", LIST_FORMAT, ...SOURCE_GLOBS],
			{
				cwd: GO_ROOT,
				stdout: "pipe",
				stderr: "pipe",
			},
		);
	} catch {
		return { files: null, error: "go list could not be started" };
	}
	const settled = await withTimeout(list, LIST_TIMEOUT_MS);
	if (settled === "timeout") {
		return {
			files: null,
			error: "go list timed out resolving the build closure",
		};
	}
	const [stdout, stderr] = await Promise.all([
		new Response(list.stdout).text(),
		new Response(list.stderr).text(),
	]);
	if (settled !== 0) {
		return {
			files: null,
			error: `go list failed (exit ${settled}): ${stderr.trim()}`,
		};
	}
	const files = parseListedInputs(stdout, WORKSPACE_ROOT);
	return files.length === 0
		? { files: null, error: "go list returned no in-repo build inputs" }
		: { files, error: null };
}

async function newestSourceMtime(): Promise<SourceScan> {
	const { files, error } = await linkedInputs();
	if (files === null) return { newest: null, error };
	let newest = 0;
	for (const file of [
		...files,
		join(GO_ROOT, "go.mod"),
		join(GO_ROOT, "go.sum"),
	]) {
		try {
			newest = Math.max(newest, (await stat(file)).mtimeMs);
		} catch {
			return { newest: null, error: `build input is unreadable: ${file}` };
		}
	}
	return { newest, error: null };
}

/**
 * Await a child's exit, or "timeout". Kills and reaps on expiry so no child
 * outlives this process — both probes gate `devenv up`, so neither may hang.
 */
async function withTimeout(
	child: Bun.Subprocess<never, "pipe", "pipe"> | Bun.Subprocess,
	limitMs: number,
): Promise<number | "timeout"> {
	let timer: Timer | undefined;
	const expiry = new Promise<"timeout">((resolveExpiry) => {
		// biome-ignore lint/style/noRestrictedGlobals: abort timeout, not a fixed sleep
		timer = setTimeout(() => resolveExpiry("timeout"), limitMs);
	});
	try {
		const outcome = await Promise.race([child.exited, expiry]);
		if (outcome !== "timeout") return outcome;
		child.kill();
		const escalation = new Promise<"escalate">((resolveEscalation) => {
			// biome-ignore lint/style/noRestrictedGlobals: grace before SIGKILL
			setTimeout(() => resolveEscalation("escalate"), KILL_GRACE_MS).unref();
		});
		if ((await Promise.race([child.exited, escalation])) === "escalate") {
			child.kill("SIGKILL");
			await child.exited;
		}
		return "timeout";
	} finally {
		// Otherwise the pending timer holds the event loop open past a fast exit.
		clearTimeout(timer);
	}
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
	const settled = await withTimeout(child, HELP_TIMEOUT_MS);
	return settled === "timeout"
		? { kind: "timeout" }
		: { kind: "exit", code: settled };
}

function report(result: { ok: boolean; message?: string }): number {
	if (result.ok) {
		console.log(
			"operator CLI artifact is present, current, executable, and runs",
		);
		return 0;
	}
	console.error(result.message);
	return 1;
}

async function main(): Promise<number> {
	// lstat, not stat: the gate must judge the artifact build-cli wrote, so a
	// symlink standing in for it is not the subject and never passes.
	const binaryStat = await lstat(BINARY).catch(() => null);
	const binaryExists = binaryStat?.isFile() === true;
	const binaryExecutable = binaryExists
		? await access(BINARY, constants.X_OK).then(
				() => true,
				() => false,
			)
		: false;
	const base = {
		binaryExists,
		binaryExecutable,
		binaryMtimeMs: binaryStat?.mtimeMs ?? null,
	};
	// An absent or non-executable artifact is already decided, so don't pay for
	// (or hang on) `go list` and a spawn just to say so.
	if (!binaryExists || !binaryExecutable) {
		return report(
			assertCliArtifact({
				...base,
				newestSourceMtimeMs: null,
				sourceError: null,
				run: null,
			}),
		);
	}
	const run = await runHelp();
	const source = await newestSourceMtime();
	return report(
		assertCliArtifact({
			...base,
			newestSourceMtimeMs: source.newest,
			sourceError: source.error,
			run,
		}),
	);
}

process.exit(await main());
