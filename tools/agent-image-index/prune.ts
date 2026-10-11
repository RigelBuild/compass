#!/usr/bin/env bun

const REGISTRY = "https://ghcr.io";
const REPOSITORY = "rigelbuild/compass-agent";
const API = "https://api.github.com";
const PACKAGE_VERSIONS = `${API}/orgs/RigelBuild/packages/container/compass-agent/versions`;
const MANIFEST_ACCEPT = [
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
].join(", ");
const WEEK_MS = 7 * 24 * 60 * 60 * 1000;
const DIGEST_PATTERN = /^sha256:[a-f0-9]{64}$/;

export interface PackageVersion {
	readonly id: number;
	readonly name: string;
	readonly created_at: string;
	readonly metadata: {
		readonly container: { readonly tags: readonly string[] };
	};
}

export interface ReasonedVersion {
	readonly version: PackageVersion;
	readonly reason: string;
}

export interface RetentionPlan {
	readonly keep: readonly ReasonedVersion[];
	readonly delete: readonly ReasonedVersion[];
}

export interface Pins {
	readonly digests: ReadonlySet<string>;
	readonly tag: string;
}
function gitTagSha12(tag: string): string | undefined {
	return /^git-([0-9a-f]{12})(?:-(?:amd64|arm64))?$/.exec(tag)?.[1];
}
function indexTagSha12(tag: string): string | undefined {
	return /^git-([0-9a-f]{12})$/.exec(tag)?.[1];
}

function baseKeepReason(
	version: PackageVersion,
	windowSha12: ReadonlySet<string>,
	mainSha12: ReadonlySet<string>,
	pins: Pins,
): string | undefined {
	const tags = version.metadata.container.tags;
	if (pins.digests.has(version.name)) return "pinned digest";
	if (tags.includes(pins.tag)) return `lock tag ${pins.tag}`;
	for (const tag of tags) {
		const sha12 = gitTagSha12(tag);
		if (sha12 && windowSha12.has(sha12)) return `window tag ${tag}`;
	}
	const ordinaryTag = tags.find((tag) => !tag.startsWith("git-"));
	if (ordinaryTag !== undefined) return `non-git tag ${ordinaryTag}`;
	for (const tag of tags) {
		if (!tag.startsWith("git-")) continue;
		const sha12 = /^git-([0-9a-f]{12})(?:-|$)/.exec(tag)?.[1];
		if (sha12 && !mainSha12.has(sha12)) return `off-main tag ${tag}`;
		if (!sha12) return `unjudged git tag ${tag}`;
	}
	return undefined;
}
function isYoungUntagged(version: PackageVersion, now: Date): boolean {
	return (
		version.metadata.container.tags.length === 0 &&
		Date.parse(version.created_at) > now.getTime() - WEEK_MS
	);
}

function keptIndexFor(
	version: PackageVersion,
	keptDigests: ReadonlySet<string>,
	indexChildren: ReadonlyMap<string, ReadonlySet<string>>,
): string | undefined {
	for (const digest of keptDigests) {
		if (indexChildren.get(digest)?.has(version.name)) return digest;
	}
	return undefined;
}

