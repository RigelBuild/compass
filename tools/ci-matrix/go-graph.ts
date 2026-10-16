import { relative, sep } from "node:path";
import type { GoGraph, GoSuite } from "./go-impact.ts";
import { E2E_BUILT_BINARIES, E2E_ROOTS } from "./go-impact.ts";

interface GoListPackage {
	readonly ImportPath: string;
	readonly Dir: string;
	readonly GoFiles?: readonly string[];
	readonly CgoFiles?: readonly string[];
	readonly TestGoFiles?: readonly string[];
	readonly XTestGoFiles?: readonly string[];
	readonly Error?: { readonly Err?: string };
}

function isStringArray(value: unknown): value is readonly string[] {
	return (
		Array.isArray(value) && value.every((item) => typeof item === "string")
	);
}

function isGoListPackage(value: unknown): value is GoListPackage {
	return (
		typeof value === "object" &&
		value !== null &&
		"ImportPath" in value &&
		typeof value.ImportPath === "string" &&
		"Dir" in value &&
		typeof value.Dir === "string" &&
		(!("GoFiles" in value) || isStringArray(value.GoFiles)) &&
		(!("CgoFiles" in value) || isStringArray(value.CgoFiles)) &&
		(!("TestGoFiles" in value) || isStringArray(value.TestGoFiles)) &&
		(!("XTestGoFiles" in value) || isStringArray(value.XTestGoFiles)) &&
		(!("Error" in value) ||
			value.Error === undefined ||
			(typeof value.Error === "object" &&
				value.Error !== null &&
				(!("Err" in value.Error) || typeof value.Error.Err === "string")))
	);
}

type JsonScanState = { depth: number; inString: boolean; escaped: boolean };

function consumeJsonCharacter(
	character: string,
	state: JsonScanState,
): boolean {
	if (state.inString) {
		if (state.escaped) state.escaped = false;
		else if (character === "\\") state.escaped = true;
		else if (character === '"') state.inString = false;
		return false;
	}
	if (character === '"') state.inString = true;
	else if (character === "{") state.depth += 1;
	else if (character === "}") state.depth -= 1;
	return state.depth === 0;
}

function packageEnd(source: string, from: number): number {
	const state: JsonScanState = { depth: 1, inString: false, escaped: false };
	for (let index = from + 1; index < source.length; index += 1) {
		const character = source[index];
		if (character !== undefined && consumeJsonCharacter(character, state))
			return index;
	}
	throw new Error("go list returned an incomplete JSON package object");
}

function parsePackageStream(source: string): GoListPackage[] {
	const packages: GoListPackage[] = [];
	for (let index = 0; index < source.length; index += 1) {
		if (source[index] !== "{") continue;
		const end = packageEnd(source, index);
		const parsed: unknown = JSON.parse(source.slice(index, end + 1));
		if (!isGoListPackage(parsed)) {
			throw new Error(
				`go list returned an invalid package: ${source.slice(index, end + 1)}`,
			);
		}
		packages.push(parsed);
		index = end;
	}
	if (packages.length === 0) {
		throw new Error(
			"go list returned an incomplete or empty JSON package stream",
		);
	}
	return packages;
}

const commandEnv = {
	...process.env,
	GOFLAGS: "-mod=mod",
	GOPROXY: "off",
	CGO_ENABLED: "1",
};

function relativeDir(goDir: string, pkg: GoListPackage): string {
	const dir = relative(goDir, pkg.Dir).split(sep).join("/");
	return dir === "" ? "." : dir;
}

async function listPackages(
	goDir: string,
	args: readonly string[],
): Promise<GoListPackage[]> {
	const child = Bun.spawn(["go", "list", "-e", "-json", ...args], {
		cwd: goDir,
		env: commandEnv,
		stdout: "pipe",
		stderr: "pipe",
	});
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(child.stdout).text(),
		new Response(child.stderr).text(),
		child.exited,
	]);
	if (exitCode !== 0 && stdout.trim() === "") {
		throw new Error(`go list ${args.join(" ")} failed: ${stderr.trim()}`);
	}
	return parsePackageStream(stdout);
}

function testFiles(pkg: GoListPackage): string[] {
	return [...(pkg.TestGoFiles ?? []), ...(pkg.XTestGoFiles ?? [])].sort();
}

function filesByDirectory(
	packages: readonly GoListPackage[],
	goDir: string,
): Map<string, GoListPackage> {
	return new Map(packages.map((pkg) => [relativeDir(goDir, pkg), pkg]));
}

function changedTestRoots(
	untagged: ReadonlyMap<string, GoListPackage>,
	tagged: ReadonlyMap<string, GoListPackage>,
): string[] {
	return [...new Set([...untagged.keys(), ...tagged.keys()])]
		.filter((root) => {
			const plainFiles = untagged.get(root);
			const taggedFiles = tagged.get(root);
			const plainTests = plainFiles === undefined ? [] : testFiles(plainFiles);
			const taggedTests =
				taggedFiles === undefined ? [] : testFiles(taggedFiles);
			return JSON.stringify(plainTests) !== JSON.stringify(taggedTests);
		})
		.sort();
}

function erroredPackageRoots(
	untagged: ReadonlyMap<string, GoListPackage>,
	tagged: ReadonlyMap<string, GoListPackage>,
): string[] {
	return [...new Set([...untagged.keys(), ...tagged.keys()])]
		.filter((root) => {
			const plain = untagged.get(root)?.Error?.Err;
			const withTag = tagged.get(root)?.Error?.Err;
			return (
				(plain !== undefined && plain !== "") ||
				(withTag !== undefined && withTag !== "")
			);
		})
		.sort();
}

