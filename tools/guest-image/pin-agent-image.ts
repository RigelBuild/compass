// Pin the published compass-agent image the guest rootfs derives from
// (guest-image/agent-oci.lock).
//
// Two modes. `--tag git-<sha12>` pins an explicit build. `--relock` is the
// Renovate postUpgradeTask: it re-derives the whole lock after the regex
// manager bumped part of it.
//
// WHY --relock EXISTS (do not "simplify" it to a bare regex bump). The lock
// carries the per-layer descriptor digests the nix fixed-output fetches key
// on, and Renovate cannot compute those: a regex manager can move the tag and
// manifest digest, but the layer set it leaves behind still describes the OLD
// manifest, so every fetch would fail on every Renovate PR. This is the same
// failure class tools/renovate/refresh-devenv-lock.ts names for devenv — a
// rev-only rewrite leaves paired fields stale — and the same remedy: a
// customManager PLUS a postUpgradeTask that makes the lock genuinely
// consistent. The digest-only DefaultPostgresImage pin is NOT the precedent;
// there a regex rewrite completes the update.
//
// HOW A NEW PIN IS DISCOVERED. The pinned tag is per-commit immutable
// (`git-<sha12>`), so no datasource can order it and Renovate tracks the
// moving `:latest` digest instead. The relock then resolves which immutable
// tag `:latest` currently points at. That is sound because the publish lane
// asserts `:latest` and `:git-<sha12>` share a config digest and fails closed
// otherwise (.github/workflows/release.yml, publish-image).
//
// Needs `skopeo` and network on PATH. Reads are anonymous: the package is
// public, so no registry credentials are required.

import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
	AGENT_REPO,
	EXIT,
	lockFromInspect,
	locksEqual,
	PinError,
	type PinLock,
	renderLock,
	validatePin,
} from "./pin-core.ts";

const LOCK_PATH = join(
	import.meta.dir,
	"..",
	"..",
	"guest-image",
	"agent-oci.lock",
);

/** Where the moving tag lives. Only ever used to DISCOVER an immutable tag;
 * never written into a lock. */
const DISCOVERY_TAG = "latest";

type Inspected = { digest: string; manifest: unknown };

async function skopeo(args: string[]): Promise<string> {
	const proc = Bun.spawn(["skopeo", ...args], {
		stdout: "pipe",
		stderr: "pipe",
	});
	const [out, err, code] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
		proc.exited,
	]);
	if (code !== 0) {
		throw new PinError(
			`skopeo ${args.join(" ")} failed (exit ${code}): ${err.trim()}`,
			EXIT.registryFailed,
		);
	}
	return out;
}

/** The manifest body plus the digest the registry resolved for the reference.
 * Both come from skopeo so the pair is consistent; deriving the digest by
 * re-hashing locally would assert our own hashing instead of the registry's. */
async function inspect(reference: string): Promise<Inspected> {
	const raw = await skopeo(["inspect", "--raw", `docker://${reference}`]);
	const meta = await skopeo(["inspect", `docker://${reference}`]);

	let manifest: unknown;
	let digest: unknown;
	try {
		manifest = JSON.parse(raw);
		digest = (JSON.parse(meta) as Record<string, unknown>).Digest;
	} catch (err) {
		throw new PinError(
			`could not parse skopeo output for ${reference}: ${String(err)}`,
			EXIT.registryFailed,
		);
	}
	if (typeof digest !== "string") {
		throw new PinError(
			`skopeo reported no digest for ${reference}`,
			EXIT.registryFailed,
		);
	}
	return { digest, manifest };
}

/** The digest a reference resolves to, without pulling the manifest body.
 * Tag discovery compares only digests, and the agent manifest is ~120 layers,
 * so fetching bodies would download a lot to read one field. */
async function resolvedDigest(reference: string): Promise<string> {
	const meta = await skopeo([
		"inspect",
		"--format",
		"{{.Digest}}",
		`docker://${reference}`,
	]);
	const digest = meta.trim();
	if (!digest) {
		throw new PinError(
			`skopeo reported no digest for ${reference}`,
			EXIT.registryFailed,
		);
	}
	return digest;
}

