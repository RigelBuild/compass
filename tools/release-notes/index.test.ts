// Unit tests for the release-notes pure core (index.ts).
//
// These defend the generator's contract (design record §Plan T2, Fork 2):
// the image-present body carries the ref + digest; the image-ABSENT case
// DEGRADES to the recorded-absence line rather than failing; the nix-outputs
// manifest echoes the sha/version/tag and carries every output verbatim; and
// the one build version string is echoed into the body.
//
// Only the PURE core is exercised — the edge (skopeo / nix / file writes) is
// import.meta.main-guarded, so importing index.ts never runs it. No network.

import { describe, expect, test } from "bun:test";
import {
	type AssembleInput,
	assemble,
	classifyImageResult,
	IMAGE_ABSENT_LINE,
	type NixOutput,
	parseArgs,
	requireImageAtRelease,
} from "./index.ts";

const OUTPUTS: NixOutput[] = [
	{ name: "bun", path: "/nix/store/aaa-bun", narHash: "sha256-bun" },
	{ name: "go", path: "/nix/store/bbb-go", narHash: "sha256-go" },
];

function input(over: Partial<AssembleInput> = {}): AssembleInput {
	return {
		sha: "0123456789ab",
		version: "0.1.0+g0123456789ab",
		tag: "build-0123456789ab",
		assets: ["compass_build-0123456789ab_linux-amd64", "SHA256SUMS"],
		image: {
			ref: "ghcr.io/rigelbuild/compass-agent@sha256:dead",
			digest: "sha256:dead",
		},
		nixOutputs: OUTPUTS,
		...over,
	};
}

describe("image-present body — carries the ref and digest", () => {
	test("both the pullable ref and config digest appear in the body", () => {
		const { body } = assemble(input());
		expect(body).toContain(
			"image: `ghcr.io/rigelbuild/compass-agent@sha256:dead`",
		);
		expect(body).toContain("digest: `sha256:dead`");
		// The absence line must NOT appear when the image is present.
		expect(body).not.toContain(IMAGE_ABSENT_LINE);
	});
});

describe("image-absent degradation — a null image is a recorded absence, not a failure", () => {
	test("a null image emits the absence line and does not throw", () => {
		const { body } = assemble(input({ image: null }));
		expect(body).toContain(IMAGE_ABSENT_LINE);
		// No dangling digest/ref lines leak through.
		expect(body).not.toContain("digest: `");
		expect(body).not.toContain("image: `ghcr.io");
	});
});

describe("manifest assembly — echoes identity and carries every output", () => {
	test("the manifest mirrors the sha/version/tag and the full output list", () => {
		const { manifest } = assemble(input());
		expect(manifest).toEqual({
			sha: "0123456789ab",
			version: "0.1.0+g0123456789ab",
			tag: "build-0123456789ab",
			outputs: OUTPUTS,
		});
	});

	test("the manifest is unaffected by whether the image is present", () => {
		const withImage = assemble(input()).manifest;
		const withoutImage = assemble(input({ image: null })).manifest;
		expect(withoutImage).toEqual(withImage);
	});

	test("an output with an unknown narHash renders `(unknown)` in the body", () => {
		const { body } = assemble(
			input({
				nixOutputs: [{ name: "go", path: "/nix/store/bbb-go", narHash: null }],
			}),
		);
		expect(body).toContain("- `go`: `/nix/store/bbb-go` ((unknown))");
	});

	test("no nix outputs still yields a body and an empty output list", () => {
		const { body, manifest } = assemble(input({ nixOutputs: [] }));
		expect(body).toContain("(none recorded)");
		expect(manifest.outputs).toEqual([]);
	});
});

describe("version-stamp echo — the one build version string appears in the body", () => {
	test("the body carries the version and the assets", () => {
		const { body } = assemble(input());
		expect(body).toContain("Version: `0.1.0+g0123456789ab`");
		expect(body).toContain("Commit: `0123456789ab`");
		expect(body).toContain("- `compass_build-0123456789ab_linux-amd64`");
		expect(body).toContain("- `SHA256SUMS`");
	});
});

