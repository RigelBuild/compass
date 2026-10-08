import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	BIOME_CATALOG_KEY,
	meissaInnerNixpkgsRev,
	rewriteCatalogPin,
} from "./refresh-biome-catalog.core.ts";

// Unit tests for the pure core of refresh-biome-catalog.ts: resolving Meissa's
// raw nixpkgs rev out of devenv.lock and rewriting the biome catalog pin. No
// nix/network/git — those live in the entry point.

const repoRoot = join(import.meta.dir, "..", "..");

describe("meissaInnerNixpkgsRev", () => {
	const realLock = () => readFileSync(join(repoRoot, "devenv.lock"), "utf8");

	// The real-manifest guard: a devenv lock-format change fails HERE, loudly,
	// instead of evaluating a stale or wrong rev in production.
	test("recovers Meissa's nixpkgs-src rev from the real devenv.lock", () => {
		const nodes = JSON.parse(realLock()).nodes;
		const meissaChannel = nodes[nodes[nodes.root.inputs.meissa].inputs.nixpkgs];
		const src = nodes[meissaChannel.inputs["nixpkgs-src"]];
		expect(src.locked.owner).toBe("NixOS");
		expect(meissaInnerNixpkgsRev(realLock())).toBe(src.locked.rev);
	});

	// Meissa's channel and root's channel are both devenv-nixpkgs nodes, each
	// with a nixpkgs-src. The bare `nixpkgs-src` key belongs to whichever won the
	// name; follow input names so biome comes from Meissa's tree only.
	test("follows meissa → nixpkgs → nixpkgs-src, not root's channel", () => {
		const lock = JSON.stringify({
			nodes: {
				meissa: { inputs: { nixpkgs: "nixpkgs_2" } },
				nixpkgs: { inputs: { "nixpkgs-src": "nixpkgs-src" } },
				"nixpkgs-src": { locked: { rev: "a".repeat(40) } },
				nixpkgs_2: { inputs: { "nixpkgs-src": "nixpkgs-src_2" } },
				"nixpkgs-src_2": { locked: { rev: "b".repeat(40) } },
				root: { inputs: { meissa: "meissa", nixpkgs: "nixpkgs" } },
			},
		});
		expect(meissaInnerNixpkgsRev(lock)).toBe("b".repeat(40));
	});

	test("throws on invalid JSON", () => {
		expect(() => meissaInnerNixpkgsRev("{not json")).toThrow(/not valid JSON/);
	});

	test("throws when root has no meissa input", () => {
		const noMeissa = JSON.stringify({
			nodes: {
				"nixpkgs-src": { locked: { rev: "a".repeat(40) } },
				root: { inputs: { nixpkgs: "nixpkgs" } },
			},
		});
		expect(() => meissaInnerNixpkgsRev(noMeissa)).toThrow(/no 'meissa' input/);
	});

	test("throws on a non-40-hex rev (shape drift)", () => {
		const shortRev = JSON.stringify({
			nodes: {
				meissa: { inputs: { nixpkgs: "n" } },
				n: { inputs: { "nixpkgs-src": "s" } },
				s: { locked: { rev: "abc123" } },
				root: { inputs: { meissa: "meissa" } },
			},
		});
		expect(() => meissaInnerNixpkgsRev(shortRev)).toThrow(
			/no 40-hex locked rev/,
		);
	});
});

describe("rewriteCatalogPin", () => {
	const pkg = () => readFileSync(join(repoRoot, "package.json"), "utf8");

	// Read a catalog pin's current value straight from the live manifest, so the
	// idempotency assertion tracks whatever is pinned today instead of a hardcoded
	// literal a routine biome bump would invalidate. Guards each access with
	// in/typeof rather than an inline cast.
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
