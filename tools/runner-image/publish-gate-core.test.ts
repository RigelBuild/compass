import { describe, expect, test } from "bun:test";
import { decidePublish } from "./publish-gate-core.ts";
import {
	closureChanges,
	EXIT,
	type ImageLane,
	type ProbeResult,
	RetagError,
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

/** A linear main history where each commit lists the paths it changed. */
type Commit = { sha: string; paths: string[] };

function sha(n: number): string {
	return n.toString(16).padStart(4, "0").padEnd(40, "a");
}

/** A registry holding `:git-<sha12>` tags, read the way skopeo answers. */
class Registry {
	readonly tags = new Set<string>();

	probe = async (short: string): Promise<ProbeResult> =>
		this.tags.has(short)
			? { exitCode: 0, stdout: MANIFEST, stderr: "" }
			: {
					exitCode: 1,
					stdout: "",
					stderr: `time="x" level=fatal msg="Error parsing image name \\"docker://r:git-${short}\\": reading manifest git-${short} in r: manifest unknown"`,
				};
}

function historyTo(commits: Commit[], head: string) {
	const at = commits.findIndex((c) => c.sha === head);
	const ancestors = commits
		.slice(0, at + 1)
		.map((c) => c.sha)
		.reverse();
	const changedSince = async (from: string) => {
		const start = commits.findIndex((c) => c.sha === from);
		return commits.slice(start + 1, at + 1).flatMap((c) => c.paths);
	};
	return { ancestors, changedSince };
}

function gate(registry: Registry, commits: Commit[], head: string) {
	return decidePublish({
		...historyTo(commits, head),
		probe: registry.probe,
		closurePaths: CLOSURE,
		lane: "server",
	});
}

function pair(registry: Registry, commits: Commit[], head: string) {
	const probe = registry.probe;
	return resolveMainPair({
		...historyTo(commits, head),
		probe: { server: probe, runner: probe } satisfies Record<
			ImageLane,
			typeof probe
		>,
		closurePaths: { server: CLOSURE, runner: CLOSURE },
	});
}

describe("decidePublish under back-to-back main merges", () => {
	// Run 39415809041: a closure merge, then a docs merge ~20s later. The first
	// merge's publish was still queued when the second merge's gate ran.
	const commits: Commit[] = [
		{ sha: sha(1), paths: [] },
		{ sha: sha(2), paths: ["go/e2e/main_test.go"] },
		{ sha: sha(3), paths: ["docs/notes.md"] },
	];

	test("a before-sha diff skips the second merge, so tagging it fails exit 3", async () => {
		const registry = new Registry();
		registry.tags.add(sha(1).slice(0, 12));
		// The old gate diffed only the push's before..head.
		const oldGate = closureChanges(CLOSURE, commits[2]?.paths ?? [], "server");
		expect(oldGate).toEqual([]);
		const err = await pair(registry, commits, sha(3)).catch((e) => e);
		expect(err).toBeInstanceOf(RetagError);
		expect((err as RetagError).code).toBe(EXIT.noAncestor);
	});

	test("the second merge publishes while the first merge's publish is queued", async () => {
		const registry = new Registry();
		registry.tags.add(sha(1).slice(0, 12));
		expect((await gate(registry, commits, sha(3))).shouldPublish).toBe(true);
	});

	test("every head in a burst of 3 tags a closure-matching image, in any publish order", async () => {
		const burst: Commit[] = [
			...commits,
			{ sha: sha(4), paths: ["server-image/Dockerfile"] },
		];
		const heads = burst.slice(1).map((c) => c.sha);
		for (const order of [
			[0, 1, 2],
			[2, 1, 0],
			[1, 2, 0],
		]) {
			const registry = new Registry();
			registry.tags.add(sha(1).slice(0, 12));
			// Every gate runs before any queued publish lands.
			const decisions = await Promise.all(
				heads.map((h) => gate(registry, burst, h)),
			);
			expect(decisions.map((d) => d.shouldPublish)).toEqual([true, true, true]);
			for (const i of order) registry.tags.add(heads[i]?.slice(0, 12) ?? "");
			for (const head of heads) {
				const resolved = await pair(registry, burst, head);
				expect(resolved.server.commit).toBe(head);
			}
		}
	});

	test("a head with no closure change since the newest published image skips", async () => {
		const registry = new Registry();
		registry.tags.add(sha(2).slice(0, 12));
		const decision = await gate(registry, commits, sha(3));
		expect(decision.shouldPublish).toBe(false);
		const resolved = await pair(registry, commits, sha(3));
		expect(resolved.server.commit).toBe(sha(2));
	});

	test("an already published head skips", async () => {
		const registry = new Registry();
		registry.tags.add(sha(3).slice(0, 12));
		expect((await gate(registry, commits, sha(3))).shouldPublish).toBe(false);
	});

	test("no published ancestor in the walk publishes", async () => {
		expect((await gate(new Registry(), commits, sha(3))).shouldPublish).toBe(
			true,
		);
	});

	test("an ambiguous registry answer fails the gate instead of skipping", async () => {
		const err = await decidePublish({
			...historyTo(commits, sha(3)),
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
