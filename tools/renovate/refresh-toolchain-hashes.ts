#!/usr/bin/env bun
// Renovate postUpgradeTask: refresh the vendored-toolchain Nix hashes after a
// per-language pin bump. Each `tools/toolchain/versions/<lang>.nix` file is the
// single source of truth for its tool's version, and pins a `sha256-` next to
// each version-interpolated source URL, one per platform leg:
//
//   tools/toolchain/versions/bun.nix   bun  (x86_64-linux, aarch64-linux, aarch64-darwin)
//   tools/toolchain/versions/node.nix  node (x86_64-linux, aarch64-linux, aarch64-darwin)
//   tools/toolchain/versions/moon.nix  moon (x86_64-linux, aarch64-linux, aarch64-darwin)
//
// A Renovate pin bump leaves those hashes stale → the CI image build fails until
// they're refreshed. Renovate runs this after applying the update so the same PR
// lands green. `fetchurl` hashes the downloaded file directly, so a `nix store
// prefetch-file` of the same URL yields the matching SRI hash. Go is NOT handled
// here: its binary and per-platform hashes come from the go-overlay input, which
// ships the hashes for each version — a go bump touches no pin file, so this
// script no-ops on it.
//
// Self-gating: for each pin file, act only when it differs from the base branch,
// so it's a cheap no-op on every non-toolchain Renovate branch (no prefetch),
// and a bun-only bump re-prefetches bun.nix alone, leaving node/moon untouched.
//
// Requires `nix` (with the nix-command experimental feature) on PATH — provided
// by the self-hosted Renovate runner's environment. Run via `bun` (already on
// the runner PATH; it runs `bun install --lockfile-only` as an allowed command).
//
// This is the bun/TypeScript port of the internal monorepo's refresh-toolchain-hashes
// (RIG-2432), scoped to compass's three vendored binary toolchains (bun, node,
// moon). Every observable behaviour — the per-file self-gate, the per-leg hash
// fail-loud on a missing marker / hash line, and idempotence — is preserved 1:1.

import { $ } from "bun";

// Repo-root-relative path constants. Exported so the test can parse them from
// the shipped script and derive its fixture paths — a rebase that moves these
// files and rewrites the constants keeps the test green with no edits.
export const BUN_NIX = "tools/toolchain/versions/bun.nix";
export const NODE_NIX = "tools/toolchain/versions/node.nix";
export const MOON_NIX = "tools/toolchain/versions/moon.nix";

// ── Pure rewrite (string in, string out) ─────────────────────────────────────
//
// Replace the `hash = "sha256-...";` value on the line that immediately follows
// the line containing the STATIC fragment `marker` (a substring present verbatim
// regardless of version — the `.nix` URLs interpolate the version, so the
// concrete version never appears literally). Throws if `newSri` is empty, if the
// marker isn't found, or if no `hash = "sha256-` line follows it, so a silent
// no-op can't ship a stale pin. Idempotent: a matching hash yields no net change.
export function rewriteHash(
	fileText: string,
	marker: string,
	newSri: string,
	file: string,
): string {
	if (!newSri) {
		throw new Error(
			`renovate-refresh: empty prefetched hash for '${marker}' in ${file}`,
		);
	}
	const lines = fileText.split("\n");
	const start = lines.findIndex((l) => l.includes(marker));
	if (start === -1) {
		throw new Error(
			`renovate-refresh: marker '${marker}' not found in ${file}`,
		);
	}
	for (let i = start + 1; i < lines.length; i++) {
		const line = lines[i];
		if (line?.includes('hash = "sha256-')) {
			lines[i] = line.replace(/sha256-[^"]*/, newSri);
			return lines.join("\n");
		}
	}
	throw new Error(
		`renovate-refresh: no 'hash =' line after '${marker}' in ${file}`,
	);
}

// ── Read a pin file's version attr (nix: `version = "1.3.14";`). ──
export function readVersion(nixText: string, file: string): string {
	const m = nixText.match(/^\s*version\s*=\s*"([^"]*)"/m);
	if (!m?.[1]) {
		throw new Error(
			`renovate-refresh: could not read version attr from ${file}`,
		);
	}
	return m[1];
}

// ── Prefetch a URL → SRI (sha256-...) hash, matching pkgs.fetchurl. ──
async function sriForUrl(url: string): Promise<string> {
	const out =
		await $`nix store prefetch-file --json --hash-type sha256 ${url}`.text();
	const m = out.match(/"hash":\s*"(sha256-[^"]*)"/);
	if (!m?.[1]) {
		throw new Error(
			`renovate-refresh: could not parse SRI from nix prefetch of ${url}`,
		);
	}
	return m[1];
}

