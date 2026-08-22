import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { chmod, cp, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { $ } from "bun";
import { readVersion, rewriteHash } from "./refresh-toolchain-hashes.ts";

// Regression test for tools/renovate/refresh-toolchain-hashes.ts (RIG-2432,
// ported from the internal monorepo's test).
//
// The bug this guards against: the script's path constants are all
// repo-root-relative (the pin files), but the gate `git diff --quiet "$base_ref"
// -- <pin>` uses git's cwd-relative pathspec. If the script `cd`s to the wrong
// depth, the gate tests a nonexistent path → always "unchanged" → the refresher
// early-exits "nothing to do" on EVERY branch. Stale nix SRI hashes then ship
// and the pin-bump PR goes red on a fetchurl mismatch.
//
// The fix: resolve the repo root via `git rev-parse --show-toplevel` and `cd`
// there — no hardcoded `..` depth to drift when the script moves. The gate's
// pathspec then resolves the real root pin files.
//
// This drives the ACTUAL shipped script (read relative to this test file, never
// re-implemented) inside a throwaway git tree, because the failure mode is
// precisely the cwd × git-cwd-relative-pathspec interaction — only a real run in
// a real git repo exercises it. The fixture's pin-file paths are DERIVED from
// the script's own exported BUN_NIX / NODE_NIX / MOON_NIX constants (not
// hardcoded), so a rebase that moves those files and rewrites the constants
// keeps this test green without edits.
//
// Network-free & deterministic: the script prefetches SRI hashes via
// `nix store prefetch-file` (see `sriForUrl`), so we put a fake `nix` on PATH
// that derives a deterministic SRI from the prefetched URL. The run is then
// fully offline, and asserting the bun darwin hash line got the URL-SPECIFIC
// stub value proves both that the per-file gate fired AND that the correct
// (bumped-version) URL was prefetched.

// The shipped script under test, sitting next to this file in tools/renovate/.
const REAL_SCRIPT = join(import.meta.dir, "refresh-toolchain-hashes.ts");
// The path Renovate invokes it by (cwd = repo root): `bun tools/renovate/…`.
const SCRIPT_REL = "tools/renovate/refresh-toolchain-hashes.ts";
// The shipped script's text — parsed once for its declared nix-file constants.
const SCRIPT_TEXT = readFileSync(REAL_SCRIPT, "utf8");

// Derive the fixture's pin-file paths from the script's OWN exported const
// declarations, so the fixture follows the script instead of hardcoding today's
// layout. Anchor on the `export const KEY = "value"` ASSIGNMENT and THROW on a
// miss: a parse drift must fail loudly, never silently fall back to a stale path.
function pinPathsFromScript(scriptText: string): {
	bun: string;
	node: string;
	moon: string;
} {
	// Tolerate biome wrapping a long declaration onto the next line
	// (`export const … =\n\t"…"`): `\s*` after `=` spans the break.
	const parse = (key: string): string => {
		const v = scriptText.match(
			new RegExp(`\\bexport const ${key} =\\s*"([^"]+)"`),
		)?.[1];
		if (!v) {
			throw new Error(
				`could not parse ^export const ${key} = "…" from refresh-toolchain-hashes.ts`,
			);
		}
		return v;
	};
	return {
		bun: parse("BUN_NIX"),
		node: parse("NODE_NIX"),
		moon: parse("MOON_NIX"),
	};
}
const PIN_PATHS = pinPathsFromScript(SCRIPT_TEXT);

// The static tail of the bun darwin fetchurl URL — the marker the bun refresh's
// darwin leg keys on, the line the loud-fail test drops, and the leg the
// per-language gate test asserts against.
const BUN_DARWIN_MARKER = "/bun-darwin-aarch64.zip";
// The bumped-version bun darwin URL the fixed script prefetches after the bump
// below. The stub SRI is a function of this exact string, so a wrong URL or a
// mis-interpolated version would yield a different hash and fail the assertion.
const BUN_DARWIN_URL = `https://github.com/oven-sh/bun/releases/download/bun-v1.3.14${BUN_DARWIN_MARKER}`;

// Mirror the stub `nix`'s URL→SRI derivation so the expected value is
// computable in-process. sha256(url) hex, matching `printf %s "$url" |
// sha256sum` in STUB_NIX.
function stubSriForUrl(url: string): string {
	const digest = new Bun.CryptoHasher("sha256").update(url).digest("hex");
	return `sha256-stub-${digest}`;
}

// A distinct placeholder per hash line so an accidental rewrite is unmistakable
// and unrelated lines can be told apart. None equals any stub value.
const PLACEHOLDER_BUN_DARWIN = "sha256-PLACEHOLDERbundarwin0000000000000000=";
const PLACEHOLDER_NODE_DARWIN = "sha256-PLACEHOLDERnodedarwin000000000000000=";
const PLACEHOLDER_MOON_DARWIN = "sha256-PLACEHOLDERmoondarwin000000000000000=";

// Hermetic git: no user/global/system config leakage, identity from env only.
const HERMETIC_ENV = {
	...process.env,
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "RIG-2432 test",
	GIT_AUTHOR_EMAIL: "rig2432@example.invalid",
	GIT_COMMITTER_NAME: "RIG-2432 test",
	GIT_COMMITTER_EMAIL: "rig2432@example.invalid",
};

// Each pin file carries a `version` attr + all three platform legs (the CI image
// + dev shell are multi-arch: x86_64-linux, aarch64-linux, aarch64-darwin), each
// a url+hash marker pair the script's per-leg refresh keys on. A missing marker
// makes the script exit 1, so each fixture mirrors the real marker/hash-line
// structure verbatim. `version` is what the script's readVersion parses.
const BUN_NIX_FIXTURE = `rec {
  version = "1.3.13";
  url = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.13/bun-linux-x64-baseline.zip";
  hash = "sha256-PLACEHOLDERbunx64000000000000000000000=";
  url = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.13/bun-linux-aarch64.zip";
  hash = "sha256-PLACEHOLDERbunaarch64000000000000000000=";
  url = "https://github.com/oven-sh/bun/releases/download/bun-v1.3.13/bun-darwin-aarch64.zip";
  hash = "${PLACEHOLDER_BUN_DARWIN}";
}
`;
const NODE_NIX_FIXTURE = `rec {
  version = "24.18.0";
  url = "https://nodejs.org/dist/v24.18.0/node-v24.18.0-linux-x64.tar.xz";
  hash = "sha256-PLACEHOLDERnodex64000000000000000000000=";
  url = "https://nodejs.org/dist/v24.18.0/node-v24.18.0-linux-arm64.tar.xz";
  hash = "sha256-PLACEHOLDERnodearm6400000000000000000000=";
  url = "https://nodejs.org/dist/v24.18.0/node-v24.18.0-darwin-arm64.tar.gz";
  hash = "${PLACEHOLDER_NODE_DARWIN}";
}
`;
const MOON_NIX_FIXTURE = `rec {
  version = "2.4.5";
  url = "https://github.com/moonrepo/moon/releases/download/v2.4.5/moon_cli-x86_64-unknown-linux-musl.tar.xz";
  hash = "sha256-PLACEHOLDERmoonx86000000000000000000000=";
  url = "https://github.com/moonrepo/moon/releases/download/v2.4.5/moon_cli-aarch64-unknown-linux-musl.tar.xz";
  hash = "sha256-PLACEHOLDERmoonaarch64000000000000000000=";
  url = "https://github.com/moonrepo/moon/releases/download/v2.4.5/moon_cli-aarch64-apple-darwin.tar.xz";
  hash = "${PLACEHOLDER_MOON_DARWIN}";
}
`;

// A fake `nix`: derive a deterministic SRI from the prefetched URL (the last
// arg of `nix store prefetch-file --json --hash-type sha256 <url>`) and emit it
// as the JSON `sriForUrl` extracts. URL-dependent, so a wrong-URL or
// bad-version-interpolation bug yields a DIFFERENT hash and is caught. Offline.
const STUB_NIX = `#!/usr/bin/env bash
url="\${@: -1}"
digest="$(printf %s "$url" | sha256sum | cut -d' ' -f1)"
printf '{"hash":"sha256-stub-%s"}\\n' "$digest"
`;

// The `hash = "sha256-…";` value on the line that (per rewriteHash's logic)
// immediately follows the url line containing `marker`.
function hashAfterMarker(nixText: string, marker: string): string | undefined {
	const lines = nixText.split("\n");
	const start = lines.findIndex((l) => l.includes(marker));
	if (start === -1) return undefined;
	for (let i = start + 1; i < lines.length; i++) {
		const m = lines[i]?.match(/hash = "(sha256-[^"]*)"/);
		if (m) return m[1];
	}
	return undefined;
}

