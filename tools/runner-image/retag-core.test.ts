import { describe, expect, test } from "bun:test";
import {
	assertCoherent,
	assertNoClosureChange,
	classifyProbe,
	closureChanges,
	EXIT,
	MAX_WALK,
	manifestIdentity,
	type ProbeResult,
	RetagError,
	releaseTag,
	resolveAncestor,
	sha12,
} from "./retag-core.ts";

const CONFIG_A =
	"sha256:2125c2a158a8329c40ac7c1daa8909f09fe82a5231ff0df05319d0be4d4c4bb6";
const CONFIG_B =
	"sha256:3125c2a158a8329c40ac7c1daa8909f09fe82a5231ff0df05319d0be4d4c4bb6";

function manifest(configDigest: string): string {
	return JSON.stringify({
		schemaVersion: 2,
		mediaType: "application/vnd.oci.image.manifest.v1+json",
		config: { digest: configDigest },
	});
}

/** A 40-hex commit sha whose first 12 chars are `prefix` padded with `0`. */
function commit(prefix: string): string {
	return prefix.padEnd(40, "0");
}

const found = (stdout: string): ProbeResult => ({
	exitCode: 0,
	stdout,
	stderr: "",
});

/** Real skopeo 1.23.0 `inspect --raw` stderr, captured verbatim with the time
 * field fixed and the probed tag substituted. */
function skopeoFatal(tag: string, repo: string, reason: string): ProbeResult {
	return {
		exitCode: 1,
		stdout: "",
		stderr: `time="2026-09-25T22:37:37-04:00" level=fatal msg="Error parsing image name \\"docker://${repo}:${tag}\\": ${reason}"`,
	};
}
const RUNNER = "ghcr.io/rigelbuild/compass-runner";
const TAG = "git-aaaaaaaaaaaa";
// A missing tag on a reachable package (captured against the public agent package).
const absentFor = (tag: string): ProbeResult =>
	skopeoFatal(
		tag,
		RUNNER,
		`reading manifest ${tag} in ${RUNNER}: manifest unknown`,
	);
const absent = absentFor(TAG);
const timeout = skopeoFatal(
	TAG,
	RUNNER,
	`fetching manifest ${TAG} in ${RUNNER}: pinging container registry ghcr.io: Get \\"https://ghcr.io/v2/\\": dial tcp 140.82.112.33:443: i/o timeout`,
);

async function expectRetagError(
	run: () => Promise<unknown> | unknown,
	code: number,
): Promise<RetagError> {
	try {
		await run();
	} catch (err) {
		expect(err).toBeInstanceOf(RetagError);
		if (!(err instanceof RetagError)) throw err;
		expect(err.code).toBe(code);
		return err;
	}
	throw new Error("expected a RetagError");
}

