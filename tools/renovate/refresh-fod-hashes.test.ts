import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { $ } from "bun";
import {
	assertFodTableInvariants,
	FOD_ENTRIES,
	type FodEntry,
	hashOnMarker,
	parseGotForFragment,
	refreshEntry,
	rewriteInlineHash,
} from "./refresh-fod-hashes.ts";

// Regression + unit test for tools/renovate/refresh-fod-hashes.ts (PR #579).
//
// The bug this guards against: a dependency bump moves a pinned Nix
// fixed-output-derivation hash (the Go `vendorHash` in guest-image/default.nix on
// a gomod bump; the bun `outputHash` in agent-image/entrypoint.nix on a bun/
// catalog bump), and nothing regenerates it, so the image build fails
// `hash mismatch in fixed-output derivation` and the bump PR goes red. The
// refresher recomputes the hash IN the bump branch so the PR lands green.
//
// It also guards the SECOND half of that pin's story: `outputHash` is realised by
// two builders (guest-image with root's pkgs, the agent image with its own), so
// the table pairs an AUTHORITATIVE entry that writes the value with a VERIFY
// entry that recomputes it through the agent-image vehicle and compares. Equal is
// a quiet no-op; a difference must throw naming both SRIs, because no single
// literal can then satisfy both builders.
//
// The failure mode is precisely the script's cwd × git-cwd-relative-pathspec
// gate interaction plus the fake-hash→build→parse-`got:` recovery — only a real
// run in a real git repo exercises it. So this drives the ACTUAL shipped script
// (never a re-implementation) inside a throwaway git tree with a FAKE `nix` on
// PATH that emits the `hash mismatch … got: <sri>` shape keyed to the faked pin.
//
// Network-free & deterministic: the stub `nix` derives a stable `got:` SRI from
// the drv fragment it's asked to build, so a run is fully offline and asserting a
// pin took the fragment-specific stub value proves both that the per-FOD gate
// fired AND that the correct derivation's hash was parsed and written back. The
// stub also honours a per-vehicle divergence knob, which is how the two-builders
// disagreement is exercised without two real nixpkgs.
//
// The fixture layout, trigger files, vehicles, and pin markers are DERIVED from
// the script's own exported FOD_ENTRIES table, so a rebase that edits the table
// keeps this honest without a second edit.

const SCRIPT_REL = "tools/renovate/refresh-fod-hashes.ts";
const REAL_SCRIPT = join(import.meta.dir, "refresh-fod-hashes.ts");

// Resolve each FOD entry from the shipped table by its stable id, throwing on
// drift so the fixture follows the script. A returning helper (not a top-level
// `if (!x) throw`) gives a non-nullable type that narrows into the closures
// below. Keyed on `id`, not `drvFragment`: the two entrypoint.nix entries share a
// fragment by design — they are the same derivation seen through two nixpkgs.
function mustFind(id: string): FodEntry {
	const entry = FOD_ENTRIES.find((e) => e.id === id);
	if (!entry) {
		throw new Error(`fixture drift: expected a '${id}' FOD entry`);
	}
	return entry;
}
const GO_ENTRY = mustFind("guestd-go-vendor");
const BUN_WRITE_ENTRY = mustFind("agent-node-modules-root-pkgs");
const BUN_VERIFY_ENTRY = mustFind("agent-node-modules-agent-image-pkgs");
// The real repo, for the table properties that must hold against files on disk
// (as opposed to the hermetic fixture repo the gate tests build below).
const repoRoot = join(import.meta.dir, "..", "..");

// Hermetic git: no user/global/system config leakage, identity from env only.
const HERMETIC_ENV = {
	...process.env,
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "PR579 test",
	GIT_AUTHOR_EMAIL: "pr579@example.invalid",
	GIT_COMMITTER_NAME: "PR579 test",
	GIT_COMMITTER_EMAIL: "pr579@example.invalid",
};

// A distinct placeholder SRI per pinned FOD file so an accidental rewrite is
// unmistakable and unrelated lines stay distinguishable. None equals a stub value.
const PLACEHOLDER_GO = "sha256-PLACEHOLDERgovendor00000000000000000000=";
const PLACEHOLDER_BUN = "sha256-PLACEHOLDERbunoutput00000000000000000000=";

