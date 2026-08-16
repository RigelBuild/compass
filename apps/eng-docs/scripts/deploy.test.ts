import { expect, test } from "bun:test";
import {
	changedDocPages,
	changedFilesArgs,
	commentBody,
	type DeployEnv,
	deployBranch,
	encodeRoutePath,
	escapeLinkText,
	ghToken,
	isPullRequest,
	parseChangedFiles,
	parsePreviewUrl,
	recordedCommitSha,
	resolvePreviewComment,
	wranglerArgs,
} from "./deploy.ts";

// ── isPullRequest ────────────────────────────────────────────────────────────

test("isPullRequest is true only for a pull_request event", () => {
	expect(isPullRequest({ GITHUB_EVENT_NAME: "pull_request" })).toBe(true);
});

test("isPullRequest is false for a push event", () => {
	expect(isPullRequest({ GITHUB_EVENT_NAME: "push" })).toBe(false);
});

test("isPullRequest is false when the event is unset", () => {
	expect(isPullRequest({})).toBe(false);
});

// ── deployBranch ─────────────────────────────────────────────────────────────

test("deployBranch is the production 'main' alias on a push", () => {
	expect(deployBranch({ GITHUB_EVENT_NAME: "push" })).toBe("main");
});

test("deployBranch uses the PR source branch on a pull_request", () => {
	expect(
		deployBranch({
			GITHUB_EVENT_NAME: "pull_request",
			GITHUB_HEAD_REF: "feat/x",
		}),
	).toBe("feat/x");
});

test("deployBranch trims surrounding whitespace off the PR source branch", () => {
	expect(
		deployBranch({
			GITHUB_EVENT_NAME: "pull_request",
			GITHUB_HEAD_REF: "  feat/x  ",
		}),
	).toBe("feat/x");
});

test("deployBranch throws on a PR with an unset source branch (never overwrite production)", () => {
	expect(() => deployBranch({ GITHUB_EVENT_NAME: "pull_request" })).toThrow(
		/GITHUB_HEAD_REF is not set/,
	);
});

test("deployBranch throws on a PR with an empty source branch", () => {
	expect(() =>
		deployBranch({
			GITHUB_EVENT_NAME: "pull_request",
			GITHUB_HEAD_REF: "",
		}),
	).toThrow(/refusing to deploy a preview/);
});

test("deployBranch throws on a PR with a whitespace-only source branch", () => {
	expect(() =>
		deployBranch({
			GITHUB_EVENT_NAME: "pull_request",
			GITHUB_HEAD_REF: "   ",
		}),
	).toThrow();
});

// ── recordedCommitSha ────────────────────────────────────────────────────────

test("recordedCommitSha is GITHUB_SHA on a push (the branch head)", () => {
	expect(
		recordedCommitSha({ GITHUB_EVENT_NAME: "push", GITHUB_SHA: "pushsha" }),
	).toBe("pushsha");
});

test("recordedCommitSha is PR_HEAD_SHA on a PR (the branch head, NOT the merge commit)", () => {
	// The load-bearing invariant: on a pull_request event GITHUB_SHA is the
	// ephemeral refs/pull/N/merge commit, so the recorded SHA must be PR_HEAD_SHA
	// (github.event.pull_request.head.sha) — never GITHUB_SHA.
	expect(
		recordedCommitSha({
			GITHUB_EVENT_NAME: "pull_request",
			PR_HEAD_SHA: "prheadsha",
			GITHUB_SHA: "mergesha",
		}),
	).toBe("prheadsha");
});

test("recordedCommitSha never returns GITHUB_SHA (the merge commit) on a PR", () => {
	expect(
		recordedCommitSha({
			GITHUB_EVENT_NAME: "pull_request",
			PR_HEAD_SHA: "prheadsha",
			GITHUB_SHA: "mergesha",
		}),
	).not.toBe("mergesha");
});

