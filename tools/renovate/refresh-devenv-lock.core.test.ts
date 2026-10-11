import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	changedDevenvLocks,
	DEVENV_LOCK_PATHS,
	DEVENV_LOCK_SCOPES,
	devenvForkLockedRev,
} from "./refresh-devenv-lock.core.ts";

// Unit tests for the pure decision core (RIG-2815): which devenv locks a branch
// touched, and reading the fork rev out of a lock. These assert the decisions a
// wrong line would corrupt: relocking the wrong scope (and leaving it unrelocked),
// or reading a stale rev.

const repoRoot = join(import.meta.dir, "..", "..");

describe("DEVENV_LOCK_SCOPES", () => {
	// The cwd IS the scope selector — devenv resolves devenv.lock relative to it —
	// so each scope's cwd must hold its lock. A mismatch relocks the sibling and
	// leaves this lock unrelocked.
	test("every scope's relock cwd is the directory holding that scope's lock", () => {
		for (const { lock, cwd } of Object.values(DEVENV_LOCK_SCOPES)) {
			const dir = lock.includes("/")
				? lock.slice(0, lock.lastIndexOf("/"))
				: ".";
			expect(cwd).toBe(dir);
		}
	});

	// Both governed locks must exist at these paths and pin the fork.
	test("both scope locks exist in the real tree and pin a 40-hex fork rev", () => {
		expect(DEVENV_LOCK_PATHS).toEqual([
			"devenv.lock",
			"agent-image/devenv.lock",
		]);
		for (const lock of DEVENV_LOCK_PATHS) {
			const text = readFileSync(join(repoRoot, lock), "utf8");
			expect(devenvForkLockedRev(text)).toMatch(/^[a-f0-9]{40}$/);
		}
	});

	// RD-1 unifies the source, not the lock files. A fork bump now moves both revs
	// in one PR, but the locks remain separate files.
	test("the two scopes are distinct lock files", () => {
		const locks = Object.values(DEVENV_LOCK_SCOPES).map((s) => s.lock);
		expect(new Set(locks).size).toBe(locks.length);
	});
});

describe("changedDevenvLocks", () => {
	const ROOT = "devenv.lock";
	const AGENT = "agent-image/devenv.lock";

	test("selects the root scope when only the root lock changed", () => {
		expect(changedDevenvLocks([ROOT])).toEqual(["root"]);
	});

	test("selects the agent-image scope when only that lock changed", () => {
		expect(changedDevenvLocks([AGENT])).toEqual(["agent-image"]);
	});
	// The self-gate: on every unrelated Renovate branch the task must be a cheap
	// no-op, not a spurious relock. Dropping this gate would make the task
	// re-lock (and network) on branches it has nothing to do with.
	test("returns [] when no devenv lock changed (the self-gate no-op)", () => {
		expect(changedDevenvLocks([])).toEqual([]);
		expect(changedDevenvLocks(["package.json", "bun.lock"])).toEqual([]);
	});

	// Paths are compared EXACTLY, as `git diff --name-only` emits them: a
	// same-named lock elsewhere in the tree is not either governed scope.
	test("does not mistake a same-named lock elsewhere for a governed scope", () => {
		expect(changedDevenvLocks(["guest-image/devenv.lock"])).toEqual([]);
		expect(changedDevenvLocks(["a/agent-image/devenv.lock"])).toEqual([]);
	});

	// Both matches are expected in the one fork group. Preserve the declared
	// scope order regardless of the order `git diff` reports changed paths.
	test("returns both scopes, root first, whatever the diff order", () => {
		expect(changedDevenvLocks([AGENT, ROOT])).toEqual(["root", "agent-image"]);
		expect(changedDevenvLocks([ROOT, AGENT])).toEqual(["root", "agent-image"]);
		expect(changedDevenvLocks(["bun.lock", AGENT, ROOT])).toEqual([
			"root",
			"agent-image",
		]);
	});
});

describe("devenvForkLockedRev", () => {
	// Real-manifest guard: the extraction must recover the fork rev from each
	// checked-in lock, so a devenv lock-format change (the node moves/renames)
	// fails HERE, loudly, instead of the task logging `undefined` and relocking
	// blind.
	test.each(["devenv.lock", "agent-image/devenv.lock"])(
		"recovers the fork rev from the real %s",
		(lock) => {
			const text = readFileSync(join(repoRoot, lock), "utf8");
			const rev = devenvForkLockedRev(text);
			expect(rev).toMatch(/^[a-f0-9]{40}$/);
			expect(rev).toBe(JSON.parse(text).nodes.devenv.locked.rev);
		},
	);

	test("throws on invalid JSON", () => {
		expect(() => devenvForkLockedRev("{not json")).toThrow(/not valid JSON/);
	});

	test("throws when the devenv node is absent", () => {
		const noNode = JSON.stringify({
			nodes: { nixpkgs: { locked: { rev: "x" } } },
		});
		expect(() => devenvForkLockedRev(noNode)).toThrow(/devenv fork rev/);
	});

	test("throws on a non-40-hex rev (shape drift)", () => {
		const shortRev = JSON.stringify({
			nodes: { devenv: { locked: { rev: "abc123" } } },
		});
		expect(() => devenvForkLockedRev(shortRev)).toThrow(/devenv fork rev/);
	});
});
