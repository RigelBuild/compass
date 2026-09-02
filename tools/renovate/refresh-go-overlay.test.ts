import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Orchestration harness for tools/renovate/refresh-go-overlay.ts (RIG-3100).
//
// The pure transforms (rev/version extraction) are unit-tested in
// refresh-go-overlay.core.test.ts. This drives the SHIPPED entry point end to
// end inside a throwaway git repo with stub `devenv`/`nix` on PATH, so the step
// SEQUENCING is exercised offline + deterministically: self-gate → read target
// → advance overlay → validate eval. The real nix eval + devenv re-lock run for
// real against the vendored devenv in the PR's own CI; here they are stubbed so
// the failure mode under test is the script's own control flow, not the
// network.

// The shipped entry point, invoked as Renovate will: `bun tools/renovate/…ts`,
// cwd = repo root. Copied into the throwaway repo so the SHIPPED file runs, not
// a re-implementation.
const SCRIPT_REL = "tools/renovate/refresh-go-overlay.ts";
const CORE_REL = "tools/renovate/refresh-go-overlay.core.ts";
const REAL_SCRIPT = join(import.meta.dir, "refresh-go-overlay.ts");
const REAL_CORE = join(import.meta.dir, "refresh-go-overlay.core.ts");

const GO_NIX_REL = "tools/toolchain/versions/go.nix";
const GATE_REL = "tools/toolchain/gate-tools.nix";
const DEVENV_LOCK_REL = "devenv.lock";

// Hermetic git: identity from env only, no user/global/system config leakage.
const HERMETIC_ENV = {
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "t",
	GIT_AUTHOR_EMAIL: "t@t",
	GIT_COMMITTER_NAME: "t",
	GIT_COMMITTER_EMAIL: "t@t",
	HOME: "/dev/null",
};

const OVERLAY_REV_BASE = "1111111111111111111111111111111111111111";
const OVERLAY_REV_BUMP = "2222222222222222222222222222222222222222";

const GO_VERSION_BASE = "1.26.6";
const GO_VERSION_BUMP = "1.27.0";

function goNix(version: string): string {
	return `{ version = "${version}"; }\n`;
}

// Minimal devenv.lock carrying only the go-overlay node the core reads
// (nodes['go-overlay'].locked.rev) — the field the script logs across the
// advance.
function devenvLock(overlayRev: string): string {
	return `${JSON.stringify(
		{
			nodes: {
				"go-overlay": {
					locked: {
						lastModified: 1,
						narHash: "sha256-AAAA",
						owner: "purpleclay",
						repo: "go-overlay",
						rev: overlayRev,
						type: "github",
					},
					original: {
						owner: "purpleclay",
						repo: "go-overlay",
						type: "github",
					},
				},
				root: { inputs: { "go-overlay": "go-overlay" } },
			},
			root: "root",
			version: 7,
		},
		null,
		2,
	)}\n`;
}

// A stand-in gate-tools.nix the stub `nix` never actually evaluates (the stub
// intercepts `nix eval`), present only so the file the script names exists in
// the tree — the SHIPPED script passes its path to `nix eval -f`.
const GATE_STUB = "{ }\n";

// Stub `devenv`: on `devenv update go-overlay`, rewrite devenv.lock to the
// BUMPED overlay rev — simulating the real re-lock advancing the input to the
// latest upstream rev. Any other invocation is a no-op success. Offline.
const STUB_DEVENV = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "update" ]; then
  cat > devenv.lock <<'LOCK'
${devenvLock(OVERLAY_REV_BUMP)}
LOCK
fi
exit 0
`;

// Stub `nix`: intercept `nix eval … -f <gate> langs.go.version` and model the
// two go-overlay outcomes the script must tell apart. It records its args so a
// test can assert the SHIPPED script evals the REAL CI target (a typo'd
// file/attr would otherwise pass green here), then branches on sentinel files:
//   .force-eval-fail — the PRIMARY RIG-3100 failure: a too-old overlay makes
//     go-bin.versions.<new> a MISSING attr, so `nix eval` exits non-zero.
//   .force-resolved  — a partial/wrong advance: the eval SUCCEEDS but yields a
//     DIFFERENT version, exercising the resolved !== target equality guard.
// With neither, it reads the version go.nix now pins out of the tree — modelling
// the real overlay resolving go-bin.versions.<pin> once the input is advanced.
const STUB_NIX = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "eval" ]; then
  echo "$@" > .nix-eval-args
  if [ -f .force-eval-fail ]; then
    echo "error: attribute matching the pinned go version is missing" >&2
    exit 1
  fi
  if [ -f .force-resolved ]; then
    cat .force-resolved
    exit 0
  fi
  grep -oE 'version = "[^"]+"' ${GO_NIX_REL} | head -1 | sed -E 's/version = "([^"]+)"/\\1/' | tr -d '\\n'
  exit 0
fi
exit 0
`;

