// Deploy the built docsite (dist/) to Cloudflare Pages, then on a PR upsert a
// preview-URL comment. Invoked by the compass-eng-docs:deploy /
// compass-eng-docs:deploy-preview moon tasks (CD, runInCI:false) — gated to
// push:main / pull_request by the standalone .github/workflows/eng-docs-deploy.yml
// GitHub Actions workflow (SEA-1765). The engineering docsite lives on
// Cloudflare Pages under the compass-eng-docs project.
//
// Adapted from sealed apps/docs/scripts/deploy.ts — they share the same
// Cloudflare Pages deploy + PR-preview-comment shape and the three constants
// below (PROJECT_NAME, COMMENT_MARKER, SITE_LABEL). This docsite build gathers
// the compass repo's markdown, so its preview comment also deep-links the .md
// pages a PR changed (changedDocPages + commentBody's "Changed pages" section).
//
// Construction vs execution: the pure functions below build typed arg arrays +
// parse output; a thin `$` runner at the bottom executes. The pure parts are
// unit-tested (deploy.test.ts) with no live wrangler/gh.
//
// Env (GitHub Actions injects the GITHUB_* set; PR_* are passed explicitly from
// github.event.pull_request; the CLOUDFLARE_*/GH_TOKEN are declared secrets):
//   CLOUDFLARE_API_TOKEN   — wrangler auth (secret).
//   CLOUDFLARE_ACCOUNT_ID  — Cloudflare account (secret).
//   GITHUB_EVENT_NAME      — "push" (production) or "pull_request" (preview).
//   GITHUB_HEAD_REF        — PR source branch (the preview branch name; PR only).
//   GITHUB_SHA             — the checked-out commit; the branch head on push, but
//                            the ephemeral merge commit on a PR (NOT recorded).
//   PR_HEAD_SHA            — github.event.pull_request.head.sha, the PR branch
//                            head — the commit recorded in the preview comment.
//   GITHUB_REPOSITORY      — owner/name, for the gh comment + changed-files lookup.
//   PR_NUMBER              — PR number, for the gh comment + changed-files lookup.
//   GH_TOKEN               — gh auth for the preview comment + the PR changed-files
//                            lookup (PR only).
//
// Cloudflare Pages keys deployments by --branch: "main" is the production
// alias; any other branch is a preview deployment with its own stable URL.

import { dirname, join } from "node:path";
import { $ } from "bun";
import { classify, isExcluded, parseExclusions, routeSlug } from "./gather.ts";

const PROJECT_NAME = "compass-eng-docs";
const COMMENT_MARKER = "<!-- compass-eng-docs-preview -->";
const SITE_LABEL = "Compass engineering docs";

/** The environment the deploy reads — the subset of `process.env` we depend on. */
export interface DeployEnv {
	GITHUB_EVENT_NAME?: string;
	GITHUB_HEAD_REF?: string;
	GITHUB_SHA?: string;
	PR_HEAD_SHA?: string;
	GITHUB_REPOSITORY?: string;
	PR_NUMBER?: string;
	GH_TOKEN?: string;
	CLOUDFLARE_API_TOKEN?: string;
	CLOUDFLARE_ACCOUNT_ID?: string;
}

/** True when this run is a pull_request event (→ preview deploy + comment). */
export function isPullRequest(env: DeployEnv): boolean {
	return env.GITHUB_EVENT_NAME === "pull_request";
}

/**
 * The Cloudflare Pages `--branch` to deploy under. Production (push:main) uses
 * the "main" alias; a PR uses its SOURCE branch. Never fall back to a target
 * branch on a PR — that would risk deploying a preview over production — so an
 * unset source branch on a PR throws.
 */
export function deployBranch(env: DeployEnv): string {
	if (isPullRequest(env)) {
		const source = env.GITHUB_HEAD_REF?.trim();
		if (!source) {
			throw new Error(
				"GITHUB_HEAD_REF is not set; refusing to deploy a preview (would risk overwriting production)",
			);
		}
		return source;
	}
	return "main";
}

/**
 * The commit SHA recorded in the preview comment. On a PR this is the PR branch
 * HEAD (PR_HEAD_SHA = github.event.pull_request.head.sha), NEVER GITHUB_SHA —
 * on a pull_request event GITHUB_SHA is the ephemeral refs/pull/N/merge commit,
 * not the branch head, so recording it would make the changed-page deep links
 * point at a commit the reviewer never pushed. On a push GITHUB_SHA is the
 * branch head and is correct.
 */
