// Gather the reviewed monorepo markdown into the Starlight content collection
// (SEA-1764). The canonical sources carry no frontmatter and open with a `# H1`;
// Starlight's docsSchema requires a `title`. This mirrors each source into
// apps/eng-docs/src/content/docs/ with a derived `title:` block, the now-
// duplicated body H1 removed, and in-repo links rewritten to site routes /
// GitHub blobs.
//
// Coverage is the whole monorepo: every tracked `*.md` except the canonical
// top-level exclusion set (`.markdownlint-cli2.jsonc` `ignores`, the single
// source of truth) and generated `outputs/`. Each file is classified into one
// nav SECTION (Designs, Specs, Architecture, Packages, Contributing); the
// section drives both its dest path under the content root and the generated
// sidebar. This file stays the single gather entry point and emits
// `src/sidebar.generated.ts` for astro.config.mjs to import.
//
// The pipeline splits pure construction (the exported functions below, unit-
// tested in gather.test.ts) from execution (main(), a thin fs runner).

import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { basename, dirname, join } from "node:path";
import { Glob } from "bun";
import { slug as githubSlug } from "github-slugger";

/** The GitHub repo the docsite renders — for edit links + code-file link rewrites. */
const REPO_SLUG = "sealedsecurity/compass";

/**
 * The `docs/` subtrees that map to a nav section by their directory name. Each
 * renders under its own section verbatim (the on-disk layout is the taxonomy).
 */
const DOMAINS = ["designs", "specs", "architecture"] as const;

/**
 * A nav section: its sidebar label and the content-root directory its pages
 * live under. Order here is the sidebar order.
 */
export interface Section {
	readonly key: string;
	readonly label: string;
}

/** The nav taxonomy, in sidebar order. `key` is the content-root subdirectory. */
export const SECTIONS: readonly Section[] = [
	{ key: "designs", label: "Designs" },
	{ key: "specs", label: "Specs" },
	{ key: "architecture", label: "Architecture" },
	{ key: "packages", label: "Packages" },
	{ key: "contributing", label: "Contributing" },
] as const;

/** Repo-root files that are contributor-facing conventions, not package docs. */
const CONTRIBUTING_FILES = new Set([
	"README.md",
	"AGENTS.md",
	"CONTRIBUTING.md",
	"forks/README.md",
]);

/** A classified source: which section it belongs to and where it renders. */
export interface Classified {
	/** Section key (a `SECTIONS` entry). */
	section: string;
	/** Path under the content root (`src/content/docs/`), no leading slash. */
	destRel: string;
}

/**
 * The package a non-`docs/` source belongs to, split into a grouping `id` and
 * the path `rest` within that package. `apps/<x>` is a two-level unit (group by
 * `apps-<x>`); a file directly under a top dir (e.g. `ci/README.md`) groups by
 * that dir. Drives `packages/<id>/<rest>` so each package's docs sit together
 * without repeating the package prefix in the path.
 */
function packagePath(sourcePath: string): { id: string; rest: string } {
	const parts = sourcePath.split("/");
	if (parts[0] === "apps" && parts.length > 2) {
		return { id: `${parts[0]}-${parts[1]}`, rest: parts.slice(2).join("/") };
	}
	return { id: parts[0], rest: parts.slice(1).join("/") };
}

/**
 * Classify a repo-relative markdown path into its nav section + dest path.
 * The single source of truth for where every gathered file lands — both the
 * on-disk mirror and (via the dest path) its site route.
 *
 * - `docs/<domain>/…` keeps its natural layout under the matching section.
 * - Root README/AGENTS/CONTRIBUTING/forks-README → `contributing/`.
 * - everything else (per-package README/AGENTS) → `packages/<pkg>/…`.
 */
