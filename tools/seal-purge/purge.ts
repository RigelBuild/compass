// Re-runnable codemod: purge the internal `sealedsecurity` org token from the
// public RigelBuild/compass repo (RIG-1893). Context-aware because the correct
// replacement depends on the surface:
//
//   - GitHub org slug (compass)  → `RigelBuild/compass` (the GitHub org is
//     RigelBuild; go-get meta always returns capital; matches merged #458).
//   - ghcr namespace             → `rigelbuild` (lowercase; OCI registry names
//     MUST be lowercase; live code ships `ghcr.io/rigelbuild/...`, RIG-1967).
//   - Linear workspace slug      → `rigelbuild` (lowercase; linear.app/rigelbuild).
//   - email / web host           → `rigel.build`.
//
// This codemod handles ONLY the mechanical, unambiguous classes above. Two
// classes are deliberately NOT here because they need per-file judgment, not a
// token swap, and are done as reviewed manual edits:
//   - `sealedsecurity/sealed` (the internal monorepo): Matt ruled SCRUB — a
//     public repo must not reference an internal monorepo at all (not even
//     repointed to the internal monorepo). That means removing the link / genericising the
//     fixture, which is semantic.
//   - the dead `seal` Rust component (`seal-daemon`, `oss/seal`, `seal-runtime`)
//     in historical design records: prose-bound, too few for a safe regex.
//
// SCOPE CARVE-OUTS (never rewritten — verified 2026-08-21):
//   - `forks/<name>/**`     vendored upstream subtrees; byte-identity is the
//                           verification basis. Editing poisons Copybara.
//                           (First-party `forks/README.md` is NOT carved out —
//                           it is linted compass-authored prose, scrubbed
//                           directly. Fork-spoke slugs there are RigelBuild/*.)
//   - `LICENSE*`            legal entity "Sealed Security, Inc." — legal ruling.
//   - `tools/docs-migrate`  the `linear.app/sealedsecurity/...` strings are
//                           STRIP-INPUT fixtures for the sanitizer that removes
//                           such links; the old name IS the contract, not a leak.
//   - `Warden`              a CURRENT core Compass product concept, left intact.
//   - English "seal(ed)"    crypto verb (sealed payload/blob/sum) — domain vocab.
//
// Usage: bun run tools/seal-purge/purge.ts [--check]
//   --check : report files that would still change, exit 1 if any (CI drift guard).

import { $ } from "bun";

/** A single ordered literal-regex rewrite, applied globally per file. */
export interface Rule {
	readonly re: RegExp;
	readonly to: string;
	readonly why: string;
}

/**
 * Ordered rewrites. Order matters: ghcr / Linear / host rules run BEFORE the
 * generic `sealedsecurity/compass` rule so a ghcr or linear ref is claimed by
 * its specific (lowercase) rule first, never by the capital-slug rule.
 */
export const RULES: readonly Rule[] = [
	{
		re: /ghcr\.io\/sealedsecurity\//g,
		to: "ghcr.io/rigelbuild/",
		why: "ghcr namespace is lowercase (OCI registry rule, RIG-1967)",
	},
	{
		re: /linear\.app\/sealedsecurity\//g,
		to: "linear.app/rigelbuild/",
		why: "Linear workspace slug is lowercase rigelbuild",
	},
	{
		re: /sealedsecurity\.com/g,
		to: "rigel.build",
		why: "web host / email domain → rigel.build (preserves any subdomain / local-part)",
	},
	{
		re: /github\.com\/sealedsecurity\/sealed\//g,
		to: "github.com/RigelBuild/compass/",
		why: "internal-monorepo demo/test URLs → the public compass repo (a public repo must not link an internal monorepo, RIG-1893)",
	},
	{
		re: /"sealedsecurity\/sealed"/g,
		to: '"RigelBuild/compass"',
		why: "quoted internal-monorepo repo-name literals (UI demo/test fixtures) → public compass repo. Bare-prose/colon-path forms are handled manually (semantic).",
	},
	{
		re: /sealedsecurity\/compass/g,
		to: "RigelBuild/compass",
		why: "public compass repo slug → capital RigelBuild (github + bare). Runs after ghcr so compass-agent is already lowercased.",
	},
];

/** Files that carry the token but MUST NOT be rewritten by this codemod. */
export function isCarveOut(path: string): boolean {
	return (
		(path.startsWith("forks/") && path !== "forks/README.md") ||
		path.startsWith("LICENSE") ||
		path.startsWith("tools/docs-migrate/") ||
		path.startsWith("tools/seal-purge/") // this codemod's own doc-comment
	);
}

/** Apply all rules to one file's content; returns the rewritten text. */
export function transform(content: string): string {
	let out = content;
	for (const rule of RULES) out = out.replace(rule.re, rule.to);
	return out;
}

async function trackedFilesWithToken(): Promise<string[]> {
	const raw = await $`git grep -lI sealedsecurity`.nothrow().text();
	return raw
		.split("\n")
		.map((l) => l.trim())
		.filter((l) => l.length > 0 && !isCarveOut(l));
}

async function main() {
	const check = process.argv.includes("--check");
	const files = await trackedFilesWithToken();
	const changed: string[] = [];

	for (const path of files) {
		const before = await Bun.file(path).text();
		const after = transform(before);
		if (after !== before) {
			changed.push(path);
			if (!check) await Bun.write(path, after);
		}
	}

	if (check) {
		if (changed.length > 0) {
			console.error(
				`seal-purge drift — ${changed.length} file(s) carry a mechanically-rewritable token:`,
			);
			for (const c of changed) console.error(`  ${c}`);
			process.exit(1);
		}
		console.log("seal-purge --check: clean");
		return;
	}

	console.log(`seal-purge: rewrote ${changed.length} file(s):`);
	for (const c of changed) console.log(`  ${c}`);
}

if (import.meta.main) await main();
