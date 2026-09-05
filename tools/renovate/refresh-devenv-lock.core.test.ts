import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import {
	changedDevenvLock,
	DEVENV_LOCK_PATHS,
	DEVENV_LOCK_SCOPES,
	devenvForkLockedRev,
} from "./refresh-devenv-lock.core.ts";

// Unit tests for the pure decision core of refresh-devenv-lock.ts (RIG-2815):
// deciding WHICH of the two devenv locks a Renovate branch touched, and reading
// the RigelBuild/devenv fork rev out of a lock. No devenv / network / git —
// that shell-out lives in the entry point and runs for real on the PR's own
// branch. These assert the decisions a wrong line would silently corrupt:
// relocking the wrong scope (whose write the rule's one-lock fileFilters then
// discards), or reading a stale/wrong rev.

const repoRoot = join(import.meta.dir, "..", "..");

describe("DEVENV_LOCK_SCOPES", () => {
	// The cwd IS the scope selector — devenv resolves devenv.yaml/devenv.lock
	// relative to it — so each scope's cwd must be the directory its lock lives
	// in. A mismatch would relock the OTHER scope's lock while the rule's
	// fileFilters names this one, so Renovate would commit nothing and the rev
	// bump would ship unrelocked.
	test("every scope's relock cwd is the directory holding that scope's lock", () => {
		for (const { lock, cwd } of Object.values(DEVENV_LOCK_SCOPES)) {
			const dir = lock.includes("/")
				? lock.slice(0, lock.lastIndexOf("/"))
				: ".";
			expect(cwd).toBe(dir);
		}
	});

	// Ground truth: both locks the config's two managers govern must actually
	// exist at these paths, and each must be a devenv lock pinning the fork.
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

	// RD-1: unify the SOURCE, do NOT reconcile the locks. The two locks tracking
	// the fork independently means their revs may legitimately differ — this test
	// documents that they are read as separate scopes, never compared for
	// equality anywhere in the task.
	test("the two scopes are distinct files (independent cadences, not reconciled)", () => {
		const locks = Object.values(DEVENV_LOCK_SCOPES).map((s) => s.lock);
		expect(new Set(locks).size).toBe(locks.length);
	});
});

describe("changedDevenvLock", () => {
	const ROOT = "devenv.lock";
	const AGENT = "agent-image/devenv.lock";

	test("selects the root scope when only the root lock changed", () => {
		expect(changedDevenvLock([ROOT])).toBe("root");
	});

	test("selects the agent-image scope when only that lock changed", () => {
		expect(changedDevenvLock([AGENT])).toBe("agent-image");
	});

	// The self-gate: on every unrelated Renovate branch the task must be a cheap
	// no-op, not a spurious relock. Dropping this gate would make the task
	// re-lock (and network) on branches it has nothing to do with.
	test("returns null when no devenv lock changed (the self-gate no-op)", () => {
		expect(changedDevenvLock([])).toBeNull();
		expect(changedDevenvLock(["package.json", "bun.lock"])).toBeNull();
	});

	// Paths are compared EXACTLY, as `git diff --name-only` emits them: a
	// same-named lock elsewhere in the tree is not either governed scope, and
	// treating it as one would relock a file no rule's fileFilters admits.
	test("does not mistake a same-named lock elsewhere for a governed scope", () => {
		expect(changedDevenvLock(["forks/devenv/devenv.lock"])).toBeNull();
		expect(changedDevenvLock(["a/agent-image/devenv.lock"])).toBeNull();
	});

	// Fail loud on the shape the two distinct groupNames exist to prevent. Each
	// rule's fileFilters names ONE lock, so a two-lock branch would have Renovate
	// commit one relock and silently DISCARD the other — a PR that bumped a rev
	// without relocking it. Better to die in-branch than ship that.
	test("throws when BOTH locks changed (the groupName isolation broke)", () => {
		expect(() => changedDevenvLock([ROOT, AGENT])).toThrow(
			/2 devenv locks changed/,
		);
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
