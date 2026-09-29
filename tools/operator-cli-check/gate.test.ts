import { afterAll, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, utimes, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

// Drives the real script, so it covers what the pure tests cannot: the go list
// subprocess, the spawn probe, and the DEVENV_STATE wiring. Both live defects
// this gate shipped with were in exactly that layer.
const GATE = resolve(import.meta.dir, "index.ts");
const roots: string[] = [];

async function stateDir(): Promise<string> {
	const root = await mkdtemp(join(tmpdir(), "cli-check-"));
	roots.push(root);
	await mkdir(join(root, "compass"), { recursive: true });
	return root;
}

async function runGate(
	state: string | null,
): Promise<{ code: number; text: string }> {
	const env = { ...process.env };
	if (state === null) delete env.DEVENV_STATE;
	else env.DEVENV_STATE = state;
	const child = Bun.spawn(["bun", "run", GATE], {
		env,
		stdout: "pipe",
		stderr: "pipe",
	});
	const [stdout, stderr, code] = await Promise.all([
		new Response(child.stdout).text(),
		new Response(child.stderr).text(),
		child.exited,
	]);
	return { code, text: stdout + stderr };
}

afterAll(async () => {
	for (const root of roots) await rm(root, { recursive: true, force: true });
});

describe("the gate end to end", () => {
	test("names an absent artifact without paying for the dependency scan", async () => {
		// The cheap verdict returns before `go list`, so this also pins that
		// ordering: it would blow the test timeout if the scan ran here.
		const { code, text } = await runGate(await stateDir());
		expect(code).toBe(1);
		expect(text).toContain("operator CLI is absent");
	});
	test("refuses to run without a state dir instead of checking a relative path", async () => {
		const { code, text } = await runGate(null);
		expect(code).toBe(2);
		expect(text).toContain("DEVENV_STATE is unset");
	});
	test("names a corrupt artifact instead of letting the spawn throw", async () => {
		const state = await stateDir();
		const binary = join(state, "compass/compass");
		await writeFile(binary, "not an executable", { mode: 0o755 });
		const { code, text } = await runGate(state);
		expect(code).toBe(1);
		expect(text).toContain("corrupt or unrunnable");
	});
	test("rejects a symlink standing in for the built artifact", async () => {
		const state = await stateDir();
		// The target must be runnable and newer than every source, so the only
		// thing left to fail on is that the artifact is a link, not a file.
		const target = join(state, "real-binary");
		await writeFile(target, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
		await Bun.spawn(["ln", "-s", target, join(state, "compass/compass")])
			.exited;
		const { code, text } = await runGate(state);
		expect(code).toBe(1);
		expect(text).toContain("operator CLI is absent");
	});
	// The only case that reaches `go list -deps`, which resolves the whole
	// module closure and is slow on CI's cold cache — bun's 5s default is not a
	// budget for it. Crash guard at 2x the gate's own 30s go list ceiling, so a
	// correct run cannot race it; the hang risk is the subprocess.
	// biome-ignore lint/plugin: hung-child crash guard, far above a correct run
	test("resolves the real build closure and reports a runnable stub as stale", async () => {
		const state = await stateDir();
		const binary = join(state, "compass/compass");
		// Runnable, so the run checks pass and freshness is what decides —
		// and its mtime is older than the repo's newest source.
		await writeFile(binary, "#!/bin/sh\nexit 0\n", { mode: 0o755 });
		await utimes(binary, new Date(0), new Date(0));
		const { code, text } = await runGate(state);
		expect(code).toBe(1);
		expect(text).toContain("operator CLI is stale");
	}, 60_000);
});
