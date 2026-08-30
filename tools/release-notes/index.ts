#!/usr/bin/env bun
// release-notes (T2) — the Release body + nix-outputs manifest generator.
//
// PURE CORE: `assemble(input)` translates the gathered facts (sha, version,
// tag, asset list, the GHCR image digest OR null, and the parsed nix path-info
// identity) into the Release body markdown + the nix-outputs manifest object.
// It is a pure function: no I/O, no skopeo/nix/git invocation, no clock, no
// `process`/`env`/`Bun` access. A null image digest is not a failure — it
// DEGRADES to a recorded-absence line in the body (the image lane is
// paths-filtered independently and may not have run for a go-only push).
//
// THE EDGE: `main()` (guarded by `import.meta.main`) parses argv, gathers the
// inputs (queries GHCR with the fork skopeo, runs `nix path-info` over the
// toolchain `langs` set), calls the pure core, and — unless `--dry-run` — writes
// the body + manifest files. Guarding behind `import.meta.main` lets the test
// import the pure core without firing the edge.

import { $ } from "bun";

// ── Pure-core types ────────────────────────────────────────────────────────

/** One nix output's identity, as `nix path-info --json` reports it. */
export type NixOutput = {
	/** the derivation/output name (e.g. "go", "bun", "agent-image-spec") */
	name: string;
	/** the store path */
	path: string;
	/** the NAR hash (present for a realised path; null if unknown) */
	narHash: string | null;
};

/** The GHCR image identity for the sha, or null when not yet published. */
export type ImageIdentity = {
	/** the pullable ref by digest, e.g. "ghcr.io/rigelbuild/compass-agent@sha256:…" */
	ref: string;
	/** the config digest, e.g. "sha256:…" */
	digest: string;
};

/** The input the pure core receives. */
export type AssembleInput = {
	/** the 12-hex short sha this build was cut from */
	sha: string;
	/** the one version string stamped into every binary (e.g. "0.1.0+g<sha12>") */
	version: string;
	/** the Release name/tag (e.g. "build-<sha12>") */
	tag: string;
	/** the binary + checksum asset filenames attached to the Release */
	assets: string[];
	/** the GHCR image identity, or null when the image is not yet published */
	image: ImageIdentity | null;
	/** the nix build outputs (toolchain langs set + optional image spec) */
	nixOutputs: NixOutput[];
};

/** The manifest written to nix-outputs.json. */
export type NixManifest = {
	sha: string;
	version: string;
	tag: string;
	outputs: NixOutput[];
};

export type AssembleOutput = {
	/** the Release body markdown */
	body: string;
	/** the object serialised to nix-outputs.json */
	manifest: NixManifest;
};

// ── Pure core ────────────────────────────────────────────────────────────────

/** The line recorded when the image is not yet published for this build. */
export const IMAGE_ABSENT_LINE = "image: not yet published for this build";

/**
 * Assemble the Release body markdown + the nix-outputs manifest object.
 * Pure — no I/O. A null `image` degrades to IMAGE_ABSENT_LINE, never a throw.
 */
export function assemble(input: AssembleInput): AssembleOutput {
	const lines: string[] = [];

	lines.push(`# ${input.tag}`);
	lines.push("");
	lines.push(`Version: \`${input.version}\``);
	lines.push(`Commit: \`${input.sha}\``);
	lines.push("");

	// Image identity — a durable pointer to the immutable GHCR artifact, or a
	// recorded absence (Fork 2(ii)). Never a failure: the image lane is
	// paths-filtered independently of this lane.
	lines.push("## Container image");
	lines.push("");
	if (input.image === null) {
		lines.push(IMAGE_ABSENT_LINE);
	} else {
		lines.push(`image: \`${input.image.ref}\``);
		lines.push(`digest: \`${input.image.digest}\``);
	}
	lines.push("");

	// Binaries — what consumers download; verify against SHA256SUMS.
	lines.push("## Assets");
	lines.push("");
	for (const asset of input.assets) {
		lines.push(`- \`${asset}\``);
	}
	lines.push("");

	// Nix build-output identity — the verifiable statement of which outputs this
	// build produced, without shipping the closure (Fork 2(iii)).
	lines.push("## Nix outputs");
	lines.push("");
	if (input.nixOutputs.length === 0) {
		lines.push("(none recorded)");
	} else {
		for (const out of input.nixOutputs) {
			const hash = out.narHash ?? "(unknown)";
			lines.push(`- \`${out.name}\`: \`${out.path}\` (${hash})`);
		}
	}
	lines.push("");
	lines.push(
		"The machine-readable manifest is attached as `nix-outputs.json`.",
	);
	lines.push("");

	const manifest: NixManifest = {
		sha: input.sha,
		version: input.version,
		tag: input.tag,
		outputs: input.nixOutputs,
	};

	return { body: `${lines.join("\n")}\n`, manifest };
}

