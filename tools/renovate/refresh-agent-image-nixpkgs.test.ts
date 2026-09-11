import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";
import {
	AGENT_IMAGE_DIR,
	AGENT_IMAGE_LOCK,
	agentImageNixpkgsRev,
	NIXPKGS_INPUT,
} from "./refresh-agent-image-nixpkgs.core.ts";
import { FOD_ENTRIES } from "./refresh-fod-hashes.ts";

// Unit tests for the pure core of refresh-agent-image-nixpkgs.ts: reading the
// devenv-nixpkgs CHANNEL rev out of the agent base image's devenv lock, and the
// scope geometry the relock depends on. No devenv / network / git — that
// shell-out lives in the entry point and runs for real on the PR's own branch.
// These assert what a wrong line would silently corrupt: relocking the wrong
// scope (whose write the rule's two-file fileFilters then discards), or reading
// the fork / inner-nixpkgs rev instead of the channel rev.

const repoRoot = join(import.meta.dir, "..", "..");

// A lock with the same node AMBIGUITY the real one has: an outer `nixpkgs`
// channel node, its transitive `nixpkgs-src` (upstream NixOS/nixpkgs), and the
// `devenv` fork node — so a reader that grabbed the wrong node passes nothing.
function agentImageLock(
	channelRev: string,
	innerRev: string,
	forkRev: string,
): string {
	return `${JSON.stringify(
		{
			nodes: {
				devenv: {
					locked: {
						lastModified: 3,
						narHash: "sha256-FORK",
						owner: "RigelBuild",
						repo: "devenv",
						rev: forkRev,
						type: "github",
					},
					original: { owner: "RigelBuild", repo: "devenv", type: "github" },
				},
				nixpkgs: {
					inputs: { "nixpkgs-src": "nixpkgs-src" },
					locked: {
						lastModified: 1,
						narHash: "sha256-CHANNEL",
						owner: "cachix",
						repo: "devenv-nixpkgs",
						rev: channelRev,
						type: "github",
					},
					original: {
						owner: "cachix",
						ref: "rolling",
						repo: "devenv-nixpkgs",
						type: "github",
					},
				},
				"nixpkgs-src": {
					flake: false,
					locked: {
						lastModified: 2,
						narHash: "sha256-INNER",
						owner: "NixOS",
						repo: "nixpkgs",
						rev: innerRev,
						type: "github",
					},
				},
				root: { inputs: { devenv: "devenv", nixpkgs: "nixpkgs" } },
			},
			root: "root",
			version: 7,
		},
		null,
		2,
	)}\n`;
}

const CHANNEL_REV = "1111111111111111111111111111111111111111";
const INNER_REV = "2222222222222222222222222222222222222222";
const FORK_REV = "3333333333333333333333333333333333333333";

