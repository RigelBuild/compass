import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	BIOME_CATALOG_KEY,
	channelNixpkgsRev,
	innerNixpkgsRev,
	rewriteCatalogPin,
	rewriteFlakeNixpkgsUrl,
} from "./refresh-devenv-nixpkgs.core.ts";

// Unit tests for the pure transform core of refresh-devenv-nixpkgs.ts
// (RIG-2432): reading the inner nixpkgs rev out of devenv.lock and rewriting
// the biome catalog pin. No nix / network / git — those shell-outs live in the
// entry point and are exercised by the PR's own CI run. These assert the
// parsing + rewriting that a wrong line would silently corrupt.

const repoRoot = join(import.meta.dir, "..", "..");

describe("innerNixpkgsRev", () => {
	// The extraction must recover the real inner NixOS/nixpkgs rev from the
	// checked-in devenv.lock — the real-manifest guard style: a devenv
	// lock-format change (the node moves/renames) fails HERE, loudly, instead of
	// silently evaluating a stale/wrong rev in production.
	test("recovers the nixpkgs-src rev from the real devenv.lock", () => {
		const lock = readFileSync(join(repoRoot, "devenv.lock"), "utf8");
		const rev = innerNixpkgsRev(lock);
		expect(rev).toMatch(/^[a-f0-9]{40}$/);
	});

	// The outer `nixpkgs` node (the devenv-nixpkgs channel rev) and the inner
	// `nixpkgs-src` node (upstream NixOS/nixpkgs) are DIFFERENT revs; step 3
	// evals the INNER one. A parser that grabbed the first `rev` or the outer
	// node would eval the wrong tree. Assert they differ and we return the inner.
	test("returns the inner src rev, not the outer channel rev", () => {
		const lock = readFileSync(join(repoRoot, "devenv.lock"), "utf8");
		const parsed = JSON.parse(lock);
		const outer = parsed.nodes.nixpkgs.locked.rev;
		const inner = parsed.nodes["nixpkgs-src"].locked.rev;
		expect(outer).not.toBe(inner);
		expect(innerNixpkgsRev(lock)).toBe(inner);
	});

	test("throws on invalid JSON", () => {
		expect(() => innerNixpkgsRev("{not json")).toThrow(/not valid JSON/);
	});

	test("throws when the nixpkgs-src node is absent", () => {
		const noSrc = JSON.stringify({
			nodes: { nixpkgs: { locked: { rev: "x" } } },
		});
		expect(() => innerNixpkgsRev(noSrc)).toThrow(/nixpkgs-src rev/);
	});

	test("throws on a non-40-hex rev (shape drift)", () => {
		const shortRev = JSON.stringify({
			nodes: { "nixpkgs-src": { locked: { rev: "abc123" } } },
		});
		expect(() => innerNixpkgsRev(shortRev)).toThrow(/nixpkgs-src rev/);
	});
});

describe("rewriteCatalogPin", () => {
	const pkg = () => readFileSync(join(repoRoot, "package.json"), "utf8");

	// Read a catalog pin's current value straight from the live manifest, so the
	// idempotency assertion tracks whatever is pinned today instead of a
	// hardcoded literal that a routine linter bump (e.g. a Renovate biome bump)
	// would silently invalidate — turning this into a red gate on every future
	// bump. Guards each access with in/typeof (package.json is valid JSON;
	// parse it) rather than an inline cast.
	const currentCatalogPin = (key: string): string => {
		const parsed: unknown = JSON.parse(pkg());
		const isObj = (v: unknown): v is Record<string, unknown> =>
			typeof v === "object" && v !== null;
		if (
			isObj(parsed) &&
			"workspaces" in parsed &&
			isObj(parsed.workspaces) &&
			"catalog" in parsed.workspaces &&
			isObj(parsed.workspaces.catalog)
		) {
			const pin = parsed.workspaces.catalog[key];
			if (typeof pin === "string") return pin;
		}
		throw new Error(
			`catalog pin "${key}" not found in the package.json catalog block.`,
		);
	};

	test("rewrites the biome catalog pin to the new version", () => {
		const out = rewriteCatalogPin(pkg(), BIOME_CATALOG_KEY, "2.5.6");
		expect(out).toContain('"@biomejs/biome": "2.5.6"');
	});

	// The catalog block is the pin; a `"@biomejs/biome": "catalog:"` CONSUMER
	// reference lives elsewhere in the same file and must NOT be rewritten — it
	// carries the literal `catalog:` sentinel, not a version. This is the whole
	// reason the rewrite is scoped to the catalog block.
	test("leaves the same-named catalog: consumer reference untouched", () => {
		const before = pkg();
		// The real manifest has both the pin and at least one `"…": "catalog:"`
		// consumer for biome — guard that the fixture premise holds.
		expect(before).toContain('"@biomejs/biome": "catalog:"');
		const out = rewriteCatalogPin(before, BIOME_CATALOG_KEY, "2.5.6");
		expect(out).toContain('"@biomejs/biome": "catalog:"');
		// Exactly one line changed (the pin), nothing else.
		const changed = out
			.split("\n")
			.filter((line, i) => line !== before.split("\n")[i]);
		expect(changed).toEqual(['\t\t\t"@biomejs/biome": "2.5.6",']);
	});

	// A channel bump that doesn't move biome leaves the pin alone: rewriting to
	// the current value yields byte-identical text (so the entry point's no-op
	// branch — skip write + skip bun install — fires correctly).
	test("is idempotent: rewrite to current value yields identical text", () => {
		const before = pkg();
		const current = currentCatalogPin(BIOME_CATALOG_KEY);
		expect(rewriteCatalogPin(before, BIOME_CATALOG_KEY, current)).toBe(before);
	});

	test("throws when the catalog pin key is absent (fail loud)", () => {
		expect(() => rewriteCatalogPin(pkg(), "@nonexistent/pkg", "1.0.0")).toThrow(
			/not found in the package.json catalog block/,
		);
	});

	test("throws when there is no catalog block at all", () => {
		expect(() =>
			rewriteCatalogPin('{"name":"x"}', BIOME_CATALOG_KEY, "1.0.0"),
		).toThrow(/no "catalog" block/);
	});
});

