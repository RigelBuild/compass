export interface ClosureTarget {
	label: string;
	dir: string;
	lock: string;
	triggers: readonly string[];
	closure:
		| { kind: "build"; attr: string }
		| { kind: "container"; name: string };
}

export const CLOSURE_TARGETS: readonly ClosureTarget[] = [
	{
		label: "dev shell",
		dir: ".",
		lock: "devenv.lock",
		triggers: ["tools/toolchain/versions/*.nix"],
		closure: { kind: "build", attr: "packageDiff.closure" },
	},
	{
		label: "agent image",
		dir: "agent-image",
		lock: "agent-image/devenv.lock",
		triggers: ["tools/toolchain/versions/bun.nix"],
		closure: { kind: "container", name: "agent" },
	},
];

function matchesTrigger(path: string, trigger: string): boolean {
	if (!trigger.includes("*")) return path === trigger;
	const expression = new RegExp(
		`^${trigger
			.split("*")
			.map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
			.join(".*")}$`,
	);
	return expression.test(path);
}

export function targetsFor(changedFiles: readonly string[]): ClosureTarget[] {
	return CLOSURE_TARGETS.filter((target) =>
		changedFiles.some(
			(file) =>
				file === target.lock ||
				target.triggers.some((trigger) => matchesTrigger(file, trigger)),
		),
	);
}

export interface NameVersion {
	name: string;
	version: string;
}

const OUTPUT_SUFFIX = /-(?:bin|dev|lib|out|man|doc)$/;

export function splitDrvName(storeName: string): NameVersion {
	const nameWithoutHash = storeName.replace(/^\/nix\/store\/[0-9a-z]{32}-/, "");
	const versionSeparator = /-(?=[^A-Za-z])/.exec(nameWithoutHash);
	if (!versionSeparator || versionSeparator.index === undefined) {
		return { name: nameWithoutHash, version: "" };
	}
	const name = nameWithoutHash.slice(0, versionSeparator.index);
	const version = nameWithoutHash
		.slice(versionSeparator.index + 1)
		.replace(OUTPUT_SUFFIX, "");
	if (name === "" || version === "") {
		throw new Error(`Invalid Nix store name: ${JSON.stringify(storeName)}`);
	}
	return { name, version };
}

export function packageVersions(storeNames: readonly string[]): NameVersion[] {
	return storeNames.map((storeName) => splitDrvName(storeName));
}

function isStringArray(value: unknown): value is string[] {
	return (
		Array.isArray(value) && value.every((entry) => typeof entry === "string")
	);
}

export function parseDevenvEval(json: string, attr: string): string[] {
	let value: unknown;
	try {
		value = JSON.parse(json);
	} catch (error) {
		throw new Error(`devenv eval output is not valid JSON: ${String(error)}`);
	}
	if (isStringArray(value)) return value;
	if (isRecord(value)) {
		const result = value[attr];
		if (isStringArray(result)) return result;
	}
	throw new Error(`devenv eval output must contain a string array at ${attr}.`);
}

export function parseDevenvBuild(json: string, attr: string): string {
	let value: unknown;
	try {
		value = JSON.parse(json);
	} catch (error) {
		throw new Error(`devenv build output is not valid JSON: ${String(error)}`);
	}
	const path = isRecord(value) ? value[attr] : undefined;
	if (typeof path !== "string" || !path.startsWith("/nix/store/")) {
		throw new Error(
			`devenv build output must map ${attr} to a Nix store path.`,
		);
	}
	return path;
}

function isErrorCode(value: string): value is (typeof ERROR_CODES)[number] {
	return ERROR_CODES.some((code) => code === value);
}

export function parsePathInfo(json: string): string[] {
	let value: unknown;
	try {
		value = JSON.parse(json);
	} catch (error) {
		throw new Error(`nix path-info output is not valid JSON: ${String(error)}`);
	}
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new Error(
			"nix path-info output must be a JSON object keyed by store path.",
		);
	}
	const paths = Object.keys(value);
	if (!paths.every((path) => path.startsWith("/nix/store/"))) {
		throw new Error("nix path-info output contains a non-store path key.");
	}
	return paths;
}

export function parsePathInfoVersions(json: string): NameVersion[] {
	return parsePathInfo(json).map(splitDrvName);
}

export interface PackageChange {
	name: string;
	from: string[];
	to: string[];
}

export function diffVersions(
	base: readonly NameVersion[],
	head: readonly NameVersion[],
): PackageChange[] {
	const versionsByName = (entries: readonly NameVersion[]) => {
		const result = new Map<string, Set<string>>();
		for (const entry of entries) {
			if (entry.version === "") continue;
			const versions = result.get(entry.name) ?? new Set<string>();
			versions.add(entry.version);
			result.set(entry.name, versions);
		}
		return result;
	};
	const baseVersions = versionsByName(base);
	const headVersions = versionsByName(head);
	const names = [
		...new Set([...baseVersions.keys(), ...headVersions.keys()]),
	].sort();
	const changes: PackageChange[] = [];
	for (const name of names) {
		const from = [...(baseVersions.get(name) ?? [])].sort();
		const to = [...(headVersions.get(name) ?? [])].sort();
		if (from.length === 0 && to.length === 0) continue;
		if (
			from.length === to.length &&
			from.every((version, index) => version === to[index])
		) {
			continue;
		}
		changes.push({ name, from, to });
	}
	return changes;
}