/** Resolve which immutable build tag `:latest` currently points at, by
 * matching manifest digests. Fails loud rather than falling back to pinning
 * `:latest`, which would defeat the whole lock.
 *
 * Newest-first: the registry lists tags oldest-first, and the tag we want is
 * almost always the newest, so walking the list in order costs a round trip
 * per historical tag (measured: ~115 misses, two minutes) to find the one at
 * the end. Reversing makes the common case one probe. */
async function discoverBuildTag(): Promise<string> {
	const target = await resolvedDigest(`${AGENT_REPO}:${DISCOVERY_TAG}`);
	const listed = await skopeo(["list-tags", `docker://${AGENT_REPO}`]);

	let tags: unknown;
	try {
		tags = (JSON.parse(listed) as Record<string, unknown>).Tags;
	} catch (err) {
		throw new PinError(
			`could not parse tag list: ${String(err)}`,
			EXIT.registryFailed,
		);
	}
	if (!Array.isArray(tags)) {
		throw new PinError("registry returned no tag list", EXIT.registryFailed);
	}

	const candidates = tags
		.filter((t): t is string => typeof t === "string" && t.startsWith("git-"))
		.reverse();
	for (const tag of candidates) {
		if ((await resolvedDigest(`${AGENT_REPO}:${tag}`)) === target) return tag;
	}
	throw new PinError(
		`no git-<sha12> tag resolves to the same manifest as :${DISCOVERY_TAG} (${target}) across ${candidates.length} candidate tag(s); the publish lane asserts the pair shares a digest, so this means an incoherent publish`,
		EXIT.digestMismatch,
	);
}

function readLock(): PinLock | undefined {
	let body: string;
	try {
		body = readFileSync(LOCK_PATH, "utf8");
	} catch {
		return undefined;
	}
	try {
		return validatePin(JSON.parse(body));
	} catch (err) {
		if (err instanceof PinError) throw err;
		throw new PinError(
			`${LOCK_PATH} is not valid JSON: ${String(err)}`,
			EXIT.badLock,
		);
	}
}

async function pin(tag: string): Promise<number> {
	const { digest, manifest } = await inspect(`${AGENT_REPO}:${tag}`);
	const next = lockFromInspect(AGENT_REPO, tag, digest, manifest);

	const current = readLock();
	if (current && locksEqual(current, next)) {
		console.log(`agent-oci.lock already pins ${tag} (${digest}) — no change`);
		return 0;
	}

	writeFileSync(LOCK_PATH, renderLock(next));
	console.log(
		`pinned ${AGENT_REPO}:${tag}\n  digest: ${digest}\n  layers: ${next.layers.length}`,
	);
	return 0;
}

function usage(): number {
	console.error(
		"usage: bun tools/guest-image/pin-agent-image.ts --tag git-<sha12>\n" +
			"       bun tools/guest-image/pin-agent-image.ts --relock",
	);
	return EXIT.usage;
}

async function main(): Promise<number> {
	const argv = process.argv.slice(2);

	if (argv.includes("--relock")) {
		if (argv.length !== 1) return usage();
		// Self-gate: a relock on a branch with no lock is a no-op, so the task
		// stays cheap on every unrelated Renovate branch.
		if (!readLock()) {
			console.log("no agent-oci.lock on this branch — nothing to relock");
			return 0;
		}
		return await pin(await discoverBuildTag());
	}

	const i = argv.indexOf("--tag");
	const tag = i === -1 ? undefined : argv[i + 1];
	if (tag === undefined || argv.length !== 2) return usage();
	return await pin(tag);
}

try {
	process.exit(await main());
} catch (err) {
	if (err instanceof PinError) {
		console.error(`pin-agent-image: ${err.message}`);
		process.exit(err.code);
	}
	throw err;
}