test("recordedCommitSha is undefined on a PR with no PR_HEAD_SHA", () => {
	expect(
		recordedCommitSha({
			GITHUB_EVENT_NAME: "pull_request",
			GITHUB_SHA: "mergesha",
		}),
	).toBeUndefined();
});

// ── wranglerArgs ─────────────────────────────────────────────────────────────

test("wranglerArgs builds the exact production argv for 'main'", () => {
	expect(wranglerArgs("main")).toEqual([
		"wrangler",
		"pages",
		"deploy",
		"dist",
		"--project-name=compass-eng-docs",
		"--branch=main",
		"--commit-dirty=true",
	]);
});

test("wranglerArgs builds the exact preview argv for a feature branch", () => {
	expect(wranglerArgs("feat/x")).toEqual([
		"wrangler",
		"pages",
		"deploy",
		"dist",
		"--project-name=compass-eng-docs",
		"--branch=feat/x",
		"--commit-dirty=true",
	]);
});

// ── changedFilesArgs ─────────────────────────────────────────────────────────

test("changedFilesArgs builds the exact GET argv with per_page in the query string", () => {
	// The whole shape is regression-locked: per_page rides the URL query string,
	// NOT a -F flag. `pulls/{n}/files` is GET-only; `gh api` switches to POST the
	// moment any parameter flag (-F/-f/--field/--raw-field) is present, which
	// 404s and (behind fetchChangedDocPages's fail-soft) silently drops the whole
	// changed-pages section.
	expect(changedFilesArgs("RigelBuild/compass", "634")).toEqual([
		"api",
		"--paginate",
		"repos/RigelBuild/compass/pulls/634/files?per_page=100",
		"--jq",
		".[] | [.filename, .status] | @tsv",
	]);
});

test("changedFilesArgs passes NO parameter flag (a -F/-f would flip the GET-only endpoint to POST)", () => {
	// The defect guard, invariant-level: not one argv element may be a field flag,
	// so a future edit re-introducing `-F per_page` reddens here regardless of
	// exact-argv churn elsewhere.
	const args = changedFilesArgs("owner/repo", "1");
	for (const flag of ["-F", "-f", "--field", "--raw-field"]) {
		expect(args).not.toContain(flag);
	}
	// per_page must instead be carried on the endpoint (the query string).
	expect(args.some((a) => a.includes("?per_page=100"))).toBe(true);
});

// ── parsePreviewUrl ──────────────────────────────────────────────────────────

test("parsePreviewUrl extracts the pages.dev URL from a wrangler line", () => {
	const output =
		"Deploying to Cloudflare Pages...\n" +
		"Take a peek over at https://abc123.compass-eng-docs.pages.dev\n";
	expect(parsePreviewUrl(output)).toBe(
		"https://abc123.compass-eng-docs.pages.dev",
	);
});

test("parsePreviewUrl returns the LAST pages.dev URL when several are present", () => {
	const output = [
		"https://first.compass-eng-docs.pages.dev",
		"noise in the middle",
		"https://second.compass-eng-docs.pages.dev",
		"Take a peek over at https://final-abc123.compass-eng-docs.pages.dev",
	].join("\n");
	expect(parsePreviewUrl(output)).toBe(
		"https://final-abc123.compass-eng-docs.pages.dev",
	);
});

test("parsePreviewUrl returns null when no pages.dev URL is present", () => {
	expect(parsePreviewUrl("Deploy complete. See https://example.com/x")).toBe(
		null,
	);
});

test("parsePreviewUrl returns null for empty output", () => {
	expect(parsePreviewUrl("")).toBe(null);
});

// ── changedDocPages ──────────────────────────────────────────────────────────

// Hermetic markdownlint config literal mirroring the real .markdownlint-cli2.jsonc
// shape (no file/network read). parseExclusions() reads `ignores` and appends
// `**/outputs/**`; isExcluded() additionally hard-excludes `apps/eng-docs/**`.
const markdownlintConfig = JSON.stringify({
	globs: ["**/*.md"],
	gitignore: true,
	ignores: ["forks/*/**"],
});

