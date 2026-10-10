import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	BIOME_CATALOG_KEY,
	meissaFlakeRef,
	rewriteCatalogPin,
} from "./refresh-biome-catalog.core.ts";

// Unit tests for the pure core of refresh-biome-catalog.ts: building the locked
// Meissa flake ref out of devenv.lock and rewriting the biome catalog pin. No
// nix/network/git — those live in the entry point.

const repoRoot = join(import.meta.dir, "..", "..");

describe("meissaFlakeRef", () => {
	const realLock = () => readFileSync(join(repoRoot, "devenv.lock"), "utf8");
	const REV = "a".repeat(40);
	const NAR = "sha256-hTbUK2SWyMCnTsliex+x98361TfchhpuHAKQUopMsYQ=";
	const github = (rev: string, narHash: string) => ({
		locked: {
			narHash,
			owner: "RigelBuild",
			repo: "meissa",
			rev,
			type: "github",
		},
	});
	const withMeissa = (node: unknown) =>
		JSON.stringify({ nodes: { m: node, root: { inputs: { meissa: "m" } } } });

	// The real-manifest guard: a devenv lock-format change fails HERE, loudly,
	// instead of evaluating a stale or wrong biome in production.
	test("builds the locked ref from the real devenv.lock", () => {
		const { nodes } = JSON.parse(realLock());
		const { owner, repo, rev, narHash } =
			nodes[nodes.root.inputs.meissa].locked;
		expect(meissaFlakeRef(realLock())).toBe(
			`github:${owner}/${repo}/${rev}?narHash=${narHash}`,
		);
	});

	// Node keys are not stable; only root's `meissa` input names the node.
	test("follows root's meissa input, not a bare meissa node key", () => {
		const lock = JSON.stringify({
			nodes: {
				meissa: github("b".repeat(40), NAR),
				meissa_2: github(REV, NAR),
				root: { inputs: { meissa: "meissa_2" } },
			},
		});
		expect(meissaFlakeRef(lock)).toBe(
			`github:RigelBuild/meissa/${REV}?narHash=${NAR}`,
		);
	});

	test("throws on invalid JSON", () => {
		expect(() => meissaFlakeRef("{not json")).toThrow(/not valid JSON/);
	});

	test.each([
		[
			"root has no meissa input",
			JSON.stringify({ nodes: { meissa: github(REV, NAR), root: {} } }),
		],
		[
			"root names a missing node",
			JSON.stringify({ nodes: { root: { inputs: { meissa: "m" } } } }),
		],
		["the rev is not 40-hex", withMeissa(github("abc123", NAR))],
		["the narHash is absent", withMeissa(github(REV, ""))],
		[
			"the node is not a github lock",
			withMeissa({ locked: { ...github(REV, NAR).locked, type: "path" } }),
		],
	])("throws when %s", (_label, lock) => {
		expect(() => meissaFlakeRef(lock)).toThrow(/root.inputs → meissa/);
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
