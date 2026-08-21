import { describe, expect, test } from "bun:test";
import {
	buildFrontmatter,
	buildIndex,
	buildSidebar,
	classify,
	destRelPath,
	editUrlFor,
	extractTitle,
	isExcluded,
	parseExclusions,
	rewriteLinks,
	routeSlug,
	stripFirstH1,
	transform,
} from "./gather.ts";

// -- extractTitle -------------------------------------------------------------
// The docs/ sources carry no frontmatter — every file opens with a `# H1`.
// Starlight's docsSchema requires a `title`, so the gather derives it from
// that first heading.

describe("extractTitle", () => {
	test("returns the text of the first H1", () => {
		expect(extractTitle("# Technology stack (engineering)\n\nbody")).toBe(
			"Technology stack (engineering)",
		);
	});

	test("ignores leading blank lines and whitespace before the H1", () => {
		expect(extractTitle("\n\n   # Observability\n")).toBe("Observability");
	});

	test("takes the first H1 when several headings follow", () => {
		expect(extractTitle("# First\n\n## Second\n\n# Third")).toBe("First");
	});

	test("does not treat an H2 as the title", () => {
		expect(extractTitle("## Not a title\n\n# Real title")).toBe("Real title");
	});

	test("trims trailing whitespace off the heading text", () => {
		expect(extractTitle("#   Padded   \n")).toBe("Padded");
	});

	test("falls back to the basename (no extension) when there is no H1", () => {
		expect(
			extractTitle("no heading here\n", "docs/specs/tools/README.md"),
		).toBe("README");
	});

	test("does not mistake a `#` inside a fenced code block for the title", () => {
		const src = "```sh\n# a shell comment\n```\n\n# Real heading\n";
		expect(extractTitle(src)).toBe("Real heading");
	});

	test("does not mistake a `#` inside a tilde-fenced code block for the title", () => {
		const src = "~~~sh\n# a shell comment\n~~~\n\n# Real heading\n";
		expect(extractTitle(src)).toBe("Real heading");
	});
});

// -- buildFrontmatter ---------------------------------------------------------
// Real titles contain YAML-significant characters (`:`, backticks, `&`, `(`,
// em-dash). The injected block must round-trip through a YAML parser, so the
// title is always double-quoted with embedded quotes/backslashes escaped. The
// optional second arg adds an `editUrl:` line for gathered-outside-docs pages.

describe("buildFrontmatter", () => {
	test("wraps a plain title in a double-quoted YAML scalar", () => {
		expect(buildFrontmatter("Observability")).toBe(
			'---\ntitle: "Observability"\n---\n',
		);
	});

	test("keeps a title containing a colon safe (would break bare YAML)", () => {
		expect(buildFrontmatter("CI/CD evolution: TS dynamic pipelines")).toBe(
			'---\ntitle: "CI/CD evolution: TS dynamic pipelines"\n---\n',
		);
	});

	test("escapes embedded double quotes", () => {
		expect(buildFrontmatter('The "read layer"')).toBe(
			'---\ntitle: "The \\"read layer\\""\n---\n',
		);
	});

	test("escapes backslashes before quotes so the escape is unambiguous", () => {
		expect(buildFrontmatter("a\\b")).toBe('---\ntitle: "a\\\\b"\n---\n');
	});

	test("leaves backticks, ampersands and parens intact inside the quotes", () => {
		expect(buildFrontmatter("Per-PR candidate CI image (`:pr-<N>`)")).toBe(
			'---\ntitle: "Per-PR candidate CI image (`:pr-<N>`)"\n---\n',
		);
	});

	test("adds an editUrl line under the title when one is given", () => {
		expect(
			buildFrontmatter(
				"Observability",
				"https://github.com/RigelBuild/compass/edit/main/ci/README.md",
			),
		).toBe(
			'---\ntitle: "Observability"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/ci/README.md"\n---\n',
		);
	});

	test("still escapes the title when an editUrl is present", () => {
		expect(
			buildFrontmatter(
				'The "read layer"',
				"https://github.com/RigelBuild/compass/edit/main/docs/a.md",
			),
		).toBe(
			'---\ntitle: "The \\"read layer\\""\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/a.md"\n---\n',
		);
	});
});

