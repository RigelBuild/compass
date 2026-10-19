import { describe, expect, mock, test } from "bun:test";
import {
	assertCoherent,
	assertNoClosureChange,
	classifyProbe,
	closureChanges,
	copyArgv,
	EXIT,
	inspectArgv,
	MAX_WALK,
	mainTag,
	manifestIdentity,
	type ProbeResult,
	RetagError,
	releaseTag,
	resolveAncestor,
	resolveMainPair,
	sha12,
	splitNulPaths,
	tagMainPair,
} from "./retag-core.ts";

describe("registry argv", () => {
	test("inspect uses anonymous credentials", () => {
		expect(inspectArgv("ghcr.io/x/y:git-abcdef012345")).toEqual([
			"inspect",
			"--no-creds",
			"--raw",
			"docker://ghcr.io/x/y:git-abcdef012345",
		]);
	});

	test("copy uses anonymous source and destination auth, never a shared authfile", () => {
		const argv = copyArgv(
			"ghcr.io/x/y@sha256:abc",
			"ghcr.io/x/y:v1.2.3",
			"/auth.json",
		);
		expect(argv).toEqual([
			"copy",
			"--multi-arch",
			"all",
			"--preserve-digests",
			"--src-no-creds",
			"--dest-authfile",
			"/auth.json",
			"docker://ghcr.io/x/y@sha256:abc",
			"docker://ghcr.io/x/y:v1.2.3",
		]);
		expect(argv).not.toContain("--authfile");
	});
});

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

describe("tagMainPair", () => {
	test("resolves the whole pair before writing, server first", async () => {
		const events: string[] = [];
		const resolved = {
			runner: {
				commit: commit("aaaa"),
				sha12: "aaaa00000000",
				position: 1,
				raw: manifest(CONFIG_A),
			},
			server: {
				commit: commit("bbbb"),
				sha12: "bbbb00000000",
				position: 1,
				raw: manifest(CONFIG_B),
			},
		};
		await tagMainPair({
			resolve: async () => {
				events.push("resolve");
				return resolved;
			},
			write: async (lane) => {
				events.push(lane);
			},
		});
		expect(events).toEqual(["resolve", "server", "runner"]);
	});

	test("does not write when pair resolution rejects a closure change", async () => {
		const write = mock(async (_lane: "server" | "runner") => {});
		const ancestors = [commit("aaaa"), commit("bbbb")];
		await expect(
			tagMainPair({
				resolve: () =>
					resolveMainPair({
						ancestors,
						probe: {
							runner: async (short) =>
								short === "aaaa00000000"
									? absentFor(`git-${short}`)
									: found(manifest(CONFIG_A)),
							server: async () => found(manifest(CONFIG_B)),
						},
						changedSince: async (commit) =>
							commit === ancestors[1] ? ["go/cmd/compass-runner/main.go"] : [],
						closurePaths: { runner: CLOSURE, server: CLOSURE },
					}),
				write,
			}),
		).rejects.toThrow("predates runner closure changes");
		expect(write).not.toHaveBeenCalled();
	});
});
describe("mainTag", () => {
	test("uses the first-parent count and fixed sha12", () => {
		expect(mainTag(1638, commit("abcdef123456"))).toBe(
			"main-1638-abcdef123456",
		);
	});

	test.each([
		[0, commit("abcdef123456")],
		[1.5, commit("abcdef123456")],
		[1638, "abcdef123456"],
	])("rejects invalid count or commit: %s", async (count, sha) => {
		await expectRetagError(() => mainTag(count, sha), EXIT.usage);
	});
});

