#!/usr/bin/env bun
// The I/O shell of the guest-image publish lane: realise the three assets,
// assemble the OCI layout, guard the build tag, push, and assert the registry
// resolved what was built. Every mapping and fail-closed decision lives in
// publish-core.ts so it is unit-testable without a registry.

import { spawnSync } from "node:child_process";
import {
	mkdirSync,
	readFileSync,
	rmSync,
	statSync,
	writeFileSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
	ASSET_ORDER,
	type Asset,
	type AssetName,
	annotationViolations,
	buildTag,
	digestRef,
	EXIT,
	type LayoutPlan,
	layoutPlan,
	manifestDigest,
	tagDisposition,
} from "./publish-core.ts";

const USAGE =
	"usage: publish.ts --repo <repo> --sha <sha12> [--layout <dir>] [--dry-run]";

function fail(code: number, message: string): never {
	console.error(`::error::guest-image publish: ${message}`);
	process.exit(code);
}

function flag(name: string): string | undefined {
	const at = process.argv.indexOf(`--${name}`);
	if (at === -1) return undefined;
	const value = process.argv[at + 1];
	// A following flag means this one's value is missing, not that it is `--sha`.
	return value === undefined || value.startsWith("--") ? undefined : value;
}

function run(
	command: string,
	args: readonly string[],
	cwd: string,
): { ok: boolean; stdout: string; stderr: string } {
	const result = spawnSync(command, [...args], {
		cwd,
		encoding: "utf8",
		maxBuffer: 64 * 1024 * 1024,
	});
	if (result.error)
		return {
			ok: false,
			stdout: "",
			stderr: `${command}: ${result.error.message}`,
		};
	return {
		ok: result.status === 0,
		stdout: result.stdout ?? "",
		stderr: result.stderr ?? "",
	};
}

function sha256OfFile(path: string): string {
	const hasher = new Bun.CryptoHasher("sha256");
	hasher.update(readFileSync(path));
	return `sha256:${hasher.digest("hex")}`;
}

const repo = flag("repo");
const sha = flag("sha");
const dryRun = process.argv.includes("--dry-run");
if (repo === undefined || sha === undefined) fail(EXIT.usage, USAGE);
if (!/^[0-9a-f]{12}$/.test(sha))
	fail(EXIT.usage, `--sha must be 12 lowercase hex characters, got ${sha}`);

const here = dirname(fileURLToPath(import.meta.url));
const workspaceRoot = join(here, "..", "..");
const guestDir = join(workspaceRoot, "guest-image");
const layoutDir = flag("layout") ?? join(guestDir, "oci-layout");

// The same three attrs the build gate realises, so a publish never packs a
// triple the gate has not already built.
const built = run(
	"nix",
	[
		"build",
		"-f",
		"default.nix",
		"compass-guest-kernel",
		"compass-guest-rootfs",
		"compass-guest-initrd",
		"--no-link",
		"--print-out-paths",
	],
	guestDir,
);
if (!built.ok)
	fail(
		EXIT.badLayout,
		`realising the guest assets failed: ${built.stderr.trim()}`,
	);
const outPaths = built.stdout
	.trim()
	.split("\n")
	.map((line) => line.trim())
	.filter((line) => line !== "");
const [kernelDir, rootfsPath, initrdPath] = outPaths;
if (
	kernelDir === undefined ||
	rootfsPath === undefined ||
	initrdPath === undefined ||
	outPaths.length !== 3
) {
	fail(
		EXIT.badLayout,
		`expected three store paths from the build, got ${outPaths.length}`,
	);
}

// The kernel attr yields a directory; its bzImage is the blob.
const assetPaths: Readonly<Record<AssetName, string>> = {
	kernel: join(kernelDir, "bzImage"),
	rootfs: rootfsPath,
	initrd: initrdPath,
};

const assets: Record<AssetName, Asset> = {} as Record<AssetName, Asset>;
for (const name of ASSET_ORDER) {
	const path = assetPaths[name];
	let size: number;
	try {
		size = statSync(path).size;
	} catch (error) {
		fail(EXIT.badLayout, `${name}: ${path} is not readable (${String(error)})`);
	}
	assets[name] = { digest: sha256OfFile(path), size };
}