// -- stripFirstH1 -------------------------------------------------------------
// Starlight renders the frontmatter `title` as the page's <h1>. Leaving the
// original `# H1` in the body would double it, so the gather removes exactly
// the first H1 line (and a single blank line after it) and nothing else.

describe("stripFirstH1", () => {
	test("removes the first H1 line", () => {
		expect(stripFirstH1("# Title\n\nbody\n")).toBe("body\n");
	});

	test("removes only ONE blank line after the H1", () => {
		expect(stripFirstH1("# Title\n\n\nbody\n")).toBe("\nbody\n");
	});

	test("leaves a later H1 in place (only the first is stripped)", () => {
		expect(stripFirstH1("# One\n\nmid\n\n# Two\n")).toBe("mid\n\n# Two\n");
	});

	test("is a no-op when the body has no H1", () => {
		expect(stripFirstH1("no heading\n")).toBe("no heading\n");
	});

	test("preserves body content that follows immediately (no blank line)", () => {
		expect(stripFirstH1("# Title\nbody\n")).toBe("body\n");
	});

	test("skips a `#` inside a leading fenced code block", () => {
		const src = "```sh\n# not a heading\n```\n\n# Real Title\n\nbody\n";
		expect(stripFirstH1(src)).toBe("```sh\n# not a heading\n```\n\nbody\n");
	});
});

// -- destRelPath --------------------------------------------------------------
// The docs/ subtree layout is preserved verbatim under src/content/docs/ so
// Starlight's slug + the per-domain sidebar autogenerate match the on-disk
// taxonomy.

describe("destRelPath", () => {
	test("maps a docs/ path to the same relative path under content", () => {
		expect(destRelPath("docs/designs/repo/compass-eng-docs/design.md")).toBe(
			"designs/repo/compass-eng-docs/design.md",
		);
	});

	test("handles a top-level doc directly under a domain", () => {
		expect(destRelPath("docs/architecture/overview.md")).toBe(
			"architecture/overview.md",
		);
	});
});

// -- classify -----------------------------------------------------------------
// The single source of truth for where each gathered file lands. Every path is
// routed to exactly one nav section + a collision-free dest path.

describe("classify", () => {
	test("keeps a docs/<domain> file under its section verbatim", () => {
		expect(classify("docs/designs/repo/compass-eng-docs/design.md")).toEqual({
			section: "designs",
			destRel: "designs/repo/compass-eng-docs/design.md",
		});
	});

	test("routes each docs domain by its directory name", () => {
		expect(classify("docs/specs/tools/setup.md").section).toBe("specs");
		expect(classify("docs/architecture/overview.md").section).toBe(
			"architecture",
		);
	});

	// Regression 1: root README.md and forks/README.md share a basename and must
	// NOT collide under contributing/ — the slug disambiguates by source path.
	test("root README.md and forks/README.md land at different contributing dests", () => {
		const root = classify("README.md");
		const forks = classify("forks/README.md");
		expect(root).toEqual({
			section: "contributing",
			destRel: "contributing/README.md",
		});
		expect(forks).toEqual({
			section: "contributing",
			destRel: "contributing/forks-README.md",
		});
		expect(root.destRel).not.toBe(forks.destRel);
	});

	test("routes the other contributing files (AGENTS, CONTRIBUTING) too", () => {
		expect(classify("AGENTS.md")).toEqual({
			section: "contributing",
			destRel: "contributing/AGENTS.md",
		});
		expect(classify("CONTRIBUTING.md")).toEqual({
			section: "contributing",
			destRel: "contributing/CONTRIBUTING.md",
		});
	});

	// Regression 2: a top-level package file drops its dir exactly once — no
	// packages/go/go/README.md duplication.
	test("a top-level package file drops its dir once (no path duplication)", () => {
		expect(classify("go/README.md")).toEqual({
			section: "packages",
			destRel: "packages/go/README.md",
		});
	});

	// Regression 2 (apps form): group by apps-<child> and drop the first two
	// segments so the package prefix is not repeated in the rest.
	test("an apps/<x> file groups by apps-<x> and drops two segments", () => {
		expect(classify("apps/eng-docs/README.md")).toEqual({
			section: "packages",
			destRel: "packages/apps-eng-docs/README.md",
		});
	});

	test("a deeper apps/<x> file keeps the path below the package", () => {
		expect(classify("apps/web/docs/guide.md")).toEqual({
			section: "packages",
			destRel: "packages/apps-web/docs/guide.md",
		});
	});

	// Regression: a root single-segment file (empty `rest`) must not leave a
	// trailing slash in destRel — that defeats routeSlug's `.md$` strip and yields
	// a 404 route. `SECURITY.md` → `packages/security`, not `packages/security.md/`.
	test("a root single-segment file has no trailing slash (route resolves)", () => {
		expect(classify("SECURITY.md")).toEqual({
			section: "packages",
			destRel: "packages/SECURITY.md",
		});
		expect(routeSlug(classify("SECURITY.md").destRel)).toBe(
			"/packages/security",
		);
	});
});