describe("resolveMainPair", () => {
	const ancestors = [commit("aaaa"), commit("bbbb")];
	test("resolves both lane images published at the head", async () => {
		const closurePaths = { runner: CLOSURE, server: CLOSURE };
		const pair = await resolveMainPair({
			ancestors,
			probe: {
				runner: async () => found(manifest(CONFIG_A)),
				server: async () => found(manifest(CONFIG_B)),
			},
			changedSince: async () => [],
			closurePaths,
		});
		expect(pair.runner.position).toBe(1);
		expect(pair.server.position).toBe(1);
	});

	test("resolves an ancestor when its lane closure is unchanged", async () => {
		const closurePaths = { runner: CLOSURE, server: CLOSURE };
		const pair = await resolveMainPair({
			ancestors,
			probe: {
				runner: async (short) =>
					short === "aaaa00000000"
						? absentFor(`git-${short}`)
						: found(manifest(CONFIG_A)),
				server: async () => found(manifest(CONFIG_B)),
			},
			changedSince: async () => ["CHANGELOG.md"],
			closurePaths,
		});
		expect(pair.runner.commit).toBe(commit("bbbb"));
		expect(pair.runner.position).toBe(2);
	});
	test("rejects an ancestor when its lane closure changed", async () => {
		const closurePaths = { runner: CLOSURE, server: CLOSURE };
		await expectRetagError(
			() =>
				resolveMainPair({
					ancestors,
					probe: {
						runner: async (short) =>
							short === "aaaa00000000"
								? absentFor(`git-${short}`)
								: found(manifest(CONFIG_A)),
						server: async () => found(manifest(CONFIG_B)),
					},
					changedSince: async () => ["go/cmd/compass-runner/main.go"],
					closurePaths,
				}),
			EXIT.noAncestor,
		);
	});
});
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

	test("a public package denied anonymously with 403 remains ambiguous", () => {
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
		// over: treating every not-found response as absent could hide auth and repo faults.
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
			"runner",
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
			() => resolveAncestor([commit("aaaa")], async () => absent, "runner"),
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
					"runner",
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
				resolveAncestor(
					ancestors,
					async (tag) => {
						probes += 1;
						return tag === sha12(ancestors[MAX_WALK] ?? "")
							? found(manifest(CONFIG_A))
							: absentFor(`git-${tag}`);
					},
					"runner",
				),
			EXIT.noAncestor,
		);
		expect(probes).toBe(MAX_WALK);
		expect(err.message).toContain(`${MAX_WALK}`);
		expect(err.message).toContain("publish-runner-image");
	});

	test("the remediation names its HEAD-of-main precondition", async () => {
		const err = await expectRetagError(
			() => resolveAncestor([], async () => found("{}"), "runner"),
			EXIT.noAncestor,
		);
		expect(err.message).toContain("still main's HEAD");
	});

	test("the MAX_WALK-th ancestor is still in range", async () => {
		const ancestors = Array.from({ length: MAX_WALK }, (_, i) =>
			commit(i.toString(16).padStart(4, "0")),
		);
		const last = sha12(ancestors[MAX_WALK - 1] ?? "");
		const result = await resolveAncestor(
			ancestors,
			async (tag) =>
				tag === last ? found(manifest(CONFIG_A)) : absentFor(`git-${tag}`),
			"runner",
		);
		expect(result.position).toBe(MAX_WALK);
	});

	test("an empty walk fails as no ancestor", async () => {
		await expectRetagError(
			() => resolveAncestor([], async () => found("{}"), "runner"),
			EXIT.noAncestor,
		);
	});
});

