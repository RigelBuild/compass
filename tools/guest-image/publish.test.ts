import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import {
	chmodSync,
	existsSync,
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { EXIT } from "./publish-core.ts";

// The shell's observable contract: stdout carries the digest reference and
// nothing else, the layout holds one blob per declared digest, and a registry
// that resolves something else fails closed. The pure-core suite cannot cover
// any of it, so a regression here would otherwise ship green.

const cli = join(import.meta.dir, "publish.ts");
const REPO = "ghcr.io/rigelbuild/compass-guest-image";
const SHA = "0123456789ab";

let sandbox = "";
let binDir = "";
let storeDir = "";

/** A stub on PATH ahead of the real tool, so no test touches nix or a registry. */
function stub(name: string, body: string): void {
	const path = join(binDir, name);
	writeFileSync(path, `#!/usr/bin/env bash\n${body}\n`);
	chmodSync(path, 0o755);
}

function runCli(
	args: readonly string[],
	env: Readonly<Record<string, string>> = {},
) {
	const result = spawnSync("bun", [cli, ...args], {
		encoding: "utf8",
		env: { ...process.env, PATH: `${binDir}:${process.env.PATH}`, ...env },
	});
	return {
		code: result.status ?? -1,
		stdout: result.stdout ?? "",
		stderr: result.stderr ?? "",
	};
}

beforeEach(() => {
	sandbox = mkdtempSync(join(tmpdir(), "guest-publish-"));
	binDir = join(sandbox, "bin");
	storeDir = join(sandbox, "store");
	mkdirSync(binDir, { recursive: true });
	// The kernel attr yields a DIRECTORY whose bzImage is the blob; the other
	// two attrs are the blobs themselves.
	mkdirSync(join(storeDir, "linux-0.0.0"), { recursive: true });
	writeFileSync(join(storeDir, "linux-0.0.0", "bzImage"), "KERNEL-BYTES");
	writeFileSync(join(storeDir, "rootfs.erofs"), "ROOTFS-BYTES");
	writeFileSync(join(storeDir, "initrd"), "INITRD-BYTES");
	stub(
		"moon",
		`echo "  banner line moon prints"
echo "${join(storeDir, "linux-0.0.0")}"
echo "${join(storeDir, "rootfs.erofs")}"
echo "${join(storeDir, "initrd")}"
echo "Tasks: 1 completed"`,
	);
	stub("git", 'echo "d3adb33fd3adb33fd3adb33fd3adb33fd3adb33f"');
});

afterEach(() => {
	rmSync(sandbox, { recursive: true, force: true });
});

function layoutPath(): string {
	return join(sandbox, "layout");
}

function dryRun(env: Readonly<Record<string, string>> = {}) {
	return runCli(
		["--repo", REPO, "--sha", SHA, "--layout", layoutPath(), "--dry-run"],
		env,
	);
}

describe("the dry-run contract", () => {
	test("stdout is exactly one digest reference and nothing else", () => {
		const { code, stdout } = dryRun();
		expect(code).toBe(0);
		expect(stdout.trimEnd().split("\n")).toHaveLength(1);
		expect(stdout.trim()).toMatch(new RegExp(`^${REPO}@sha256:[0-9a-f]{64}$`));
	});

	test("writes one blob per declared digest, so the layout a registry reads is complete", () => {
		dryRun();
		const index = JSON.parse(
			readFileSync(join(layoutPath(), "index.json"), "utf8"),
		);
		const manifestDigest = index.manifests[0].digest.slice("sha256:".length);
		const manifest = JSON.parse(
			readFileSync(
				join(layoutPath(), "blobs", "sha256", manifestDigest),
				"utf8",
			),
		);
		for (const descriptor of [...manifest.layers, manifest.config]) {
			const blob = join(
				layoutPath(),
				"blobs",
				"sha256",
				descriptor.digest.slice("sha256:".length),
			);
			expect(existsSync(blob)).toBe(true);
			expect(readFileSync(blob).byteLength).toBe(descriptor.size);
		}
	});

	test("selects store paths from moon's framed output rather than line positions", () => {
		dryRun();
		const index = JSON.parse(
			readFileSync(join(layoutPath(), "index.json"), "utf8"),
		);
		const manifestDigest = index.manifests[0].digest.slice("sha256:".length);
		const manifest = JSON.parse(
			readFileSync(
				join(layoutPath(), "blobs", "sha256", manifestDigest),
				"utf8",
			),
		);
		// The kernel layer must be the bzImage inside the directory attr.
		expect(manifest.layers[0].size).toBe("KERNEL-BYTES".length);
		expect(manifest.layers[1].size).toBe("ROOTFS-BYTES".length);
	});

	test("clears a stale layout, so the digest assertion cannot read a previous run's blobs", () => {
		mkdirSync(join(layoutPath(), "blobs", "sha256"), { recursive: true });
		// The marker is what identifies this as a layout the lane may replace.
		writeFileSync(
			join(layoutPath(), "oci-layout"),
			'{"imageLayoutVersion":"1.0.0"}',
		);
		const stale = join(layoutPath(), "blobs", "sha256", "stale");
		writeFileSync(stale, "LEFTOVER");
		dryRun();
		expect(existsSync(stale)).toBe(false);
	});

	test("refuses to recursively delete a --layout path that is not a layout", () => {
		mkdirSync(layoutPath(), { recursive: true });
		const bystander = join(layoutPath(), "important.txt");
		writeFileSync(bystander, "DO NOT DELETE");
		const { code } = dryRun();
		expect(code).toBe(EXIT.usage);
		expect(readFileSync(bystander, "utf8")).toBe("DO NOT DELETE");
	});
	test("a build failure is a layout fault, not a push attempt", () => {
		stub("moon", 'echo "boom" >&2; exit 1');
		const { code, stdout } = dryRun();
		expect(code).toBe(EXIT.badLayout);
		expect(stdout.trim()).toBe("");
	});

	test("an empty --repo is a usage error before any realisation work", () => {
		const { code } = runCli([
			"--repo",
			"",
			"--sha",
			SHA,
			"--layout",
			layoutPath(),
			"--dry-run",
		]);
		expect(code).toBe(EXIT.usage);
		// The multi-GiB realisation must not have run to reject an empty flag.
		expect(existsSync(layoutPath())).toBe(false);
	});

	test("a malformed agent-oci.lock is a layout fault, not a stack trace", () => {
		const lock = join(
			import.meta.dir,
			"..",
			"..",
			"guest-image",
			"agent-oci.lock",
		);
		const saved = readFileSync(lock, "utf8");
		writeFileSync(lock, "{ not json");
		try {
			expect(dryRun().code).toBe(EXIT.badLayout);
		} finally {
			writeFileSync(lock, saved);
		}
	});

	test("an asset path that stats but is not a regular file fails closed", () => {
		// A directory where a blob belongs: stat succeeds, reading it does not.
		stub(
			"moon",
			`echo "${join(storeDir, "linux-0.0.0")}"
echo "${storeDir}"
echo "${join(storeDir, "initrd")}"`,
		);
		expect(dryRun().code).toBe(EXIT.badLayout);
	});

	test("a build emitting the wrong number of store paths fails closed", () => {
		stub("moon", `echo "${join(storeDir, "rootfs.erofs")}"`);
		expect(dryRun().code).toBe(EXIT.badLayout);
	});
});

describe("the push contract", () => {
	function publish(env: Readonly<Record<string, string>> = {}) {
		return runCli(
			["--repo", REPO, "--sha", SHA, "--layout", layoutPath()],
			env,
		);
	}

	/** Records argv so a test can assert what skopeo was actually asked to do. */
	function stubSkopeo(script: string): string {
		const log = join(sandbox, "skopeo.log");
		stub("skopeo", `echo "$@" >> "${log}"\n${script}`);
		return log;
	}

	test("publishes when the tag is absent, then prints the registry-resolved digest", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then
  if [ -f "${join(sandbox, "pushed")}" ]; then cat "${join(sandbox, "manifest")}"; exit 0; fi
  echo "manifest unknown" >&2; exit 1
fi
if [ "$1" = copy ]; then touch "${join(sandbox, "pushed")}"; exit 0; fi`);
		// The registry echoes back the manifest this run built.
		dryRun();
		const index = JSON.parse(
			readFileSync(join(layoutPath(), "index.json"), "utf8"),
		);
		const digest = index.manifests[0].digest.slice("sha256:".length);
		writeFileSync(
			join(sandbox, "manifest"),
			readFileSync(join(layoutPath(), "blobs", "sha256", digest)),
		);
		const { code, stdout } = publish();
		expect(code).toBe(0);
		expect(stdout.trim()).toBe(`${REPO}@${index.manifests[0].digest}`);
	});

	test("a registry resolving a different manifest exits digestMismatch", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then
  if [ -f "${join(sandbox, "pushed")}" ]; then echo '{"schemaVersion":2,"impostor":true}'; exit 0; fi
  echo "manifest unknown" >&2; exit 1
fi
if [ "$1" = copy ]; then touch "${join(sandbox, "pushed")}"; exit 0; fi`);
		expect(publish().code).toBe(EXIT.digestMismatch);
	});

	test("an ambiguous probe failure refuses to push at all", () => {
		const log = stubSkopeo('echo "i/o timeout" >&2; exit 1');
		expect(publish().code).toBe(EXIT.pushFailed);
		expect(readFileSync(log, "utf8")).not.toContain("copy");
	});

	test("threads --authfile to every registry call when the login pinned one", () => {
		const log = stubSkopeo('echo "manifest unknown" >&2; exit 1');
		publish({ REGISTRY_AUTH_FILE: "/tmp/creds.json" });
		expect(readFileSync(log, "utf8")).toContain("--authfile /tmp/creds.json");
	});
});