export function recordedCommitSha(env: DeployEnv): string | undefined {
	return isPullRequest(env) ? env.PR_HEAD_SHA : env.GITHUB_SHA;
}

/** The `wrangler pages deploy` argv for a given branch. */
export function wranglerArgs(branch: string): string[] {
	return [
		"wrangler",
		"pages",
		"deploy",
		"dist",
		`--project-name=${PROJECT_NAME}`,
		`--branch=${branch}`,
		// The CI checkout has generated (untracked) dist/ contents; without this
		// wrangler warns and skips commit metadata.
		"--commit-dirty=true",
	];
}

/**
 * Extract the preview deployment URL from wrangler's output. It prints a line
 * like "Take a peek over at https://<hash>.compass-eng-docs.pages.dev"; return
 * the last pages.dev URL, or null when none is present.
 */
export function parsePreviewUrl(wranglerOutput: string): string | null {
	const matches = wranglerOutput.match(/https:\/\/[a-z0-9.-]+\.pages\.dev/g);
	return matches && matches.length > 0 ? (matches.at(-1) ?? null) : null;
}

/**
 * The `gh api` argv that lists a PR's changed files as `filename<TAB>status`
 * lines. `per_page=100` rides the URL query string, NOT a `-F`/`-f` flag: any
 * parameter flag makes `gh api` switch the request to POST, and
 * `pulls/{n}/files` is GET-only — a `-F per_page` would POST, 404, and (behind
 * the caller's fail-soft) silently drop the whole changed-pages section. Kept
 * pure + exported so the GET-safe shape is regression-locked in tests, the same
 * construction/execution split as `wranglerArgs`.
 */
export function changedFilesArgs(repo: string, pr: string): string[] {
	return [
		"api",
		"--paginate",
		`repos/${repo}/pulls/${pr}/files?per_page=100`,
		"--jq",
		".[] | [.filename, .status] | @tsv",
	];
}

/** A changed file as reported by the GitHub PR files API. */
export interface ChangedFile {
	/** Repo-relative path, e.g. "docs/designs/platform/foo.md". */
	filename: string;
	/** "added" | "modified" | "removed" | "renamed" | … — deleted files are dropped. */
	status: string;
}

/** A changed docsite page: the changed source path and the site route it renders at. */
export interface ChangedPage {
	/** The changed source path as it appears in the PR, e.g. "docs/designs/platform/foo.md". */
	sourcePath: string;
	/**
	 * The site route it renders at (no host), e.g. "/designs/platform/foo". This
	 * is a RAW route — NOT URL-safe. Any consumer building a link target from it
	 * MUST pass it through `encodeRoutePath` first (as `commentBody` does); a
	 * `sourcePath` is an attacker-controlled git filename, so an un-encoded route
	 * can carry a `)`/space that breaks or spoofs the Markdown link.
	 */
	route: string;
}

/**
 * The docsite pages a PR's changed files map to. Keeps only files that actually
 * render on the docsite — markdown, not deleted, and not excluded from the
 * gather — then maps each through the gather's own classify→routeSlug chain (the
 * same mapping gather.ts uses to place a page), so every route is guaranteed to
 * resolve on the deployed preview. Pure: the gh lookup + config read live in
 * fetchChangedDocPages (the thin `$` runner below).
 *
 * @param changed the PR's changed files (filename + status)
 * @param markdownlintConfig the raw .markdownlint-cli2.jsonc (source of the
 *   exclusion set, via parseExclusions) — the single source of truth for what
 *   the gather drops.
 */