test("changedDocPages maps a docs/ file to its site route", () => {
	// classify("docs/designs/repo/foo.md") → destRel "designs/repo/foo.md"
	// (domain "designs" ∈ DOMAINS); routeSlug drops .md + lowercases →
	// "/designs/repo/foo".
	expect(
		changedDocPages(
			[{ filename: "docs/designs/repo/foo.md", status: "modified" }],
			markdownlintConfig,
		),
	).toEqual([
		{
			sourcePath: "docs/designs/repo/foo.md",
			route: "/designs/repo/foo",
		},
	]);
});

test("changedDocPages slugifies a dotted directory segment", () => {
	// A synthetic docs path with a dotted directory segment. changedDocPages is
	// a pure classifier (no disk read), so the fixture need not name a real file
	// — it exercises routeSlug slugifying each segment, stripping the dot:
	// `v1.2` → `v12`. The dotted route would 404 otherwise.
	expect(
		changedDocPages(
			[
				{
					filename: "docs/designs/product/v1.2/design.md",
					status: "modified",
				},
			],
			markdownlintConfig,
		),
	).toEqual([
		{
			sourcePath: "docs/designs/product/v1.2/design.md",
			route: "/designs/product/v12/design",
		},
	]);
});

test("changedDocPages maps a package doc via packages/", () => {
	// "go/README.md" is not a docs/ path, not in CONTRIBUTING_FILES;
	// packagePath → { id: "go", rest: "README.md" } → destRel
	// "packages/go/README.md"; routeSlug lowercases → "/packages/go/readme".
	expect(
		changedDocPages(
			[{ filename: "go/README.md", status: "added" }],
			markdownlintConfig,
		),
	).toEqual([{ sourcePath: "go/README.md", route: "/packages/go/readme" }]);
});

test("changedDocPages drops a non-markdown file", () => {
	// .png fails the /\.md$/ extension gate — dropped before classify.
	expect(
		changedDocPages(
			[{ filename: "docs/designs/repo/diagram.png", status: "added" }],
			markdownlintConfig,
		),
	).toEqual([]);
});

test("changedDocPages drops a file excluded by the markdownlint ignores", () => {
	// "forks/oh-my-pi/readme.md" matches the "forks/*/**" ignore glob →
	// isExcluded true.
	expect(
		changedDocPages(
			[{ filename: "forks/oh-my-pi/readme.md", status: "added" }],
			markdownlintConfig,
		),
	).toEqual([]);
});

test("changedDocPages drops the docsite's own tree (apps/eng-docs/**)", () => {
	// isExcluded hard-excludes anything under apps/eng-docs/ so the gathered site
	// never mirrors itself.
	expect(
		changedDocPages(
			[{ filename: "apps/eng-docs/src/content/x.md", status: "modified" }],
			markdownlintConfig,
		),
	).toEqual([]);
});

test("changedDocPages drops a deleted (removed) markdown file", () => {
	// A removed file has no rendered page — the status !== "removed" gate.
	expect(
		changedDocPages(
			[{ filename: "docs/specs/web/gone.md", status: "removed" }],
			markdownlintConfig,
		),
	).toEqual([]);
});

test("changedDocPages returns [] for empty input", () => {
	expect(changedDocPages([], markdownlintConfig)).toEqual([]);
});

test("changedDocPages returns [] when every file is dropped", () => {
	expect(
		changedDocPages(
			[
				{ filename: "docs/designs/repo/diagram.png", status: "added" }, // non-md
				{ filename: "docs/specs/web/gone.md", status: "removed" }, // removed
				{ filename: "apps/eng-docs/src/content/x.md", status: "modified" }, // docsite tree
				{ filename: "forks/oh-my-pi/readme.md", status: "added" }, // markdownlint-excluded
			],
			markdownlintConfig,
		),
	).toEqual([]);
});