// -- parseExclusions ----------------------------------------------------------
// Reads the canonical .markdownlint-cli2.jsonc `ignores` (single source of
// truth), tolerating // line comments, and always appends the one glob the
// gather owns: generated outputs/.

describe("parseExclusions", () => {
	test("strips // line comments, keeps ignores, appends the owned glob", () => {
		const config = [
			"{",
			"\t// scopes the linter to the repo",
			'\t"globs": ["**/*.md"],',
			"\t// exclusions below",
			'\t"ignores": [',
			'\t\t"forks/*/**",',
			'\t\t"config/prompts/**",',
			'\t\t"config/agents/**"',
			"\t]",
			"}",
		].join("\n");
		expect(parseExclusions(config)).toEqual([
			"forks/*/**",
			"config/prompts/**",
			"config/agents/**",
			"**/outputs/**",
		]);
	});

	test("still yields the owned glob when the config has no ignores", () => {
		expect(parseExclusions('{\n\t"globs": ["**/*.md"]\n}')).toEqual([
			"**/outputs/**",
		]);
	});
});

// -- isExcluded ---------------------------------------------------------------
// A path is dropped from the gather when it is the docsite's own tree (so it
// never mirrors itself) or matches an exclusion glob. Globs are path-segment
// anchored: ** any depth, **/ any depth incl zero, * one segment.

describe("isExcluded", () => {
	test("excludes the docsite's own tree even with no exclusion globs", () => {
		expect(isExcluded("apps/eng-docs/src/whatever.md", [])).toBe(true);
	});

	test("does not exclude an ordinary package doc", () => {
		expect(
			isExcluded("go/README.md", [
				"forks/*/**",
				"config/prompts/**",
				"**/outputs/**",
			]),
		).toBe(false);
	});

	test("excludes a vendored fork subtree via forks/*/**", () => {
		expect(isExcluded("forks/oh-my-pi/README.md", ["forks/*/**"])).toBe(true);
		expect(isExcluded("forks/oh-my-pi/src/deep/x.md", ["forks/*/**"])).toBe(
			true,
		);
	});

	test("keeps the first-party forks/README.md (forks/*/** does not match it)", () => {
		// The glob requires a fork dir between forks/ and the file; forks/README.md
		// has none, so it stays linted and gathered.
		expect(isExcluded("forks/README.md", ["forks/*/**"])).toBe(false);
	});

	test("excludes any outputs/ directory at any depth via **/outputs/**", () => {
		expect(isExcluded("apps/foo/outputs/report.md", ["**/outputs/**"])).toBe(
			true,
		);
	});

	test("matches **/outputs/** at depth zero (top-level outputs/)", () => {
		expect(isExcluded("outputs/report.md", ["**/outputs/**"])).toBe(true);
	});

	test("**/outputs/** is segment-anchored, not a substring match", () => {
		// `myoutputs` is a different directory than `outputs` — must not match.
		expect(isExcluded("apps/myoutputs/report.md", ["**/outputs/**"])).toBe(
			false,
		);
	});

	test("excludes the agent-context payload subtrees via config/prompts/**", () => {
		expect(isExcluded("config/prompts/manager.md", ["config/prompts/**"])).toBe(
			true,
		);
	});
});