// Minimal nix files carrying the real markers the script keys on. The script only
// reads/rewrites the marker line, so the surrounding nix need not be buildable —
// the fake `nix` never actually evaluates it. The marker already ends in
// `sha256-`, so interpolate the placeholder body WITHOUT its own `sha256-` prefix
// (else the line carries `sha256-sha256-…`).
const bodyOf = (sri: string) => sri.replace(/^sha256-/, "");
const GO_NIX_FIXTURE = `let
  guestd = pkgs.buildGoModule {
    ${GO_ENTRY.marker}${bodyOf(PLACEHOLDER_GO)}";
  };
in guestd
`;
// The flake.nix mirror carries the SAME Go vendorHash marker; the script writes
// the recomputed value here too (FodEntry.mirrorFiles). A distinct placeholder so
// a missing-mirror regression is unmistakable (it would stay at this value).
const PLACEHOLDER_GO_MIRROR = "sha256-PLACEHOLDERflakemirror00000000000000000=";
const GO_MIRROR_FIXTURE = `let
  compass-app = pkgs.buildGoModule {
    ${GO_ENTRY.marker}${bodyOf(PLACEHOLDER_GO_MIRROR)}";
  };
in compass-app
`;
const BUN_NIX_FIXTURE = `let
  nodeModules = pkgs.stdenv.mkDerivation {
    ${BUN_WRITE_ENTRY.marker}${bodyOf(PLACEHOLDER_BUN)}";
  };
in nodeModules
`;
// A stand-in for each vehicle the table names that is not itself a pin file. The
// stub nix never evaluates it; it exists so the fixture tree has the file the
// script passes to `nix build -f`, matching the real layout.
const VEHICLE_FIXTURE = "{ }\n";

// A fake `nix`: emit a `hash mismatch` block for BOTH FODs (the real build with
// --keep-going reports every stale FOD), each with a fragment-derived
// deterministic `got:` SRI. Shape matches what parseGotForFragment scans for.
// Offline.
//
// STUB_DIVERGE_VEHICLE, when set to a `-f <file>` value, makes THAT vehicle
// report a different SRI for the same fragment — the two-builders-disagree case,
// which is otherwise only reachable with two real, skewed nixpkgs revisions.
const STUB_NIX = `#!/usr/bin/env bash
# Find the \`-f <file>\` vehicle so a per-vehicle divergence can be simulated.
vehicle=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-f" ]; then vehicle="$arg"; fi
  prev="$arg"
done
salt=""
if [ -n "\${STUB_DIVERGE_VEHICLE:-}" ] && [ "$vehicle" = "$STUB_DIVERGE_VEHICLE" ]; then
  salt="diverged"
fi
emit() {
  local frag="$1"
  local digest
  digest="$(printf %s "$salt$frag" | sha256sum | cut -d' ' -f1)"
  echo "error: hash mismatch in fixed-output derivation '/nix/store/deadbeef-compass-\${frag}.drv':" >&2
  echo "         specified: sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" >&2
  echo "            got:    sha256-stub-\${digest}" >&2
}
emit go-modules
emit node-modules
exit 1
`;

// Mirror the stub's fragment→SRI derivation so the expected value is computable
// in-process. `salt` matches the stub's divergence prefix.
function stubSriForFragment(fragment: string, salt = ""): string {
	const hasher = new Bun.CryptoHasher("sha256");
	hasher.update(`${salt}${fragment}`);
	return `sha256-stub-${hasher.digest("hex")}`;
}