const revision = run("git", ["rev-parse", "HEAD"], workspaceRoot);
if (!revision.ok)
	fail(
		EXIT.badLayout,
		`reading the source revision failed: ${revision.stderr.trim()}`,
	);

const lockText = readFileSync(join(guestDir, "agent-oci.lock"), "utf8");
const lock: unknown = JSON.parse(lockText);
if (
	typeof lock !== "object" ||
	lock === null ||
	!("digest" in lock) ||
	typeof lock.digest !== "string"
) {
	fail(
		EXIT.badLayout,
		"agent-oci.lock has no string digest; the artifact's agent provenance would be unset",
	);
}
const agentImageDigest = lock.digest;

let plan: LayoutPlan;
try {
	plan = layoutPlan(assets, {
		revision: revision.stdout.trim(),
		agentImageDigest,
	});
} catch (error) {
	fail(EXIT.badLayout, error instanceof Error ? error.message : String(error));
}

const violations = annotationViolations(plan.annotations);
if (violations.length > 0)
	fail(EXIT.secretFound, `secret-shaped annotations: ${violations.join(", ")}`);

// A stale layout would let the digest assertion below compare against blobs
// this run never wrote.
rmSync(layoutDir, { recursive: true, force: true });
const blobs = join(layoutDir, "blobs", "sha256");
mkdirSync(blobs, { recursive: true });
writeFileSync(
	join(layoutDir, "oci-layout"),
	JSON.stringify({ imageLayoutVersion: "1.0.0" }),
);
writeFileSync(
	join(blobs, plan.config.digest.slice("sha256:".length)),
	plan.configBytes,
);
for (const name of ASSET_ORDER) {
	const digest = assets[name].digest.slice("sha256:".length);
	writeFileSync(join(blobs, digest), readFileSync(assetPaths[name]));
}
writeFileSync(
	join(blobs, plan.manifestDescriptor.digest.slice("sha256:".length)),
	plan.manifest,
);
writeFileSync(join(layoutDir, "index.json"), plan.index);

const local = plan.manifestDescriptor.digest;
if (dryRun) {
	console.error(`guest-image publish: dry run wrote ${layoutDir}`);
	console.log(digestRef(repo, local));
	process.exit(0);
}

const tag = buildTag(repo, sha);
// The login runs in a separate process, so every call must name the same creds
// file; skopeo's default location is environment-dependent on hosted runners.
const authFile = process.env.REGISTRY_AUTH_FILE;
const auth = authFile === undefined ? [] : ["--authfile", authFile];
const probe = run(
	"skopeo",
	["inspect", ...auth, "--raw", `docker://${tag}`],
	workspaceRoot,
);
const disposition = tagDisposition(
	{ exitCode: probe.ok ? 0 : 1, stdout: probe.stdout, stderr: probe.stderr },
	local,
);
if (disposition.action === "abort")
	fail(EXIT.pushFailed, disposition.reason ?? "refusing to publish");
if (disposition.action === "skip") {
	console.error(`guest-image publish: ${tag} already holds this artifact`);
	console.log(digestRef(repo, local));
	process.exit(0);
}

const pushed = run(
	"skopeo",
	["copy", ...auth, `oci:${layoutDir}`, `docker://${tag}`],
	workspaceRoot,
);
if (!pushed.ok)
	fail(EXIT.pushFailed, `pushing ${tag} failed: ${pushed.stderr.trim()}`);

// The registry is authoritative: a transport that rewrote the manifest must
// fail closed rather than have its digest published as ours.
const resolved = run(
	"skopeo",
	["inspect", ...auth, "--raw", `docker://${tag}`],
	workspaceRoot,
);
if (!resolved.ok)
	fail(EXIT.pushFailed, `re-reading ${tag} failed: ${resolved.stderr.trim()}`);
const remote = manifestDigest(resolved.stdout);
if (remote !== local)
	fail(EXIT.digestMismatch, `registry resolved ${remote}, built ${local}`);

console.log(digestRef(repo, remote));