// -- editUrlFor ---------------------------------------------------------------
// The per-page "edit this file" URL points at the true canonical source path
// on GitHub's main branch, regardless of where the page is mirrored on-site.

describe("editUrlFor", () => {
	test("builds the GitHub edit URL for a repo-relative source path", () => {
		expect(editUrlFor("docs/designs/repo/compass-eng-docs/design.md")).toBe(
			"https://github.com/RigelBuild/compass/edit/main/docs/designs/repo/compass-eng-docs/design.md",
		);
	});
});

// -- rewriteLinks -------------------------------------------------------------
// In-repo links are rewritten so they resolve on the site: relative links to
// gathered *.md become site routes; relative links to any other in-repo file
// become GitHub blob URLs; absolute/anchor/root-absolute/mailto are untouched.
// Anchors and queries ride along; `.`/`..` are collapsed against the source dir.

describe("rewriteLinks", () => {
	const src = "docs/designs/repo/compass-eng-docs/design.md";
	// The gather's collected file list. Only a link resolving to a member .md
	// becomes a site route; everything else degrades to a GitHub source URL.
	const gathered = new Set([
		"docs/specs/platform/technology-stack.md",
		"docs/designs/repo/compass-eng-docs/sibling.md",
	]);

	test("rewrites a relative .md link to its site route, collapsing `..`", () => {
		expect(
			rewriteLinks(
				"see [tech](../../../specs/platform/technology-stack.md)",
				src,
				gathered,
			),
		).toBe("see [tech](/specs/platform/technology-stack)");
	});

	test("preserves a #anchor suffix on a rewritten .md route", () => {
		expect(
			rewriteLinks(
				"see [tech](../../../specs/platform/technology-stack.md#deploy)",
				src,
				gathered,
			),
		).toBe("see [tech](/specs/platform/technology-stack#deploy)");
	});

	test("preserves a ?query suffix on a rewritten .md route", () => {
		expect(rewriteLinks("see [x](./sibling.md?v=1)", src, gathered)).toBe(
			"see [x](/designs/repo/compass-eng-docs/sibling?v=1)",
		);
	});

	test("keeps a link title after rewriting the .md target", () => {
		expect(
			rewriteLinks(
				'see [tech](../../../specs/platform/technology-stack.md "Stack")',
				src,
				gathered,
			),
		).toBe('see [tech](/specs/platform/technology-stack "Stack")');
	});

	test("rewrites a relative non-md link to a GitHub blob URL", () => {
		expect(
			rewriteLinks(
				"run [deploy](./deploy.ts)",
				"apps/eng-docs/scripts/notes.md",
				gathered,
			),
		).toBe(
			"run [deploy](https://github.com/RigelBuild/compass/blob/main/apps/eng-docs/scripts/deploy.ts)",
		);
	});

	test("collapses `..` when resolving a non-md blob target", () => {
		expect(
			rewriteLinks("[u](../shared/util.ts)", "ci/scripts/build.md", gathered),
		).toBe(
			"[u](https://github.com/RigelBuild/compass/blob/main/ci/shared/util.ts)",
		);
	});

	test("leaves absolute, protocol-relative, anchor, root-absolute and mailto links untouched", () => {
		const links =
			"[abs](https://example.com/a) " +
			"[proto](//cdn.example.com/x.md) " +
			"[anchor](#section) " +
			"[root](/already/routed) " +
			"[mail](mailto:team@example.com)";
		expect(rewriteLinks(links, src, gathered)).toBe(links);
	});

	// -- the gathered-set guard --------------------------------------------------
	// An in-repo .md that was NOT gathered has no site route, so it must degrade
	// to its visible GitHub source instead of a silent 404. Membership in
	// `gathered` — not the .md extension — is what decides route vs blob.

	test("degrades an ungathered .md link to its GitHub blob source", () => {
		// infrastructure.md resolves in-tree but is absent from `gathered`.
		expect(
			rewriteLinks(
				"see [infra](./infrastructure.md)",
				"docs/specs/platform/technology-stack.md",
				gathered,
			),
		).toBe(
			"see [infra](https://github.com/RigelBuild/compass/blob/main/docs/specs/platform/infrastructure.md)",
		);
	});

	test("carries #anchor and ?query onto an ungathered .md blob fallback", () => {
		expect(
			rewriteLinks(
				"see [infra](./infrastructure.md#deploy)",
				"docs/specs/platform/technology-stack.md",
				gathered,
			),
		).toBe(
			"see [infra](https://github.com/RigelBuild/compass/blob/main/docs/specs/platform/infrastructure.md#deploy)",
		);
		expect(
			rewriteLinks(
				"see [infra](./infrastructure.md?v=1)",
				"docs/specs/platform/technology-stack.md",
				gathered,
			),
		).toBe(
			"see [infra](https://github.com/RigelBuild/compass/blob/main/docs/specs/platform/infrastructure.md?v=1)",
		);
	});

	test("discriminates directory (tree) from file (blob) by the trailing slash", () => {
		const from = "docs/README.md";
		// Same base path: the trailing slash — not the extension — picks tree.
		expect(rewriteLinks("[d](../tools/docs-publish/)", from, gathered)).toBe(
			"[d](https://github.com/RigelBuild/compass/tree/main/tools/docs-publish)",
		);
		expect(rewriteLinks("[f](../tools/docs-publish)", from, gathered)).toBe(
			"[f](https://github.com/RigelBuild/compass/blob/main/tools/docs-publish)",
		);
	});

	test("membership in `gathered` — not the .md extension — flips route vs blob", () => {
		const link = "see [infra](./infrastructure.md)";
		const from = "docs/specs/platform/technology-stack.md";
		const target = "docs/specs/platform/infrastructure.md";
		// Identical link + source: routed only when the target is gathered.
		expect(rewriteLinks(link, from, new Set([target]))).toBe(
			"see [infra](/specs/platform/infrastructure)",
		);
		expect(rewriteLinks(link, from, new Set())).toBe(
			"see [infra](https://github.com/RigelBuild/compass/blob/main/docs/specs/platform/infrastructure.md)",
		);
	});
});