export function changedDocPages(
	changed: readonly ChangedFile[],
	markdownlintConfig: string,
): ChangedPage[] {
	const exclusions = parseExclusions(markdownlintConfig);
	const pages: ChangedPage[] = [];
	for (const { filename, status } of changed) {
		// Deleted files no longer render — linking them would 404.
		if (status === "removed") continue;
		// Only files the gather actually renders become pages. Match its
		// membership test exactly (gather.ts main()): a case-SENSITIVE `.md`
		// (Bun's `**/*.md` glob does not match `.MD`, and routeSlug strips `.md`
		// case-sensitively — a `/i` gate here would link a page the gather never
		// produced), minus node_modules/dist and the VCS/tooling dotdirs the
		// gather skips, minus the shared exclusion set. Keeping these in lockstep
		// is what makes the docstring's "route guaranteed to resolve" true.
		if (!/\.md$/.test(filename)) continue;
		if (filename.includes("node_modules/") || filename.includes("/dist/")) {
			continue;
		}
		if (
			/(^|\/)\.(git|astro|direnv|moon|vscode|idea|pagefind)\//.test(filename)
		) {
			continue;
		}
		// A file the gather drops is not on the docsite — same exclusion set.
		if (isExcluded(filename, exclusions)) continue;
		// Same classify→routeSlug chain the gather uses to place the page, so
		// the route is guaranteed to resolve on the deployed preview.
		pages.push({
			sourcePath: filename,
			route: routeSlug(classify(filename).destRel),
		});
	}
	return pages;
}

/**
 * Parse the `gh api …/files --jq '.[] | [.filename, .status] | @tsv'` output
 * into changed files. Each non-empty line is `filename<TAB>status`; jq's `@tsv`
 * escapes any literal tab/newline inside a field, so splitting on a real tab is
 * unambiguous. Pure (text in, structs out) so it unit-tests with no live gh.
 */
export function parseChangedFiles(tsv: string): ChangedFile[] {
	const files: ChangedFile[] = [];
	for (const raw of tsv.split("\n")) {
		const line = raw.endsWith("\r") ? raw.slice(0, -1) : raw;
		if (line.length === 0) continue;
		const tab = line.indexOf("\t");
		if (tab === -1) continue;
		files.push({
			filename: line.slice(0, tab),
			status: line.slice(tab + 1),
		});
	}
	return files;
}

/** Preview metadata surfaced in the PR comment. */
export interface PreviewMeta {
	previewUrl: string;
	branch: string;
	commitSha?: string;
	/**
	 * The docsite pages this PR added/changed (from changedDocPages). When
	 * present and non-empty, commentBody appends a "Changed pages" section of
	 * direct deep-links into the preview; empty/omitted → no section.
	 */
	changedPages?: readonly ChangedPage[];
}

/**
 * Escape the characters that let an attacker-controlled label break out of the
 * Markdown link text or inject markup into the bot's comment: the link-breaking
 * `\`, `[`, `]` and the code-span-opening `` ` `` (backslash-escaped) and the
 * HTML-significant `<`, `>`, `&` (entity-encoded). The label is a git filename
 * from the PR's changed-files list — never checked out on CI, so `<`/`>`/`&`/`"`
 * are all legal in it — and
 * GitHub renders a raw `<img>`/`<details>`/`<a>` in comment Markdown (its
 * sanitizer blocks JS/XSS and camo-proxies images, but the tags themselves are
 * permitted), so an unescaped `<` is real content-spoofing into a trusted
 * comment, not just a broken link. Encoding `&` too (not only `<`/`>`) keeps the
 * escape faithful: a name literally containing `&lt;` becomes `&amp;lt;` and
 * renders as literal text, so no pre-existing entity-like sequence is silently
 * interpreted as the `<` it spells.
 */
