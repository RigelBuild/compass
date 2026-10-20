import { describe, expect, test } from "bun:test";
import { decidePublish } from "./publish-gate-core.ts";
import {
	closureChanges,
	EXIT,
	type ProbeResult,
	type RetagError,
	resolveMainPair,
} from "./retag-core.ts";

const CLOSURE = "go/**\nserver-image/**\n";
const MANIFEST = JSON.stringify({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	config: {
		digest:
			"sha256:2125c2a158a8329c40ac7c1daa8909f09fe82a5231ff0df05319d0be4d4c4bb6",
	},
});

/** A linear main history. Each commit sets file contents; null deletes. */
type Commit = { sha: string; set: Record<string, string | null> };

function sha(n: number): string {
	return n.toString(16).padStart(4, "0").padEnd(40, "a");
}

function short(commit: string): string {
	return commit.slice(0, 12);
}

/** A registry holding `:git-<sha12>` tags, answering the way skopeo does. */
class Registry {
	readonly tags = new Set<string>();

	constructor(...published: string[]) {
		for (const commit of published) this.tags.add(short(commit));
	}

	probe = async (tag: string): Promise<ProbeResult> =>
		this.tags.has(tag)
			? { exitCode: 0, stdout: MANIFEST, stderr: "" }
			: {
					exitCode: 1,
					stdout: "",
					stderr: `time="x" level=fatal msg="Error parsing image name \\"docker://r:git-${tag}\\": reading manifest git-${tag} in r: manifest unknown"`,
				};
}

function tree(commits: Commit[], upTo: number): Map<string, string> {
	const files = new Map<string, string>();
	for (const c of commits.slice(0, upTo + 1)) {
		for (const [path, body] of Object.entries(c.set)) {
			if (body === null) files.delete(path);
			else files.set(path, body);
		}
	}
	return files;
}

/** `git diff A head` (net tree change) and `git log A..head` (every touch). */
function history(commits: Commit[], head: string) {
	const at = commits.findIndex((c) => c.sha === head);
	const index = (commit: string) => commits.findIndex((c) => c.sha === commit);
	return {
		ancestors: commits
			.slice(0, at + 1)
			.map((c) => c.sha)
			.reverse(),
		changedSince: async (from: string) => {
			const before = tree(commits, index(from));
			const after = tree(commits, at);
			const paths = new Set([...before.keys(), ...after.keys()]);
			return [...paths].filter((p) => before.get(p) !== after.get(p));
		},
		touchedSince: async (from: string) =>
			commits.slice(index(from) + 1, at + 1).flatMap((c) => Object.keys(c.set)),
	};
}

function gate(registry: Registry, commits: Commit[], head: string) {
	const { ancestors, touchedSince } = history(commits, head);
	return decidePublish({
		ancestors,
		probe: registry.probe,
		touchedSince,
		closurePaths: CLOSURE,
		lane: "server",
	});
}

function pair(registry: Registry, commits: Commit[], head: string) {
	const { ancestors, changedSince } = history(commits, head);
	return resolveMainPair({
		ancestors,
		changedSince,
		probe: { server: registry.probe, runner: registry.probe },
		closurePaths: { server: CLOSURE, runner: CLOSURE },
	});
}

/** Head index → commits whose queued publish lands at that point. */
type Landings = Record<number, string[]>;

/**
 * One release run per head: its gate, then (if it publishes) its own index,
 * then tag-main. `landBefore[i]` lists heads whose queued publish lands just
 * before head i's gate; `landBetween[i]` lands between its gate and tag-main.
 * Returns each head's resolved server commit, or the exit code it failed with.
 */
async function runBurst(
	commits: Commit[],
	heads: string[],
	registry: Registry,
	landBefore: Landings = {},
	landBetween: Landings = {},
): Promise<(string | number)[]> {
	const out: (string | number)[] = [];
	for (const [i, head] of heads.entries()) {
		for (const c of landBefore[i] ?? []) registry.tags.add(short(c));
		const decision = await gate(registry, commits, head);
		for (const c of landBetween[i] ?? []) registry.tags.add(short(c));
		if (decision.shouldPublish) registry.tags.add(short(head));
		try {
			out.push((await pair(registry, commits, head)).server.commit);
		} catch (err) {
			out.push((err as RetagError).code);
		}
	}
	return out;
}