// -- transform (whole-file pipeline) -----------------------------------------
// The composed transform: inject the derived title + per-page editUrl as
// frontmatter, drop the now-duplicated body H1, and rewrite in-repo body links.

describe("transform", () => {
	// The gather's collected list; the body-links test asserts one member routes.
	const gathered = new Set(["docs/specs/platform/technology-stack.md"]);

	test("prepends title + editUrl frontmatter and strips the body H1", () => {
		const src = "# Observability\n\nThe platform...\n";
		expect(
			transform(src, "docs/specs/platform/observability.md", gathered),
		).toBe(
			'---\ntitle: "Observability"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/specs/platform/observability.md"\n---\nThe platform...\n',
		);
	});

	test("uses the basename fallback and injects no-op strip when no H1", () => {
		const src = "prose with no heading\n";
		expect(transform(src, "docs/specs/tools/README.md", gathered)).toBe(
			'---\ntitle: "README"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/specs/tools/README.md"\n---\nprose with no heading\n',
		);
	});

	test("preserves a title with a colon end-to-end", () => {
		const src = "# Docs consumption: Notion as the read layer\n\nWhy...\n";
		expect(
			transform(src, "docs/designs/platform/docs-consumption.md", gathered),
		).toBe(
			'---\ntitle: "Docs consumption: Notion as the read layer"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/designs/platform/docs-consumption.md"\n---\nWhy...\n',
		);
	});

	test("a doc opening with a fenced `#` keeps the fence and strips the real H1", () => {
		const src = "```sh\n# example\n```\n\n# Setup\n\nSteps...\n";
		expect(transform(src, "docs/specs/tools/setup.md", gathered)).toBe(
			'---\ntitle: "Setup"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/specs/tools/setup.md"\n---\n```sh\n# example\n```\n\nSteps...\n',
		);
	});

	test("a doc opening with a tilde-fenced `#` keeps the fence and strips the real H1", () => {
		const src = "~~~sh\n# example\n~~~\n\n# Setup\n\nSteps...\n";
		expect(transform(src, "docs/specs/tools/setup.md", gathered)).toBe(
			'---\ntitle: "Setup"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/specs/tools/setup.md"\n---\n~~~sh\n# example\n~~~\n\nSteps...\n',
		);
	});

	test("rewrites body links while injecting the editUrl (md→route, code→blob)", () => {
		const src =
			"# Docsite\n\nSee [tech](../../specs/platform/technology-stack.md) and [deploy](./deploy.ts).\n";
		expect(transform(src, "docs/designs/platform/docsite.md", gathered)).toBe(
			'---\ntitle: "Docsite"\neditUrl: "https://github.com/RigelBuild/compass/edit/main/docs/designs/platform/docsite.md"\n---\nSee [tech](/specs/platform/technology-stack) and [deploy](https://github.com/RigelBuild/compass/blob/main/docs/designs/platform/deploy.ts).\n',
		);
	});
});

