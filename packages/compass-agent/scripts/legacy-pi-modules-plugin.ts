// Copy of upstream's legacy-pi-virtual-module.ts, with package-root resolution only.
// Remove when upstream exposes a package-root option.
import * as path from "node:path";

export const LEGACY_PI_MODULES_SPECIFIER = "omp-legacy-pi-modules";
const VIRTUAL_NAMESPACE = "omp-legacy-pi-modules-build";

interface BundledPackage {
	readonly dir: string;
	readonly name: string;
	readonly identifier: string;
	readonly rootShim: string | null;
}

const BUNDLED_PACKAGES: readonly BundledPackage[] = [
	{
		dir: "agent",
		name: "@oh-my-pi/pi-agent-core",
		identifier: "PiAgentCore",
		rootShim: null,
	},
	{
		dir: "ai",
		name: "@oh-my-pi/pi-ai",
		identifier: "PiAi",
		rootShim: "legacy-pi-ai-shim.ts",
	},
	{
		dir: "coding-agent",
		name: "@oh-my-pi/pi-coding-agent",
		identifier: "PiCodingAgent",
		rootShim: "legacy-pi-coding-agent-shim.ts",
	},
	{
		dir: "natives",
		name: "@oh-my-pi/pi-natives",
		identifier: "PiNatives",
		rootShim: null,
	},
	{
		dir: "tui",
		name: "@oh-my-pi/pi-tui",
		identifier: "PiTui",
		rootShim: "legacy-pi-tui-shim.ts",
	},
	{
		dir: "utils",
		name: "@oh-my-pi/pi-utils",
		identifier: "PiUtils",
		rootShim: null,
	},
];

const TYPEBOX_MODULE_KEY = "typebox";
const TYPEBOX_COMPAT_MODULE = "legacy-typebox.ts";
const SKIPPED_WILDCARD_BASENAMES = new Set(["index"]);
const MAIN_THREAD_UNSAFE_WILDCARD_BASENAMES = new Set(["worker-entry"]);
const codingAgentDir = path.dirname(
	Bun.resolveSync("@oh-my-pi/pi-coding-agent/package.json", import.meta.dir),
);

/** One namespace module the binary must retain for legacy Pi extension imports. */
export interface LegacyPiModuleEntry {
	/** Canonical import key exposed to extensions. */
	readonly key: string;
	/** Unique identifier used by the virtual module's generated import. */
	readonly binding: string;
	/** Package or absolute source specifier compiled into the binary. */
	readonly importSpecifier: string;
}