// ── The edge (impure) ──────────────────────────────────────────────────────

/** The GHCR repo the agent image publishes to (release.yml publish-image). */
const IMAGE_REPO = "ghcr.io/rigelbuild/compass-agent";

type Args = {
	sha: string;
	version: string;
	tag: string;
	assets: string[];
	bodyOut: string;
	manifestOut: string;
	dryRun: boolean;
};

/** Parse argv into the edge's inputs. Repeated `--asset` accumulates. */
export function parseArgs(argv: string[]): Args {
	const args: Args = {
		sha: "",
		version: "",
		tag: "",
		assets: [],
		bodyOut: "RELEASE_BODY.md",
		manifestOut: "nix-outputs.json",
		dryRun: false,
	};
	for (let i = 0; i < argv.length; i++) {
		const flag = argv[i];
		if (flag === "--dry-run") {
			args.dryRun = true;
			continue;
		}
		const value = argv[++i];
		if (value === undefined) {
			throw new Error(`release-notes: flag ${flag} needs a value`);
		}
		switch (flag) {
			case "--sha":
				args.sha = value;
				break;
			case "--version":
				args.version = value;
				break;
			case "--tag":
				// --tag is opaque passthrough: it now carries the semver `vX.Y.Z`
				// (release) rather than the old `git-<sha>`/`build-<sha>`, but
				// nothing parses or assumes its shape — assemble() only prints it
				// — so no shape-specific handling is needed here.
				args.tag = value;
				break;
			case "--asset":
				args.assets.push(value);
				break;
			case "--body-out":
				args.bodyOut = value;
				break;
			case "--manifest-out":
				args.manifestOut = value;
				break;
			default:
				throw new Error(`release-notes: unknown flag ${flag}`);
		}
	}
	if (args.sha === "" || args.version === "" || args.tag === "") {
		throw new Error("release-notes: --sha, --version, and --tag are required");
	}
	return args;
}

/**
 * Decide the image identity from a raw `skopeo inspect` result — the
 * load-bearing branch of the skopeo-provisioning fix, kept pure so it is
 * unit-tested (the edge that runs skopeo cannot be exercised where skopeo is
 * always present):
 *  - exit 127 => THROW: skopeo is not on PATH, a workflow bootstrap regression;
 *    fail LOUD so a missing tool can never masquerade as an absent image tag.
 *  - any other non-zero => null: a 404 for an unpublished tag or a transient
 *    transport error DEGRADES (the image lane is paths-filtered independently,
 *    and a re-run converges the pointer once the image publishes).
 *  - exit 0 but unparseable output or no `.config.digest` => null.
 *  - exit 0 with a digest => the pullable @digest ref + the digest.
 */
export function classifyImageResult(result: {
	exitCode: number;
	stdout: string;
	stderr: string;
}): ImageIdentity | null {
	if (result.exitCode === 127) {
		throw new Error(
			`release-notes: skopeo not found on PATH (exit 127); the workflow must provision the fork skopeo before generating the release body. stderr: ${result.stderr.trim()}`,
		);
	}
	if (result.exitCode !== 0) {
		return null;
	}
	let digest: string;
	try {
		const raw = JSON.parse(result.stdout) as {
			config?: { digest?: string };
		};
		digest = raw.config?.digest ?? "";
	} catch {
		return null;
	}
	if (digest === "") {
		return null;
	}
	return { ref: `${IMAGE_REPO}@${digest}`, digest };
}