export function escapeLinkText(text: string): string {
	return text.replace(/[\\[\]<>&`]/g, (c) => {
		switch (c) {
			case "<":
				return "&lt;";
			case ">":
				return "&gt;";
			case "&":
				return "&amp;";
			default:
				return `\\${c}`;
		}
	});
}

/**
 * Percent-encode a site route for use inside a Markdown link target, per path
 * segment (so the `/` separators survive). `encodeURIComponent` handles spaces
 * (`%20`) and most punctuation but deliberately leaves `(` and `)` un-encoded,
 * and a `)` in the target closes the Markdown `(...)` early — so a route from a
 * filename containing a paren would truncate the link and 404. Encode those two
 * explicitly after `encodeURIComponent` so every route the deploy links is a
 * well-formed target.
 */
export function encodeRoutePath(route: string): string {
	return route
		.split("/")
		.map((seg) =>
			encodeURIComponent(seg).replace(/[()]/g, (c) =>
				c === "(" ? "%28" : "%29",
			),
		)
		.join("/");
}

/**
 * The marker-prefixed PR comment body: names the site, links the preview, and
 * records the branch + commit it reflects so a reviewer can see what's deployed.
 */
export function commentBody(meta: PreviewMeta): string {
	const source = meta.commitSha
		? `\`${meta.branch}\` at \`${meta.commitSha.slice(0, 7)}\``
		: `\`${meta.branch}\``;
	const base = `${COMMENT_MARKER}\n**${SITE_LABEL} preview:** ${meta.previewUrl}\n\nDeployed from ${source}.`;
	// Additive "Changed pages" section: one deep-link per changed docsite page,
	// so a reviewer clicks straight to the rendered page. Omitted/empty → the
	// base body is byte-identical to before this feature.
	const pages = meta.changedPages ?? [];
	if (pages.length === 0) return base;
	// sourcePath is an attacker-controllable git filename (it arrives from the
	// PR's changed-files list). Backslash-escape the Markdown link-breaking
	// chars in the label, and encode the route so a space/`)` can't terminate
	// the URL — a crafted `.md` name can't inject a link or spoof the target.
	const links = pages
		.map(
			(p) =>
				`- [${escapeLinkText(p.sourcePath)}](${meta.previewUrl}${encodeRoutePath(p.route)})`,
		)
		.join("\n");
	return `${base}\n\n**Changed pages:**\n${links}`;
}

/** The gh token for the preview comment. */
export function ghToken(env: DeployEnv): string | undefined {
	return env.GH_TOKEN?.trim() || undefined;
}

/** The inputs needed to post the preview comment, once resolved from the env. */
export interface PreviewCommentInputs {
	repo: string;
	pr: string;
	token: string;
}

/**
 * Resolve what's needed to post the preview comment, or the reason it can't be.
 * On a pull request the comment is a required outcome, so missing inputs are an
 * error (a red step) rather than a silent skip — a preview that never surfaces
 * its URL is a broken deploy from the PR author's view.
 */
export function resolvePreviewComment(
	env: DeployEnv,
): { ok: true; inputs: PreviewCommentInputs } | { ok: false; reason: string } {
	const token = ghToken(env);
	const pr = env.PR_NUMBER?.trim();
	const repo = env.GITHUB_REPOSITORY?.trim();
	const missing: string[] = [];
	if (!token) missing.push("GitHub token (GH_TOKEN)");
	if (!pr) missing.push("PR number (PR_NUMBER)");
	if (!repo) missing.push("repo (GITHUB_REPOSITORY)");
	if (!token || !pr || !repo) {
		return {
			ok: false,
			reason: `cannot post preview comment: missing ${missing.join(", ")}`,
		};
	}
	return { ok: true, inputs: { repo, pr, token } };
}

// ── Execution (thin `$` runner) ─────────────────────────────────────────────

async function main(): Promise<void> {
	const env = process.env as DeployEnv;
	if (!env.CLOUDFLARE_API_TOKEN || !env.CLOUDFLARE_ACCOUNT_ID) {
		throw new Error(
			"CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID are required",
		);
	}
	const branch = deployBranch(env);
	console.log(`Deploying dist/ to Cloudflare Pages (branch: ${branch})...`);

	// wrangler reads CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID from the env.
	// Capture combined output so we can parse the preview URL for the comment.
	const args = wranglerArgs(branch);
	const result = await $`bunx ${args}`.nothrow();
	const out = result.stdout.toString() + result.stderr.toString();
	console.log(out);
	if (result.exitCode !== 0) {
		throw new Error(`wrangler pages deploy failed (exit ${result.exitCode})`);
	}

	// Non-PR (production) deploy: nothing more to do.
	if (!isPullRequest(env)) return;

	const previewUrl = parsePreviewUrl(out);
	if (!previewUrl) {
		throw new Error(
			"deploy succeeded but no preview URL was found in wrangler output",
		);
	}
	console.log(`Preview URL: ${previewUrl}`);

	// On a PR the preview comment is required: any failure to post it fails the
	// step, so a broken comment can't hide behind a green check.
	const comment = resolvePreviewComment(env);
	if (!comment.ok) throw new Error(comment.reason);
	const changedPages = await fetchChangedDocPages(
		comment.inputs.repo,
		comment.inputs.pr,
		comment.inputs.token,
	);
	await upsertPreviewComment({
		repo: comment.inputs.repo,
		pr: comment.inputs.pr,
		token: comment.inputs.token,
		body: commentBody({
			previewUrl,
			branch,
			// The PR branch head, NEVER GITHUB_SHA (the merge commit) on a PR.
			commitSha: recordedCommitSha(env),
			changedPages,
		}),
	});
}

/**
 * The docsite pages a PR changed, resolved live: list the PR's changed files
 * via the GitHub API (not a local `git diff` — the CI agent clone is shallow,
 * so local history is unreliable), then map them through the pure
 * changedDocPages against the repo's canonical markdownlint exclusion set.
 * Returns [] (no section) on any lookup failure — the deep-links are a
 * convenience, never worth failing an otherwise-good deploy over.
 */
async function fetchChangedDocPages(
	repo: string,
	pr: string,
	token: string,
): Promise<ChangedPage[]> {
	const ghEnv = { ...process.env, GH_TOKEN: token };
	// `@tsv` emits one `filename<TAB>status` line per file, escaping any tab/
	// newline inside a field — parsed by the pure parseChangedFiles (no JSON
	// shape to assert). Args built by the pure changedFilesArgs (GET-safe
	// per_page in the query string, not a POST-flipping -F flag).
	const files = await $`gh ${changedFilesArgs(repo, pr)}`
		.env(ghEnv)
		.nothrow()
		.quiet();
	if (files.exitCode !== 0) {
		console.log(
			`could not list changed files (exit ${files.exitCode}): ${files.stderr.toString().trim()}; omitting the changed-pages section`,
		);
		return [];
	}
	// The exclusion set's single source of truth is the repo-root markdownlint
	// config (the same file gather.ts reads). This file is apps/eng-docs/scripts/
	// deploy.ts, so the repo root is three dirname hops up from its path
	// (scripts → eng-docs → apps → root), matching gather.ts's own walk.
	// Guarded like the gh call above: a missing/renamed config, a JSONC parse
	// failure, or a classify throw must also omit the section, never fail the
	// deploy — the docstring's "any lookup failure" contract covers this half too.
	try {
		const repoRoot = dirname(
			dirname(dirname(dirname(Bun.fileURLToPath(import.meta.url)))),
		);
		const markdownlintConfig = await Bun.file(
			join(repoRoot, ".markdownlint-cli2.jsonc"),
		).text();
		return changedDocPages(
			parseChangedFiles(files.stdout.toString()),
			markdownlintConfig,
		);
	} catch (err) {
		console.log(
			`could not resolve changed docsite pages (${err}); omitting the changed-pages section`,
		);
		return [];
	}
}

/** Upsert the single marker comment on the PR via the gh CLI. */
async function upsertPreviewComment(opts: {
	repo: string;
	pr: string;
	token: string;
	body: string;
}): Promise<void> {
	const ghEnv = { ...process.env, GH_TOKEN: opts.token };
	// Find an existing marker comment to edit; else create one. A failed
	// lookup (rate-limit, transient network, auth) must not be read as "no
	// comment found" — that would create a duplicate every push, breaking the
	// single-marker contract. Fail loud on a non-zero exit; only an empty
	// stdout on exit 0 means no existing comment.
	const existing =
		await $`gh api --paginate ${`repos/${opts.repo}/issues/${opts.pr}/comments`} --jq ${`.[] | select(.body | startswith("${COMMENT_MARKER}")) | .id`}`
			.env(ghEnv)
			.nothrow()
			.quiet();
	if (existing.exitCode !== 0) {
		throw new Error(
			`failed to look up existing PR comments (exit ${existing.exitCode})`,
		);
	}
	const id = existing.stdout.toString().trim().split("\n")[0]?.trim();
	if (id) {
		await $`gh api --method PATCH ${`repos/${opts.repo}/issues/comments/${id}`} -f ${`body=${opts.body}`}`
			.env(ghEnv)
			.quiet();
		console.log("updated existing preview comment");
	} else {
		await $`gh pr comment ${opts.pr} --repo ${opts.repo} --body ${opts.body}`
			.env(ghEnv)
			.quiet();
		console.log("created preview comment");
	}
}

if (import.meta.main) {
	await main();
}
