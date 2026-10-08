// Tests for the pure half of the flake nixpkgs-pin parity gate.
//
// The property under test throughout is the one the gate exists for: it must be
// CAPABLE OF FAILING on a genuine skew, and it must never turn "I could not read
// a rev" into a pass. So the extractor is tested against the real lock shape and
// against every way a rev can be absent, and the comparator against a match, a
// mismatch, and each missing side.

import { describe, expect, test } from "bun:test";
import { compareRevs, nixpkgsLockedRev } from "./flake-parity-core.ts";

// The pinned rev both locks record today (devenv.lock:190, flake.lock).
const PINNED = "c946ff36bf193309589932c371bd5ae6653c912e";

// A minimal flake-lock-shaped document — root's `nixpkgs` input naming a node
// whose `locked.rev` both real files carry, with the surrounding keys nix
// writes so the fixture is a realistic shape rather than only the fields read.
const lockWithRev = (rev: string): string =>
	JSON.stringify({
		nodes: {
			nixpkgs: {
				locked: {
					lastModified: 1785104946,
					owner: "cachix",
					repo: "devenv-nixpkgs",
					rev,
					type: "github",
				},
				original: {
					owner: "cachix",
					ref: "rolling",
					repo: "devenv-nixpkgs",
					type: "github",
				},
			},
			root: { inputs: { nixpkgs: "nixpkgs" } },
		},
		root: "root",
		version: 7,
	});

// A lock whose root resolves `nixpkgs` to `node`, so each case below fails on
// the node's own shape rather than on a missing root input.
const rooted = (node: unknown): string =>
	JSON.stringify({
		nodes: { nixpkgs: node, root: { inputs: { nixpkgs: "nixpkgs" } } },
	});

describe("nixpkgsLockedRev", () => {
	test("reads the nixpkgs locked rev from a lock document", () => {
		expect(nixpkgsLockedRev(lockWithRev(PINNED))).toBe(PINNED);
	});

	// An unfollowed input's own nixpkgs takes the bare `nixpkgs` key and root's
	// channel becomes `nixpkgs_2`. Reading by key would compare the wrong rev.
	test("follows root's nixpkgs input, not the bare nixpkgs node key", () => {
		const other = "92fc8111c5c49aee8efb1ccc9fb90ad55ae91139";
		const renamed = JSON.stringify({
			nodes: {
				nixpkgs: { locked: { rev: other } },
				nixpkgs_2: { locked: { rev: PINNED } },
				root: { inputs: { nixpkgs: "nixpkgs_2" } },
			},
		});
		expect(nixpkgsLockedRev(renamed)).toBe(PINNED);
	});

	// Every form below is a way the node can be absent. Each must yield null so
	// the caller refuses rather than compares against a fabricated value — the
	// false-green this gate exists to prevent.
	test.each([
		["no nixpkgs node", JSON.stringify({ nodes: { root: {} }, version: 7 })],
		[
			"root names a missing node",
			JSON.stringify({
				nodes: {
					nixpkgs: { locked: { rev: PINNED } },
					root: { inputs: { nixpkgs: "nixpkgs_2" } },
				},
			}),
		],
		[
			"root has no inputs",
			JSON.stringify({ nodes: { nixpkgs: { locked: { rev: PINNED } } } }),
		],
		["nixpkgs node without locked", rooted({})],
		["locked without rev", rooted({ locked: { owner: "cachix" } })],
		["rev is not a string", rooted({ locked: { rev: 42 } })],
		["rev is empty", rooted({ locked: { rev: "" } })],
		["nodes missing entirely", JSON.stringify({ version: 7 })],
		// A corrupt / merge-conflicted lock is not valid JSON — it must fail
		// closed (null) rather than throw a raw SyntaxError out of the extractor.
		["source is not valid JSON", "not json{"],
	])("yields null when %s", (_label, source) => {
		expect(nixpkgsLockedRev(source)).toBeNull();
	});
});

describe("compareRevs", () => {
	test("passes when both locks pin the same rev", () => {
		expect(compareRevs(PINNED, PINNED).ok).toBe(true);
	});

	test("fails on a genuine skew, and the report names both revs", () => {
		const skewed = "0000000000000000000000000000000000000000";
		const result = compareRevs(skewed, PINNED);
		expect(result.ok).toBe(false);
		expect(result.report).toContain(skewed);
		expect(result.report).toContain(PINNED);
	});

	// A rev that could not be read is a failure, never a skip — matching
	// parity-core's unverifiable-is-a-failure rule.
	test("fails when the flake rev could not be read", () => {
		expect(compareRevs(null, PINNED).ok).toBe(false);
	});

	test("fails when the devenv rev could not be read", () => {
		expect(compareRevs(PINNED, null).ok).toBe(false);
	});

	test("names both files when neither rev could be read", () => {
		const result = compareRevs(null, null);
		expect(result.ok).toBe(false);
		expect(result.report).toContain("flake.lock");
		expect(result.report).toContain("devenv.lock");
	});
});