test("changedDocPages preserves input order and multiplicity, dropping in place", () => {
	// Two kept files with a dropped .png and a duplicate interleaved: the output is
	// the kept pages in input order, duplicates retained (no dedup, no reorder).
	// docs/specs/web/api.md → destRel "specs/web/api.md" ("specs" ∈ DOMAINS) →
	// "/specs/web/api".
	expect(
		changedDocPages(
			[
				{ filename: "docs/designs/repo/foo.md", status: "modified" },
				{ filename: "docs/designs/repo/diagram.png", status: "added" }, // dropped
				{ filename: "docs/designs/repo/foo.md", status: "added" }, // duplicate
				{ filename: "docs/specs/web/api.md", status: "modified" },
			],
			markdownlintConfig,
		),
	).toEqual([
		{
			sourcePath: "docs/designs/repo/foo.md",
			route: "/designs/repo/foo",
		},
		{
			sourcePath: "docs/designs/repo/foo.md",
			route: "/designs/repo/foo",
		},
		{ sourcePath: "docs/specs/web/api.md", route: "/specs/web/api" },
	]);
});

test("changedDocPages drops an uppercase .MD file (case-SENSITIVE extension gate)", () => {
	// The gate is /\.md$/ (NO /i): gather's Glob("**/*.md") is case-sensitive and
	// routeSlug strips ".md" case-sensitively, so a `.MD` page the gather never
	// produced must be dropped — linking it would 404.
	expect(
		changedDocPages(
			[{ filename: "docs/designs/repo/FOO.MD", status: "added" }],
			markdownlintConfig,
		),
	).toEqual([]);
});

test("changedDocPages keeps a renamed markdown file (only 'removed' is dropped)", () => {
	// The status gate drops only "removed"; "renamed"/"added"/"modified" all
	// render, so a renamed page is mapped like any other.
	expect(
		changedDocPages(
			[{ filename: "docs/designs/repo/foo.md", status: "renamed" }],
			markdownlintConfig,
		),
	).toEqual([
		{
			sourcePath: "docs/designs/repo/foo.md",
			route: "/designs/repo/foo",
		},
	]);
});

test("changedDocPages drops node_modules/ and /dist/ paths (gather secondary skips)", () => {
	// Parity with the gather's Glob scan skips — a page there never renders;
	// classify would otherwise map it to a dead route.
	expect(
		changedDocPages(
			[
				{ filename: "node_modules/p/readme.md", status: "added" },
				{ filename: "packages/x/dist/gen.md", status: "modified" },
			],
			markdownlintConfig,
		),
	).toEqual([]);
});

// Each VCS/tooling dotdir the gather skips — one test per alt so dropping any
// single alt from the regex reddens a named case. Each path passes every other
// guard (it is .md, not removed, not node_modules/dist, not in the exclusion
// globs), so its drop is attributable to the dotdir skip alone.
for (const dir of [
	"git",
	"astro",
	"direnv",
	"moon",
	"vscode",
	"idea",
	"pagefind",
]) {
	test(`changedDocPages drops the .${dir}/ tooling dotdir (gather secondary skip)`, () => {
		expect(
			changedDocPages(
				[{ filename: `.${dir}/notes.md`, status: "added" }],
				markdownlintConfig,
			),
		).toEqual([]);
	});
}

// ── parseChangedFiles ────────────────────────────────────────────────────────

test("parseChangedFiles maps each TAB-separated line to a ChangedFile", () => {
	expect(
		parseChangedFiles(
			"docs/a.md\tmodified\ngo/README.md\tadded\ndocs/my design.md\trenamed",
		),
	).toEqual([
		{ filename: "docs/a.md", status: "modified" },
		{ filename: "go/README.md", status: "added" },
		// a filename with spaces is fine — the split is on TAB, not whitespace.
		{ filename: "docs/my design.md", status: "renamed" },
	]);
});

