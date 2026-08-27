// Tests for the pure half of the devenv-CLI source tool (RIG-2546 §T1).
//
// The properties under test: the tool is SOURCE-AGNOSTIC (it reads whatever
// owner/repo/rev the named lock names — cachix upstream or the RigelBuild fork),
// it FAILS LOUD on any shape drift (missing node, short rev, non-github type)
// rather than resolving a stale/wrong source, and its dependency-free
// convention is a CHECKED property, not a comment. The bin-dir shim's
// single-binary invariant (RD-3) is unit-checked via the pure shimPlan helper.

import { describe, expect, test } from "bun:test";
import { devenvSource, flakeref, parseArgs, shimPlan } from "./core.ts";

// A cachix-shaped node (the root lock today) — WITH a `dir: src/modules` field,
// which the tool must ignore.
const CACHIX_LOCK = JSON.stringify({
	nodes: {
		devenv: {
			locked: {
				dir: "src/modules",
				owner: "cachix",
				repo: "devenv",
				rev: "0bf6765ce7071d98ed137ecfe02d1e435007c971",
				type: "github",
			},
		},
	},
});

// A RigelBuild-shaped node (the agent-image lock) — no `dir` field.
const RIGELBUILD_LOCK = JSON.stringify({
	nodes: {
		devenv: {
			locked: {
				owner: "RigelBuild",
				repo: "devenv",
				rev: "15a81f3e15619187fcbe10c2eac40878e0b4ce28",
				type: "github",
			},
		},
	},
});

describe("devenvSource", () => {
	test("parses a cachix-shaped node, ignoring the dir field", () => {
		expect(devenvSource(CACHIX_LOCK)).toEqual({
			owner: "cachix",
			repo: "devenv",
			rev: "0bf6765ce7071d98ed137ecfe02d1e435007c971",
		});
	});

	test("parses a RigelBuild-shaped node — the tool is source-agnostic", () => {
		expect(devenvSource(RIGELBUILD_LOCK)).toEqual({
			owner: "RigelBuild",
			repo: "devenv",
			rev: "15a81f3e15619187fcbe10c2eac40878e0b4ce28",
		});
	});

	test("throws when the devenv node is absent", () => {
		const lock = JSON.stringify({ nodes: { root: { locked: {} } } });
		expect(() => devenvSource(lock)).toThrow(/nodes\.devenv\.locked absent/);
	});

	test("throws on a rev shorter than 40 hex", () => {
		const lock = JSON.stringify({
			nodes: {
				devenv: {
					locked: {
						owner: "cachix",
						repo: "devenv",
						rev: "0bf6765c",
						type: "github",
					},
				},
			},
		});
		expect(() => devenvSource(lock)).toThrow(/40-hex devenv rev/);
	});

	test("throws on a non-github node type", () => {
		const lock = JSON.stringify({
			nodes: {
				devenv: {
					locked: {
						owner: "cachix",
						repo: "devenv",
						rev: "0bf6765ce7071d98ed137ecfe02d1e435007c971",
						type: "git",
					},
				},
			},
		});
		expect(() => devenvSource(lock)).toThrow(/expected "github"/);
	});

	test("throws when the devenv node has no owner", () => {
		const lock = JSON.stringify({
			nodes: {
				devenv: {
					locked: {
						repo: "devenv",
						rev: "0bf6765ce7071d98ed137ecfe02d1e435007c971",
						type: "github",
					},
				},
			},
		});
		expect(() => devenvSource(lock)).toThrow(/no owner/);
	});

	test("throws when the devenv node has no repo", () => {
		const lock = JSON.stringify({
			nodes: {
				devenv: {
					locked: {
						owner: "cachix",
						rev: "0bf6765ce7071d98ed137ecfe02d1e435007c971",
						type: "github",
					},
				},
			},
		});
		expect(() => devenvSource(lock)).toThrow(/no repo/);
	});

	test("throws on an owner with flakeref-reshaping characters", () => {
		const lock = JSON.stringify({
			nodes: {
				devenv: {
					locked: {
						owner: "a/b#x",
						repo: "devenv",
						rev: "0bf6765ce7071d98ed137ecfe02d1e435007c971",
						type: "github",
					},
				},
			},
		});
		expect(() => devenvSource(lock)).toThrow(/bare github owner/);
	});

	test("throws on invalid JSON", () => {
		expect(() => devenvSource("not json")).toThrow(/not valid JSON/);
	});
});