describe("manifestIdentity", () => {
	test("digests the exact manifest bytes and reads the config digest", () => {
		const raw = manifest(CONFIG_A);
		const expected = `sha256:${new Bun.CryptoHasher("sha256").update(raw).digest("hex")}`;
		expect(manifestIdentity(raw)).toEqual({
			kind: "manifest",
			manifestDigest: expected,
			configDigest: CONFIG_A,
		});
	});

	test("returns both index members with their platforms and digests", () => {
		const raw = JSON.stringify({
			schemaVersion: 2,
			mediaType: "application/vnd.oci.image.index.v1+json",
			manifests: [
				{ digest: CONFIG_A, platform: { os: "linux", architecture: "amd64" } },
				{ digest: CONFIG_B, platform: { os: "linux", architecture: "arm64" } },
			],
		});
		const expected = `sha256:${new Bun.CryptoHasher("sha256").update(raw).digest("hex")}`;
		expect(manifestIdentity(raw)).toEqual({
			kind: "index",
			manifestDigest: expected,
			members: [
				{ platform: "linux/amd64", digest: CONFIG_A },
				{ platform: "linux/arm64", digest: CONFIG_B },
			],
		});
	});

	test("rejects an index missing linux/arm64", async () => {
		const raw = JSON.stringify({
			schemaVersion: 2,
			mediaType: "application/vnd.oci.image.index.v1+json",
			manifests: [
				{ digest: CONFIG_A, platform: { os: "linux", architecture: "amd64" } },
			],
		});
		await expectRetagError(() => manifestIdentity(raw), EXIT.badManifest);
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
	test("fails when an index member digest changes", async () => {
		const sourceRaw = JSON.stringify({
			schemaVersion: 2,
			mediaType: "application/vnd.oci.image.index.v1+json",
			manifests: [
				{ digest: CONFIG_A, platform: { os: "linux", architecture: "amd64" } },
				{ digest: CONFIG_B, platform: { os: "linux", architecture: "arm64" } },
			],
		});
		const changedRaw = JSON.stringify({
			schemaVersion: 2,
			mediaType: "application/vnd.oci.image.index.v1+json",
			manifests: [
				{ digest: CONFIG_A, platform: { os: "linux", architecture: "amd64" } },
				{
					digest: `sha256:${"c".repeat(64)}`,
					platform: { os: "linux", architecture: "arm64" },
				},
			],
		});
		await expectRetagError(
			() =>
				assertCoherent(
					manifestIdentity(sourceRaw),
					manifestIdentity(changedRaw),
				),
			EXIT.incoherent,
		);
	});
});

// A subset of the workflow's RUNNER_IMAGE_CLOSURE_PATHS shape, as the env var
// delivers it: both entry kinds (`/**` prefix, exact file), not a mirror.
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
			closureChanges(
				CLOSURE,
				["docs/x.md", "go/cmd/compass-runner/main.go"],
				"runner",
			),
		).toEqual(["go/cmd/compass-runner/main.go"]);
	});

	test("an exact entry matches only that path", () => {
		expect(
			closureChanges(CLOSURE, ["flake.nix", "flake.nix.bak"], "runner"),
		).toEqual(["flake.nix"]);
	});

	test("a /** prefix does not match a sibling that shares its name", () => {
		expect(
			closureChanges(
				CLOSURE,
				["runner-image-extra/a", "tools/runner-imagex"],
				"runner",
			),
		).toEqual([]);
	});

	test("a release commit touching only version files is not a closure change", () => {
		expect(
			closureChanges(
				CLOSURE,
				[".release-please-manifest.json", "CHANGELOG.md", "version.txt"],
				"runner",
			),
		).toEqual([]);
	});

	test("an empty closure set is refused rather than matching nothing", async () => {
		await expectRetagError(
			() => closureChanges(" \n\n", ["runner-image/Dockerfile"], "runner"),
			EXIT.usage,
		);
	});

	test("names the server closure when its path set is empty", () => {
		expect(() => closureChanges(" \n\n", [], "server")).toThrow(
			"the server closure path set is empty",
		);
	});

	test("reports server closure changes and recovery action", () => {
		expect(() =>
			assertNoClosureChange(
				"go/server/**",
				"bbbb00000000",
				["go/server/serve.go"],
				"server",
			),
		).toThrow("predates server closure changes it lacks");
	});

	test("server release remediation names the server publish job", async () => {
		await expect(
			resolveAncestor([], async () => found("{}"), "server"),
		).rejects.toThrow("publish-server-image");
	});
});

describe("splitNulPaths", () => {
	test("splits NUL-terminated names and drops the trailing empty entry", () => {
		expect(splitNulPaths("a/b.go\0runner-image/Dockerfile\0")).toEqual([
			"a/b.go",
			"runner-image/Dockerfile",
		]);
	});

	test("keeps a name with a space or a newline whole", () => {
		// -z prints names raw, so a newline is part of the name, not a separator.
		expect(splitNulPaths("docs/a b.md\0runner-image/odd\nname\0")).toEqual([
			"docs/a b.md",
			"runner-image/odd\nname",
		]);
	});

	test("an empty diff is no names", () => {
		expect(splitNulPaths("")).toEqual([]);
	});
});
describe("assertNoClosureChange", () => {
	test("passes when nothing between source and release is in the closure", () => {
		expect(() =>
			assertNoClosureChange(
				CLOSURE,
				"bbbb00000000",
				["CHANGELOG.md"],
				"runner",
			),
		).not.toThrow();
	});

	test("fails as no-ancestor, naming the skipped closure paths", async () => {
		const err = await expectRetagError(
			() =>
				assertNoClosureChange(
					CLOSURE,
					"bbbb00000000",
					["CHANGELOG.md", "runner-image/Dockerfile"],
					"runner",
				),
			EXIT.noAncestor,
		);
		expect(err.message).toContain("runner-image/Dockerfile");
		expect(err.message).toContain("bbbb00000000");
		expect(err.message).toContain("still main's HEAD");
	});
});
