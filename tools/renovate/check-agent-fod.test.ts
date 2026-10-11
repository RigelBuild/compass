import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Drives the shipped check against a stub `nix`, so each exit path is proven
// without realising the real FOD.
const SCRIPT = join(import.meta.dir, "check-agent-fod.ts");

let bin: string;

async function stubNix(body: string): Promise<void> {
	await Bun.write(join(bin, "nix"), `#!/usr/bin/env bash\n${body}\n`);
	await chmod(join(bin, "nix"), 0o755);
}

async function runCheck() {
	const res = await $`bun ${SCRIPT}`
		.env({ ...process.env, PATH: `${bin}:${process.env.PATH}` })
		.quiet()
		.nothrow();
	return {
		exitCode: res.exitCode,
		stdout: res.stdout.toString(),
		stderr: res.stderr.toString(),
	};
}

beforeEach(async () => {
	bin = await mkdtemp(join(tmpdir(), "check-agent-fod-"));
});

afterEach(async () => {
	await rm(bin, { recursive: true, force: true });
});
// The arm64 PR job runs this same script, so the expected pin follows the host.
const HOST_SYSTEM = process.arch === "arm64" ? "aarch64-linux" : "x86_64-linux";

describe("check-agent-fod.ts", () => {
	test("passes when the FOD builds", async () => {
		await stubNix("exit 0");
		const res = await runCheck();
		expect(res.exitCode).toBe(0);
		expect(res.stdout).toContain("outputHash matches");
	});

	test("a stale pin names the lockfile and the refresher", async () => {
		await stubNix(`cat >&2 <<'OUT'
error: hash mismatch in fixed-output derivation '/nix/store/x-compass-agent-node-modules-0.1.0.drv':
         specified: sha256-old=
            got:    sha256-new=
OUT
exit 1`);
		const res = await runCheck();
		expect(res.exitCode).toBe(1);
		expect(res.stderr).toContain(
			`agent-image/entrypoint.nix ${HOST_SYSTEM} outputHash is stale`,
		);
		expect(res.stderr).toContain("bun.lock");
		expect(res.stderr).toContain("sha256-new=");
		expect(res.stderr).toContain("bun tools/renovate/refresh-fod-hashes.ts");
	});

	test("a build failure without a mismatch is not reported as a stale pin", async () => {
		await stubNix("echo 'error: builder failed' >&2; exit 1");
		const res = await runCheck();
		expect(res.exitCode).toBe(1);
		expect(res.stderr).toContain("failed without a hash mismatch");
		expect(res.stderr).toContain("builder failed");
		expect(res.stderr).not.toContain("is stale");
	});

	test("builds only this host's node_modules FOD, not the image", async () => {
		await stubNix('echo "$@" > "$(dirname "$0")/args"; exit 0');
		await runCheck();
		const args = await Bun.file(join(bin, "args")).text();
		expect(args).toContain(
			`agent-image-fod-vehicle.nix compass-agent.nodeModulesBySystem.${HOST_SYSTEM} `,
		);
	});
});
