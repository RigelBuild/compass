#!/usr/bin/env bun
// Lock-integrity gate: recompute each devenv lock's github narHash/lastModified
// from its rev and fail when they disagree, so a half-relock turns rollup red.
// Exit 0 = every checked node is consistent; 1 = mismatch or unverifiable.

import { spawnSync } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
	changedNodeNames,
	type IntegrityCheck,
	integrityReport,
	isTransientFetchError,
	type LockedGithubNode,
	lockedGithubNodes,
	type Prefetched,
	parsePrefetch,
	prefetchRef,
} from "./devenv-lock-integrity.core.ts";
import { DEVENV_LOCK_PATHS } from "./refresh-devenv-lock.core.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const RETRY_DELAY_MS = 60_000;

function git(args: string[]): { ok: boolean; stdout: string; stderr: string } {
	const r = spawnSync("git", args, { cwd: repoRoot, encoding: "utf8" });
	return { ok: r.status === 0, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
}

function baseLockText(mergeBase: string, path: string): string | null {
	if (!git(["cat-file", "-e", `${mergeBase}:${path}`]).ok) return null;
	const shown = git(["show", `${mergeBase}:${path}`]);
	if (!shown.ok)
		throw new Error(
			`git show ${mergeBase}:${path} failed: ${shown.stderr.trim()}`,
		);
	return shown.stdout;
}

function prefetchOnce(
	ref: string,
	env: NodeJS.ProcessEnv,
): Prefetched | { error: string } {
	const r = spawnSync(
		"nix",
		[
			"flake",
			"prefetch",
			"--json",
			"--extra-experimental-features",
			"nix-command flakes",
			ref,
		],
		{ env, encoding: "utf8" },
	);
	if (r.status !== 0) {
		const tail = (r.stderr || r.error?.message || `exit ${r.status}`)
			.trim()
			.split("\n")
			.slice(-3);
		return { error: tail.join(" | ") };
	}
	try {
		return parsePrefetch(r.stdout);
	} catch (e) {
		return { error: (e as Error).message };
	}
}

function prefetch(
	ref: string,
	env: NodeJS.ProcessEnv,
): Prefetched | { error: string } {
	const first = prefetchOnce(ref, env);
	if (!("error" in first) || !isTransientFetchError(first.error)) return first;
	console.log(
		`transient fetch error for ${ref}; retrying once in ${RETRY_DELAY_MS / 1000}s`,
	);
	// biome-ignore lint/plugin: a production backoff before the single retry, not a test wait.
	Bun.sleepSync(RETRY_DELAY_MS);
	return prefetchOnce(ref, env);
}

type Target = { lock: string; node: LockedGithubNode };

/** Every locked node, or on a PR only those changed since the merge-base. */
function selectTargets(mergeBase: string | null): Target[] {
	return DEVENV_LOCK_PATHS.flatMap((lock) => {
		const head = readFileSync(join(repoRoot, lock), "utf8");
		const nodes = lockedGithubNodes(head);
		if (mergeBase === null) return nodes.map((node) => ({ lock, node }));
		const wanted = changedNodeNames(baseLockText(mergeBase, lock), head);
		return nodes
			.filter((n) => wanted.has(n.node))
			.map((node) => ({ lock, node }));
	});
}

function verify(targets: readonly Target[]): boolean {
	// A fresh cache per run: a warm fetcher cache can map a rev to a stale hash.
	const cacheDir = mkdtempSync(join(tmpdir(), "lock-integrity-"));
	const env = {
		...process.env,
		NIX_CACHE_HOME: cacheDir,
		XDG_CACHE_HOME: cacheDir,
		// Clearing tokens sends fetches to public archive URLs, not the metered REST API.
		NIX_CONFIG: `${process.env.NIX_CONFIG ?? ""}\naccess-tokens =`,
	};
	try {
		const byRef = new Map<string, Prefetched | { error: string }>();
		const checks: IntegrityCheck[] = targets.map(({ lock, node }) => {
			const ref = prefetchRef(node);
			let observed = byRef.get(ref);
			if (observed === undefined) {
				observed = prefetch(ref, env);
				byRef.set(ref, observed);
			}
			return { lock, node, observed };
		});
		const result = integrityReport(checks);
		console.log(result.report);
		return result.ok;
	} finally {
		rmSync(cacheDir, { recursive: true, force: true });
	}
}

function main(): number {
	const baseRef = process.env.GITHUB_BASE_REF ?? "";
	const prMode =
		process.env.GITHUB_EVENT_NAME === "pull_request" && baseRef !== "";
	let mergeBase: string | null = null;
	if (prMode) {
		const mb = git(["merge-base", `origin/${baseRef}`, "HEAD"]);
		if (!mb.ok) {
			console.error(
				`lock-integrity: cannot resolve merge-base with origin/${baseRef}: ${mb.stderr.trim()}`,
			);
			return 1;
		}
		mergeBase = mb.stdout.trim();
	}
	const targets = selectTargets(mergeBase);
	const mode = prMode
		? `PR mode (base origin/${baseRef} @ ${mergeBase})`
		: "full sweep";
	console.log(`lock-integrity: ${mode}, ${targets.length} node(s) to check`);
	if (targets.length === 0) return 0;
	return verify(targets) ? 0 : 1;
}

process.exit(main());