test("parseChangedFiles ignores a trailing newline (no empty trailing entry)", () => {
	expect(parseChangedFiles("docs/a.md\tmodified\n")).toEqual([
		{ filename: "docs/a.md", status: "modified" },
	]);
});

test("parseChangedFiles treats CRLF line endings like LF", () => {
	// gh on a Windows runner may emit \r\n; the \r must not leak into status.
	expect(
		parseChangedFiles("docs/a.md\tmodified\r\ngo/README.md\tadded\r\n"),
	).toEqual([
		{ filename: "docs/a.md", status: "modified" },
		{ filename: "go/README.md", status: "added" },
	]);
});

test("parseChangedFiles drops a line with no tab", () => {
	expect(
		parseChangedFiles("docs/a.md\tmodified\nnotabline\ngo/README.md\tadded"),
	).toEqual([
		{ filename: "docs/a.md", status: "modified" },
		{ filename: "go/README.md", status: "added" },
	]);
});

test("parseChangedFiles skips blank lines", () => {
	expect(
		parseChangedFiles("docs/a.md\tmodified\n\ngo/README.md\tadded"),
	).toEqual([
		{ filename: "docs/a.md", status: "modified" },
		{ filename: "go/README.md", status: "added" },
	]);
});

test("parseChangedFiles splits on the FIRST tab, keeping the remainder as status", () => {
	// indexOf('\t') → everything after the first tab is the status verbatim.
	expect(parseChangedFiles("a\tb\tc")).toEqual([
		{ filename: "a", status: "b\tc" },
	]);
});

// ── escapeLinkText / encodeRoutePath ─────────────────────────────────────────

test("escapeLinkText backslash-escapes \\ [ ] ` and HTML-entity-encodes < > &", () => {
	// \ [ ] ` structure Markdown (link + code span) → backslash-escaped, inert;
	// < > & are HTML-significant (GitHub renders a raw <img>/<details> in comment
	// Markdown) → entity-encoded, so an attacker-named .md can't inject markup or
	// break the link.
	expect(escapeLinkText("foo](evil).md")).toBe("foo\\](evil).md");
	expect(escapeLinkText("a[b]c\\d")).toBe("a\\[b\\]c\\\\d");
	// a backtick would open a GFM code span that swallows adjacent link lines →
	// backslash-escaped so the filename char stays inert.
	expect(escapeLinkText("a`b.md")).toBe("a\\`b.md");
	expect(escapeLinkText('<img src="x">.md')).toBe('&lt;img src="x"&gt;.md');
	expect(escapeLinkText("a&b")).toBe("a&amp;b");
	expect(escapeLinkText("<details>")).toBe("&lt;details&gt;");
	// A filename literally containing an entity-like sequence is escaped
	// faithfully: the `&` is encoded so `&lt;` renders as the literal text
	// "&lt;", never decoded to a `<`. This is why escapeLinkText encodes `&`.
	expect(escapeLinkText("a&lt;b")).toBe("a&amp;lt;b");
	expect(escapeLinkText("&amp;")).toBe("&amp;amp;");
	// a clean filename passes through untouched.
	expect(escapeLinkText("designs/repo/foo.md")).toBe("designs/repo/foo.md");
});

test("encodeRoutePath encodes each segment, keeps /, and encodes ( ) that would close a Markdown link", () => {
	// space + clean route:
	expect(encodeRoutePath("/designs/my doc/a b")).toBe(
		"/designs/my%20doc/a%20b",
	);
	expect(encodeRoutePath("/designs/repo/foo")).toBe("/designs/repo/foo");
	// ( ) → %28 %29: encodeURIComponent leaves them raw, and a raw ) closes the
	// Markdown (...) target early → 404. Encoded explicitly per segment.
	expect(encodeRoutePath("/x/foo)bar")).toBe("/x/foo%29bar");
	expect(encodeRoutePath("/x/foo(bar)")).toBe("/x/foo%28bar%29");
	// # is already encoded by encodeURIComponent — lock it.
	expect(encodeRoutePath("/x/a#b")).toBe("/x/a%23b");
});

