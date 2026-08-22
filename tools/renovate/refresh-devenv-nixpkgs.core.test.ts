import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	BIOME_CATALOG_KEY,
	innerNixpkgsRev,
	rewriteCatalogPin,
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