describe("channelNixpkgsRev", () => {
	// The channel rev (outer nixpkgs node) is what flake.nix pins and the
	// flake-parity gate compares — DISTINCT from innerNixpkgsRev. A lock-shape
	// change fails HERE, loudly, instead of aligning flake.nix to a wrong rev.
	test("recovers the outer nixpkgs channel rev from the real devenv.lock", () => {
		const lock = readFileSync(join(repoRoot, "devenv.lock"), "utf8");
		const rev = channelNixpkgsRev(lock);
		expect(rev).toMatch(/^[a-f0-9]{40}$/);
		expect(rev).toBe(JSON.parse(lock).nodes.nixpkgs.locked.rev);
	});

	// The counterpart to innerNixpkgsRev's mirror test: this reads the OUTER
	// channel node, not the inner src node. Assert they differ and we return the
	// outer — the flake pins the channel rev, so grabbing the inner would align
	// flake.nix to the wrong tree and leave the parity gate red.
	test("returns the outer channel rev, not the inner src rev", () => {
		const lock = readFileSync(join(repoRoot, "devenv.lock"), "utf8");
		const parsed = JSON.parse(lock);
		const outer = parsed.nodes.nixpkgs.locked.rev;
		const inner = parsed.nodes["nixpkgs-src"].locked.rev;
		expect(outer).not.toBe(inner);
		expect(channelNixpkgsRev(lock)).toBe(outer);
	});

	test("throws on invalid JSON", () => {
		expect(() => channelNixpkgsRev("{not json")).toThrow(/not valid JSON/);
	});

	test("throws when the outer nixpkgs node is absent", () => {
		const noNode = JSON.stringify({
			nodes: { "nixpkgs-src": { locked: { rev: "x" } } },
		});
		expect(() => channelNixpkgsRev(noNode)).toThrow(/nixpkgs rev/);
	});

	test("throws on a non-40-hex rev (shape drift)", () => {
		const shortRev = JSON.stringify({
			nodes: { nixpkgs: { locked: { rev: "abc123" } } },
		});
		expect(() => channelNixpkgsRev(shortRev)).toThrow(/nixpkgs rev/);
	});
});

describe("rewriteFlakeNixpkgsUrl", () => {
	const flake = () => readFileSync(join(repoRoot, "flake.nix"), "utf8");
	const NEW_REV = "0123456789abcdef0123456789abcdef01234567";

	// Read the channel rev the flake currently pins straight from the live
	// flake.nix, so the idempotency + change assertions track whatever is pinned
	// today rather than a hardcoded literal a routine devenv-nixpkgs bump would
	// silently invalidate into a red gate.
	const currentFlakeRev = (): string => {
		const rev = /github:cachix\/devenv-nixpkgs\/([a-f0-9]{40})/.exec(
			flake(),
		)?.[1];
		if (rev === undefined)
			throw new Error("no devenv-nixpkgs pin in flake.nix");
		return rev;
	};

	test("rewrites the flake.nix nixpkgs pin to the new rev", () => {
		const out = rewriteFlakeNixpkgsUrl(flake(), NEW_REV);
		expect(out).toContain(`github:cachix/devenv-nixpkgs/${NEW_REV}`);
		// The OLD pin URL is gone (the bare rev still appears in the PIN
		// DISCIPLINE comment prose, so assert on the URL, not the rev alone).
		expect(out).not.toContain(
			`github:cachix/devenv-nixpkgs/${currentFlakeRev()}`,
		);
	});

	// Only the one URL rev changes — nothing else in the flake is touched.
	test("changes exactly the pinned rev, one line", () => {
		const before = flake();
		const out = rewriteFlakeNixpkgsUrl(before, NEW_REV);
		const changed = out
			.split("\n")
			.filter((line, i) => line !== before.split("\n")[i]);
		expect(changed).toEqual([
			`  inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/${NEW_REV}";`,
		]);
	});

	// A channel bump landing on the same rev (or a re-run) yields byte-identical
	// text, so the entry point's no-op branch — skip write + skip flake update —
	// fires correctly.
	test("is idempotent: rewrite to current rev yields identical text", () => {
		const before = flake();
		expect(rewriteFlakeNixpkgsUrl(before, currentFlakeRev())).toBe(before);
	});

	test("throws on a non-40-hex rev (fail loud)", () => {
		expect(() => rewriteFlakeNixpkgsUrl(flake(), "abc123")).toThrow(
			/non-40-hex rev/,
		);
	});

	test("throws when the flake has no devenv-nixpkgs pin", () => {
		expect(() =>
			rewriteFlakeNixpkgsUrl(
				'{ inputs.nixpkgs.url = "github:NixOS/nixpkgs"; }',
				NEW_REV,
			),
		).toThrow(/no github:cachix\/devenv-nixpkgs/);
	});
});
