import * as path from "node:path";

export const LEGACY_PI_MODULES_SPECIFIER = "omp-legacy-pi-modules";
const VIRTUAL_NAMESPACE = "omp-legacy-pi-modules-build";
const TYPEBOX_MODULE_KEY = "typebox";
const TYPEBOX_COMPAT_MODULE = "legacy-typebox.ts";
const SKIPPED_WILDCARD_BASENAMES = new Set(["index"]);
const MAIN_THREAD_UNSAFE_WILDCARD_BASENAMES = new Set(["worker-entry"]);

interface BundledPackage {
	readonly name: string;
	readonly identifier: string;
	readonly rootShim: string | null;
}

const BUNDLED_PACKAGES: readonly BundledPackage[] = [
	{
		name: "@oh-my-pi/pi-agent-core",
		identifier: "PiAgentCore",
		rootShim: null,
	},
	{
		name: "@oh-my-pi/pi-ai",
		identifier: "PiAi",
		rootShim: "legacy-pi-ai-shim.ts",
	},
	{
		name: "@oh-my-pi/pi-coding-agent",
		identifier: "PiCodingAgent",
		rootShim: "legacy-pi-coding-agent-shim.ts",
	},
	{ name: "@oh-my-pi/pi-natives", identifier: "PiNatives", rootShim: null },
	{
		name: "@oh-my-pi/pi-tui",
		identifier: "PiTui",
		rootShim: "legacy-pi-tui-shim.ts",
	},
	{ name: "@oh-my-pi/pi-utils", identifier: "PiUtils", rootShim: null },
];

export interface LegacyPiModuleEntry {
	readonly key: string;
	readonly binding: string;
	readonly importSpecifier: string;
}

interface WildcardPattern {
	readonly exportPrefix: string;
	readonly exportSuffix: string;
	readonly sourcePrefix: string;
	readonly sourceSuffix: string;
}

interface PackageManifest {
	readonly name: string;
	readonly exports?: unknown;
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
	if (
		typeof value === "object" &&
		value !== null &&
		!Array.isArray(value) &&
		"import" in value &&
		typeof value.import === "string"
	) {
		return value.import;
	}
	return null;
}

async function readPackageManifest(
	manifestPath: string,
): Promise<PackageManifest | null> {
	let manifest: unknown;
	try {
		manifest = await Bun.file(manifestPath).json();
	} catch (error) {
		if (
			typeof error === "object" &&
			error !== null &&
			"code" in error &&
			error.code === "ENOENT"
		) {
			return null;
		}
		throw error;
	}
	if (
		typeof manifest !== "object" ||
		manifest === null ||
		Array.isArray(manifest) ||
		!("name" in manifest) ||
		typeof manifest.name !== "string"
	) {
		return null;
	}
	return {
		name: manifest.name,
		exports: "exports" in manifest ? manifest.exports : undefined,
	};
}

async function resolvePackageRoot(
	packageName: string,
	fromDirectory: string,
): Promise<string> {
	let manifestPath: string | undefined;
	try {
		manifestPath = Bun.resolveSync(
			`${packageName}/package.json`,
			fromDirectory,
		);
	} catch {
		// Some packages do not export package.json; resolve their entrypoint below.
	}
	if (manifestPath) {
		const manifest = await readPackageManifest(manifestPath);
		if (manifest?.name === packageName) return path.dirname(manifestPath);
	}

	const entrypoint = Bun.resolveSync(packageName, fromDirectory);
	let directory = path.dirname(entrypoint);
	while (true) {
		const manifest = await readPackageManifest(
			path.join(directory, "package.json"),
		);
		if (manifest?.name === packageName) return directory;
		const parent = path.dirname(directory);
		if (parent === directory) break;
		directory = parent;
	}
	throw new Error(
		`Cannot locate installed package root for ${packageName} from ${fromDirectory}`,
	);
}

function isEnoent(error: unknown): boolean {
	return (
		typeof error === "object" &&
		error !== null &&
		"code" in error &&
		error.code === "ENOENT"
	);
}

interface EntryCollector {
	readonly entries: LegacyPiModuleEntry[];
	readonly keys: Set<string>;
	readonly bindings: Set<string>;
}