describe("decidePublish under back-to-back main merges", () => {
	// Run 39415809041: a closure merge, then a docs merge ~20s later, while the
	// first merge's publish was still queued.
	const commits: Commit[] = [
		{ sha: sha(1), set: { "go/a.go": "1" } },
		{ sha: sha(2), set: { "go/e2e/main_test.go": "1" } },
		{ sha: sha(3), set: { "docs/notes.md": "1" } },
	];

	test("the old before-sha gate skips the docs merge and pairing exits 3", async () => {
		const registry = new Registry(sha(1));
		expect(closureChanges(CLOSURE, ["docs/notes.md"], "server")).toEqual([]);
		const err = await pair(registry, commits, sha(3)).catch((e) => e);
		expect((err as RetagError).code).toBe(EXIT.noAncestor);
	});

	test("the docs merge publishes while the closure merge's publish is queued", async () => {
		expect(await runBurst(commits, [sha(3)], new Registry(sha(1)))).toEqual([
			sha(3),
		]);
	});

	test("a docs merge after the closure image landed skips and tags that image", async () => {
		const registry = new Registry(sha(1));
		expect(await runBurst(commits, [sha(2), sha(3)], registry)).toEqual([
			sha(2),
			sha(2),
		]);
		expect(registry.tags.has(short(sha(3)))).toBe(false);
	});

	test("a burst of 3 tags a closure-matching image per head under every landing order", async () => {
		const burst: Commit[] = [
			...commits,
			{ sha: sha(4), set: { "server-image/Dockerfile": "1" } },
		];
		const heads = [sha(2), sha(3), sha(4)];
		// Each head's own publish lands in its run; the earlier heads' queued
		// publishes land before or between the later gates.
		const orders: [Landings, Landings][] = [
			[{}, {}],
			[{ 2: [sha(2)] }, {}],
			[{}, { 1: [sha(2)] }],
			[{}, { 2: [sha(2), sha(3)] }],
		];
		for (const [before, between] of orders) {
			const registry = new Registry(sha(1));
			const resolved = await runBurst(burst, heads, registry, before, between);
			for (const [i, head] of heads.entries()) {
				expect(typeof resolved[i]).toBe("string");
				const { changedSince } = history(burst, head);
				const lacks = closureChanges(
					CLOSURE,
					await changedSince(resolved[i] as string),
					"server",
				);
				expect(lacks).toEqual([]);
			}
		}
	});

	test("a head that reverts a queued closure change publishes, so a late landing cannot red it", async () => {
		const revert: Commit[] = [
			{ sha: sha(1), set: { "go/a.go": "1" } },
			{ sha: sha(2), set: { "go/a.go": "2" } },
			{ sha: sha(3), set: { "go/a.go": "1" } },
		];
		const decision = await gate(new Registry(sha(1)), revert, sha(3));
		expect(decision.shouldPublish).toBe(true);
		// sha(2)'s publish lands between sha(3)'s gate and its tag-main.
		expect(
			await runBurst(
				revert,
				[sha(3)],
				new Registry(sha(1)),
				{},
				{
					0: [sha(2)],
				},
			),
		).toEqual([sha(3)]);
	});

	test("an already published head skips", async () => {
		expect(
			(await gate(new Registry(sha(3)), commits, sha(3))).shouldPublish,
		).toBe(false);
	});

	test("no published ancestor in the walk publishes", async () => {
		expect((await gate(new Registry(), commits, sha(3))).shouldPublish).toBe(
			true,
		);
	});

	test("an ambiguous registry answer fails the gate instead of skipping", async () => {
		const { ancestors, touchedSince } = history(commits, sha(3));
		const err = await decidePublish({
			ancestors,
			touchedSince,
			probe: async () => ({
				exitCode: 1,
				stdout: "",
				stderr: "received unexpected HTTP status: 503",
			}),
			closurePaths: CLOSURE,
			lane: "server",
		}).catch((e) => e);
		expect((err as RetagError).code).toBe(EXIT.registryFailed);
	});
});