describe("classifyProbe", () => {
	test("a zero exit is found and carries the manifest bytes", () => {
		expect(classifyProbe(found("{}"), TAG)).toEqual({
			kind: "found",
			raw: "{}",
		});
	});

	test("a missing tag (manifest unknown) is a definitive absence", () => {
		expect(classifyProbe(absent, TAG).kind).toBe("absent");
	});

	test("manifest unknown with a registry-appended detail is still absent", () => {
		// mcr.microsoft.com appends its own reason after the error code.
		const repo = "mcr.microsoft.com/x";
		expect(
			classifyProbe(
				skopeoFatal(
					TAG,
					repo,
					`reading manifest ${TAG} in ${repo}: manifest unknown: manifest tagged by \\"${TAG}\\" is not found`,
				),
				TAG,
			).kind,
		).toBe("absent");
	});

	test("a bare 404 on the probed tag's manifest read is absent", () => {
		const repo = "127.0.0.1:5056/rigelbuild/compass-runner";
		expect(
			classifyProbe(
				skopeoFatal(
					TAG,
					repo,
					`reading manifest ${TAG} in ${repo}: StatusCode: 404, \\"\\"`,
				),
				TAG,
			).kind,
		).toBe("absent");
	});

	test("an absence reported for a different tag is ambiguous", () => {
		// Only an answer about the tag this probe asked for may skip an ancestor.
		expect(classifyProbe(absent, "git-bbbbbbbbbbbb").kind).toBe("ambiguous");
	});

	test("a router 404 page is ambiguous, not a missing tag", () => {
		// A plain-text body means a proxy or a wrong path answered, not the
		// registry's manifest store; every ancestor would read absent.
		const repo = "127.0.0.1:5056/plain/x";
		expect(
			classifyProbe(
				skopeoFatal(
					TAG,
					repo,
					`reading manifest ${TAG} in ${repo}: StatusCode: 404, \\"404 page not found\\"`,
				),
				TAG,
			).kind,
		).toBe("ambiguous");
	});

	test("repository not found is ambiguous", () => {
		const repo = "127.0.0.1:5056/reponf/x";
		expect(
			classifyProbe(
				skopeoFatal(
					TAG,
					repo,
					`reading manifest ${TAG} in ${repo}: name unknown: repository not found`,
				),
				TAG,
			).kind,
		).toBe("ambiguous");
	});

	test("a private package without auth (403 Forbidden) is ambiguous", () => {
		expect(
			classifyProbe(
				skopeoFatal(
					TAG,
					RUNNER,
					`fetching manifest ${TAG} in ${RUNNER}: Requesting bearer token: received unexpected HTTP status: 403 Forbidden`,
				),
				TAG,
			).kind,
		).toBe("ambiguous");
	});

	test("a generic 'not found' stays ambiguous", () => {
		// The agent lane's broad `*"not found"*` belt is deliberately not carried
		// over: on this private package it would read auth and repo faults as absent.
		expect(
			classifyProbe(
				skopeoFatal(
					TAG,
					RUNNER,
					`fetching manifest ${TAG} in ${RUNNER}: blob not found`,
				),
				TAG,
			).kind,
		).toBe("ambiguous");
	});

	test("a transport fault is ambiguous, never absent", () => {
		expect(classifyProbe(timeout, TAG).kind).toBe("ambiguous");
	});

	test("a 404 inside the probed sha12 does not read as an absence", () => {
		// The tag is echoed in skopeo's error text, and a sha12 can contain 404.
		const tag = "git-ab404cd00000";
		expect(
			classifyProbe(
				skopeoFatal(
					tag,
					RUNNER,
					`fetching manifest ${tag} in ${RUNNER}: pinging container registry ghcr.io: dial tcp: i/o timeout`,
				),
				tag,
			).kind,
		).toBe("ambiguous");
	});
});

describe("sha12", () => {
	test("truncates a full sha to exactly 12 chars", () => {
		expect(sha12("0123456789abcdef0123456789abcdef01234567")).toBe(
			"0123456789ab",
		);
	});

	test("rejects anything but a full 40-hex sha", async () => {
		await expectRetagError(() => sha12("0123456789ab"), EXIT.usage);
		await expectRetagError(
			() => sha12("0123456789ABCDEF0123456789abcdef01234567"),
			EXIT.usage,
		);
	});
});

describe("releaseTag", () => {
	test("accepts a semver release tag", () => {
		expect(releaseTag("v1.2.3")).toBe("v1.2.3");
		expect(releaseTag("v0.10.0-rc.1")).toBe("v0.10.0-rc.1");
	});

	test("refuses the moving tag and any non-release tag", async () => {
		// :latest is owned by the per-push lane; a release mint must never move it.
		await expectRetagError(() => releaseTag("latest"), EXIT.usage);
		await expectRetagError(() => releaseTag("git-0123456789ab"), EXIT.usage);
		await expectRetagError(() => releaseTag("1.2.3"), EXIT.usage);
		await expectRetagError(() => releaseTag(""), EXIT.usage);
	});
});

