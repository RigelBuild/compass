#!/usr/bin/env bun

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { hostPlatform } from "../runner-image/build-core.ts";
import {
	buildTag,
	digestRef,
	EXIT,
	layoutDigest,
	tagDisposition,
} from "./core.ts";

const USAGE =
	"usage: publish.ts --repo <repo> --sha <sha12> [--arch-suffix <amd64|arm64>] [--out <dir>]";
const workspaceRoot = resolve(import.meta.dir, "..", "..");
const laneDir = import.meta.dir;

function fail(code: number, message: string): never {
	console.error(`gateway-image publish: ${message}`);
	process.exit(code);
}

function parseArgs(argv: readonly string[]): {
	repo: string;
	sha: string;
	out: string;
	archSuffix: "amd64" | "arm64" | undefined;
} {
	const values: Partial<
		Record<"--repo" | "--sha" | "--out" | "--arch-suffix", string>
	> = {};
	for (let index = 0; index < argv.length; index += 1) {
		const name = argv[index];
		const value = argv[index + 1];
		if (
			(name !== "--repo" &&
				name !== "--sha" &&
				name !== "--out" &&
				name !== "--arch-suffix") ||
			value === undefined ||
			value === "" ||
			value.startsWith("--") ||
			values[name] !== undefined
		) {
			fail(EXIT.usage, USAGE);
		}
		values[name] = value;
		index += 1;
	}
	const repo = values["--repo"];
	const sha = values["--sha"];
	const archValue = values["--arch-suffix"];
	if (repo === undefined || sha === undefined) fail(EXIT.usage, USAGE);
	if (!/^[0-9a-f]{12}$/.test(sha)) {
		fail(EXIT.usage, `--sha must be 12 lowercase hex characters, got ${sha}`);
	}
	if (
		archValue !== undefined &&
		archValue !== "amd64" &&
		archValue !== "arm64"
	) {
		fail(EXIT.usage, "--arch-suffix must be amd64 or arm64");
	}
	const platform = hostPlatform(process.arch);
	if (
		archValue !== undefined &&
		archValue !== platform.slice("linux/".length)
	) {
		fail(
			EXIT.usage,
			`--arch-suffix ${archValue} does not match host platform ${platform}`,
		);
	}
	return {
		repo,
		sha,
		archSuffix:
			archValue === "amd64" || archValue === "arm64" ? archValue : undefined,
		out: resolve(workspaceRoot, values["--out"] ?? join(laneDir, "out")),
	};
}

type CommandResult = { ok: boolean; stdout: Buffer; stderr: Buffer };

function run(command: string, args: readonly string[]): CommandResult {
	const result = spawnSync(command, [...args], {
		cwd: workspaceRoot,
		maxBuffer: 64 * 1024 * 1024,
	});
	if (result.error) {
		return {
			ok: false,
			stdout: Buffer.alloc(0),
			stderr: Buffer.from(`${command}: ${result.error.message}`),
		};
	}
	return {
		ok: result.status === 0,
		stdout: result.stdout ?? Buffer.alloc(0),
		stderr: result.stderr ?? Buffer.alloc(0),
	};
}

const { repo, sha, out, archSuffix } = parseArgs(process.argv.slice(2));
const indexPath = join(out, "oci", "index.json");
let localDigest: string;
try {
	localDigest = layoutDigest(readFileSync(indexPath, "utf8"));
} catch (error) {
	fail(EXIT.badLayout, `reading ${indexPath} failed: ${String(error)}`);
}

const tag = buildTag(repo, sha, archSuffix);
const authFile = process.env.REGISTRY_AUTH_FILE;
const destinationAuth = authFile === undefined ? [] : ["--authfile", authFile];
const probe = run("skopeo", [
	"inspect",
	...destinationAuth,
	"--raw",
	`docker://${tag}`,
]);
const disposition = tagDisposition(
	{
		exitCode: probe.ok ? 0 : 1,
		stdout: probe.stdout.toString("utf8"),
		stderr: probe.stderr.toString("utf8"),
	},
	localDigest,
);
if (disposition.action === "abort") {
	fail(EXIT.pushFailed, disposition.reason ?? "refusing to publish");
}
if (disposition.action === "skip") {
	console.error(`gateway-image publish: ${tag} already holds this artifact`);
	console.log(digestRef(repo, localDigest));
	process.exit(0);
}

const layoutDir = join(out, "oci");
const pushed = run("skopeo", [
	"copy",
	"--src-no-creds",
	...(authFile === undefined ? [] : ["--dest-authfile", authFile]),
	"--preserve-digests",
	`oci:${layoutDir}`,
	`docker://${tag}`,
]);
if (!pushed.ok) {
	fail(
		EXIT.pushFailed,
		`pushing ${tag} failed: ${pushed.stderr.toString("utf8").trim()}`,
	);
}

const resolved = run("skopeo", [
	"inspect",
	"--no-creds",
	"--raw",
	`docker://${tag}`,
]);
if (!resolved.ok) {
	fail(
		EXIT.pushFailed,
		`re-reading ${tag} failed: ${resolved.stderr.toString("utf8").trim()}`,
	);
}
const remoteDigest = `sha256:${new Bun.CryptoHasher("sha256").update(resolved.stdout).digest("hex")}`;
if (remoteDigest !== localDigest) {
	fail(
		EXIT.digestMismatch,
		`registry resolved ${remoteDigest}, built ${localDigest}`,
	);
}

console.log(digestRef(repo, remoteDigest));
