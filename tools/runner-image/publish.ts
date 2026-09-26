#!/usr/bin/env bun
// Publish the compass-runner container image by digest: scan the locally built
// image config for leaked secrets, push it UNTAGGED with rootless BuildKit,
// assert the digest the registry accepted equals the one the local build
// produced, and only then tag that digest `:git-<sha12>` with skopeo.
//
// WHY A DIGEST AND NOT A TAG. GHCR has no server-side tag immutability, so
// anyone holding packages:write can re-point a tag at other bytes. The
// DaemonSet therefore pins `repo@sha256:…`; the `:git-<sha>` tag exists so a
// human can find the build and so the release re-tag can resolve it.
//
// WHY TAG LAST. The release re-tag promotes `:git-<sha12>` to `:vX.Y.Z`. A tag
// written before the digest check would survive a failed check and be promoted.
//
// WHY THIS DELEGATES TO build.ts. buildctl exposes no `push` verb — a push is
// an EXPORTER on `build` — so publishing runs the same build with
// `--output push`. Delegating keeps ONE staging implementation: a second copy
// could drift in the details that make the digest reproducible.
//
// Needs `skopeo` on PATH and DOCKER_CONFIG naming the dir whose config.json
// holds the registry auth (BuildKit reads the same file).
//
// Usage:
//   bun tools/runner-image/publish.ts --repo <ghcr.io/owner/name> --sha <sha12>

import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
	buildTag,
	EXIT,
	isImageDigest,
	publishVerified,
	secretConfigViolations,
} from "./publish-core.ts";

const here = dirname(fileURLToPath(import.meta.url));
const workspaceRoot = join(here, "..", "..");
const ociDir = join(workspaceRoot, "runner-image", "out");

function fail(code: number, message: string): never {
	console.error(`::error::runner-image publish: ${message}`);
	process.exit(code);
}

function arg(name: string): string | undefined {
	const i = process.argv.indexOf(`--${name}`);
	return i === -1 ? undefined : process.argv[i + 1];
}

const repo = arg("repo");
const sha12 = arg("sha");
if (!repo || !sha12) {
	fail(
		EXIT.usage,
		"usage: publish.ts --repo <ghcr.io/owner/name> --sha <sha12>",
	);
}
// Validate the sha before any build or registry work.
buildTag(repo, sha12);
const dockerConfig = process.env.DOCKER_CONFIG;
if (!dockerConfig) {
	fail(
		EXIT.usage,
		"DOCKER_CONFIG is unset; it must name the dir whose config.json holds the registry auth",
	);
}
const authFile = join(dockerConfig, "config.json");

// The local layout the build lane wrote. Scanning the ASSEMBLED config (not the
// Dockerfile) is what proves what ships, and scanning BEFORE the push matters
// because deleting a tag from a public registry does not unpublish the bytes.
// The directory alone is not enough: the build lane creates it before the
// exporter writes into it, so an interrupted build leaves an empty dir whose
// index.json read would throw a raw ENOENT instead of naming the fault.
if (!existsSync(join(ociDir, "index.json"))) {
	fail(
		EXIT.usage,
		`no OCI layout at ${ociDir} — run \`bun tools/runner-image/build.ts --output oci\` (or \`moon run compass-runner-image:build\`, its default output) first`,
	);
}
const localDigest = manifestDigest(ociDir);
const violations = secretConfigViolations(imageConfig(ociDir, localDigest));
if (violations.length > 0) {
	fail(
		EXIT.secretFound,
		`image config carries secret-shaped names: ${violations.join(", ")} — rotate and rebuild`,
	);
}