describe("agent-image scope geometry", () => {
	// The cwd IS the scope selector — devenv resolves devenv.yaml/devenv.lock
	// relative to it — so the relock directory must be the directory the lock
	// lives in. A mismatch would relock the ROOT lock while this rule's
	// fileFilters names the agent-image one, so Renovate would commit nothing
	// and the rev bump would ship unrelocked.
	test("the relock cwd is the directory holding the lock", () => {
		expect(AGENT_IMAGE_LOCK.startsWith(`${AGENT_IMAGE_DIR}/`)).toBe(true);
		expect(AGENT_IMAGE_LOCK.slice(AGENT_IMAGE_DIR.length + 1)).toBe(
			"devenv.lock",
		);
	});

	// The single named input, not a bare `devenv update`: relocking every input
	// would bloat the PR's diff past the channel advance the branch is about.
	// Asserted against the real devenv.yaml rather than against the constant's own
	// literal — the hazard is the input being RENAMED upstream, which a
	// self-referential equality check cannot see.
	test("the relocked input names a real input of this scope", () => {
		const yaml = Bun.YAML.parse(
			readFileSync(join(repoRoot, AGENT_IMAGE_DIR, "devenv.yaml"), "utf8"),
		) as { inputs?: Record<string, { url?: string }> };
		const inputs = yaml.inputs ?? {};
		expect(Object.keys(inputs)).toContain(NIXPKGS_INPUT);
		expect(inputs[NIXPKGS_INPUT]?.url).toContain("devenv-nixpkgs");
	});

	// Ground truth: the lock the config's manager governs must exist at this
	// path and actually pin a 40-hex channel rev.
	test("the real lock exists and pins a 40-hex channel rev", () => {
		const text = readFileSync(join(repoRoot, AGENT_IMAGE_LOCK), "utf8");
		expect(agentImageNixpkgsRev(text)).toMatch(/^[a-f0-9]{40}$/);
	});

	// The rev read here is the one the FOD refresh hangs off: the channel
	// resolves the bun entrypoint.nix's builder uses, so the FOD table must gate
	// EVERY entry over that pin on this lock. If the table and this task disagree,
	// the entry point throws rather than shipping a possibly-stale outputHash —
	// assert the agreement here too so the drift fails in a unit test, not on a
	// branch.
	test("every FOD entry for entrypoint.nix is gated on this lock", () => {
		const entries = FOD_ENTRIES.filter(
			(e) => e.file === "agent-image/entrypoint.nix",
		);
		expect(entries.length).toBeGreaterThan(0);
		for (const entry of entries) {
			expect(entry.triggers).toContain(AGENT_IMAGE_LOCK);
			// And still on bun.lock — the manifest trigger that already existed; this
			// task ADDS a cause, it does not replace one.
			expect(entry.triggers).toContain("bun.lock");
		}
	});

	// This lock resolves the bun the OCI image build uses, so the shared
	// outputHash is only actually CHECKED for that builder if some entry realises
	// a vehicle whose pkgs come from this lock. Without one, the refresh reverts
	// to a one-builder rewrite whose divergence surfaces only on the image build.
	// The vehicle file must exist too — a table naming a deleted file would fail
	// at realise time on a branch, not here.
	test("a FOD entry realises entrypoint.nix through this lock's own pkgs", () => {
		const viaThisLock = FOD_ENTRIES.filter(
			(e) =>
				e.file === "agent-image/entrypoint.nix" &&
				e.vehicleChannelLock === AGENT_IMAGE_LOCK,
		);
		expect(viaThisLock.length).toBe(1);
		const [entry] = viaThisLock;
		if (!entry) throw new Error("expected exactly one entry via this lock");
		// It VERIFIES; the authoritative write stays with the root-pkgs vehicle, or
		// two writers over one marker would be last-write-wins.
		expect(entry.verifyOf).toBeDefined();
		expect(existsSync(join(repoRoot, entry.buildFile))).toBe(true);
	});
});

describe("agentImageNixpkgsRev", () => {
	test("reads the outer channel rev", () => {
		expect(
			agentImageNixpkgsRev(agentImageLock(CHANNEL_REV, INNER_REV, FORK_REV)),
		).toBe(CHANNEL_REV);
	});

	// The three-node ambiguity is the whole risk: `nixpkgs-src` is the upstream
	// nixpkgs the channel resolves TO (refreshed from the outer rev by the
	// relock, never tracked), and `devenv` is the fork pin a sibling manager
	// owns. Reading either would log a bogus advance and, worse, make the
	// byte-identical guard reason about the wrong field.
	test("reads neither the inner nixpkgs-src rev nor the devenv fork rev", () => {
		const rev = agentImageNixpkgsRev(
			agentImageLock(CHANNEL_REV, INNER_REV, FORK_REV),
		);
		expect(rev).not.toBe(INNER_REV);
		expect(rev).not.toBe(FORK_REV);
	});

	// Fail loud on a lock-shape change rather than silently reading wrong: the
	// lock is external-boundary data, and a task that cannot name the rev it is
	// moving must not proceed to a relock.
	test("throws when the nixpkgs node is absent", () => {
		const noNode = JSON.stringify({ nodes: { root: {} }, root: "root" });
		expect(() => agentImageNixpkgsRev(noNode)).toThrow(
			/could not read a 40-hex nixpkgs channel rev/,
		);
	});

	test("throws when the rev is not 40-hex", () => {
		expect(() =>
			agentImageNixpkgsRev(agentImageLock("abc123", INNER_REV, FORK_REV)),
		).toThrow(/could not read a 40-hex nixpkgs channel rev/);
	});

	test("throws with a named diagnosis on invalid JSON", () => {
		expect(() => agentImageNixpkgsRev("not json")).toThrow(/is not valid JSON/);
	});

	// The error must name THIS task, not a sibling: a fail-loud script's value is
	// its diagnosis, and a message reading `refresh-devenv-nixpkgs` on an
	// agent-image branch sends the reader to the wrong scope.
	test("its diagnoses name this task and this lock", () => {
		expect(() => agentImageNixpkgsRev("not json")).toThrow(
			/refresh-agent-image-nixpkgs/,
		);
		expect(() => agentImageNixpkgsRev("not json")).toThrow(
			/agent-image\/devenv\.lock/,
		);
	});
});