describe("resolveAncestor", () => {
	test("takes the newest ancestor whose tag resolves", async () => {
		const probed: string[] = [];
		const result = await resolveAncestor(
			[commit("aaaa"), commit("bbbb"), commit("cccc")],
			async (tag) => {
				probed.push(tag);
				return tag === "bbbb00000000"
					? found(manifest(CONFIG_A))
					: absentFor(`git-${tag}`);
			},
		);
		expect(result).toEqual({
			commit: commit("bbbb"),
			sha12: "bbbb00000000",
			position: 2,
			raw: manifest(CONFIG_A),
		});
		// Stops at the first hit; never probes an older ancestor.
		expect(probed).toEqual(["aaaa00000000", "bbbb00000000"]);
	});

	test("a probe answered about another tag hard-fails the walk", async () => {
		// A registry echoing the wrong tag is not a definitive answer for this one.
		await expectRetagError(
			() => resolveAncestor([commit("aaaa")], async () => absent),
			EXIT.registryFailed,
		);
	});

	test("hard-fails on an ambiguous probe rather than walking past it", async () => {
		const probed: string[] = [];
		const err = await expectRetagError(
			() =>
				resolveAncestor(
					[commit("aaaa"), commit("bbbb"), commit("cccc")],
					async (tag) => {
						probed.push(tag);
						if (tag === "aaaa00000000") return timeout;
						return found(manifest(CONFIG_A));
					},
				),
			EXIT.registryFailed,
		);
		expect(err.message).toContain("aaaa00000000");
		expect(probed).toEqual(["aaaa00000000"]);
	});

	test("probes at most MAX_WALK ancestors, then fails naming the remediation", async () => {
		const ancestors = Array.from({ length: MAX_WALK + 5 }, (_, i) =>
			commit(i.toString(16).padStart(4, "0")),
		);
		let probes = 0;
		const err = await expectRetagError(
			() =>
				resolveAncestor(ancestors, async (tag) => {
					probes += 1;
					// The 51st ancestor would resolve; the cap must stop short of it.
					return tag === sha12(ancestors[MAX_WALK] ?? "")
						? found(manifest(CONFIG_A))
						: absentFor(`git-${tag}`);
				}),
			EXIT.noAncestor,
		);
		expect(probes).toBe(MAX_WALK);
		expect(err.message).toContain(`${MAX_WALK}`);
		expect(err.message).toContain("publish-runner-image");
	});

	test("the remediation names its HEAD-of-main precondition", async () => {
		// A main dispatch publishes main's HEAD, which is an ancestor only while
		// main has not moved past the release sha.
		const err = await expectRetagError(
			() => resolveAncestor([], async () => found("{}")),
			EXIT.noAncestor,
		);
		expect(err.message).toContain("still main's HEAD");
	});

	test("the MAX_WALK-th ancestor is still in range", async () => {
		const ancestors = Array.from({ length: MAX_WALK }, (_, i) =>
			commit(i.toString(16).padStart(4, "0")),
		);
		const last = sha12(ancestors[MAX_WALK - 1] ?? "");
		const result = await resolveAncestor(ancestors, async (tag) =>
			tag === last ? found("{}") : absentFor(`git-${tag}`),
		);
		expect(result.position).toBe(MAX_WALK);
	});

	test("an empty walk fails as no ancestor", async () => {
		await expectRetagError(
			() => resolveAncestor([], async () => found("{}")),
			EXIT.noAncestor,
		);
	});
});