// Build a throwaway repo mirroring the minimal repo-root layout and commit it
// as the `main` baseline (bun 1.3.13, node 24.18.0, moon 2.4.5).
async function buildBaselineRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "rig2432-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	const binDir = join(repo, "stubbin");
	await mkdir(binDir, { recursive: true });

	// Exercise the SHIPPED script, not a hand-copy.
	await cp(REAL_SCRIPT, join(repo, SCRIPT_REL));
	// Pin files at the paths the SCRIPT declares (derived above).
	for (const [rel, body] of [
		[PIN_PATHS.bun, BUN_NIX_FIXTURE],
		[PIN_PATHS.node, NODE_NIX_FIXTURE],
		[PIN_PATHS.moon, MOON_NIX_FIXTURE],
	] as [string, string][]) {
		const abs = join(repo, rel);
		await mkdir(dirname(abs), { recursive: true });
		await Bun.write(abs, body);
	}
	await Bun.write(join(binDir, "nix"), STUB_NIX);
	await chmod(join(binDir, "nix"), 0o755);

	await $`git init -q -b main`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git add -A`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git commit -q -m baseline`.cwd(repo).env(HERMETIC_ENV).quiet();
	// Point an origin remote-tracking ref at the baseline so the script's
	// PRIMARY base-ref path (`git rev-parse --verify -q origin/${base}`, the one
	// production Renovate hits) is exercised — not just the local `main`
	// fallback. `origin` is the repo itself; no network.
	await $`git remote add origin .`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git update-ref refs/remotes/origin/main main`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
	return repo;
}

// Run the shipped script exactly as Renovate does: cwd = repo root,
// `bun tools/renovate/refresh-toolchain-hashes.ts`, with the stub `nix` first on
// PATH and the base branch pointing at the committed baseline.
async function runRefresh(repo: string) {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			RENOVATE_BASE_BRANCH: "main",
		})
		.quiet()
		.nothrow();
}

describe("tools/renovate/refresh-toolchain-hashes.ts gate (RIG-2432)", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildBaselineRepo();
	});
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	// THE regression: after a bun.nix bump, the gate must fire from the repo
	// root and rewriteHash must rewrite the vendored bun darwin SRI. On a wrong
	// cwd the gate's cwd-relative pathspec misses the root pin file → "nothing to
	// do" early-exit → placeholder survives → this fails. On the fixed
	// git-rev-parse root the hash becomes the URL-derived stub value.
	test("rewrites the vendored bun SRI after a versions/bun.nix bump", async () => {
		await Bun.write(
			join(repo, PIN_PATHS.bun),
			BUN_NIX_FIXTURE.replaceAll("1.3.13", "1.3.14"),
		);

		const res = await runRefresh(repo);
		const stdout = res.stdout.toString();

		expect(res.exitCode).toBe(0);
		// The bug's fingerprint is the early-exit; assert it did NOT happen.
		expect(stdout).not.toContain("nothing to do");
		// Load-bearing assertion: the gate fired end to end and rewriteHash
		// rewrote the bun darwin hash to the SRI for the EXACT bumped-version URL
		// — proving the correct (v1.3.14) URL was prefetched, not just "a" value.
		const nix = await readFile(join(repo, PIN_PATHS.bun), "utf8");
		expect(hashAfterMarker(nix, BUN_DARWIN_MARKER)).toBe(
			stubSriForUrl(BUN_DARWIN_URL),
		);
	});

	// The per-language gate: a bun-only bump re-prefetches bun.nix and leaves
	// node.nix and moon.nix byte-for-byte untouched. This is the granularity the
	// custom.regex managers + un-grouping rule buy — each language pin lands on
	// its own branch, and the script must not rewrite a pin file its branch
	// didn't change.
	test("a versions/bun.nix bump leaves node/moon pin files untouched", async () => {
		const nodeBefore = await readFile(join(repo, PIN_PATHS.node), "utf8");
		const moonBefore = await readFile(join(repo, PIN_PATHS.moon), "utf8");

		await Bun.write(
			join(repo, PIN_PATHS.bun),
			BUN_NIX_FIXTURE.replaceAll("1.3.13", "1.3.14"),
		);

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);

		// bun.nix darwin leg was rewritten (the gate fired for bun) …
		const bunNix = await readFile(join(repo, PIN_PATHS.bun), "utf8");
		expect(hashAfterMarker(bunNix, BUN_DARWIN_MARKER)).toBe(
			stubSriForUrl(BUN_DARWIN_URL),
		);
		// … and the other two pin files are byte-identical: their placeholder
		// hashes survive because their per-file gate never fired.
		expect(await readFile(join(repo, PIN_PATHS.node), "utf8")).toBe(nodeBefore);
		expect(await readFile(join(repo, PIN_PATHS.moon), "utf8")).toBe(moonBefore);
	});

	// This is a SILENT-no-op bug class: the script did the wrong thing and
	// exited 0. rewriteHash's defense is to fail LOUD (exit 1) when a marker line
	// is missing, so a future refactor that breaks marker detection can't quietly
	// ship stale pins. Fire the gate, drop a bun marker line, and assert the
	// script dies non-zero on the marker-not-found path.
	test("fails loud (exit≠0) when a hash marker is missing", async () => {
		// Bump so the gate fires and the run REACHES rewriteHash (not the
		// "nothing to do" early-exit), then remove a bun marker line rewriteHash
		// keys on.
		const bunPath = join(repo, PIN_PATHS.bun);
		const bumped = BUN_NIX_FIXTURE.replaceAll("1.3.13", "1.3.14");
		const withoutMarker = bumped
			.split("\n")
			.filter((l) => !l.includes("/bun-linux-x64-baseline.zip"))
			.join("\n");
		// Guard the fixture: it actually carried the marker we mean to drop.
		expect(withoutMarker).not.toBe(bumped);
		await Bun.write(bunPath, withoutMarker);

		const res = await runRefresh(repo);
		const stdout = res.stdout.toString();
		const stderr = res.stderr.toString();

		// Loud failure, not the silent exit-0 no-op.
		expect(res.exitCode).not.toBe(0);
		// Specifically the marker-not-found branch: its message is unique to that
		// path, distinguishing it from the gate no-op, the empty-hash exit, and
		// the no-hash-line exit.
		expect(stderr).toMatch(/marker .* not found/);
		// And the run got PAST the gate to prefetch/rewriteHash — proving this is
		// the rewriteHash failure, not the "nothing to do" early-exit.
		expect(stdout).toContain("prefetching");
	});

	// The self-gate contract (script header): with NO pin-file change the script
	// is a cheap no-op — it early-exits before any prefetch and touches no hash.
	// This proves the rewrite above is genuinely gated on the diff, not
	// unconditional, and guards against the gate being dropped.
	test("is a no-op when no pin file is changed", async () => {
		const res = await runRefresh(repo);
		const stdout = res.stdout.toString();

		expect(res.exitCode).toBe(0);
		expect(stdout).toContain("nothing to do");
		const nix = await readFile(join(repo, PIN_PATHS.bun), "utf8");
		expect(hashAfterMarker(nix, BUN_DARWIN_MARKER)).toBe(
			PLACEHOLDER_BUN_DARWIN,
		);
	});
});

describe("readVersion", () => {
	test("reads the version attr from a pin file", () => {
		expect(readVersion(BUN_NIX_FIXTURE, "bun.nix")).toBe("1.3.13");
		expect(readVersion(NODE_NIX_FIXTURE, "node.nix")).toBe("24.18.0");
		expect(readVersion(MOON_NIX_FIXTURE, "moon.nix")).toBe("2.4.5");
	});

	test("throws when no version attr is present", () => {
		expect(() => readVersion('rec {\n  url = "x";\n}\n', "bun.nix")).toThrow(
			/could not read version/,
		);
	});
});

describe("rewriteHash", () => {
	test("rewrites the hash line following the marker to the new SRI", () => {
		const out = rewriteHash(
			BUN_NIX_FIXTURE,
			BUN_DARWIN_MARKER,
			"sha256-newvalue000=",
			"bun.nix",
		);
		expect(hashAfterMarker(out, BUN_DARWIN_MARKER)).toBe("sha256-newvalue000=");
	});

	test("is idempotent — rewriting to the same SRI yields identical content", () => {
		const out = rewriteHash(
			BUN_NIX_FIXTURE,
			BUN_DARWIN_MARKER,
			PLACEHOLDER_BUN_DARWIN,
			"bun.nix",
		);
		expect(out).toBe(BUN_NIX_FIXTURE);
	});

	test("throws on an empty new SRI", () => {
		expect(() =>
			rewriteHash(BUN_NIX_FIXTURE, BUN_DARWIN_MARKER, "", "bun.nix"),
		).toThrow(/empty/);
	});

	test("throws when the marker is not found", () => {
		expect(() =>
			rewriteHash(BUN_NIX_FIXTURE, "/no-such-marker", "sha256-x=", "bun.nix"),
		).toThrow(/marker .* not found/);
	});

	test("throws when no hash line follows the marker", () => {
		const noHash = `rec {\n  url = "https://x${BUN_DARWIN_MARKER}";\n}\n`;
		expect(() =>
			rewriteHash(noHash, BUN_DARWIN_MARKER, "sha256-x=", "bun.nix"),
		).toThrow(/no 'hash =' line/);
	});
});