// -- routeSlug ----------------------------------------------------------------
// The Starlight route for a gathered doc: its content-relative path minus the
// .md extension, lowercased (Starlight lowercases slugs). Drives the landing
// page's links so they point at pages that exist.

describe("routeSlug", () => {
	test("maps a content-relative md path to a leading-slash route", () => {
		expect(routeSlug("designs/repo/compass-eng-docs/design.md")).toBe(
			"/designs/repo/compass-eng-docs/design",
		);
	});

	test("lowercases the route (Starlight lowercases slugs)", () => {
		expect(routeSlug("contributing/README.md")).toBe("/contributing/readme");
	});

	// Astro content collections slugify EACH path segment through github-slugger
	// (astro/dist/content/utils.js:271-272 — split on path.sep, map githubSlug,
	// join). github-slugger strips the `.`, so a directory like `compass-0.6`
	// becomes `compass-06` on the deployed site. routeSlug must match, or the
	// landing-page link 404s.
	test("slugifies a dotted directory segment like Astro (strips the dot)", () => {
		expect(routeSlug("designs/product/compass-0.6/design.md")).toBe(
			"/designs/product/compass-06/design",
		);
	});

	test("slugifies the compass-0.5-server dotted segment", () => {
		expect(routeSlug("designs/product/compass-0.5-server/design.md")).toBe(
			"/designs/product/compass-05-server/design",
		);
	});

	test("slugifies the compass-0.4 dotted segment", () => {
		expect(routeSlug("designs/product/compass-0.4/design.md")).toBe(
			"/designs/product/compass-04/design",
		);
	});

	test("leaves a non-dotted segment unchanged", () => {
		expect(routeSlug("packages/security.md")).toBe("/packages/security");
	});

	// Astro drops a trailing `/index` segment so `.../guide/index.md` renders at
	// `.../guide`, not `.../guide/index` (astro/dist/content/utils.js:272).
	test("strips a trailing /index segment", () => {
		expect(routeSlug("designs/platform/guide/index.md")).toBe(
			"/designs/platform/guide",
		);
	});

	// The strip is anchored to a full `/index` segment boundary, NOT a substring:
	// mis-anchoring it (e.g. `/index$` without the `/`) would eat the tail of a
	// real `myindex` page and 404 that route on the deployed site.
	test("does not strip a segment merely ending in index", () => {
		expect(routeSlug("designs/platform/myindex.md")).toBe(
			"/designs/platform/myindex",
		);
	});
});