export const ERROR_CODES = [
	"eval-failed",
	"build-failed",
	"path-info-failed",
] as const;

export type TargetResult =
	| { label: string; direct: PackageChange[]; transitive: PackageChange[] }
	| { label: string; error: (typeof ERROR_CODES)[number] };

export interface DiffArtifact {
	headSha: string;
	skip: boolean;
	results: TargetResult[];
}

export const MAX_ARTIFACT_BYTES = 1_048_576;
// Nix store-name characters; as list-item text they cannot form HTML, links or headings.
const SAFE_ARTIFACT_TEXT = /^[A-Za-z0-9._+?=-]{1,128}$/;
const TARGET_LABELS: Readonly<Record<string, true>> = {
	"dev shell": true,
	"agent image": true,
};

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function hasOnlyKeys(
	value: Record<string, unknown>,
	expected: readonly string[],
): boolean {
	return (
		Object.keys(value).length === expected.length &&
		expected.every((key) => Object.hasOwn(value, key))
	);
}

function parseChanges(value: unknown, field: string): PackageChange[] {
	if (!Array.isArray(value))
		throw new Error(`Artifact ${field} must be an array.`);
	const changes: PackageChange[] = [];
	for (const [index, entry] of value.entries()) {
		if (!isRecord(entry) || !hasOnlyKeys(entry, ["name", "from", "to"])) {
			throw new Error(`Artifact ${field}[${index}] has an invalid shape.`);
		}
		if (
			typeof entry.name !== "string" ||
			!SAFE_ARTIFACT_TEXT.test(entry.name)
		) {
			throw new Error(`Artifact ${field}[${index}] has an invalid name.`);
		}
		const from: unknown = entry.from;
		const to: unknown = entry.to;
		if (
			!isStringArray(from) ||
			!from.every((version) => SAFE_ARTIFACT_TEXT.test(version))
		) {
			throw new Error(
				`Artifact ${field}[${index}].from has an invalid version.`,
			);
		}
		if (
			!isStringArray(to) ||
			!to.every((version) => SAFE_ARTIFACT_TEXT.test(version))
		) {
			throw new Error(`Artifact ${field}[${index}].to has an invalid version.`);
		}
		changes.push({ name: entry.name, from, to });
	}
	return changes;
}

function parseTargetResult(entry: unknown, index: number): TargetResult {
	if (
		!isRecord(entry) ||
		typeof entry.label !== "string" ||
		!Object.hasOwn(TARGET_LABELS, entry.label)
	) {
		throw new Error(`Artifact results[${index}] has an unknown label.`);
	}
	if ("error" in entry) {
		if (
			!hasOnlyKeys(entry, ["label", "error"]) ||
			typeof entry.error !== "string" ||
			!isErrorCode(entry.error)
		) {
			throw new Error(
				`Artifact results[${index}] has an unknown error code or invalid shape.`,
			);
		}
		const error = entry.error;
		if (!isErrorCode(error)) throw new Error("Artifact error code is invalid.");
		return { label: entry.label, error };
	}
	if (!hasOnlyKeys(entry, ["label", "direct", "transitive"])) {
		throw new Error(`Artifact results[${index}] has an invalid shape.`);
	}
	return {
		label: entry.label,
		direct: parseChanges(entry.direct, `results[${index}].direct`),
		transitive: parseChanges(entry.transitive, `results[${index}].transitive`),
	};
}

function parseArtifactResults(value: unknown): TargetResult[] {
	if (!Array.isArray(value))
		throw new Error("Artifact results must be an array.");
	const results = value.map((entry, index) => parseTargetResult(entry, index));
	if (new Set(results.map((result) => result.label)).size !== results.length) {
		throw new Error("Artifact contains duplicate target labels.");
	}
	return results;
}

export function parseArtifact(raw: string): DiffArtifact {
	if (new TextEncoder().encode(raw).byteLength > MAX_ARTIFACT_BYTES) {
		throw new Error(`Artifact exceeds ${MAX_ARTIFACT_BYTES} bytes.`);
	}
	let value: unknown;
	try {
		value = JSON.parse(raw);
	} catch (error) {
		throw new Error(`Artifact is not valid JSON: ${String(error)}`);
	}
	if (!isRecord(value) || !hasOnlyKeys(value, ["headSha", "skip", "results"])) {
		throw new Error("Artifact has an invalid top-level shape.");
	}
	if (
		typeof value.headSha !== "string" ||
		!/^[a-fA-F0-9]{40}$/.test(value.headSha)
	) {
		throw new Error("Artifact headSha must be a 40-character hexadecimal SHA.");
	}
	if (typeof value.skip !== "boolean") {
		throw new Error("Artifact skip must be a boolean.");
	}
	const results = parseArtifactResults(value.results);
	if (value.skip && results.length !== 0) {
		throw new Error("A skipped artifact must not contain target results.");
	}
	return { headSha: value.headSha, skip: value.skip, results };
}