// The `sha256-…` value on the marker line, tolerating absence — the shipped
// hashOnMarker throws instead (a verify that cannot find its pin must fail
// loud), so tests asserting "unchanged/absent" use this local reader.
function maybeHashOnMarker(
	nixText: string,
	marker: string,
): string | undefined {
	const line = nixText.split("\n").find((l) => l.includes(marker));
	return line?.match(/sha256-[^"]*/)?.[0];
}

// Build a throwaway repo mirroring the minimal repo-root layout and commit it as
// the `main` baseline: both pin files, every vehicle, both trigger manifests, the
// shipped script.
async function buildBaselineRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "pr579-"));
	const write = async (rel: string, body: string) => {
		const abs = join(repo, rel);
		await mkdir(dirname(abs), { recursive: true });
		await Bun.write(abs, body);
	};

	// Exercise the SHIPPED script, not a hand-copy.
	await write(SCRIPT_REL, await readFile(REAL_SCRIPT, "utf8"));
	// Pin files at the paths the TABLE declares.
	await write(GO_ENTRY.file, GO_NIX_FIXTURE);
	await write(BUN_WRITE_ENTRY.file, BUN_NIX_FIXTURE);
	// Every vehicle the table names, unless it IS a pin file already written
	// above — derived from the shipped table so a new vehicle needs no second
	// edit here.
	const pinFiles = FOD_ENTRIES.map((e) => e.file);
	for (const entry of FOD_ENTRIES) {
		if (pinFiles.includes(entry.buildFile)) continue;
		await write(entry.buildFile, VEHICLE_FIXTURE);
	}
	// Mirror pin files the Go entry declares (same vendorHash, refreshed in
	// lockstep — derived from the shipped table so a rebase that edits mirrorFiles
	// keeps this honest with no second edit).
	for (const mirror of GO_ENTRY.mirrorFiles ?? []) {
		await write(mirror, GO_MIRROR_FIXTURE);
	}
	// Trigger manifests (content is irrelevant; only their diff-vs-base matters).
	for (const entry of FOD_ENTRIES) {
		for (const trigger of entry.triggers) {
			await write(trigger, "baseline\n");
		}
	}
	// Channel locks the divergence diagnostic quotes. Real devenv locks; only
	// `nodes.nixpkgs.locked.rev` is read, and only to name the two revs in the
	// error, so a minimal shape with distinguishable revs is enough.
	for (const entry of FOD_ENTRIES) {
		await write(
			entry.vehicleChannelLock,
			`${JSON.stringify(
				{
					nodes: {
						nixpkgs: {
							locked: { rev: revFor(entry.vehicleChannelLock) },
						},
					},
				},
				null,
				2,
			)}\n`,
		);
	}
	// The fake nix.
	const binDir = join(repo, "stubbin");
	await mkdir(binDir, { recursive: true });
	await Bun.write(join(binDir, "nix"), STUB_NIX);
	await chmod(join(binDir, "nix"), 0o755);

	await $`git init -q -b main`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git add -A`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git commit -q -m baseline`.cwd(repo).env(HERMETIC_ENV).quiet();
	// Point origin/main at the baseline so the script's PRIMARY base-ref path
	// (`git rev-parse --verify -q origin/main`) is exercised, not just the local
	// fallback. origin is the repo itself; no network.
	await $`git remote add origin .`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git update-ref refs/remotes/origin/main main`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
	return repo;
}

// A deterministic 40-hex rev per lock path, so the divergence error's two revs
// are distinguishable and assertable.
function revFor(lockPath: string): string {
	const hasher = new Bun.CryptoHasher("sha256");
	hasher.update(lockPath);
	return hasher.digest("hex").slice(0, 40);
}

// Run the shipped script exactly as Renovate does: cwd = repo root, stub nix
// first on PATH, base branch = the committed baseline.
async function runRefresh(repo: string, extraEnv: Record<string, string> = {}) {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			RENOVATE_BASE_BRANCH: "main",
			...extraEnv,
		})
		.quiet()
		.nothrow();
}

describe("tools/renovate/refresh-fod-hashes.ts gate (PR #579)", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildBaselineRepo();
	});
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	// THE regression: after a go/go.mod bump, the per-FOD gate must fire from the
	// repo root and the Go vendorHash must be rewritten to the value the build
	// reported for the go-modules FOD. On a wrong cwd the gate's cwd-relative
	// pathspec misses the root manifest → "nothing to do" → placeholder survives.
	test("refreshes the Go vendorHash after a go/go.mod bump", async () => {
		await Bun.write(join(repo, "go/go.mod"), "bumped\n");

		const res = await runRefresh(repo);
		const stdout = res.stdout.toString();

		expect(res.exitCode).toBe(0);
		expect(stdout).not.toContain("nothing to do");
		const goNix = await readFile(join(repo, GO_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(goNix, GO_ENTRY.marker)).toBe(
			stubSriForFragment("go-modules"),
		);
	});

	// RIG-2852 Gap 1: the SAME go/go.mod bump must refresh the flake.nix MIRROR to
	// the identical value — not just guest-image/default.nix. Before this fix the
	// mirror was never touched, so an auto-opened Go bump landed with flake.nix's
	// vendorHash stale and `nix flake check` red. Assert every declared mirror got
	// the go-modules SRI and none kept its distinct placeholder.
	test("refreshes every flake.nix mirror to the same SRI on a go/go.mod bump", async () => {
		await Bun.write(join(repo, "go/go.mod"), "bumped\n");

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);

		const mirrors = GO_ENTRY.mirrorFiles ?? [];
		expect(mirrors.length).toBeGreaterThan(0); // the Go entry declares flake.nix
		for (const mirror of mirrors) {
			const text = await readFile(join(repo, mirror), "utf8");
			expect(maybeHashOnMarker(text, GO_ENTRY.marker)).toBe(
				stubSriForFragment("go-modules"),
			);
			expect(text).not.toContain(bodyOf(PLACEHOLDER_GO_MIRROR));
		}
	});

	// A gomod-only bump refreshes the Go vendorHash and leaves the bun outputHash
	// pin byte-for-byte untouched — the per-FOD self-gate granularity.
	test("a go/go.mod bump leaves the bun outputHash pin untouched", async () => {
		const bunBefore = await readFile(join(repo, BUN_WRITE_ENTRY.file), "utf8");
		await Bun.write(join(repo, "go/go.mod"), "bumped\n");

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);

		const goNix = await readFile(join(repo, GO_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(goNix, GO_ENTRY.marker)).toBe(
			stubSriForFragment("go-modules"),
		);
		expect(await readFile(join(repo, BUN_WRITE_ENTRY.file), "utf8")).toBe(
			bunBefore,
		);
	});

	// A bun.lock bump refreshes only the bun outputHash; the Go vendorHash is left
	// untouched (its trigger, go/go.mod|go.sum, did not change).
	test("a bun.lock bump refreshes the bun outputHash and leaves the Go pin", async () => {
		const goBefore = await readFile(join(repo, GO_ENTRY.file), "utf8");
		await Bun.write(join(repo, "bun.lock"), "bumped\n");

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);

		const bunNix = await readFile(join(repo, BUN_WRITE_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(bunNix, BUN_WRITE_ENTRY.marker)).toBe(
			stubSriForFragment("node-modules"),
		);
		expect(await readFile(join(repo, GO_ENTRY.file), "utf8")).toBe(goBefore);
	});

	// The HIGH-finding regression (RIG-3296 review): a devenv-nixpkgs channel bump
	// that does NOT move biome leaves bun.lock untouched (refresh-devenv-nixpkgs
	// skips the relock when the catalog pin is static), yet the channel moves
	// pkgs.bun — the FOD's builder — which may move the recursive outputHash. So
	// the node-modules entry gates on devenv.lock too: a devenv.lock-only diff
	// must still refresh the bun outputHash, or a biome-static channel bump ships
	// the exact `compass-agent-node-modules` hash mismatch this task exists to
	// prevent. The Go pin is left untouched (its trigger did not change).
	test("a devenv.lock-only bump refreshes the bun outputHash (channel pkgs.bun move)", async () => {
		const goBefore = await readFile(join(repo, GO_ENTRY.file), "utf8");
		await Bun.write(join(repo, "devenv.lock"), "bumped\n");

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);

		const bunNix = await readFile(join(repo, BUN_WRITE_ENTRY.file), "utf8");
		expect(
			hashOnMarker(bunNix, BUN_WRITE_ENTRY.marker, BUN_WRITE_ENTRY.file),
		).toBe(stubSriForFragment("node-modules"));
		expect(await readFile(join(repo, GO_ENTRY.file), "utf8")).toBe(goBefore);
	});

	// Restore-on-completion: the script fakes the pin to force the mismatch, then
	// writes the REAL value — never leaving the fake all-A hash in the tree.
	test("never leaves the fake all-A SRI in a refreshed pin file", async () => {
		await Bun.write(join(repo, "go/go.sum"), "bumped\n");
		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		const goNix = await readFile(join(repo, GO_ENTRY.file), "utf8");
		expect(goNix).not.toContain("sha256-AAAAAAAA");
	});

	// The self-gate contract: with NO trigger manifest changed the script is a
	// cheap no-op — no build, no rewrite. Guards against the gate being dropped.
	test("is a no-op when no trigger manifest changed", async () => {
		const res = await runRefresh(repo);
		const stdout = res.stdout.toString();

		expect(res.exitCode).toBe(0);
		expect(stdout).toContain("nothing to do");
		const goNix = await readFile(join(repo, GO_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(goNix, GO_ENTRY.marker)).toBe(PLACEHOLDER_GO);
	});

	// Silent-no-op defense: if the build reports no `got:` for the gated FOD (a
	// broken build, or a parse regression), the script must fail LOUD (exit≠0),
	// never write an empty/placeholder pin and exit 0.
	test("fails loud (exit≠0) when the build reports no got: for the FOD", async () => {
		// Replace nix with one that fails WITHOUT a mismatch block.
		await Bun.write(
			join(repo, "stubbin", "nix"),
			"#!/usr/bin/env bash\necho 'error: build failed' >&2\nexit 1\n",
		);
		await chmod(join(repo, "stubbin", "nix"), 0o755);
		await Bun.write(join(repo, "go/go.mod"), "bumped\n");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		expect(res.stderr.toString()).toMatch(/no 'got:' SRI/);
		// The pin was restored to its original placeholder, not left faked.
		const goNix = await readFile(join(repo, GO_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(goNix, GO_ENTRY.marker)).toBe(PLACEHOLDER_GO);
	});
});

describe("two builders, one outputHash: the verify entry", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildBaselineRepo();
	});
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	// The EQUAL path — the expected outcome on a healthy branch. Both vehicles
	// report the same SRI, so the verify entry writes nothing, changes nothing,
	// and does not fail the task. The pin ends at the authoritative value.
	test("agreement between the two vehicles is a silent no-op", async () => {
		await Bun.write(join(repo, "bun.lock"), "bumped\n");

		const res = await runRefresh(repo);
		const stdout = res.stdout.toString();

		expect(res.exitCode).toBe(0);
		expect(stdout).toContain("verified");
		expect(stdout).not.toContain("cannot satisfy both");
		const bunNix = await readFile(join(repo, BUN_WRITE_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(bunNix, BUN_WRITE_ENTRY.marker)).toBe(
			stubSriForFragment("node-modules"),
		);
		// And it left no fake behind on the second realise either.
		expect(bunNix).not.toContain("sha256-AAAAAAAA");
	});

	// The MISMATCH path — the whole reason the second vehicle exists. The
	// agent-image vehicle reports a different SRI for the same FOD, meaning the
	// two channel revs' buns produce different install trees. One literal cannot
	// serve both, so the task must exit non-zero (reddening `renovate/artifacts`)
	// with a diagnosis naming BOTH SRIs, both vehicles, and both channel revs.
	test("a divergent second vehicle throws naming both SRIs, vehicles and revs", async () => {
		await Bun.write(join(repo, "bun.lock"), "bumped\n");

		const res = await runRefresh(repo, {
			STUB_DIVERGE_VEHICLE: BUN_VERIFY_ENTRY.buildFile,
		});
		const stderr = res.stderr.toString();

		expect(res.exitCode).not.toBe(0);
		expect(stderr).toContain("cannot satisfy both");
		// Both SRIs, named.
		expect(stderr).toContain(stubSriForFragment("node-modules"));
		expect(stderr).toContain(stubSriForFragment("node-modules", "diverged"));
		// Both vehicles, named.
		expect(stderr).toContain(BUN_WRITE_ENTRY.buildFile);
		expect(stderr).toContain(BUN_VERIFY_ENTRY.buildFile);
		// Both channel revs, named.
		expect(stderr).toContain(revFor(BUN_WRITE_ENTRY.vehicleChannelLock));
		expect(stderr).toContain(revFor(BUN_VERIFY_ENTRY.vehicleChannelLock));
	});

	// A verify entry must never leave its own recomputed value in the tree: the
	// authoritative write is what ships. Even on the throwing path the pin on disk
	// is the authoritative SRI, never the divergent one and never the fake.
	test("the verify entry never writes its own value, even when it diverges", async () => {
		await Bun.write(join(repo, "bun.lock"), "bumped\n");

		const res = await runRefresh(repo, {
			STUB_DIVERGE_VEHICLE: BUN_VERIFY_ENTRY.buildFile,
		});
		expect(res.exitCode).not.toBe(0);

		const bunNix = await readFile(join(repo, BUN_WRITE_ENTRY.file), "utf8");
		expect(maybeHashOnMarker(bunNix, BUN_WRITE_ENTRY.marker)).toBe(
			stubSriForFragment("node-modules"),
		);
		expect(bunNix).not.toContain("sha256-AAAAAAAA");
	});

	// Ordering is load-bearing: the authoritative write must land BEFORE the
	// verify reads its baseline, or every ordinary refresh would be reported as a
	// divergence (the verify would compare the new value against the stale pin).
	// Assert it on the observable log order, so a refactor that reorders the run
	// set fails here.
	test("writes the authoritative value before the verify reads its baseline", async () => {
		await Bun.write(join(repo, "bun.lock"), "bumped\n");

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);

		const stdout = res.stdout.toString();
		const wrote = stdout.indexOf(
			`${BUN_WRITE_ENTRY.file} -> ${stubSriForFragment("node-modules")}`,
		);
		const verified = stdout.indexOf("verifying");
		expect(wrote).toBeGreaterThanOrEqual(0);
		expect(verified).toBeGreaterThanOrEqual(0);
		expect(wrote).toBeLessThan(verified);
	});

	// Structural guard, not just ordering: refreshEntry itself refuses a verify
	// entry, so no future caller can write through one by accident and clobber the
	// authoritative value with the second builder's.
	test("refreshEntry refuses to write a verify entry", async () => {
		await expect(refreshEntry(BUN_VERIFY_ENTRY)).rejects.toThrow(
			/must never write/,
		);
	});
});

describe("parseGotForFragment", () => {
	const OUT = [
		"error: hash mismatch in fixed-output derivation '/nix/store/x-compass-go-modules.drv':",
		"         specified: sha256-AAAA=",
		"            got:    sha256-realgo=",
		"error: hash mismatch in fixed-output derivation '/nix/store/y-compass-node-modules.drv':",
		"         specified: sha256-BBBB=",
		"            got:    sha256-realbun=",
	].join("\n");

	test("returns the got: SRI for the matching drv fragment", () => {
		expect(parseGotForFragment(OUT, "go-modules")).toBe("sha256-realgo=");
		expect(parseGotForFragment(OUT, "node-modules")).toBe("sha256-realbun=");
	});

	test("does not misattribute a sibling FOD's got: to the wrong fragment", () => {
		// Only the go block present; asking for node-modules must find nothing,
		// never fall through to the go got:.
		const onlyGo = OUT.split("\n").slice(0, 3).join("\n");
		expect(parseGotForFragment(onlyGo, "node-modules")).toBeUndefined();
		expect(parseGotForFragment(onlyGo, "go-modules")).toBe("sha256-realgo=");
	});

	test("finds got: beyond a fixed 5-line window (scans to next header)", () => {
		// Guards the unbounded within-block scan against a future nix that adds
		// context lines before `got:` (or wraps the drv path) — a fixed lookahead
		// window would silently miss it and fail every FOD build.
		const wide = [
			"error: hash mismatch in fixed-output derivation '/nix/store/x-compass-go-modules.drv':",
			"         specified: sha256-AAAA=",
			"         (context line a)",
			"         (context line b)",
			"         (context line c)",
			"            got:    sha256-farbelow=",
		].join("\n");
		expect(parseGotForFragment(wide, "go-modules")).toBe("sha256-farbelow=");
	});

	test("stops at the next mismatch header (no cross-block got: bleed)", () => {
		// A fragment whose own block reports no got: (defensive) must not scan past
		// the next header and steal the sibling's got:.
		const noGotThenSibling = [
			"error: hash mismatch in fixed-output derivation '/nix/store/x-compass-go-modules.drv':",
			"         specified: sha256-AAAA=",
			"error: hash mismatch in fixed-output derivation '/nix/store/y-compass-node-modules.drv':",
			"         specified: sha256-BBBB=",
			"            got:    sha256-realbun=",
		].join("\n");
		expect(parseGotForFragment(noGotThenSibling, "go-modules")).toBeUndefined();
	});

	test("returns undefined when no mismatch is reported", () => {
		expect(
			parseGotForFragment("built '/nix/store/x'\n", "go-modules"),
		).toBeUndefined();
	});
});

describe("rewriteInlineHash", () => {
	test("rewrites the sha256 on the marker line to the new SRI", () => {
		const out = rewriteInlineHash(
			GO_NIX_FIXTURE,
			GO_ENTRY.marker,
			"sha256-newgo=",
			GO_ENTRY.file,
		);
		expect(maybeHashOnMarker(out, GO_ENTRY.marker)).toBe("sha256-newgo=");
	});

	test("is idempotent — rewriting to the same SRI yields identical content", () => {
		const out = rewriteInlineHash(
			GO_NIX_FIXTURE,
			GO_ENTRY.marker,
			PLACEHOLDER_GO,
			GO_ENTRY.file,
		);
		expect(out).toBe(GO_NIX_FIXTURE);
	});

	test("throws on an empty new SRI", () => {
		expect(() =>
			rewriteInlineHash(GO_NIX_FIXTURE, GO_ENTRY.marker, "", GO_ENTRY.file),
		).toThrow(/empty/);
	});

	test("throws when the marker is not found", () => {
		expect(() =>
			rewriteInlineHash(
				GO_NIX_FIXTURE,
				'noSuch = "sha256-',
				"sha256-x=",
				GO_ENTRY.file,
			),
		).toThrow(/marker .* not found/);
	});

	test("inserts the new SRI literally (no $-sequence interpretation)", () => {
		// A base64 SRI cannot contain `$` today, but the write must stay literal so
		// a future parse widening can't let `$&`/`$1`/`$$` in the replacement mangle
		// the pin. Feed a `$`-bearing value and assert it lands verbatim.
		const withDollars = "sha256-a$&b$1c$$d=";
		const out = rewriteInlineHash(
			GO_NIX_FIXTURE,
			GO_ENTRY.marker,
			withDollars,
			GO_ENTRY.file,
		);
		expect(maybeHashOnMarker(out, GO_ENTRY.marker)).toBe(withDollars);
	});
});

describe("hashOnMarker", () => {
	test("reads the committed SRI off the marker line", () => {
		expect(hashOnMarker(BUN_NIX_FIXTURE, BUN_WRITE_ENTRY.marker, "f.nix")).toBe(
			PLACEHOLDER_BUN,
		);
	});

	// The verify baseline must never degrade to "no mismatch observed" when the
	// pin cannot be read — that would pass a branch whose pin was never checked.
	test("throws when the marker is missing", () => {
		expect(() =>
			hashOnMarker(BUN_NIX_FIXTURE, 'noSuch = "sha256-', "f.nix"),
		).toThrow(/marker .* not found/);
	});

	test("throws when the marker line carries no SRI", () => {
		expect(() =>
			hashOnMarker('  outputHash = "";\n', "outputHash", "f.nix"),
		).toThrow(/no sha256- SRI/);
	});
});

describe("FOD_ENTRIES table invariants", () => {
	// A base row to mutate per case, so each violating table differs from a legal
	// one in exactly the property under test.
	const write: FodEntry = {
		id: "w",
		file: "a.nix",
		marker: 'outputHash = "sha256-',
		drvFragment: "node-modules",
		buildFile: "veh-a.nix",
		buildTarget: "t",
		vehicleChannelLock: "a.lock",
		triggers: ["bun.lock"],
	};
	const verify: FodEntry = {
		...write,
		id: "v",
		buildFile: "veh-b.nix",
		vehicleChannelLock: "b.lock",
		verifyOf: "w",
	};

	test("the shipped table satisfies every invariant", () => {
		expect(() => assertFodTableInvariants(FOD_ENTRIES)).not.toThrow();
	});

	// `vehicleChannelLock` is a CLAIM about `buildFile`: it names the lock whose
	// nixpkgs node supplies that vehicle's `pkgs`. Nothing in the type system ties
	// the two, so a vehicle pointed at the sibling scope's lock would satisfy
	// every structural invariant (different file name, different target) and then
	// re-derive the identical hash — a verify that always agrees and checks
	// nothing. Read the vehicle and assert it really reads the lock it declares.
	test("every entry's vehicle reads the channel lock the table declares", async () => {
		for (const entry of FOD_ENTRIES) {
			const vehicle = await readFile(join(repoRoot, entry.buildFile), "utf8");
			expect(
				vehicle.includes(entry.vehicleChannelLock),
				`${entry.buildFile} must read ${entry.vehicleChannelLock} ` +
					`(declared by entry '${entry.id}')`,
			).toBe(true);
		}
	});

	// buildFile became a per-entry field, so its existence is a property of the
	// TABLE, not of the one entry a scope-specific suite happens to look at. A
	// renamed or moved vehicle must fail here rather than at Renovate-branch time.
	test("every entry's vehicle file exists", async () => {
		for (const entry of FOD_ENTRIES) {
			expect(
				await Bun.file(join(repoRoot, entry.buildFile)).exists(),
				`${entry.buildFile} (entry '${entry.id}') must exist`,
			).toBe(true);
		}
	});

	// No gate in the repo EVALUATES the renovate vehicle: the only nix eval in CI
	// is over tools/toolchain/gate-tools.nix, and guest-image/default.nix is
	// covered by the guest-image project's own build. So a syntax break in a
	// vehicle would pass every check on the PR that ships it and first surface on
	// a live Renovate branch, where the realise fails with an eval error, the
	// refresh exits non-zero, and the bumped lock still lands beside an
	// unrefreshed pin — the silent drift this table exists to prevent, through a
	// different door. `--parse` is offline and sub-second, so it runs here.
	//
	// The precondition uses `which`, NOT `command -v`: the latter is a shell
	// BUILTIN that Bun's own shell does not implement, so it returns non-zero
	// even where nix is installed — which would skip this test everywhere and
	// leave it permanently vacuous.
	test("every entry's vehicle parses as nix", async () => {
		const nixOnPath = await $`which nix-instantiate`.nothrow().quiet();
		if (nixOnPath.exitCode !== 0) return;
		for (const entry of FOD_ENTRIES) {
			const res =
				await $`nix-instantiate --parse ${join(repoRoot, entry.buildFile)}`
					.nothrow()
					.quiet();
			expect(
				res.exitCode,
				`${entry.buildFile} (entry '${entry.id}') must parse: ` +
					res.stderr.toString(),
			).toBe(0);
		}
	});

	// The shipped pairing is the point of the whole mechanism: one writer, one
	// verifier, two vehicles over the SAME pin.
	test("the shipped table pairs the entrypoint outputHash across two vehicles", () => {
		expect(BUN_VERIFY_ENTRY.verifyOf).toBe(BUN_WRITE_ENTRY.id);
		expect(BUN_VERIFY_ENTRY.file).toBe(BUN_WRITE_ENTRY.file);
		expect(BUN_VERIFY_ENTRY.marker).toBe(BUN_WRITE_ENTRY.marker);
		expect(BUN_VERIFY_ENTRY.buildFile).not.toBe(BUN_WRITE_ENTRY.buildFile);
		expect(BUN_VERIFY_ENTRY.vehicleChannelLock).not.toBe(
			BUN_WRITE_ENTRY.vehicleChannelLock,
		);
	});

	test("rejects duplicate ids", () => {
		expect(() => assertFodTableInvariants([write, { ...write }])).toThrow(
			/duplicate FodEntry id/,
		);
	});

	// got: attribution safety, scoped to a vehicle: within ONE vehicle's build
	// output, `drvName.includes(fragment)` would let the shorter fragment bind the
	// longer entry's block and write the wrong hash.
	test("rejects a drvFragment that is a substring of another IN THE SAME vehicle", () => {
		expect(() =>
			assertFodTableInvariants([
				{ ...write, id: "a", drvFragment: "modules" },
				{
					...write,
					id: "b",
					file: "b.nix",
					drvFragment: "node-modules",
				},
			]),
		).toThrow(/ambiguous drvFragment/);
	});

	// …but the SAME fragment across two vehicles is legal and load-bearing: it is
	// literally the same derivation seen through two nixpkgs, and each realise
	// reads only its own vehicle's output.
	test("allows the same drvFragment across two different vehicles", () => {
		expect(() => assertFodTableInvariants([write, verify])).not.toThrow();
	});

	// Two writers over one pin are last-write-wins: the second realise's value
	// silently overwrites the first, hiding the divergence the pairing exists to
	// surface.
	test("rejects two authoritative entries writing the same file+marker", () => {
		expect(() =>
			assertFodTableInvariants([
				write,
				{ ...verify, verifyOf: undefined, id: "w2" },
			]),
		).toThrow(/both WRITE/);
	});

	test("rejects a verifyOf naming a nonexistent entry", () => {
		expect(() =>
			assertFodTableInvariants([{ ...verify, verifyOf: "nope" }]),
		).toThrow(/not an authoritative/);
	});

	test("rejects a verify entry checking a different pin", () => {
		expect(() =>
			assertFodTableInvariants([write, { ...verify, file: "other.nix" }]),
		).toThrow(/SAME pin/);
	});

	// A verify entry on the same vehicle re-derives the identical value: it would
	// always agree and check nothing, which is worse than absent because it reads
	// as coverage.
	test("rejects a verify entry realising the same vehicle as its target", () => {
		expect(() =>
			assertFodTableInvariants([
				write,
				{ ...verify, buildFile: write.buildFile },
			]),
		).toThrow(/SAME vehicle/);
	});

	// Divergent triggers would let the gate open the verify without its writer, so
	// the comparison baseline would be the pre-write (stale) pin.
	test("rejects a verify entry gated differently from its target", () => {
		expect(() =>
			assertFodTableInvariants([
				write,
				{ ...verify, triggers: ["bun.lock", "extra"] },
			]),
		).toThrow(/SAME triggers/);
	});

	test("rejects mirrorFiles on a verify entry", () => {
		expect(() =>
			assertFodTableInvariants([
				write,
				{ ...verify, mirrorFiles: ["flake.nix"] },
			]),
		).toThrow(/writes nothing/);
	});
});
