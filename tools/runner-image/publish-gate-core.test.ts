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

type Step = { kind: "gate" | "land" | "tag"; head: number };

/** One point in a burst: published tags, gate decisions, tag-main results. */
type BurstState = {
	tags: Set<string>;
	order: Step[];
	publishes: Map<number, boolean>;
	tagged: Map<number, string | number>;
};

/** The steps that may run next. Gates start in push order; a publishing
 * head's index lands before its own tag-main; a skipping head's tag-main runs
 * any time after its gate, even while an earlier publish is still queued. */
function nextSteps(state: BurstState, heads: string[]): Step[] {
	return [...heads.keys()].flatMap((head) => stepsFor(state, heads, head));
}

function stepsFor(state: BurstState, heads: string[], head: number): Step[] {
	const publishes = state.publishes.get(head);
	if (publishes === undefined) {
		const gateOpen = head === 0 || state.publishes.has(head - 1);
		return gateOpen ? [{ kind: "gate", head }] : [];
	}
	const landed = state.tags.has(short(heads[head] as string));
	const steps: Step[] = [];
	if (publishes && !landed) steps.push({ kind: "land", head });
	if (!state.tagged.has(head) && (!publishes || landed)) {
		steps.push({ kind: "tag", head });
	}
	return steps;
}

async function apply(
	state: BurstState,
	step: Step,
	commits: Commit[],
	heads: string[],
): Promise<BurstState> {
	const head = heads[step.head] as string;
	const registry = new Registry();
	for (const t of state.tags) registry.tags.add(t);
	const next = { ...state, order: [...state.order, step] };
	switch (step.kind) {
		case "gate": {
			const { shouldPublish } = await gate(registry, commits, head);
			return {
				...next,
				publishes: new Map(state.publishes).set(step.head, shouldPublish),
			};
		}
		case "land":
			return { ...next, tags: new Set(state.tags).add(short(head)) };
		case "tag": {
			const result = await pair(registry, commits, head).then(
				(p) => p.server.commit,
				(err: RetagError) => err.code,
			);
			return { ...next, tagged: new Map(state.tagged).set(step.head, result) };
		}
	}
}

/** Every order a burst's release runs can take, with each head's tag-main
 * result: the resolved server commit, or the exit code it failed with. */
async function* everyInterleaving(
	commits: Commit[],
	heads: string[],
	published: string[],
	state: BurstState = {
		tags: new Set(published.map(short)),
		order: [],
		publishes: new Map(),
		tagged: new Map(),
	},
): AsyncGenerator<{ order: Step[]; tagged: (string | number)[] }> {
	const steps = nextSteps(state, heads);
	if (steps.length === 0) {
		yield {
			order: state.order,
			tagged: heads.map((_, i) => state.tagged.get(i) ?? -1),
		};
		return;
	}
	for (const step of steps) {
		const next = await apply(state, step, commits, heads);
		yield* everyInterleaving(commits, heads, published, next);
	}
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
		expect(
			(await gate(new Registry(sha(1)), commits, sha(3))).shouldPublish,
		).toBe(true);
	});

	test("a docs merge after the closure image landed skips and tags that image", async () => {
		const registry = new Registry(sha(1), sha(2));
		expect((await gate(registry, commits, sha(3))).shouldPublish).toBe(false);
		expect((await pair(registry, commits, sha(3))).server.commit).toBe(sha(2));
	});

	test("every interleaving of a 3-head burst tags a closure-matching image per head", async () => {
		const burst: Commit[] = [
			...commits,
			{ sha: sha(4), set: { "server-image/Dockerfile": "1" } },
		];
		const heads = [sha(2), sha(3), sha(4)];
		let orders = 0;
		for await (const { order, tagged } of everyInterleaving(burst, heads, [
			sha(1),
		])) {
			orders++;
			for (const [i, head] of heads.entries()) {
				const resolved = tagged[i];
				if (typeof resolved !== "string") {
					throw new Error(
						`head ${i} failed with exit ${resolved} in ${JSON.stringify(order)}`,
					);
				}
				const { changedSince } = history(burst, head);
				expect(
					closureChanges(CLOSURE, await changedSince(resolved), "server"),
				).toEqual([]);
			}
		}
		expect(orders).toBeGreaterThan(10);
	});

	test("a head that reverts a queued closure change publishes, so a late landing cannot red it", async () => {
		const revert: Commit[] = [
			{ sha: sha(1), set: { "go/a.go": "1" } },
			{ sha: sha(2), set: { "go/a.go": "2" } },
			{ sha: sha(3), set: { "go/a.go": "1" } },
		];
		const decision = await gate(new Registry(sha(1)), revert, sha(3));
		expect(decision.shouldPublish).toBe(true);
		for await (const { order, tagged } of everyInterleaving(
			revert,
			[sha(2), sha(3)],
			[sha(1)],
		)) {
			expect({ order, tagged }).toEqual({ order, tagged: [sha(2), sha(3)] });
		}
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