// ── Entry-point tests: the SHIPPED script, end to end, offline ──────────────
//
// Everything above is pure. The branches that decide whether a channel bump
// ships CORRECTLY live in the entry point: the self-gate against the base ref,
// the base-ref resolution ladder, the byte-identical guard, and the argv/cwd of
// the relock. Those are the lines a plausible bug hides in — an inverted
// comparison, a wrong cwd, a swallowed failure — so they run here against a
// throwaway git repo with a stub `nix` first on PATH. No network, no devenv,
// no real nix; git is the real binary.

const HERMETIC_ENV = {
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "t",
	GIT_AUTHOR_EMAIL: "t@t",
	GIT_COMMITTER_NAME: "t",
	GIT_COMMITTER_EMAIL: "t@t",
	HOME: "/dev/null",
};

const SCRIPT_REL = "tools/renovate/refresh-agent-image-nixpkgs.ts";
const RELOCKED_CHANNEL_REV = "4444444444444444444444444444444444444444";
const STUB_SRI = "sha256-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=";

// The stub logs its cwd and argv, then writes a RELOCKED lock only when the
// post-`--` args are exactly `update nixpkgs`. So a regression that dropped the
// separator, or named another input, writes nothing and fails through BEHAVIOUR
// (the byte-identical guard fires) rather than only through an argv assertion.
const STUB_NIX = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "build" ]; then
  echo "error: hash mismatch in fixed-output derivation '/nix/store/x-compass-agent-node-modules-0.1.0.drv':" >&2
  echo "         specified: sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" >&2
  echo "            got:    ${STUB_SRI}" >&2
  exit 1
fi
if [ "\${1:-}" = "run" ]; then
  echo "$PWD" >> "$REPO_ROOT/.update-cwds"
  echo "$@" >> "$REPO_ROOT/.update-args"
  if [ -f "$REPO_ROOT/.force-noop" ]; then exit 0; fi
  if [ -f "$REPO_ROOT/.force-fail" ]; then
    echo "stub nix: forced relock failure" >&2
    exit 1
  fi
  shift 2
  if [ "\${1:-}" != "--" ]; then exit 0; fi
  shift
  if [ "\${1:-}" != "update" ] || [ "\${2:-}" != "nixpkgs" ]; then exit 0; fi
  cat > devenv.lock <<'LOCK'