describe("flakeref", () => {
	test("composes the exact cachix flakeref", () => {
		expect(flakeref(devenvSource(CACHIX_LOCK))).toBe(
			"github:cachix/devenv/0bf6765ce7071d98ed137ecfe02d1e435007c971#devenv",
		);
	});

	test("composes the exact RigelBuild flakeref", () => {
		expect(flakeref(devenvSource(RIGELBUILD_LOCK))).toBe(
			"github:RigelBuild/devenv/15a81f3e15619187fcbe10c2eac40878e0b4ce28#devenv",
		);
	});
});

describe("parseArgs", () => {
	test("parses --lock/--mode in order", () => {
		expect(parseArgs(["--lock", "devenv.lock", "--mode", "bin-dir"])).toEqual({
			lockPath: "devenv.lock",
			mode: "bin-dir",
		});
	});

	test("parses --mode/--lock in either order", () => {
		expect(
			parseArgs(["--mode", "flakeref", "--lock", "agent-image/devenv.lock"]),
		).toEqual({ lockPath: "agent-image/devenv.lock", mode: "flakeref" });
	});

	test("throws on an unknown flag", () => {
		expect(() =>
			parseArgs(["--lock", "devenv.lock", "--mode", "flakeref", "--extra"]),
		).toThrow(/unknown argument/);
	});

	test("throws when --lock is missing", () => {
		expect(() => parseArgs(["--mode", "flakeref"])).toThrow(
			/--lock <path> is required/,
		);
	});

	test("throws when --mode is missing", () => {
		expect(() => parseArgs(["--lock", "devenv.lock"])).toThrow(
			/--mode <flakeref\|bin-dir> is required/,
		);
	});

	test("throws on an invalid --mode value", () => {
		expect(() => parseArgs(["--lock", "devenv.lock", "--mode", "wat"])).toThrow(
			/invalid --mode/,
		);
	});

	test("throws when a flag is missing its value", () => {
		expect(() => parseArgs(["--lock", "--mode", "flakeref"])).toThrow(
			/--lock requires a value/,
		);
	});
});

describe("shimPlan (RD-3 single-binary invariant)", () => {
	test("plans exactly one entry named devenv pointing at the out-path bin", () => {
		const plan = shimPlan("/nix/store/abc-devenv-1.0");
		expect(plan).toEqual([
			{ link: "devenv", target: "/nix/store/abc-devenv-1.0/bin/devenv" },
		]);
		// The load-bearing property: exactly one entry, named `devenv`, so the
		// printed dir cannot put devenv's whole closure bin dir on $GITHUB_PATH.
		expect(plan).toHaveLength(1);
		expect(plan.map((l) => l.link)).toEqual(["devenv"]);
	});
});

describe("import hygiene (dependency-free convention as a checked property)", () => {
	test("core.ts and index.ts import only node:/bun: builtins or ./core", async () => {
		const root = new URL(".", import.meta.url).pathname;
		const sources = await Promise.all(
			["core.ts", "index.ts"].map((f) => Bun.file(`${root}${f}`).text()),
		);
		// Static `import ... from "x"`, side-effect `import "x"`, dynamic
		// `import("x")` / `require("x")`, and re-export `export ... from "x"` —
		// all specifier forms must resolve to a builtin or ./core, since the
		// tool runs before `bun install`.
		const specifierRes = [
			/import\s+(?:type\s+)?[^"']*?from\s+["']([^"']+)["']/g,
			/import\s+["']([^"']+)["']/g,
			/(?:import|require)\s*\(\s*["']([^"']+)["']\s*\)/g,
			/export\s+(?:type\s+)?(?:\*|\{[^}]*\}|[^;]*?)\s+from\s+["']([^"']+)["']/g,
		];
		const isAllowed = (specifier: string): boolean =>
			specifier.startsWith("node:") ||
			specifier.startsWith("bun:") ||
			specifier === "./core" ||
			specifier === "./core.ts";
		for (const source of sources) {
			for (const importRe of specifierRes) {
				for (const match of source.matchAll(importRe)) {
					const specifier = match[1];
					expect(
						isAllowed(specifier),
						`disallowed import specifier: ${specifier}`,
					).toBe(true);
				}
			}
		}
	});
});
