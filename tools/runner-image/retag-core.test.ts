import { describe, expect, test } from "bun:test";
import {
	assertCoherent,
	classifyProbe,
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
const absent: ProbeResult = {
	exitCode: 1,
	stdout: "",
	stderr:
		'time=x level=fatal msg="Error parsing image name: reading manifest git-aaaaaaaaaaaa in ghcr.io/rigelbuild/compass-runner: manifest unknown"',
};
const timeout: ProbeResult = {
	exitCode: 1,
	stdout: "",
	stderr:
		'level=fatal msg="Error parsing image name: pinging container registry ghcr.io: Get \\"https://ghcr.io/v2/\\": dial tcp: i/o timeout"',
};

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
		expect(classifyProbe(found("{}"))).toEqual({ kind: "found", raw: "{}" });
	});

	test("manifest unknown is a definitive absence", () => {
		expect(classifyProbe(absent).kind).toBe("absent");
	});

	test("an HTTP 404 is a definitive absence", () => {
		expect(
			classifyProbe({
				exitCode: 1,
				stdout: "",
				stderr: "received unexpected HTTP status: 404 Not Found",
			}).kind,
		).toBe("absent");
	});

	test("a transport fault is ambiguous, never absent", () => {
		expect(classifyProbe(timeout).kind).toBe("ambiguous");
	});

	test("a registry 5xx is ambiguous", () => {
		expect(
			classifyProbe({
				exitCode: 1,
				stdout: "",
				stderr: "received unexpected HTTP status: 503 Service Unavailable",
			}).kind,
		).toBe("ambiguous");
	});

	test("an auth failure is ambiguous", () => {
		expect(
			classifyProbe({
				exitCode: 1,
				stdout: "",
				stderr: "reading manifest git-x: unauthorized: authentication required",
			}).kind,
		).toBe("ambiguous");
	});

	test("a 404 inside the probed sha12 does not read as an absence", () => {
		// The tag name is echoed in skopeo's error text, and a sha12 can contain
		// the digits 404. Matching a bare substring would turn this transport
		// fault into "absent" and let the walk fall through to a stale ancestor.
		expect(
			classifyProbe({
				exitCode: 1,
				stdout: "",
				stderr:
					'reading manifest git-ab404cd00000 in ghcr.io/rigelbuild/compass-runner: Get "https://ghcr.io/v2/rigelbuild/compass-runner/manifests/git-ab404cd00000": dial tcp: i/o timeout',
			}).kind,
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
				return tag === "bbbb00000000" ? found(manifest(CONFIG_A)) : absent;
			},
		);
		expect(result).toEqual({
			sha12: "bbbb00000000",
			position: 2,
			raw: manifest(CONFIG_A),
		});
		// Stops at the first hit; never probes an older ancestor.
		expect(probed).toEqual(["aaaa00000000", "bbbb00000000"]);
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
						: absent;
				}),
			EXIT.noAncestor,
		);
		expect(probes).toBe(MAX_WALK);
		expect(err.message).toContain(`${MAX_WALK}`);
		expect(err.message).toContain("publish-runner-image");
	});

	test("the MAX_WALK-th ancestor is still in range", async () => {
		const ancestors = Array.from({ length: MAX_WALK }, (_, i) =>
			commit(i.toString(16).padStart(4, "0")),
		);
		const last = sha12(ancestors[MAX_WALK - 1] ?? "");
		const result = await resolveAncestor(ancestors, async (tag) =>
			tag === last ? found("{}") : absent,
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