export function classify(sourcePath: string): Classified {
	const docsMatch = sourcePath.match(/^docs\/([^/]+)\/(.+)$/);
	if (docsMatch) {
		const [, domain, rest] = docsMatch;
		if ((DOMAINS as readonly string[]).includes(domain)) {
			return { section: domain, destRel: `${domain}/${rest}` };
		}
	}
	if (CONTRIBUTING_FILES.has(sourcePath)) {
		// Disambiguate by source location so root README.md and forks/README.md
		// (both basename README.md) don't collide under contributing/.
		const slug = sourcePath.replace(/^\./, "").replace(/\//g, "-");
		return { section: "contributing", destRel: `contributing/${slug}` };
	}
	const pkg = packagePath(sourcePath);
	// A root single-segment file (e.g. `SECURITY.md`) has an empty `rest`; joining
	// it as `packages/${id}/${rest}` would leave a trailing slash, which defeats
	// routeSlug's `.md$` strip and yields a 404 route (`/packages/security.md/`).
	// Omit the empty segment so both the on-disk mirror and the route are clean.
	return {
		section: "packages",
		destRel: pkg.rest ? `packages/${pkg.id}/${pkg.rest}` : `packages/${pkg.id}`,
	};
}

/**
 * The top-level exclusion globs, read from the canonical `.markdownlint-cli2.jsonc`
 * `ignores` list (the single source of truth) plus the one the gather always
 * adds: generated `outputs/`. Declared once there, not duplicated here — a new
 * excluded dir is added in that config.
 */
export function parseExclusions(markdownlintConfig: string): string[] {
	// The file is JSONC; strip // line comments before parsing.
	const stripped = markdownlintConfig.replace(/^\s*\/\/.*$/gm, "");
	const parsed = JSON.parse(stripped) as { ignores?: string[] };
	const ignores = parsed.ignores ?? [];
	return [...ignores, "**/outputs/**"];
}

// ── Pure construction ───────────────────────────────────────────────────────

/**
 * The line index of the first markdown `# H1`, or `-1` when there is none.
 * `#` characters inside fenced code blocks are not headings and are skipped —
 * the single source of truth for "where is the H1" shared by `extractTitle`
 * (reads its text) and `stripFirstH1` (drops the line), so the two can never
 * disagree about fence context. Both CommonMark fence markers are recognized
 * (```` ``` ```` and `~~~`); a fence closes only on the same marker at a length
 * >= the opener, so a `#` inside either kind of block is never read as the H1.
 */
function firstH1Index(lines: readonly string[]): number {
	let fence: { marker: "`" | "~"; length: number } | undefined;
	for (let i = 0; i < lines.length; i++) {
		const trimmed = lines[i].trim();
		const fenceMatch = trimmed.match(/^(`{3,}|~{3,})/);
		if (fence) {
			if (
				fenceMatch &&
				fenceMatch[1][0] === fence.marker &&
				fenceMatch[1].length >= fence.length
			) {
				fence = undefined;
			}
			continue;
		}
		if (fenceMatch) {
			fence = {
				marker: fenceMatch[1][0] as "`" | "~",
				length: fenceMatch[1].length,
			};
			continue;
		}
		if (/^#\s+.+$/.test(trimmed)) return i;
	}
	return -1;
}

/**
 * The title for a source: the text of its first `# H1`, or — when a file has
 * none — its basename without extension. `#` characters inside fenced code
 * blocks are not headings and are skipped.
 */
export function extractTitle(source: string, sourcePath = ""): string {
	const lines = source.split("\n");
	const i = firstH1Index(lines);
	if (i !== -1) return lines[i].trim().replace(/^#\s+/, "").trim();
	return basename(sourcePath).replace(/\.[^.]+$/, "");
}

/**
 * A YAML frontmatter block carrying the title (and, when given, a per-page
 * `editUrl`). The title is always a double-quoted scalar so YAML-significant
 * characters in it (`:`, `#`, quotes) are inert; backslashes and double quotes
 * are escaped. With no `editUrl` the block is title-only (the docs/ case, where
 * the global editLink base already resolves correctly).
 */
export function buildFrontmatter(title: string, editUrl?: string): string {
	const escaped = title.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
	const editLine = editUrl ? `editUrl: "${editUrl}"\n` : "";
	return `---\ntitle: "${escaped}"\n${editLine}---\n`;
}

/** The canonical GitHub "edit this file" URL for a repo-relative source path. */
export function editUrlFor(sourcePath: string): string {
	return `https://github.com/${REPO_SLUG}/edit/main/${sourcePath}`;
}

/**
 * Drop the first `# H1` line (Starlight renders `title` as the page h1, so the
 * body copy would otherwise double it) plus a single blank line immediately
 * after it. A `#` inside a leading fenced code block is not the H1 and is left
 * intact; later H1s and all other content are untouched.
 */
export function stripFirstH1(source: string): string {
	const lines = source.split("\n");
	const i = firstH1Index(lines);
	if (i === -1) return source;
	const drop = lines[i + 1] === "" ? 2 : 1;
	lines.splice(i, drop);
	return lines.join("\n");
}

export function destRelPath(sourcePath: string): string {
	return sourcePath.replace(/^docs\//, "");
}

/** File extensions that render as a site page — links to these become routes. */
const MARKDOWN_EXT = /\.md$/i;

/**
 * Rewrite in-repo links in a gathered file so they resolve on the site:
 *
 * - a relative link to another **gathered** `*.md` → its site route (via
 *   `classify` + `routeSlug`), so cross-doc links stay internal;
 * - a relative link to any other in-repo target → its canonical GitHub URL:
 *   `/tree/main/…` for a directory (trailing slash), `/blob/main/…` otherwise
 *   (code, image, config — and any `.md` that was *not* gathered, e.g. an
 *   excluded tree or a stale/moved path). Routing off the actual gathered set
 *   means a link to an ungathered file degrades to its visible source on GitHub
 *   instead of a silent 404 on a route the site never generated;
 * - absolute URLs (`http(s):`, `//`), anchors (`#…`), and mailto are untouched.
 *
 * `sourcePath` is the repo-relative path of the file being transformed, used to
 * resolve relative targets against the source's directory. `gathered` is the set
 * of repo-relative paths that became site pages (the gather's own file list).
 */
export function rewriteLinks(
	source: string,
	sourcePath: string,
	gathered: ReadonlySet<string>,
): string {
	const srcDir = dirname(sourcePath);
	// Markdown inline links: [text](target) and [text](target "title").
	return source.replace(
		/(\]\()([^)\s]+)(\s+"[^"]*")?(\))/g,
		(match, open, target, title, close) => {
			if (
				/^(?:[a-z][a-z0-9+.-]*:|\/\/|#|mailto:)/i.test(target) ||
				target.startsWith("/")
			) {
				return match;
			}
			// Split off any anchor / query so it rides along to the new target.
			const hashIdx = target.search(/[#?]/);
			const path = hashIdx === -1 ? target : target.slice(0, hashIdx);
			const suffix = hashIdx === -1 ? "" : target.slice(hashIdx);
			const resolved = normalizeRepoPath(join(srcDir, path));
			let rewritten: string;
			if (MARKDOWN_EXT.test(resolved) && gathered.has(resolved)) {
				rewritten = routeSlug(classify(resolved).destRel) + suffix;
			} else {
				// Directory links (trailing slash) need GitHub's `tree` view; every
				// other target — files, and `.md` that was never gathered — is a `blob`.
				const kind = path.endsWith("/") ? "tree" : "blob";
				rewritten = `https://github.com/${REPO_SLUG}/${kind}/main/${resolved}${suffix}`;
			}
			return `${open}${rewritten}${title ?? ""}${close}`;
		},
	);
}

/** Collapse `.`/`..` segments in a POSIX repo path (no leading slash). */
function normalizeRepoPath(p: string): string {
	const out: string[] = [];
	for (const seg of p.split("/")) {
		if (seg === "" || seg === ".") continue;
		if (seg === "..") out.pop();
		else out.push(seg);
	}
	return out.join("/");
}

/**
 * Whole-file transform: inject the derived title + per-page GitHub `editUrl`,
 * strip the duplicated H1, and rewrite in-repo links to site routes / GitHub
 * blobs. The `editUrl` points at the true canonical source (`sourcePath`), so a
 * gathered file mirrored under a different content-root path still edits the
 * right file.
 */
export function transform(
	source: string,
	sourcePath: string,
	gathered: ReadonlySet<string>,
): string {
	return (
		buildFrontmatter(extractTitle(source, sourcePath), editUrlFor(sourcePath)) +
		rewriteLinks(stripFirstH1(source), sourcePath, gathered)
	);
}

/** A gathered doc's landing-page entry: its section, Starlight route, and title. */
export interface DomainEntry {
	/** Section key (a `SECTIONS` entry). */
	domain: string;
	route: string;
	title: string;
}

/**
 * The Starlight route for a content-relative markdown path. Astro content
 * collections slug a page by dropping the `.md`, slugifying EACH path segment
 * with `github-slugger` (lowercase, strip `.` and most punctuation, spaces→`-`),
 * then dropping a trailing `/index` (astro/dist/content/utils.js:271-272). We
 * reuse Astro's own slugger so a route can never drift from where the page
 * actually renders — a plain `.toLowerCase()` kept dots (`compass-0.6`), which
 * 404s against Astro's `compass-06`. Used for the landing-page links, in-page
 * cross-doc link rewrites, and the PR preview comment's changed-page deep-links —
 * all of which must resolve on the deployed site. `apps/eng-docs/package.json`'s
 * `github-slugger` must stay on the same major as astro's transitive copy, or
 * the two slug differently and routes drift again.
 */
export function routeSlug(contentRelPath: string): string {
	return `/${contentRelPath
		.replace(/\.md$/, "")
		.split("/")
		.map((segment) => githubSlug(segment))
		.join("/")
		.replace(/\/index$/, "")}`;
}

/**
 * The root landing page (`src/content/docs/index.md`). Starlight generates no
 * route for the site root or the autogenerated group dirs, so without this the
 * preview comment's root URL 404s. A `splash` page linking to the first page of
 * each populated section (a route guaranteed to exist); an empty section is
 * omitted so no link 404s.
 */
export function buildIndex(entries: readonly DomainEntry[]): string {
	const bySection = new Map(entries.map((e) => [e.domain, e]));
	const links = SECTIONS.filter((s) => bySection.has(s.key))
		.map((s) => {
			const e = bySection.get(s.key) as DomainEntry;
			return `- [${s.label}](${e.route}) — ${e.title}`;
		})
		.join("\n");
	return (
		"---\n" +
		'title: "Compass Engineering Docs"\n' +
		'description: "Public engineering docs for the compass monorepo."\n' +
		"template: splash\n" +
		"---\n\n" +
		"The compass monorepo's reviewed documentation. Browse by section:\n\n" +
		`${links}\n`
	);
}

/**
 * The generated Starlight sidebar (`src/sidebar.generated.ts`), imported by
 * astro.config.mjs. Each populated section becomes a group that autogenerates
 * from its content-root directory; an empty section is omitted so Starlight
 * never errors on a missing directory. Emitted as a typed module so the astro
 * config stays declarative.
 */
export function buildSidebar(sections: readonly string[]): string {
	const present = new Set(sections);
	const groups = SECTIONS.filter((s) => present.has(s.key)).map((s) => ({
		label: s.label,
		items: [{ autogenerate: { directory: s.key } }],
	}));
	return (
		"// Generated by scripts/gather.ts — do not edit.\n" +
		"// The monorepo docsite sidebar, one group per populated nav section\n" +
		"// (SEA-1764). Regenerated on every gather; gitignored like the content mirror.\n" +
		`export const sidebar = ${JSON.stringify(groups, null, "\t")};\n`
	);
}

// ── Execution ────────────────────────────────────────────────────────────────

/**
 * Whether a repo-relative path is excluded from the gather, given the exclusion
 * globs (from `parseExclusions`). A glob supports `**` (any depth) and `*` (one
 * segment). Also excludes the docsite's own tree so it never mirrors itself.
 */
export function isExcluded(
	sourcePath: string,
	exclusions: readonly string[],
): boolean {
	if (sourcePath.startsWith("apps/eng-docs/")) return true;
	return exclusions.some((glob) => globToRegExp(glob).test(sourcePath));
}

/** Compile a simple `**`/`*` path glob to an anchored RegExp. */
function globToRegExp(glob: string): RegExp {
	const re = glob
		.split(/(\*\*\/|\*\*|\*)/)
		.map((part) => {
			if (part === "**/") return "(?:.*/)?";
			if (part === "**") return ".*";
			if (part === "*") return "[^/]*";
			return part.replace(/[.+?^${}()|[\]\\]/g, "\\$&");
		})
		.join("");
	return new RegExp(`^${re}$`);
}

async function main(): Promise<void> {
	// apps/eng-docs/scripts/gather.ts → repo root is three levels up.
	const scriptDir = dirname(Bun.fileURLToPath(import.meta.url));
	const appDir = dirname(scriptDir);
	const repoRoot = dirname(dirname(appDir));
	const contentRoot = join(appDir, "src", "content", "docs");

	// Idempotent rebuild: clear the generated mirror, then repopulate.
	await rm(contentRoot, { recursive: true, force: true });

	// The canonical exclusion set lives in .markdownlint-cli2.jsonc (+ the one
	// the gather always adds); read it so a new excluded dir is declared once.
	const exclusions = parseExclusions(
		await readFile(join(repoRoot, ".markdownlint-cli2.jsonc"), "utf8"),
	);

	// All markdown under the repo, minus node_modules/build output and the
	// exclusion set. Sorted for deterministic per-section "first page" + output.
	const glob = new Glob("**/*.md");
	const rels: string[] = [];
	for await (const rel of glob.scan({
		cwd: repoRoot,
		onlyFiles: true,
		dot: true,
	})) {
		if (rel.includes("node_modules/") || rel.includes("/dist/")) continue;
		// dot:true surfaces .github (wanted) but also VCS/tooling dotdirs — skip those.
		if (/(^|\/)\.(git|astro|direnv|moon|vscode|idea|pagefind)\//.test(rel)) {
			continue;
		}
		if (isExcluded(rel, exclusions)) continue;
		rels.push(rel);
	}
	rels.sort();
	// The set of paths that become site pages — the source of truth for whether
	// a cross-doc `.md` link resolves to a route or falls back to its GitHub source.
	const gathered = new Set(rels);

	let count = 0;
	// First gathered page per section anchors that section's landing-page link.
	const entries: DomainEntry[] = [];
	const sectionsSeen = new Set<string>();
	for (const rel of rels) {
		const source = await Bun.file(join(repoRoot, rel)).text();
		const { section, destRel } = classify(rel);
		const dest = join(contentRoot, destRel);
		await mkdir(dirname(dest), { recursive: true });
		await writeFile(dest, transform(source, rel, gathered));
		if (!sectionsSeen.has(section)) {
			sectionsSeen.add(section);
			entries.push({
				domain: section,
				route: routeSlug(destRel),
				title: extractTitle(source, rel),
			});
		}
		count++;
	}

	// Root landing page: Starlight has no route for `/` or the group dirs, so
	// without this the preview comment's root URL 404s.
	await writeFile(join(contentRoot, "index.md"), buildIndex(entries));
	// Generated sidebar for astro.config.mjs (one group per populated section).
	await writeFile(
		join(appDir, "src", "sidebar.generated.ts"),
		buildSidebar([...sectionsSeen]),
	);
	console.log(
		`gather: wrote ${count} docs + index + sidebar into ${contentRoot}`,
	);
}

if (import.meta.main) {
	await main();
}