describe("parseArgs — the edge's argv contract", () => {
	const required = [
		"--sha",
		"abc",
		"--version",
		"0.1.0+gabc",
		"--tag",
		"build-abc",
	];

	test("all three required flags present parses, defaults fill the rest", () => {
		const args = parseArgs(required);
		expect(args.sha).toBe("abc");
		expect(args.version).toBe("0.1.0+gabc");
		expect(args.tag).toBe("build-abc");
		expect(args.assets).toEqual([]);
		expect(args.bodyOut).toBe("RELEASE_BODY.md");
		expect(args.manifestOut).toBe("nix-outputs.json");
		expect(args.dryRun).toBe(false);
	});

	test("--dry-run is a valueless flag and does not consume the next token", () => {
		const args = parseArgs([...required, "--dry-run", "--asset", "a"]);
		expect(args.dryRun).toBe(true);
		expect(args.assets).toEqual(["a"]);
	});

	test("repeated --asset accumulates in order", () => {
		const args = parseArgs([...required, "--asset", "a", "--asset", "b"]);
		expect(args.assets).toEqual(["a", "b"]);
	});

	test("a missing required flag throws", () => {
		expect(() =>
			parseArgs(["--sha", "abc", "--version", "0.1.0+gabc"]),
		).toThrow("required");
	});

	test("an unknown flag throws", () => {
		expect(() => parseArgs([...required, "--bogus", "x"])).toThrow(
			"unknown flag",
		);
	});

	test("a trailing flag with no value throws", () => {
		expect(() => parseArgs([...required, "--asset"])).toThrow("needs a value");
	});
});

describe("classifyImageResult — the skopeo-result contract (crux of the skopeo fix)", () => {
	const digestJson = JSON.stringify({ config: { digest: "sha256:beef" } });

	test("exit 127 THROWS — a missing skopeo can never masquerade as an absent image", () => {
		expect(() =>
			classifyImageResult({
				exitCode: 127,
				stdout: "",
				stderr: "skopeo: command not found",
			}),
		).toThrow("not found on PATH");
	});

	test("a non-127 non-zero (404/transport) DEGRADES to null, not a throw", () => {
		expect(
			classifyImageResult({
				exitCode: 1,
				stdout: "",
				stderr: "manifest unknown",
			}),
		).toBeNull();
	});

	test("exit 0 with a config digest yields the @digest ref and the digest", () => {
		expect(
			classifyImageResult({ exitCode: 0, stdout: digestJson, stderr: "" }),
		).toEqual({
			ref: "ghcr.io/rigelbuild/compass-agent@sha256:beef",
			digest: "sha256:beef",
		});
	});

	test("exit 0 with unparseable output degrades to null", () => {
		expect(
			classifyImageResult({ exitCode: 0, stdout: "not json", stderr: "" }),
		).toBeNull();
	});

	test("exit 0 with no config.digest degrades to null", () => {
		expect(
			classifyImageResult({ exitCode: 0, stdout: "{}", stderr: "" }),
		).toBeNull();
	});
});

describe("requireImageAtRelease — a null image is a release-time failure, a dry-run degradation", () => {
	const image = {
		ref: "ghcr.io/rigelbuild/compass-agent@sha256:beef",
		digest: "sha256:beef",
	};

	test("release + null image FAILS — returns the no-image error message", () => {
		expect(requireImageAtRelease(null, false)).toContain(
			"no resolvable container image",
		);
	});

	test("release + present image is acceptable — returns null", () => {
		expect(requireImageAtRelease(image, false)).toBeNull();
	});

	test("dry-run + null image preserves the degradation — returns null", () => {
		expect(requireImageAtRelease(null, true)).toBeNull();
	});

	test("dry-run + present image is acceptable — returns null", () => {
		expect(requireImageAtRelease(image, true)).toBeNull();
	});
});