async function buildRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "rig3100-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "tools", "toolchain", "versions"), {
		recursive: true,
	});
	await mkdir(join(repo, "stubbin"), { recursive: true });

	// Ship the REAL script + its core, so the SHIPPED file runs unmodified.
	await Bun.write(join(repo, SCRIPT_REL), await readFile(REAL_SCRIPT, "utf8"));
	await Bun.write(join(repo, CORE_REL), await readFile(REAL_CORE, "utf8"));

	await Bun.write(join(repo, GO_NIX_REL), goNix(GO_VERSION_BASE));
	await Bun.write(join(repo, GATE_REL), GATE_STUB);
	await Bun.write(join(repo, DEVENV_LOCK_REL), devenvLock(OVERLAY_REV_BASE));

	for (const [name, body] of [
		["devenv", STUB_DEVENV],
		["nix", STUB_NIX],
	] as const) {
		await Bun.write(join(repo, "stubbin", name), body);
		await chmod(join(repo, "stubbin", name), 0o755);
	}

	await $`git init -q -b main`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git add -A`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git commit -q -m baseline`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git remote add origin .`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git update-ref refs/remotes/origin/main main`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
	return repo;
}

// Run the shipped script as Renovate does: `bun tools/renovate/…ts`, cwd = repo
// root, stubs first on PATH so no real nix/devenv/network is touched. The real
// `bun` runs the script itself; git stays the real binary.
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

describe("tools/renovate/refresh-go-overlay.ts coupling (RIG-3100)", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildRepo();
	});
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	// Self-gate: with go.nix unchanged vs base, the script is a cheap no-op — no
	// overlay advance, no eval. Guards the gate against being dropped (an
	// unconditional run would re-lock + eval on EVERY Renovate branch). Gate on
	// go.nix, the file the go manager rewrites — NOT devenv.lock.
	test("is a no-op when go.nix is unchanged vs base", async () => {
		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).toContain("nothing to do");
		// devenv.lock untouched (overlay not advanced).
		const lock = await readFile(join(repo, DEVENV_LOCK_REL), "utf8");
		expect(lock).toContain(OVERLAY_REV_BASE);
		expect(lock).not.toContain(OVERLAY_REV_BUMP);
	});

	// The end-to-end happy path: bump go.nix (what the customManager's regex
	// update does), run the script, and assert it advanced the go-overlay input
	// in devenv.lock and the CI-path eval now resolves the bumped version.
	test("advances go-overlay and validates the bumped version resolves", async () => {
		await Bun.write(join(repo, GO_NIX_REL), goNix(GO_VERSION_BUMP));

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).not.toContain("nothing to do");
		expect(res.stdout.toString()).toContain(
			`target version=${GO_VERSION_BUMP}`,
		);
		// devenv.lock's go-overlay rev advanced to the bumped rev.
		const lock = await readFile(join(repo, DEVENV_LOCK_REL), "utf8");
		expect(lock).toContain(OVERLAY_REV_BUMP);
		expect(lock).not.toContain(OVERLAY_REV_BASE);
		// The validation eval saw the bumped version resolve.
		expect(res.stdout.toString()).toContain(`resolves go ${GO_VERSION_BUMP}`);
		// The eval hit the REAL CI target (gate-tools.nix langs.go.version), not a
		// typo'd file/attr that would pass green against an arg-agnostic stub.
		const evalArgs = await readFile(join(repo, ".nix-eval-args"), "utf8");
		expect(evalArgs).toContain(GATE_REL);
		expect(evalArgs).toContain("langs.go.version");
	});

	// Fail-loud, partial-advance case: the overlay advances but still resolves a
	// DIFFERENT version than the pin (a wrong/partial advance where the eval
	// SUCCEEDS). The resolved !== target equality guard must catch it and die
	// non-zero rather than ship a go bump the overlay does not actually resolve.
	test("fails loud (exit≠0) when the resolved version mismatches the pin", async () => {
		await Bun.write(join(repo, GO_NIX_REL), goNix(GO_VERSION_BUMP));
		// Force the eval to resolve a DIFFERENT version than the pin, modelling a
		// partial/wrong overlay advance whose eval succeeds — exercising the
		// equality guard (the missing-attr eval-throw is the sibling test below).
		await Bun.write(join(repo, ".force-resolved"), GO_VERSION_BASE);

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
	});

	// Fail-loud, missing-attr case: the PRIMARY RIG-3100 failure. A still-too-old
	// overlay makes `go-bin.versions.<new>` a missing attribute, so the CI-path
	// eval itself exits non-zero — it does NOT fall back to an older release. The
	// script must propagate that as a non-zero exit, in-branch, instead of red CI
	// on the PR.
	test("fails loud (exit≠0) when the validation eval fails (missing overlay attr)", async () => {
		await Bun.write(join(repo, GO_NIX_REL), goNix(GO_VERSION_BUMP));
		await Bun.write(join(repo, ".force-eval-fail"), "");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
	});
});