// ── commentBody ──────────────────────────────────────────────────────────────

test("commentBody is marker-prefixed and carries the preview URL", () => {
	const url = "https://abc123.compass-eng-docs.pages.dev";
	const body = commentBody({
		previewUrl: url,
		branch: "feat/x",
		commitSha: "abcdef1234",
	});
	expect(body.startsWith("<!-- compass-eng-docs-preview -->")).toBe(true);
	expect(body).toContain(url);
});

test("commentBody names the site and records the branch + short commit", () => {
	expect(
		commentBody({
			previewUrl: "https://x.pages.dev",
			branch: "feat/x",
			commitSha: "abcdef1234567",
		}),
	).toBe(
		"<!-- compass-eng-docs-preview -->\n**Compass engineering docs preview:** https://x.pages.dev\n\nDeployed from `feat/x` at `abcdef1`.",
	);
});

test("commentBody omits the commit clause when no sha is available", () => {
	expect(
		commentBody({ previewUrl: "https://x.pages.dev", branch: "feat/x" }),
	).toBe(
		"<!-- compass-eng-docs-preview -->\n**Compass engineering docs preview:** https://x.pages.dev\n\nDeployed from `feat/x`.",
	);
});

test("commentBody with changedPages: [] is byte-identical to no section", () => {
	// Empty changedPages must not add a section — backward-compatible with the
	// pre-feature comment string.
	expect(
		commentBody({
			previewUrl: "https://x.pages.dev",
			branch: "feat/x",
			commitSha: "abcdef1234567",
			changedPages: [],
		}),
	).toBe(
		"<!-- compass-eng-docs-preview -->\n**Compass engineering docs preview:** https://x.pages.dev\n\nDeployed from `feat/x` at `abcdef1`.",
	);
});

test("commentBody appends the Changed pages section verbatim (exact format lock)", () => {
	// The section format is review-frozen: bold heading, bullet list, escaped
	// label, single-slash `previewUrl+route` link target. Pin the whole body so
	// the bullet form, heading, and label text all regress-lock.
	expect(
		commentBody({
			previewUrl: "https://abc.pages.dev",
			branch: "feat/x",
			commitSha: "abcdef1234567",
			changedPages: [
				{
					sourcePath: "docs/designs/repo/foo.md",
					route: "/designs/repo/foo",
				},
				{ sourcePath: "go/README.md", route: "/packages/go/readme" },
			],
		}),
	).toBe(
		"<!-- compass-eng-docs-preview -->\n**Compass engineering docs preview:** https://abc.pages.dev\n\nDeployed from `feat/x` at `abcdef1`.\n\n**Changed pages:**\n- [docs/designs/repo/foo.md](https://abc.pages.dev/designs/repo/foo)\n- [go/README.md](https://abc.pages.dev/packages/go/readme)",
	);
});

test("commentBody escapes a Markdown-breaking char in an attacker-controlled sourcePath", () => {
	// sourcePath is a git filename from the PR's changed-files list (attacker-
	// controllable). The `]` in the label is backslash-escaped so a crafted name
	// like `foo](evil).md` can't close the link early and inject a spoofed target.
	const body = commentBody({
		previewUrl: "https://abc.pages.dev",
		branch: "feat/x",
		changedPages: [{ sourcePath: "foo](evil).md", route: "/designs/foo" }],
	});
	expect(body).toContain(
		"[foo\\](evil).md](https://abc.pages.dev/designs/foo)",
	);
	// the unescaped early-close link never appears
	expect(body).not.toContain("[foo](evil)");
});