/**
 * At release time a null image is a hard failure, not a degradation: a
 * published `vX.Y.Z` with no resolvable container image violates the
 * one-version-spans-the-product invariant (§A2). The dry-run/preview path keeps
 * the nullable contract — assemble() still degrades to IMAGE_ABSENT_LINE there.
 * Kept pure and exported so the edge stays a thin caller and this posture is
 * unit-tested. Returns the release-time error message, or null when the image
 * is acceptable (present, or absent-but-dry-run).
 */
export function requireImageAtRelease(
	image: ImageIdentity | null,
	dryRun: boolean,
): string | null {
	if (dryRun || image !== null) {
		return null;
	}
	return "release-notes: no resolvable container image for this release (:vX.Y.Z requires the per-push :git-<sha> image to have published; the release-image job digest-re-tags it). Refusing to generate release notes with a recorded-absence line for a real release.";
}

/**
 * Query GHCR for the image config digest at :git-<sha12>, exactly as
 * release.yml's publish-image verify does (`skopeo inspect --raw … | jq -r
 * .config.digest`). Returns null when the tag is not published — the image lane
 * is paths-filtered independently, so a go-only push has no image for its sha.
 */
async function gatherImage(sha: string): Promise<ImageIdentity | null> {
	const ref = `${IMAGE_REPO}:git-${sha}`;
	const result = await $`skopeo inspect --raw docker://${ref}`
		.nothrow()
		.quiet();
	return classifyImageResult({
		exitCode: result.exitCode,
		stdout: result.stdout.toString(),
		stderr: result.stderr.toString(),
	});
}

/** The `nix path-info --json` record shape (the fields the manifest reads). */
type PathInfoEntry = { path: string; narHash?: string };

/**
 * Resolve the toolchain `langs` set to store paths and run `nix path-info` over
 * them, mapping each language name to its output identity.
 */
async function gatherNixOutputs(): Promise<NixOutput[]> {
	const langsJson =
		await $`nix eval --json -f tools/toolchain/gate-tools.nix langs`
			.quiet()
			.text();
	const langs = JSON.parse(langsJson) as Record<string, { store?: string }>;

	const outputs: NixOutput[] = [];
	for (const name of Object.keys(langs).sort()) {
		const store = langs[name]?.store;
		if (store === undefined || store === "") {
			continue;
		}
		const infoJson = await $`nix path-info --json ${store}`.quiet().text();
		const info = JSON.parse(infoJson) as
			| PathInfoEntry[]
			| Record<string, PathInfoEntry>;
		// nix path-info emits an array (newer nix) or an object keyed by path.
		const entries: PathInfoEntry[] = Array.isArray(info)
			? info
			: Object.entries(info).map(([path, v]) => ({ path, ...v }));
		// A single-store-path query returns exactly one entry; anything else means
		// `store` did not resolve to one output path and picking [0] would record
		// an arbitrary identity — fail loud rather than ship a wrong manifest entry.
		if (entries.length !== 1) {
			throw new Error(
				`release-notes: nix path-info for ${name} (${store}) returned ${entries.length} entries, expected exactly 1`,
			);
		}
		const entry = entries[0];
		outputs.push({
			name,
			path: entry?.path ?? store,
			narHash: entry?.narHash ?? null,
		});
	}
	return outputs;
}

async function main(): Promise<void> {
	const args = parseArgs(process.argv.slice(2));

	const image = await gatherImage(args.sha);
	const releaseError = requireImageAtRelease(image, args.dryRun);
	if (releaseError !== null) {
		throw new Error(releaseError);
	}
	const nixOutputs = await gatherNixOutputs();

	const { body, manifest } = assemble({
		sha: args.sha,
		version: args.version,
		tag: args.tag,
		assets: args.assets,
		image,
		nixOutputs,
	});

	if (args.dryRun) {
		console.log("=== release body ===");
		console.log(body);
		console.log("=== nix-outputs.json ===");
		console.log(JSON.stringify(manifest, null, 2));
		return;
	}

	await Bun.write(args.bodyOut, body);
	await Bun.write(args.manifestOut, `${JSON.stringify(manifest, null, 2)}\n`);
	console.log(`release-notes: wrote ${args.bodyOut} + ${args.manifestOut}`);
}

if (import.meta.main) {
	await main();
}