describe("manifestIdentity", () => {
	test("digests the exact manifest bytes and reads the config digest", () => {
		const raw = manifest(CONFIG_A);
		const expected = `sha256:${new Bun.CryptoHasher("sha256").update(raw).digest("hex")}`;
		expect(manifestIdentity(raw)).toEqual({
			manifestDigest: expected,
			configDigest: CONFIG_A,
		});
	});

	test("a manifest without a sha256 config digest is malformed", async () => {
		// An image index has no top-level config; the lane contracts for one
		// single-platform manifest and refuses to guess a platform.
		await expectRetagError(
			() => manifestIdentity(JSON.stringify({ manifests: [] })),
			EXIT.badManifest,
		);
		await expectRetagError(
			() =>
				manifestIdentity(JSON.stringify({ config: { digest: "sha256:x" } })),
			EXIT.badManifest,
		);
		await expectRetagError(
			() => manifestIdentity("not json"),
			EXIT.badManifest,
		);
	});
});

describe("assertCoherent", () => {
	test("passes when the release tag carries the source's exact manifest", () => {
		const id = manifestIdentity(manifest(CONFIG_A));
		expect(() => assertCoherent(id, id)).not.toThrow();
	});

	test("fails on a config-digest mismatch", async () => {
		await expectRetagError(
			() =>
				assertCoherent(
					manifestIdentity(manifest(CONFIG_A)),
					manifestIdentity(manifest(CONFIG_B)),
				),
			EXIT.incoherent,
		);
	});

	test("fails when the config matches but the manifest bytes differ", async () => {
		// A rewritten manifest (e.g. recompressed layers) keeps the config but
		// changes the digest the release tag resolves to.
		const source = manifestIdentity(manifest(CONFIG_A));
		const rewritten = manifestIdentity(`${manifest(CONFIG_A)}\n`);
		await expectRetagError(
			() => assertCoherent(source, rewritten),
			EXIT.incoherent,
		);
	});
});

// The workflow's RUNNER_IMAGE_CLOSURE_PATHS block, as the env var delivers it.
const CLOSURE = `runner-image/**
tools/runner-image/**
go/go.mod
go/cmd/compass-runner/**
flake.nix
.github/workflows/release.yml
`;

describe("closureChanges", () => {
	test("a file under a /** prefix is a closure change", () => {
		expect(
			closureChanges(CLOSURE, ["docs/x.md", "go/cmd/compass-runner/main.go"]),
		).toEqual(["go/cmd/compass-runner/main.go"]);
	});

	test("an exact entry matches only that path", () => {
		expect(closureChanges(CLOSURE, ["flake.nix", "flake.nix.bak"])).toEqual([
			"flake.nix",
		]);
	});

	test("a /** prefix does not match a sibling that shares its name", () => {
		// `runner-image/**` must not claim `runner-image-extra/` or the bare dir.
		expect(
			closureChanges(CLOSURE, ["runner-image-extra/a", "tools/runner-imagex"]),
		).toEqual([]);
	});

	test("a release commit touching only version files is not a closure change", () => {
		// The exact file set release-please's release commit writes.
		expect(
			closureChanges(CLOSURE, [
				".release-please-manifest.json",
				"CHANGELOG.md",
				"version.txt",
			]),
		).toEqual([]);
	});

	test("an empty closure set is refused rather than matching nothing", async () => {
		// An unset env var would otherwise pass every diff silently.
		await expectRetagError(
			() => closureChanges(" \n\n", ["runner-image/Dockerfile"]),
			EXIT.usage,
		);
	});
});

describe("assertNoClosureChange", () => {
	test("passes when nothing between source and release is in the closure", () => {
		expect(() =>
			assertNoClosureChange(CLOSURE, "bbbb00000000", ["CHANGELOG.md"]),
		).not.toThrow();
	});

	test("fails as no-ancestor, naming the skipped closure paths", async () => {
		// The skipped commit's publish failed: re-tagging the older image would
		// ship a release without that closure change.
		const err = await expectRetagError(
			() =>
				assertNoClosureChange(CLOSURE, "bbbb00000000", [
					"CHANGELOG.md",
					"runner-image/Dockerfile",
				]),
			EXIT.noAncestor,
		);
		expect(err.message).toContain("runner-image/Dockerfile");
		expect(err.message).toContain("bbbb00000000");
		expect(err.message).toContain("still main's HEAD");
	});
});