${agentImageLock(RELOCKED_CHANNEL_REV, INNER_REV, FORK_REV)}
LOCK
fi
exit 0
`;

// A stub `bun` is NOT used: the real bun runs the shipped script. But the FOD
// refresh the script drives at the end shells `nix build`, which the stub above
// answers with exit 0 and no `got:` line — so the FOD leg is neutralised by
// pointing the table's triggers at files this fixture never changes. What these
// tests cover is the relock half; the FOD half has its own suite.
async function buildEntryRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "rig3365-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "tools", "toolchain", "devenv-cli"), {
		recursive: true,
	});
	await mkdir(join(repo, AGENT_IMAGE_DIR), { recursive: true });
	await mkdir(join(repo, "stubbin"), { recursive: true });

	// Ship the REAL script and every module it imports, so the shipped file runs
	// unmodified.
	for (const rel of [
		SCRIPT_REL,
		"tools/renovate/refresh-agent-image-nixpkgs.core.ts",
		"tools/renovate/refresh-fod-hashes.ts",
		"tools/toolchain/devenv-cli/core.ts",
	]) {
		await Bun.write(
			join(repo, rel),
			await readFile(join(repoRoot, rel), "utf8"),
		);
	}

	await Bun.write(
		join(repo, AGENT_IMAGE_LOCK),
		agentImageLock(CHANNEL_REV, INNER_REV, FORK_REV),
	);

	// The FOD leg runs after the relock, so the pin file and both vehicles have
	// to exist. The stub answers `nix build` by printing a got: line for the
	// fragment, which is what recompute parses the real value out of.
	await Bun.write(
		join(repo, "agent-image", "entrypoint.nix"),
		`{ pkgs, lib }:\n  outputHash = "${STUB_SRI}";\n`,
	);
	for (const entry of FOD_ENTRIES) {
		await mkdir(join(repo, entry.buildFile, ".."), { recursive: true });
		await Bun.write(join(repo, entry.buildFile), "{ }\n");
	}
	await Bun.write(join(repo, "stubbin", "nix"), STUB_NIX);
	await chmod(join(repo, "stubbin", "nix"), 0o755);

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

// The rev-only rewrite the customManager performs: it moves the channel rev and
// NOTHING else, so narHash still describes the base rev — the inconsistent state
// the relock exists to repair.
async function applyChannelBump(repo: string, rev: string) {
	const bumped = agentImageLock(rev, INNER_REV, FORK_REV);
	await Bun.write(join(repo, AGENT_IMAGE_LOCK), bumped);
}

async function runScript(repo: string, baseBranch = "main") {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			RENOVATE_BASE_BRANCH: baseBranch,
			REPO_ROOT: repo,
		})
		.quiet()
		.nothrow();
}

describe("tools/renovate/refresh-agent-image-nixpkgs.ts entry point", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildEntryRepo();
	});
	afterEach(async () => {
		await rm(repo, { recursive: true, force: true });
	});

	// The self-gate: this rule's branch is the only one that should relock, so a
	// branch whose lock matches base must no-op WITHOUT invoking nix. Without it,
	// every unrelated branch would pay a relock and could commit a drive-by pin.
	test("no-ops without invoking nix when the lock matches base", async () => {
		const res = await runScript(repo);
		expect(res.exitCode).toBe(0);
		expect(existsSync(join(repo, ".update-args"))).toBe(false);
		expect(res.stdout.toString() + res.stderr.toString()).toContain(
			"nothing to do",
		);
	});

	// A relock that leaves the lock byte-identical means the rev moved but the
	// narHash never re-resolved: shipping that is the silent-drift failure, so it
	// must be LOUD.
	test("fails when the relock leaves the lock byte-identical", async () => {
		await applyChannelBump(repo, RELOCKED_CHANNEL_REV);
		await Bun.write(join(repo, ".force-noop"), "");
		const res = await runScript(repo);
		expect(res.exitCode).not.toBe(0);
	});

	// A failing relock must not exit 0 — Renovate would commit the rev-only bump
	// and the branch would carry a lock describing the wrong rev.
	test("fails when the relock command itself fails", async () => {
		await applyChannelBump(repo, RELOCKED_CHANNEL_REV);
		await Bun.write(join(repo, ".force-fail"), "");
		const res = await runScript(repo);
		expect(res.exitCode).not.toBe(0);
	});

	// An unresolvable base ref must not read as "nothing changed" — that would
	// turn a broken environment into a silent skip.
	test("fails with a named error when the base ref does not resolve", async () => {
		const res = await runScript(repo, "no-such-branch");
		expect(res.exitCode).not.toBe(0);
		expect(res.stderr.toString() + res.stdout.toString()).toMatch(
			/no-such-branch/,
		);
	});

	// The relock must run IN agent-image/ (cwd is devenv's scope selector) and
	// name this scope's own fork rev — relocking from root's devenv would write
	// this lock with the wrong tool.
	test("relocks in the agent-image dir via this scope's own fork rev", async () => {
		await applyChannelBump(repo, RELOCKED_CHANNEL_REV);
		const res = await runScript(repo);
		expect(res.exitCode).toBe(0);
		const cwds = (await readFile(join(repo, ".update-cwds"), "utf8")).trim();
		expect(cwds.endsWith(`/${AGENT_IMAGE_DIR}`)).toBe(true);
		const args = (await readFile(join(repo, ".update-args"), "utf8")).trim();
		expect(args).toContain(`-- update ${NIXPKGS_INPUT}`);
		expect(args).toContain(FORK_REV);
	});
});