interface WildcardPattern {
	readonly exportPrefix: string;
	readonly exportSuffix: string;
	readonly sourcePrefix: string;
	readonly sourceSuffix: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function bindingForSubpath(identifier: string, subpath: string): string {
	const segments = subpath
		.split("/")
		.filter(Boolean)
		.map((segment) =>
			segment
				.split(/[-_]/)
				.filter(Boolean)
				.map((part) => part.charAt(0).toUpperCase() + part.slice(1))
				.join(""),
		);
	return `bundled${identifier}${segments.join("")}`;
}

function isSafeWildcardBasename(basename: string): boolean {
	if (!basename || basename.startsWith(".") || basename.startsWith("_"))
		return false;
	if (SKIPPED_WILDCARD_BASENAMES.has(basename)) return false;
	if (MAIN_THREAD_UNSAFE_WILDCARD_BASENAMES.has(basename)) return false;
	return !/\.(test|spec|d|generated|bench)$/.test(basename);
}

function parseWildcardPattern(
	exportKey: string,
	sourcePattern: string,
): WildcardPattern | null {
	const exportStar = exportKey.indexOf("*");
	const sourceStar = sourcePattern.indexOf("*");
	if (exportStar === -1 || sourceStar === -1) return null;
	if (exportKey.indexOf("*", exportStar + 1) !== -1) return null;
	if (sourcePattern.indexOf("*", sourceStar + 1) !== -1) return null;
	if (!sourcePattern.startsWith("./")) return null;
	return {
		exportPrefix: exportKey.slice(2, exportStar),
		exportSuffix: exportKey.slice(exportStar + 1),
		sourcePrefix: sourcePattern.slice(2, sourceStar),
		sourceSuffix: sourcePattern.slice(sourceStar + 1),
	};
}

function exportImportTarget(value: unknown): string | null {
	if (typeof value === "string") return value;
	if (isRecord(value) && typeof value.import === "string") return value.import;
	return null;
}

function shimSpecifier(file: string): string {
	return path.join(codingAgentDir, "src", "extensibility", file);
}

function isExportedWildcardMatch(match: string, sourceSuffix: string): boolean {
	if (!match.endsWith(sourceSuffix)) return false;
	const basename = match.slice(0, match.length - sourceSuffix.length);
	const segments = basename.split("/");
	if (
		segments.some(
			(segment) => segment.startsWith(".") || segment.startsWith("_"),
		)
	) {
		return false;
	}
	return isSafeWildcardBasename(segments.at(-1) ?? "");
}

function addEntry(
	entries: LegacyPiModuleEntry[],
	seenKeys: Set<string>,
	seenBindings: Set<string>,
	key: string,
	binding: string,
	importSpecifier: string,
): void {
	if (seenKeys.has(key)) return;
	if (seenBindings.has(binding)) {
		throw new Error(`Duplicate bundled Pi binding ${binding} for ${key}`);
	}
	seenKeys.add(key);
	seenBindings.add(binding);
	entries.push({ key, binding, importSpecifier });
}

async function addWildcardEntries(
	entries: LegacyPiModuleEntry[],
	seenKeys: Set<string>,
	seenBindings: Set<string>,
	packageRoot: string,
	packageName: string,
	identifier: string,
	exportKey: string,
	exportValue: unknown,
): Promise<void> {
	if (
		!exportKey.startsWith("./") ||
		exportKey === "." ||
		!exportKey.includes("*")
	)
		return;
	const sourcePattern = exportImportTarget(exportValue);
	if (!sourcePattern) return;
	const pattern = parseWildcardPattern(exportKey, sourcePattern);
	if (
		!pattern ||
		!/\.(ts|tsx|mts|cts|js|mjs|cjs|jsx)$/.test(pattern.sourceSuffix)
	)
		return;
	if (pattern.exportPrefix === "" || pattern.exportPrefix === "/") return;

	const sourceDir = path.join(packageRoot, pattern.sourcePrefix);
	const glob = new Bun.Glob(`**/*${pattern.sourceSuffix}`);
	const matches: string[] = [];
	try {
		for await (const match of glob.scan({ cwd: sourceDir, onlyFiles: true })) {
			matches.push(match.split(path.sep).join("/"));
		}
	} catch (error) {
		if (
			typeof error === "object" &&
			error !== null &&
			"code" in error &&
			error.code === "ENOENT"
		)
			return;
		throw error;
	}
	matches.sort();
	for (const match of matches) {
		if (!isExportedWildcardMatch(match, pattern.sourceSuffix)) continue;
		const basename = match.slice(0, match.length - pattern.sourceSuffix.length);
		const subpath = `${pattern.exportPrefix}${basename}${pattern.exportSuffix}`;
		const key = `${packageName}/${subpath}`;
		addEntry(
			entries,
			seenKeys,
			seenBindings,
			key,
			bindingForSubpath(identifier, subpath),
			key,
		);
	}
}

async function addPackageEntries(
	entries: LegacyPiModuleEntry[],
	seenKeys: Set<string>,
	seenBindings: Set<string>,
	pkg: BundledPackage,
): Promise<void> {
	const packageRoot = path.dirname(
		Bun.resolveSync(`${pkg.name}/package.json`, codingAgentDir),
	);
	const manifestPath = path.join(packageRoot, "package.json");
	const manifest: unknown = await Bun.file(manifestPath).json();
	if (!isRecord(manifest) || typeof manifest.name !== "string") {
		throw new Error(`Bundled Pi package manifest has no name: ${manifestPath}`);
	}
	const exportsField = isRecord(manifest.exports) ? manifest.exports : {};
	const rootSpecifier = pkg.rootShim
		? shimSpecifier(pkg.rootShim)
		: manifest.name;
	addEntry(
		entries,
		seenKeys,
		seenBindings,
		manifest.name,
		`bundled${pkg.identifier}`,
		rootSpecifier,
	);

	for (const exportKey in exportsField) {
		if (
			!exportKey.startsWith("./") ||
			exportKey === "." ||
			exportKey.includes("*")
		)
			continue;
		const subpath = exportKey.slice(2);
		const key = `${manifest.name}/${subpath}`;
		addEntry(
			entries,
			seenKeys,
			seenBindings,
			key,
			bindingForSubpath(pkg.identifier, subpath),
			key,
		);
	}
	for (const exportKey in exportsField) {
		await addWildcardEntries(
			entries,
			seenKeys,
			seenBindings,
			packageRoot,
			manifest.name,
			pkg.identifier,
			exportKey,
			exportsField[exportKey],
		);
	}
}

/** Derive the bundled legacy Pi surface from package exports. */
export async function collectLegacyPiModuleEntries(): Promise<
	LegacyPiModuleEntry[]
> {
	const entries: LegacyPiModuleEntry[] = [];
	const seenKeys = new Set<string>();
	const seenBindings = new Set<string>();
	for (const pkg of BUNDLED_PACKAGES) {
		await addPackageEntries(entries, seenKeys, seenBindings, pkg);
	}
	addEntry(
		entries,
		seenKeys,
		seenBindings,
		TYPEBOX_MODULE_KEY,
		"bundledTypeBoxShim",
		shimSpecifier(TYPEBOX_COMPAT_MODULE),
	);
	return entries;
}

export function renderLegacyPiVirtualModule(
	entries: readonly LegacyPiModuleEntry[],
): string {
	const loaders = entries.map(
		(entry) =>
			`const ${entry.binding} = () => import(${JSON.stringify(entry.importSpecifier)});`,
	);
	const modules = entries.map(
		(entry) => `\t${JSON.stringify(entry.key)}: ${entry.binding},`,
	);
	return [
		...loaders,
		"",
		"export const BUNDLED_PI_MODULE_LOADERS = {",
		...modules,
		"};",
		"",
	].join("\n");
}

export async function createLegacyPiModulesPlugin(): Promise<Bun.BunPlugin> {
	const source = renderLegacyPiVirtualModule(
		await collectLegacyPiModuleEntries(),
	);
	return {
		name: "omp:legacy-pi-modules",
		setup(build) {
			build.onResolve({ filter: /^@oh-my-pi\/[^/]+(?:\/.*)?$/ }, (args) => {
				for (const directory of [
					path.dirname(args.importer),
					path.resolve(import.meta.dir, ".."),
					codingAgentDir,
				]) {
					try {
						return { path: Bun.resolveSync(args.path, directory) };
					} catch {}
				}
			});
			build.onResolve({ filter: /^omp-legacy-pi-modules$/ }, () => ({
				path: LEGACY_PI_MODULES_SPECIFIER,
				namespace: VIRTUAL_NAMESPACE,
			}));
			build.onLoad({ filter: /.*/, namespace: VIRTUAL_NAMESPACE }, () => ({
				contents: source,
				loader: "ts",
				resolveDir: codingAgentDir,
			}));
		},
	};
}