export const REVIEWABLE_MARKER = "<!-- Reviewable:start -->";
export const REVIEWABLE_CUT_VERIFIED_AT = "44.155.7";
const MAX_BODY_CHARS = 65_536;

function renderChange(change: PackageChange): string {
	const from =
		change.from.length === 0 ? "(not present)" : change.from.join(", ");
	const to = change.to.length === 0 ? "(not present)" : change.to.join(", ");
	return `- ${change.name}: ${from} → ${to}`;
}

function renderClosure(
	changes: readonly PackageChange[],
	kept: number,
	maxChars: number,
): string[] {
	const lines = [
		`<details><summary>Transitive closure: ${changes.length} changes</summary>`,
		"",
	];
	if (changes.length === 0) {
		lines.push("No package version changes.");
	} else {
		lines.push(...changes.slice(0, kept).map(renderChange));
		const dropped = changes.length - kept;
		if (dropped > 0) {
			lines.push(
				"",
				`${dropped} lines omitted to fit the ${maxChars}-character body limit.`,
			);
		}
	}
	lines.push("", "</details>");
	return lines;
}

function renderTarget(
	result: TargetResult,
	runUrl: string,
	keptClosures: Readonly<Record<string, number>>,
	maxChars: number,
): string[] {
	const lines = [`### ${result.label}`];
	if ("error" in result) {
		lines.push(
			`Diff unavailable (\`${result.error}\`), see [workflow run](${runUrl}).`,
		);
		return lines;
	}
	lines.push("", "**Direct packages**");
	if (result.direct.length === 0) lines.push("No package version changes.");
	else lines.push(...result.direct.map(renderChange));
	lines.push(
		"",
		...renderClosure(
			result.transitive,
			keptClosures[result.label] ?? 0,
			maxChars,
		),
	);
	return lines;
}

function renderSectionWithClosures(
	artifact: DiffArtifact,
	runUrl: string,
	keptClosures: Readonly<Record<string, number>>,
	maxChars: number,
): string {
	const lines = [
		REVIEWABLE_MARKER,
		`<!-- nix-package-diff head=${artifact.headSha} -->`,
		"",
		"## Packages that change",
	];
	for (const result of artifact.results) {
		lines.push("", ...renderTarget(result, runUrl, keptClosures, maxChars));
	}
	return lines.join("\n");
}

function truncateClosures(
	artifact: DiffArtifact,
	runUrl: string,
	keptClosures: Record<string, number>,
	max: number,
): string {
	let section = renderSectionWithClosures(artifact, runUrl, keptClosures, max);
	for (const target of [...CLOSURE_TARGETS].reverse()) {
		while ((keptClosures[target.label] ?? 0) > 0 && section.length > max) {
			keptClosures[target.label] = (keptClosures[target.label] ?? 0) - 1;
			section = renderSectionWithClosures(artifact, runUrl, keptClosures, max);
		}
	}
	return section;
}
export function renderSection(
	artifact: DiffArtifact,
	maxChars: number,
	runUrl: string,
): string {
	const max = Math.min(MAX_BODY_CHARS, Math.max(0, Math.floor(maxChars)));
	const keptClosures: Record<string, number> = {};
	for (const result of artifact.results) {
		if ("transitive" in result)
			keptClosures[result.label] = result.transitive.length;
	}
	let section = truncateClosures(artifact, runUrl, keptClosures, max);
	if (section.length > max) {
		for (const result of artifact.results) {
			if ("transitive" in result) keptClosures[result.label] = 0;
		}
		section = renderSectionWithClosures(artifact, runUrl, keptClosures, max);
	}
	if (section.length > max) {
		throw new Error(
			"Direct package changes and section headers exceed the PR body limit.",
		);
	}
	return section;
}

export function sectionHeadSha(body: string): string | undefined {
	return /<!-- nix-package-diff head=([a-fA-F0-9]{40}) -->/.exec(body)?.[1];
}

function sectionPrefix(body: string): string {
	const marker = body.indexOf(REVIEWABLE_MARKER);
	const prefix = marker < 0 ? body : body.slice(0, marker);
	const separator =
		marker < 0 && prefix !== "" && !prefix.endsWith("\n\n") ? "\n\n" : "";
	return `${prefix}${separator}`;
}

export function upsertSection(body: string, section: string): string {
	return `${sectionPrefix(body)}${section}`;
}

export function sectionBudget(body: string): number {
	return Math.max(0, MAX_BODY_CHARS - sectionPrefix(body).length);
}