async function dependencyClosure(
	goDir: string,
	root: string,
	tag: string,
	firstPartyPackages: ReadonlySet<string>,
): Promise<{ directories: string[]; errored: boolean }> {
	const packages = await listPackages(goDir, [
		"-tags",
		tag,
		"-test",
		"-deps",
		`./${root}`,
	]);
	const directories: string[] = [];
	let errored = false;
	for (const pkg of packages) {
		if (pkg.Error?.Err !== undefined && pkg.Error.Err !== "") errored = true;
		if (firstPartyPackages.has(pkg.ImportPath))
			directories.push(relativeDir(goDir, pkg));
	}
	return { directories: [...new Set(directories)].sort(), errored };
}

function rootsWithListErrors(
	roots: Record<GoSuite, string[]>,
	untagged: ReadonlyMap<string, GoListPackage>,
	tagged: ReadonlyMap<GoSuite, ReadonlyMap<string, GoListPackage>>,
): Record<GoSuite, string[]> {
	const taggedErrors = (suite: GoSuite): string[] =>
		erroredPackageRoots(untagged, tagged.get(suite) ?? new Map());
	return {
		pgtest: taggedErrors("pgtest").filter((root) =>
			roots.pgtest.includes(root),
		),
		microvm: taggedErrors("microvm").filter((root) =>
			roots.microvm.includes(root),
		),
		e2e: taggedErrors("e2e").filter((root) => E2E_ROOTS.includes(root)),
	};
}
type SuiteClosures = {
	closure: Record<GoSuite, Record<string, string[]>>;
	erroredRoots: Record<GoSuite, string[]>;
};

async function suiteClosureForRoots(
	goDir: string,
	suite: "pgtest" | "microvm",
	roots: readonly string[],
	packageNames: ReadonlySet<string>,
	closure: Record<GoSuite, Record<string, string[]>>,
	erroredRoots: Record<GoSuite, string[]>,
): Promise<void> {
	for (const root of roots) {
		const result = await dependencyClosure(goDir, root, suite, packageNames);
		if (result.errored && !erroredRoots[suite].includes(root)) {
			erroredRoots[suite].push(root);
		}
		closure[suite][root] = result.directories;
	}
}

async function loadE2eClosures(
	goDir: string,
	packageNames: ReadonlySet<string>,
	closure: Record<GoSuite, Record<string, string[]>>,
	erroredRoots: Record<GoSuite, string[]>,
): Promise<void> {
	for (const root of E2E_ROOTS) {
		const result = await dependencyClosure(goDir, root, "podman", packageNames);
		if (result.errored) erroredRoots.e2e.push(root);
		closure.e2e[root] = result.directories;
	}
	for (const binary of E2E_BUILT_BINARIES) {
		const result = await dependencyClosure(
			goDir,
			binary,
			"podman",
			packageNames,
		);
		if (result.errored) erroredRoots.e2e.push(...E2E_ROOTS);
		for (const target of E2E_ROOTS) {
			closure.e2e[target] = [
				...new Set([...(closure.e2e[target] ?? []), ...result.directories]),
			].sort();
		}
	}
}

async function loadSuiteClosures(
	goDir: string,
	roots: Record<GoSuite, string[]>,
	untagged: ReadonlyMap<string, GoListPackage>,
	tagged: ReadonlyMap<GoSuite, ReadonlyMap<string, GoListPackage>>,
	packageNames: ReadonlySet<string>,
): Promise<SuiteClosures> {
	const closure: Record<GoSuite, Record<string, string[]>> = {
		pgtest: {},
		microvm: {},
		e2e: {},
	};
	const erroredRoots = rootsWithListErrors(roots, untagged, tagged);
	await suiteClosureForRoots(
		goDir,
		"pgtest",
		roots.pgtest,
		packageNames,
		closure,
		erroredRoots,
	);
	await suiteClosureForRoots(
		goDir,
		"microvm",
		roots.microvm,
		packageNames,
		closure,
		erroredRoots,
	);
	await loadE2eClosures(goDir, packageNames, closure, erroredRoots);
	return { closure, erroredRoots };
}

export async function loadGoGraph(goDir: string): Promise<GoGraph> {
	const untaggedPackages = await listPackages(goDir, ["./..."]);
	const untagged = filesByDirectory(untaggedPackages, goDir);
	const tagged = new Map<GoSuite, Map<string, GoListPackage>>();
	for (const suite of ["pgtest", "microvm", "e2e"] as const) {
		const tag = suite === "e2e" ? "podman" : suite;
		tagged.set(
			suite,
			filesByDirectory(
				await listPackages(goDir, ["-tags", tag, "./..."]),
				goDir,
			),
		);
	}
	const roots: Record<GoSuite, string[]> = {
		pgtest: changedTestRoots(untagged, tagged.get("pgtest") ?? new Map()),
		microvm: changedTestRoots(untagged, tagged.get("microvm") ?? new Map()),
		e2e: [...E2E_ROOTS].sort(),
	};
	const firstPartyPackages = new Set([
		...untaggedPackages.map((pkg) => pkg.ImportPath),
		...[...tagged.values()].flatMap((packages) =>
			[...packages.values()].map((pkg) => pkg.ImportPath),
		),
	]);
	const { closure, erroredRoots } = await loadSuiteClosures(
		goDir,
		roots,
		untagged,
		tagged,
		firstPartyPackages,
	);
	return {
		packages: [...untagged.keys()].sort(),
		closure,
		erroredRoots: {
			pgtest: [...new Set(erroredRoots.pgtest)].sort(),
			microvm: [...new Set(erroredRoots.microvm)].sort(),
			e2e: [...new Set(erroredRoots.e2e)].sort(),
		},
	};
}