async function updateHashFile(
	file: string,
	marker: string,
	newSri: string,
): Promise<void> {
	const text = await Bun.file(file).text();
	await Bun.write(file, rewriteHash(text, marker, newSri, file));
}

async function main(): Promise<void> {
	// Resolve the repo root from git, not a hardcoded "../" depth: Renovate
	// invokes this as a postUpgradeTask and the path constants above are
	// repo-root-relative, so a wrong cwd silently no-ops the gate. git is already
	// a hard dependency (the gate runs `git diff`), so this adds none and is
	// move-proof.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	const baseBranch = process.env.RENOVATE_BASE_BRANCH || "main";
	let baseRef = baseBranch;
	if (
		(await $`git rev-parse --verify -q origin/${baseBranch}`.nothrow().quiet())
			.exitCode === 0
	) {
		baseRef = `origin/${baseBranch}`;
	}

	// Per-tool pin file + its three platform legs. The `marker` is the STATIC
	// tail of each fetchurl URL (no version), so it matches the
	// version-interpolated `.nix` source line verbatim; `url` rebuilds the full
	// URL at the pinned version to prefetch. Each pin file carries all three legs
	// (x86_64-linux, aarch64-linux, aarch64-darwin — the CI image + dev shell are
	// multi-arch), so a refresh rewrites all three in its own file.
	const tools: { file: string; legs: { marker: string; url: string }[] }[] = [];
	const bunBase = "https://github.com/oven-sh/bun/releases/download";
	const nodeBase = "https://nodejs.org/dist";
	const moonBase = "https://github.com/moonrepo/moon/releases/download";

	// ── Per-file gate: act only on the pin files this branch changed vs base. ──
	// A go-overlay input bump touches no pin file, so the loop finds nothing to do
	// and the script no-ops; a bun-only bump refreshes bun.nix alone.
	let acted = false;
	for (const [file, build] of [
		[
			BUN_NIX,
			(v: string) => [
				{
					marker: "/bun-linux-x64-baseline.zip",
					url: `${bunBase}/bun-v${v}/bun-linux-x64-baseline.zip`,
				},
				{
					marker: "/bun-linux-aarch64.zip",
					url: `${bunBase}/bun-v${v}/bun-linux-aarch64.zip`,
				},
				{
					marker: "/bun-darwin-aarch64.zip",
					url: `${bunBase}/bun-v${v}/bun-darwin-aarch64.zip`,
				},
			],
		],
		[
			NODE_NIX,
			(v: string) => [
				{
					marker: "-linux-x64.tar.xz",
					url: `${nodeBase}/v${v}/node-v${v}-linux-x64.tar.xz`,
				},
				{
					marker: "-linux-arm64.tar.xz",
					url: `${nodeBase}/v${v}/node-v${v}-linux-arm64.tar.xz`,
				},
				{
					marker: "-darwin-arm64.tar.gz",
					url: `${nodeBase}/v${v}/node-v${v}-darwin-arm64.tar.gz`,
				},
			],
		],
		[
			MOON_NIX,
			(v: string) => [
				{
					marker: "/moon_cli-x86_64-unknown-linux-musl.tar.xz",
					url: `${moonBase}/v${v}/moon_cli-x86_64-unknown-linux-musl.tar.xz`,
				},
				{
					marker: "/moon_cli-aarch64-unknown-linux-musl.tar.xz",
					url: `${moonBase}/v${v}/moon_cli-aarch64-unknown-linux-musl.tar.xz`,
				},
				{
					marker: "/moon_cli-aarch64-apple-darwin.tar.xz",
					url: `${moonBase}/v${v}/moon_cli-aarch64-apple-darwin.tar.xz`,
				},
			],
		],
	] as [string, (v: string) => { marker: string; url: string }[]][]) {
		const gate = await $`git diff --quiet ${baseRef} -- ${file}`
			.nothrow()
			.quiet();
		if (gate.exitCode === 0) continue;
		const version = readVersion(await Bun.file(file).text(), file);
		tools.push({ file, legs: build(version) });
		console.log(`renovate-refresh: ${file} -> ${version}; prefetching ...`);
		acted = true;
	}

	if (!acted) {
		console.log(
			`renovate-refresh: no versions/*.nix pin changed vs ${baseRef}; nothing to do.`,
		);
		return;
	}

	for (const tool of tools) {
		for (const leg of tool.legs) {
			await updateHashFile(tool.file, leg.marker, await sriForUrl(leg.url));
		}
	}

	console.log("renovate-refresh: toolchain hashes refreshed.");
}

// Run only when executed directly (not imported by the test file).
if (import.meta.main) {
	await main();
}
