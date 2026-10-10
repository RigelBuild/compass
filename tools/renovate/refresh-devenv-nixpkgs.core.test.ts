import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	channelNixpkgsRev,
	rewriteFlakeNixpkgsUrl,
} from "./refresh-devenv-nixpkgs.core.ts";

// Unit tests for the pure transform core of refresh-devenv-nixpkgs.ts (RIG-2432):
// reading the channel rev out of devenv.lock and rewriting flake.nix's pin.
// No nix/network/git — those live in the entry point. These assert the
// parsing + rewriting a wrong line would corrupt.

const repoRoot = join(import.meta.dir, "..", "..");

describe("channelNixpkgsRev", () => {
	// The channel rev (root's nixpkgs input) is what flake.nix pins and the
	// flake-parity gate compares. A lock-shape change fails HERE, loudly,
	// instead of aligning flake.nix to a wrong rev.
	const realLock = () => readFileSync(join(repoRoot, "devenv.lock"), "utf8");
	const realNodes = () => JSON.parse(realLock()).nodes;

	test("recovers root's nixpkgs channel rev from the real devenv.lock", () => {
		const nodes = realNodes();
		const rev = channelNixpkgsRev(realLock());
		expect(rev).toMatch(/^[a-f0-9]{40}$/);
		expect(rev).toBe(nodes[nodes.root.inputs.nixpkgs].locked.rev);
	});

	// The flake pins the channel rev, so grabbing the channel's inner src would
	// align flake.nix to the wrong tree and leave the parity gate red.
	test("returns the outer channel rev, not the inner src rev", () => {
		const nodes = realNodes();
		const channel = nodes[nodes.root.inputs.nixpkgs];
		const inner = nodes[channel.inputs["nixpkgs-src"]].locked.rev;
		expect(channel.locked.rev).not.toBe(inner);
		expect(channelNixpkgsRev(realLock())).toBe(channel.locked.rev);
	});

	// An unfollowed input's own nixpkgs (Meissa's) takes the bare `nixpkgs` key
	// and root's channel becomes `nixpkgs_2`; reading by key would move
	// flake.nix to Meissa's channel.
	test("follows root's nixpkgs input, not the bare nixpkgs node key", () => {
		const renamed = JSON.stringify({
			nodes: {
				nixpkgs: { locked: { rev: "a".repeat(40) } },
				nixpkgs_2: { locked: { rev: "b".repeat(40) } },
				root: { inputs: { meissa: "meissa", nixpkgs: "nixpkgs_2" } },
			},
		});
		expect(channelNixpkgsRev(renamed)).toBe("b".repeat(40));
	});

	test("throws on invalid JSON", () => {
		expect(() => channelNixpkgsRev("{not json")).toThrow(/not valid JSON/);
	});

	test("throws when root has no nixpkgs input", () => {
		const noInput = JSON.stringify({
			nodes: { nixpkgs: { locked: { rev: "a".repeat(40) } }, root: {} },
		});
		expect(() => channelNixpkgsRev(noInput)).toThrow(/no 'nixpkgs' input/);
	});

	test("throws when the named channel node is absent", () => {
		const noNode = JSON.stringify({
			nodes: { root: { inputs: { nixpkgs: "nixpkgs_2" } } },
		});
		expect(() => channelNixpkgsRev(noNode)).toThrow(/no 40-hex locked rev/);
	});

	test("throws on a non-40-hex rev (shape drift)", () => {
		const shortRev = JSON.stringify({
			nodes: {
				nixpkgs: { locked: { rev: "abc123" } },
				root: { inputs: { nixpkgs: "nixpkgs" } },
			},
		});
		expect(() => channelNixpkgsRev(shortRev)).toThrow(/no 40-hex locked rev/);
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