// -- buildIndex ---------------------------------------------------------------
// Starlight has no route for the site root or the autogenerated group dirs, so
// the gather emits a splash landing linking to the first page of each populated
// section. Links follow SECTIONS order and use SECTIONS labels; an empty
// section is omitted so no link 404s. `DomainEntry.domain` is a section key.

describe("buildIndex", () => {
	// Entries given out of SECTIONS order to prove the output is re-ordered.
	const entries = [
		{ domain: "contributing", route: "/contributing/readme", title: "Readme" },
		{
			domain: "architecture",
			route: "/architecture/overview",
			title: "Overview",
		},
		{
			domain: "designs",
			route: "/designs/repo/compass-eng-docs/design",
			title: "Docsite",
		},
	];

	test("emits splash frontmatter with a title", () => {
		const md = buildIndex(entries);
		expect(md).toContain('title: "Compass Engineering Docs"');
		expect(md).toContain("template: splash");
	});

	test("links each section to its real page route with the SECTIONS label", () => {
		const md = buildIndex(entries);
		expect(md).toContain(
			"- [Designs](/designs/repo/compass-eng-docs/design) — Docsite",
		);
		expect(md).toContain("- [Architecture](/architecture/overview) — Overview");
		expect(md).toContain("- [Contributing](/contributing/readme) — Readme");
	});

	test("orders the links by SECTIONS, not by entry order", () => {
		const md = buildIndex(entries);
		expect(md.indexOf("[Designs]")).toBeLessThan(md.indexOf("[Architecture]"));
		expect(md.indexOf("[Architecture]")).toBeLessThan(
			md.indexOf("[Contributing]"),
		);
	});

	test("omits a section with no gathered page", () => {
		// specs/packages empty this run → not linked (would 404).
		const md = buildIndex(entries);
		expect(md).not.toContain("[Specs]");
		expect(md).not.toContain("[Packages]");
	});
});

// -- buildSidebar -------------------------------------------------------------
// The generated sidebar module: one autogenerate group per populated section,
// in SECTIONS order, with absent sections omitted. Emitted as a TS module, so
// we parse the `export const sidebar = [...]` array back out and assert on it.

interface SidebarGroup {
	label: string;
	items: Array<{ autogenerate: { directory: string } }>;
}

function parseSidebar(module: string): SidebarGroup[] {
	const match = module.match(/export const sidebar = ([\s\S]*);\n$/);
	if (!match) throw new Error("sidebar export not found in generated module");
	return JSON.parse(match[1]) as SidebarGroup[];
}

describe("buildSidebar", () => {
	test("emits groups in SECTIONS order regardless of input order, with SECTIONS labels", () => {
		// All five keys, shuffled — output must be the canonical order.
		const groups = parseSidebar(
			buildSidebar([
				"contributing",
				"packages",
				"architecture",
				"specs",
				"designs",
			]),
		);
		expect(groups.map((g) => g.label)).toEqual([
			"Designs",
			"Specs",
			"Architecture",
			"Packages",
			"Contributing",
		]);
		expect(groups.map((g) => g.items[0].autogenerate.directory)).toEqual([
			"designs",
			"specs",
			"architecture",
			"packages",
			"contributing",
		]);
	});

	test("omits sections that are not present", () => {
		const groups = parseSidebar(buildSidebar(["packages", "designs"]));
		expect(groups.map((g) => g.label)).toEqual(["Designs", "Packages"]);
		expect(groups.map((g) => g.items[0].autogenerate.directory)).toEqual([
			"designs",
			"packages",
		]);
	});
});