test("commentBody escapes the label AND encodes the route for one changed page (exact body)", () => {
	// Combined worst case in a single entry: sourcePath needs < > entity-encoded
	// (the ) stays literal — not in the escape set), and the route's ) must be
	// %29 so the Markdown link target isn't truncated. Whole body byte-exact.
	expect(
		commentBody({
			previewUrl: "https://abc.pages.dev",
			branch: "feat/x",
			commitSha: "abcdef1234567",
			changedPages: [{ sourcePath: "<x>foo).md", route: "/designs/foo)bar" }],
		}),
	).toBe(
		"<!-- compass-eng-docs-preview -->\n**Compass engineering docs preview:** https://abc.pages.dev\n\nDeployed from `feat/x` at `abcdef1`.\n\n**Changed pages:**\n- [&lt;x&gt;foo).md](https://abc.pages.dev/designs/foo%29bar)",
	);
});

// ── ghToken ──────────────────────────────────────────────────────────────────

test("ghToken returns GH_TOKEN", () => {
	expect(ghToken({ GH_TOKEN: "gh-tok" })).toBe("gh-tok");
});

test("ghToken trims GH_TOKEN", () => {
	expect(ghToken({ GH_TOKEN: "  gh-tok  " })).toBe("gh-tok");
});

test("ghToken is undefined when GH_TOKEN is blank", () => {
	expect(ghToken({ GH_TOKEN: "   " })).toBeUndefined();
});

test("ghToken is undefined when GH_TOKEN is not set", () => {
	const env: DeployEnv = {};
	expect(ghToken(env)).toBeUndefined();
});

// ── resolvePreviewComment ─────────────────────────────────────────────────────

const commentEnv: DeployEnv = {
	GITHUB_EVENT_NAME: "pull_request",
	GITHUB_HEAD_REF: "feat/x",
	PR_NUMBER: "42",
	GITHUB_REPOSITORY: "RigelBuild/compass",
	GH_TOKEN: "gh-tok",
};

test("resolvePreviewComment returns the resolved inputs when all are present", () => {
	const result = resolvePreviewComment(commentEnv);
	expect(result).toEqual({
		ok: true,
		inputs: { repo: "RigelBuild/compass", pr: "42", token: "gh-tok" },
	});
});

test("resolvePreviewComment trims the PR number and repo", () => {
	const result = resolvePreviewComment({
		...commentEnv,
		PR_NUMBER: "  42  ",
		GITHUB_REPOSITORY: "  RigelBuild/compass  ",
	});
	expect(result).toEqual({
		ok: true,
		inputs: { repo: "RigelBuild/compass", pr: "42", token: "gh-tok" },
	});
});

test("resolvePreviewComment fails (not skips) when no GitHub token is set", () => {
	const { GH_TOKEN: _t, ...noToken } = commentEnv;
	const result = resolvePreviewComment(noToken);
	expect(result.ok).toBe(false);
	if (!result.ok) expect(result.reason).toContain("GitHub token");
});

test("resolvePreviewComment fails when the PR number is missing", () => {
	const { PR_NUMBER: _p, ...noPr } = commentEnv;
	const result = resolvePreviewComment(noPr);
	expect(result.ok).toBe(false);
	if (!result.ok) expect(result.reason).toContain("PR number");
});

test("resolvePreviewComment fails when the repo is missing", () => {
	const { GITHUB_REPOSITORY: _r, ...noRepo } = commentEnv;
	const result = resolvePreviewComment(noRepo);
	expect(result.ok).toBe(false);
	if (!result.ok) expect(result.reason).toContain("repo");
});

test("resolvePreviewComment names every missing input at once", () => {
	const result = resolvePreviewComment({ GITHUB_EVENT_NAME: "pull_request" });
	expect(result.ok).toBe(false);
	if (!result.ok) {
		expect(result.reason).toContain("GitHub token");
		expect(result.reason).toContain("PR number");
		expect(result.reason).toContain("repo");
	}
});