/** Classifies every registry version without performing I/O. */
export function planRetention(
	versions: readonly PackageVersion[],
	windowSha12: ReadonlySet<string>,
	mainSha12: ReadonlySet<string>,
	pins: Pins,
	indexChildren: ReadonlyMap<string, ReadonlySet<string>>,
	now: Date,
): RetentionPlan {
	const keep: ReasonedVersion[] = [];
	const candidates: PackageVersion[] = [];
	const directKeepDigests = new Set<string>();

	for (const version of versions) {
		const reason = baseKeepReason(version, windowSha12, mainSha12, pins);
		if (reason !== undefined) {
			keep.push({ version, reason });
			directKeepDigests.add(version.name);
		} else {
			candidates.push(version);
		}
	}

	const deletions: ReasonedVersion[] = [];
	for (const version of candidates) {
		const keptByIndex = keptIndexFor(version, directKeepDigests, indexChildren);
		if (keptByIndex !== undefined) {
			keep.push({ version, reason: `child of kept index ${keptByIndex}` });
		} else if (isYoungUntagged(version, now)) {
			keep.push({ version, reason: "untagged version younger than 7 days" });
		} else if (version.metadata.container.tags.length === 0) {
			deletions.push({
				version,
				reason: "untagged version at least 7 days old",
			});
		} else {
			deletions.push({
				version,
				reason: "all git tags are outside the main window",
			});
		}
	}

	return { keep, delete: deletions };
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parsePackageVersions(value: unknown): PackageVersion[] {
	if (!Array.isArray(value))
		throw new Error("GitHub package versions response was not an array");
	return value.map((entry, index) => {
		if (!isRecord(entry))
			throw new Error(`GitHub package version ${index} was not an object`);
		const metadata = entry.metadata;
		if (!isRecord(metadata) || !isRecord(metadata.container)) {
			throw new Error(
				`GitHub package version ${index} had no container metadata`,
			);
		}
		const tags = metadata.container.tags;
		if (
			typeof entry.id !== "number" ||
			!Number.isSafeInteger(entry.id) ||
			typeof entry.name !== "string" ||
			!DIGEST_PATTERN.test(entry.name) ||
			typeof entry.created_at !== "string" ||
			!Number.isFinite(Date.parse(entry.created_at)) ||
			!Array.isArray(tags) ||
			!tags.every((tag) => typeof tag === "string")
		) {
			throw new Error(`GitHub package version ${index} had invalid fields`);
		}
		return {
			id: entry.id,
			name: entry.name,
			created_at: entry.created_at,
			metadata: { container: { tags } },
		};
	});
}

function parseJsonRecord(
	value: unknown,
	description: string,
): Record<string, unknown> {
	if (!isRecord(value))
		throw new Error(`${description} response was not an object`);
	return value;
}

function parseDigest(value: unknown, description: string): string {
	if (typeof value !== "string" || !DIGEST_PATTERN.test(value)) {
		throw new Error(`${description} was not a sha256 digest`);
	}
	return value;
}

function readEnvInteger(
	name: string,
	fallback: number,
	minimum: number,
): number {
	const raw = process.env[name];
	if (raw === undefined || raw === "") return fallback;
	if (!/^\d+$/.test(raw)) throw new Error(`${name} must be an integer`);
	const value = Number(raw);
	if (!Number.isSafeInteger(value) || value < minimum) {
		throw new Error(`${name} must be at least ${minimum}`);
	}
	return value;
}

async function anonymousToken(): Promise<string> {
	const response = await fetch(
		`${REGISTRY}/token?scope=repository:${REPOSITORY}:pull`,
	);
	if (!response.ok)
		throw new Error(`GHCR token request failed: ${response.status}`);
	const body = parseJsonRecord(await response.json(), "GHCR token");
	if (typeof body.token !== "string" || body.token.length === 0) {
		throw new Error("GHCR token response carried no token");
	}
	return body.token;
}

async function listVersions(token: string): Promise<PackageVersion[]> {
	const versions: PackageVersion[] = [];
	for (let page = 1; ; page++) {
		const response = await fetch(
			`${PACKAGE_VERSIONS}?per_page=100&page=${page}`,
			{
				headers: {
					Authorization: `Bearer ${token}`,
					Accept: "application/vnd.github+json",
					"X-GitHub-Api-Version": "2022-11-28",
				},
			},
		);
		if (!response.ok)
			throw new Error(
				`GitHub package versions request failed: ${response.status}`,
			);
		const pageVersions = parsePackageVersions(await response.json());
		if (pageVersions.length === 0) return versions;
		versions.push(...pageVersions);
	}
}

async function readPins(): Promise<Pins> {
	const value: unknown = await Bun.file("guest-image/agent-oci.lock").json();
	const lock = parseJsonRecord(value, "agent OCI lock");
	if (typeof lock.tag !== "string" || lock.tag.length === 0) {
		throw new Error("agent OCI lock had no tag");
	}
	const digests = new Set<string>([
		parseDigest(lock.digest, "agent OCI lock digest"),
	]);
	if (lock.platforms !== undefined) {
		if (!isRecord(lock.platforms))
			throw new Error("agent OCI lock platforms was not an object");
		for (const [platform, value] of Object.entries(lock.platforms)) {
			if (!isRecord(value))
				throw new Error(
					`agent OCI lock platform ${platform} was not an object`,
				);
			digests.add(
				parseDigest(value.digest, `agent OCI lock ${platform} digest`),
			);
		}
	}
	return { digests, tag: lock.tag };
}

function gitRevList(args: readonly string[]): string[] {
	const result = Bun.spawnSync(["git", "rev-list", ...args]);
	if (result.exitCode !== 0)
		throw new Error(`git rev-list failed: ${result.stderr.toString()}`);
	return result.stdout.toString().split("\n").filter(Boolean);
}

async function indexChildrenFor(
	versions: readonly PackageVersion[],
	windowSha12: ReadonlySet<string>,
	mainSha12: ReadonlySet<string>,
	pins: Pins,
	token: string,
): Promise<Map<string, ReadonlySet<string>>> {
	const directVersions = versions.filter(
		(version) =>
			baseKeepReason(version, windowSha12, mainSha12, pins) !== undefined,
	);
	const children = new Map<string, ReadonlySet<string>>();
	for (const version of directVersions) {
		const response = await fetch(
			`${REGISTRY}/v2/${REPOSITORY}/manifests/${version.name}`,
			{
				headers: {
					Authorization: `Bearer ${token}`,
					Accept: MANIFEST_ACCEPT,
				},
			},
		);
		if (response.status !== 200) {
			throw new Error(
				`GHCR manifest request failed for ${version.name}: ${response.status}`,
			);
		}
		const manifest = parseJsonRecord(
			await response.json(),
			`GHCR manifest ${version.name}`,
		);
		const manifests = manifest.manifests;
		if (manifests === undefined) {
			children.set(version.name, new Set());
			continue;
		}
		if (!Array.isArray(manifests)) {
			throw new Error(`GHCR manifest ${version.name} had invalid children`);
		}
		const childDigests = new Set<string>();
		for (const [index, child] of manifests.entries()) {
			if (!isRecord(child))
				throw new Error(
					`GHCR manifest ${version.name} child ${index} was not an object`,
				);
			childDigests.add(
				parseDigest(child.digest, `GHCR manifest ${version.name} child digest`),
			);
		}
		children.set(version.name, childDigests);
	}
	return children;
}

function printPlan(plan: RetentionPlan): void {
	console.log("Retention plan:");
	for (const entry of plan.keep) {
		const tags = entry.version.metadata.container.tags.join(",") || "untagged";
		console.log(`KEEP ${entry.version.name} [${tags}] — ${entry.reason}`);
	}
	for (const entry of plan.delete) {
		const tags = entry.version.metadata.container.tags.join(",") || "untagged";
		console.log(`DELETE ${entry.version.name} [${tags}] — ${entry.reason}`);
	}
}

async function run(): Promise<void> {
	const window = readEnvInteger("WINDOW", 200, 200);
	const maxDelete = readEnvInteger("MAX_DELETE", 500, 0);
	const githubToken = process.env.GITHUB_TOKEN;
	if (!githubToken) throw new Error("GITHUB_TOKEN is required");

	const windowCommits = gitRevList([
		"--first-parent",
		`--max-count=${window}`,
		"HEAD",
	]);
	if (windowCommits.length < window) {
		throw new Error(
			`git history has ${windowCommits.length} first-parent commits; WINDOW=${window}, refusing a shallow/incomplete checkout`,
		);
	}
	const mainCommits = gitRevList(["--first-parent", "HEAD"]);
	const windowSha12 = new Set(
		windowCommits.map((commit) => commit.slice(0, 12)),
	);
	const mainSha12 = new Set(mainCommits.map((commit) => commit.slice(0, 12)));
	const [versions, pins, registryToken] = await Promise.all([
		listVersions(githubToken),
		readPins(),
		anonymousToken(),
	]);
	const indexChildren = await indexChildrenFor(
		versions,
		windowSha12,
		mainSha12,
		pins,
		registryToken,
	);
	const plan = planRetention(
		versions,
		windowSha12,
		mainSha12,
		pins,
		indexChildren,
		new Date(),
	);
	const hasWindowIndexTag = plan.keep.some((entry) =>
		entry.version.metadata.container.tags.some((tag) => {
			const sha12 = indexTagSha12(tag);
			return sha12 !== undefined && windowSha12.has(sha12);
		}),
	);
	if (!hasWindowIndexTag) {
		throw new Error(
			"no kept version carries an in-window git index tag; refusing to prune",
		);
	}
	if (plan.delete.length > maxDelete) {
		throw new Error(
			`planned delete count ${plan.delete.length} exceeds MAX_DELETE=${maxDelete}; refusing to prune`,
		);
	}

	const dryRun =
		process.env.PRUNE_DRY_RUN === "1" || process.argv.includes("--dry-run");
	printPlan(plan);
	if (dryRun) {
		console.log(
			`Summary: kept ${plan.keep.length}, would delete ${plan.delete.length}`,
		);
		return;
	}

	let deleted = 0;
	for (const { version } of plan.delete) {
		const response = await fetch(`${PACKAGE_VERSIONS}/${version.id}`, {
			method: "DELETE",
			headers: {
				Authorization: `Bearer ${githubToken}`,
				Accept: "application/vnd.github+json",
				"X-GitHub-Api-Version": "2022-11-28",
			},
		});
		if (response.status !== 204) {
			throw new Error(
				`GitHub package version deletion failed for ${version.id}: ${response.status}`,
			);
		}
		deleted++;
	}
	console.log(`Summary: kept ${plan.keep.length}, deleted ${deleted}`);
}

if (import.meta.main) await run();