function addEntry(
	collector: EntryCollector,
	key: string,
	binding: string,
	importSpecifier: string,
): void {
	if (collector.keys.has(key)) return;
	if (collector.bindings.has(binding)) {
		throw new Error(`Duplicate bundled Pi binding ${binding} for ${key}`);
	}
	collector.keys.add(key);
	collector.bindings.add(binding);
	collector.entries.push({ key, binding, importSpecifier });
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function wildcardEntryForMatch(
	match: string,
	pattern: WildcardPattern,
	packageName: string,
	identifier: string,
): LegacyPiModuleEntry | null {
	if (!match.endsWith(pattern.sourceSuffix)) return null;
	const basename = match.slice(0, match.length - pattern.sourceSuffix.length);
	const segments = basename.split("/");
	if (
		segments.some(
			(segment) => segment.startsWith(".") || segment.startsWith("_"),
		)
	)
		return null;
	if (!isSafeWildcardBasename(segments.at(-1) ?? "")) return null;
	const subpath = `${pattern.exportPrefix}${basename}${pattern.exportSuffix}`;
	return {
		key: `${packageName}/${subpath}`,
		binding: bindingForSubpath(identifier, subpath),
		importSpecifier: `${packageName}/${subpath}`,
	};
}

async function addWildcardEntries(
	collector: EntryCollector,
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

	const sourceDirectory = path.join(packageRoot, pattern.sourcePrefix);
	const matches: string[] = [];
	try {
		const glob = new Bun.Glob(`**/*${pattern.sourceSuffix}`);
		for await (const match of glob.scan({
			cwd: sourceDirectory,
			onlyFiles: true,
		})) {
			matches.push(match.split(path.sep).join("/"));
		}
	} catch (error) {
		if (isEnoent(error)) return;
		throw error;
	}
	matches.sort();
	for (const match of matches) {
		const entry = wildcardEntryForMatch(
			match,
			pattern,
			packageName,
			identifier,
		);
		if (entry)
			addEntry(collector, entry.key, entry.binding, entry.importSpecifier);
	}
}

async function addPackageEntries(
	collector: EntryCollector,
	pkg: BundledPackage,
	codingAgentRoot: string,
): Promise<void> {
	const packageRoot = await resolvePackageRoot(pkg.name, codingAgentRoot);
	const manifestPath = path.join(packageRoot, "package.json");
	const manifest = await readPackageManifest(manifestPath);
	if (!manifest)
		throw new Error(`Bundled Pi package manifest has no name: ${manifestPath}`);
	const exportsField = isRecord(manifest.exports) ? manifest.exports : {};
	const rootSpecifier = pkg.rootShim
		? path.join(codingAgentRoot, "src", "extensibility", pkg.rootShim)
		: manifest.name;
	addEntry(collector, manifest.name, `bundled${pkg.identifier}`, rootSpecifier);

	for (const [exportKey] of Object.entries(exportsField)) {
		if (
			!exportKey.startsWith("./") ||
			exportKey === "." ||
			exportKey.includes("*")
		)
			continue;
		const subpath = exportKey.slice(2);
		addEntry(
			collector,
			`${manifest.name}/${subpath}`,
			bindingForSubpath(pkg.identifier, subpath),
			`${manifest.name}/${subpath}`,
		);
	}
	for (const [exportKey, exportValue] of Object.entries(exportsField)) {
		await addWildcardEntries(
			collector,
			packageRoot,
			manifest.name,
			pkg.identifier,
			exportKey,
			exportValue,
		);
	}
}

/** Derive the bundled legacy Pi surface from the installed packages' exports. */
export async function collectLegacyPiModuleEntries(
	packageDirectory = path.resolve(import.meta.dir, ".."),
): Promise<LegacyPiModuleEntry[]> {
	const collector: EntryCollector = {
		entries: [],
		keys: new Set(),
		bindings: new Set(),
	};
	const codingAgentRoot = await resolvePackageRoot(
		"@oh-my-pi/pi-coding-agent",
		packageDirectory,
	);
	for (const pkg of BUNDLED_PACKAGES)
		await addPackageEntries(collector, pkg, codingAgentRoot);
	addEntry(
		collector,
		TYPEBOX_MODULE_KEY,
		"bundledTypeBoxShim",
		path.join(codingAgentRoot, "src", "extensibility", TYPEBOX_COMPAT_MODULE),
	);
	return collector.entries;
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
	const packageDirectory = path.resolve(import.meta.dir, "..");
	const codingAgentDirectory = path.dirname(
		Bun.resolveSync("@oh-my-pi/pi-coding-agent", packageDirectory),
	);
	return {
		name: "compass:legacy-pi-modules",
		setup(build) {
			build.onResolve({ filter: /^@oh-my-pi\/[^/]+(?:\/.*)?$/ }, (args) => {
				for (const directory of [
					path.dirname(args.importer),
					packageDirectory,
					codingAgentDirectory,
					path.join(codingAgentDirectory, "src"),
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
				resolveDir: codingAgentDirectory,
			}));
		},
	};
}
