#!/usr/bin/env bun
// Publish the compass-runner container image by digest (R2 of the Compass
// Runner containerization record): scan the locally built image config for
// leaked secrets, push with rootless BuildKit, then assert the digest the
// registry accepted equals the one the local build produced.
//
// WHY A DIGEST AND NOT A TAG. GHCR has no server-side tag immutability, so
// anyone holding packages:write can re-point a tag at other bytes. The
// DaemonSet therefore pins `repo@sha256:…`; the `:git-<sha>` tag exists only so
// a human can find the build.
//
// WHY THIS DELEGATES TO build.ts. buildctl exposes no `push` verb — a push is
// an EXPORTER on `build` — so publishing runs the same build with
// `--output push`. Delegating keeps ONE staging implementation: a second copy
// could drift in the details that make the digest reproducible.
//
// Usage:
//   bun tools/runner-image/publish.ts --repo <ghcr.io/owner/name> --sha <sha12>
//                                     [--expect-digest sha256:…]

import { spawnSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
	buildTag,
	digestFromMetadata,
	digestRef,
	EXIT,
	isImageDigest,
	secretEnvViolations,
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
		"usage: publish.ts --repo <ghcr.io/owner/name> --sha <sha12> [--expect-digest sha256:…]",
	);
}
const tag = buildTag(repo, sha12);

// The local layout the build lane wrote. Scanning the ASSEMBLED config (not the
// Dockerfile) is what proves what ships, and scanning BEFORE the push matters
// because deleting a tag from a public registry does not unpublish the bytes.
// The directory alone is not enough: the build lane creates it before the
// exporter writes into it, so an interrupted build leaves an empty dir whose
// index.json read would throw a raw ENOENT instead of naming the fault.
if (!existsSync(join(ociDir, "index.json"))) {
	fail(
		EXIT.usage,
		`no OCI layout at ${ociDir} — run \`moon run compass-runner-image:build\` first`,
	);
}
const localDigest = manifestDigest(ociDir);
const violations = secretEnvViolations(imageConfigEnv(ociDir, localDigest));
if (violations.length > 0) {
	fail(
		EXIT.secretFound,
		`image config env carries secret-shaped names: ${violations.join(", ")} — rotate and rebuild`,
	);
}

const metadataFile = join(
	mkdtempSync(join(tmpdir(), "runner-image-")),
	"meta.json",
);
const push = spawnSync(
	"bun",
	[
		join(here, "build.ts"),
		"--output",
		"push",
		"--tag",
		tag,
		"--metadata-file",
		metadataFile,
	],
	{ cwd: workspaceRoot, stdio: "inherit", timeout: 60 * 60 * 1000 },
);
if (push.status !== 0) {
	fail(EXIT.pushFailed, `push to ${tag} exited ${push.status ?? "on signal"}`);
}

const metadata: unknown = JSON.parse(readFileSync(metadataFile, "utf8"));
const pushedDigest = digestFromMetadata(metadata);
if (!pushedDigest) {
	fail(EXIT.pushFailed, `${metadataFile} carries no containerimage.digest`);
}

// The push exporter's digest must equal the digest the local build produced.
// Unequal means the published bytes are not the reviewed, locally-reproduced
// bytes — which is the whole point of pinning a digest downstream.
if (pushedDigest !== localDigest) {
	fail(
		EXIT.digestMismatch,
		`local build produced ${localDigest} but the push resolved ${pushedDigest}`,
	);
}

// An explicit expectation from the caller (a prior run's digest) turns
// reproducibility into a gate rather than a claim.
const expected = arg("expect-digest");
if (expected && expected !== pushedDigest) {
	fail(
		EXIT.digestMismatch,
		`expected ${expected} but published ${pushedDigest} — the build is not reproducible`,
	);
}

// The deployable reference, on stdout so the workflow step can capture it.
console.log(digestRef(repo, pushedDigest));

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
	// Fail rather than pick one: a multi-manifest layout means the build emitted
	// something other than the single linux/amd64 image this lane contracts for.
	fail(
		EXIT.usage,
		`${dir}/index.json does not carry exactly one sha256 manifest`,
	);
}

/** The `Env` of the image config the layout's manifest points at. */
function imageConfigEnv(dir: string, manifest: string): readonly string[] {
	const blob = (digest: string): unknown =>
		JSON.parse(readFileSync(join(dir, "blobs", ...digest.split(":")), "utf8"));
	const descriptor: unknown = blob(manifest);
	if (
		!descriptor ||
		typeof descriptor !== "object" ||
		!("config" in descriptor)
	) {
		fail(EXIT.usage, `manifest ${manifest} carries no config descriptor`);
	}
	const config = descriptor.config;
	if (!config || typeof config !== "object" || !("digest" in config)) {
		fail(EXIT.usage, `manifest ${manifest} config descriptor has no digest`);
	}
	const digest = config.digest;
	if (typeof digest !== "string") {
		fail(EXIT.usage, `manifest ${manifest} config digest is not a string`);
	}
	const image: unknown = blob(digest);
	if (image && typeof image === "object" && "config" in image) {
		const inner = image.config;
		if (inner && typeof inner === "object" && "Env" in inner) {
			const env = inner.Env;
			if (Array.isArray(env)) {
				return env.filter((e): e is string => typeof e === "string");
			}
		}
	}
	// No Env at all is legitimate (nothing to leak), so this is not a failure.
	return [];
}