// The metadata dir is a throwaway; remove it on every exit path (including a
// fail()'s process.exit) so a dev-box run leaves nothing behind. force:true
// keeps the handler from throwing, and it changes no exit code.
const metadataDir = mkdtempSync(join(tmpdir(), "runner-image-"));
process.on("exit", () => rmSync(metadataDir, { recursive: true, force: true }));
const metadataFile = join(metadataDir, "meta.json");
const result = publishVerified(
	{ repo, sha12, localDigest, authFile },
	{
		push: () => {
			const push = spawnSync(
				"bun",
				[
					join(here, "build.ts"),
					"--output",
					"push",
					"--tag",
					repo,
					"--metadata-file",
					metadataFile,
				],
				{ cwd: workspaceRoot, stdio: "inherit", timeout: 60 * 60 * 1000 },
			);
			return {
				status: push.status,
				metadata:
					push.status === 0
						? JSON.parse(readFileSync(metadataFile, "utf8"))
						: undefined,
			};
		},
		skopeo: (args) => {
			// skopeo's progress goes to stderr: the workflow captures this script's
			// single stdout line as the ref.
			const copy = spawnSync("skopeo", [...args], {
				encoding: "utf8",
				stdio: ["ignore", process.stderr.fd, "pipe"],
			});
			// A spawn failure (skopeo not on PATH) has no stderr; report its error.
			const stderr = copy.error ? String(copy.error) : (copy.stderr ?? "");
			return { status: copy.status, stderr };
		},
	},
);
if (!result.ok) fail(result.code, result.message);

// The deployable reference, on stdout so the workflow step can capture it.
console.log(result.ref);

/** The single sha256 manifest an OCI layout's index names. */
function manifestDigest(dir: string): string {
	const index: unknown = JSON.parse(
		readFileSync(join(dir, "index.json"), "utf8"),
	);
	if (index && typeof index === "object" && "manifests" in index) {
		const manifests = index.manifests;
		if (Array.isArray(manifests) && manifests.length === 1) {
			const first: unknown = manifests[0];
			if (first && typeof first === "object" && "digest" in first) {
				const digest = first.digest;
				if (typeof digest === "string" && isImageDigest(digest)) return digest;
			}
		}
	}
	// A layout that exists but names other than one sha256 manifest is a
	// malformed local artifact, not a usage mistake: the build emitted something
	// other than the single linux/amd64 image this lane contracts for.
	fail(
		EXIT.badLayout,
		`${dir}/index.json does not carry exactly one sha256 manifest`,
	);
}

/** The `Env` and `Labels` of the image config the layout's manifest points at.
 * Both are secret-scanned: a leaked credential shows up as a name in either. */
function imageConfig(
	dir: string,
	manifest: string,
): { env: readonly string[]; labels: Readonly<Record<string, string>> } {
	const image: unknown = readBlob(dir, manifestConfigDigest(dir, manifest));
	// No config, no Env, or no Labels is all legitimate (nothing to leak), so an
	// absence is an empty result, never a failure.
	if (!image || typeof image !== "object" || !("config" in image)) {
		return { env: [], labels: {} };
	}
	const inner = image.config;
	if (!inner || typeof inner !== "object") return { env: [], labels: {} };
	const env =
		"Env" in inner && Array.isArray(inner.Env)
			? inner.Env.filter((e): e is string => typeof e === "string")
			: [];
	const labels: Record<string, string> = {};
	if ("Labels" in inner && inner.Labels && typeof inner.Labels === "object") {
		for (const [key, value] of Object.entries(inner.Labels)) {
			if (typeof value === "string") labels[key] = value;
		}
	}
	return { env, labels };
}

function readBlob(dir: string, digest: string): unknown {
	return JSON.parse(
		readFileSync(join(dir, "blobs", ...digest.split(":")), "utf8"),
	);
}

/** The config blob's own digest, resolved through the manifest descriptor. A
 * missing or non-string config digest is a malformed layout, not a usage
 * mistake. */
function manifestConfigDigest(dir: string, manifest: string): string {
	const descriptor: unknown = readBlob(dir, manifest);
	if (
		!descriptor ||
		typeof descriptor !== "object" ||
		!("config" in descriptor)
	) {
		fail(EXIT.badLayout, `manifest ${manifest} carries no config descriptor`);
	}
	const config = descriptor.config;
	if (!config || typeof config !== "object" || !("digest" in config)) {
		fail(
			EXIT.badLayout,
			`manifest ${manifest} config descriptor has no digest`,
		);
	}
	const digest = config.digest;
	if (typeof digest !== "string") {
		fail(EXIT.badLayout, `manifest ${manifest} config digest is not a string`);
	}
	return digest;
}
