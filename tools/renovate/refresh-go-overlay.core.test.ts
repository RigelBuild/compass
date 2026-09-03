import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { goOverlayLockedRev, goPinVersion } from "./refresh-go-overlay.core.ts";

// Unit tests for the pure transform core of refresh-go-overlay.ts (RIG-3100):
// reading the go-overlay locked rev out of devenv.lock and the go version out
// of go.nix. No nix / network / git — those shell-outs live in the entry point
// and are exercised by the PR's own CI run. These assert the parsing that a
// wrong line would silently corrupt (validating the eval against a stale rev or
// an `undefined` version).

const repoRoot = join(import.meta.dir, "..", "..");

describe("goOverlayLockedRev", () => {
	// The extraction must recover the real go-overlay rev from the checked-in
	// devenv.lock — the real-manifest guard style: a devenv lock-format change
	// (the node moves/renames) fails HERE, loudly, instead of silently
	// validating against a stale/wrong overlay rev in production.
	test("recovers the go-overlay rev from the real devenv.lock", () => {
		const lock = readFileSync(join(repoRoot, "devenv.lock"), "utf8");
		const rev = goOverlayLockedRev(lock);
		expect(rev).toMatch(/^[a-f0-9]{40}$/);
		expect(rev).toBe(JSON.parse(lock).nodes["go-overlay"].locked.rev);
	});

	test("throws on invalid JSON", () => {
		expect(() => goOverlayLockedRev("{not json")).toThrow(/not valid JSON/);
	});

	test("throws when the go-overlay node is absent", () => {
		const noNode = JSON.stringify({
			nodes: { nixpkgs: { locked: { rev: "x" } } },
		});
		expect(() => goOverlayLockedRev(noNode)).toThrow(/go-overlay rev/);
	});

	test("throws on a non-40-hex rev (shape drift)", () => {
		const shortRev = JSON.stringify({
			nodes: { "go-overlay": { locked: { rev: "abc123" } } },
		});
		expect(() => goOverlayLockedRev(shortRev)).toThrow(/go-overlay rev/);
	});
});

describe("goPinVersion", () => {
	// The extraction must recover the real go version from the checked-in go.nix
	// — the same literal the config.json5 go manager's matchString keys off, so a
	// go-pin format change fails HERE instead of validating the eval against an
	// undefined target.
	test("recovers the go version from the real go.nix", () => {
		const goNix = readFileSync(
			join(repoRoot, "tools", "toolchain", "versions", "go.nix"),
			"utf8",
		);
		const version = goPinVersion(goNix);
		expect(version).toMatch(/^\d+\.\d+/);
	});

	test("reads the version out of a version-only pin literal", () => {
		expect(goPinVersion('{ version = "1.27.0"; }')).toBe("1.27.0");
	});

	test("throws when the version literal is absent", () => {
		expect(() => goPinVersion("{ }")).toThrow(/dotted go version/);
	});

	test("throws on a non-dotted version (shape drift)", () => {
		expect(() => goPinVersion('{ version = "latest"; }')).toThrow(
			/dotted go version/,
		);
	});
});
