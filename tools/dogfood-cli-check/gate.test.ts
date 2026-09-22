import { afterAll, describe, expect, test } from "bun:test";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
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
	test("resolves the real build closure rather than hanging or crashing", async () => {
		// An absent binary is the cheap verdict, so reaching this message proves
		// the state path resolved and no unnamed exception escaped.
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
});
