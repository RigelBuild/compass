import { describe, expect, test } from "bun:test";
import {
	type PackageVersion,
	planRetention,
	type RetentionPlan,
	recheckSkipReason,
	retentionWindow,
} from "./prune.ts";

const NOW = new Date("2026-10-11T12:00:00.000Z");
const OLD = "2026-09-01T12:00:00.000Z";
const YOUNG = "2026-10-10T12:00:00.000Z";
const WINDOW_SHA = "a".repeat(12);
const OLD_MAIN_SHA = "b".repeat(12);
const OFF_MAIN_SHA = "c".repeat(12);
const DIGEST_PIN = `sha256:${"d".repeat(64)}`;
const LOCK_TAG = "git-eeeeeeeeeeee";

interface VersionInput {
	readonly id: number;
	readonly name: string;
	readonly created_at?: string;
	readonly tags?: readonly string[];
}

function version(input: VersionInput): PackageVersion {
	return {
		id: input.id,
		name: input.name,
		created_at: input.created_at ?? OLD,
		metadata: { container: { tags: input.tags ?? [] } },
	};
}

function classify(
	versions: readonly PackageVersion[],
	children = new Map<string, ReadonlySet<string>>(),
): RetentionPlan {
	return planRetention(
		versions,
		new Set([WINDOW_SHA]),
		new Set([WINDOW_SHA, OLD_MAIN_SHA]),
		{ digests: new Set([DIGEST_PIN]), tag: LOCK_TAG },
		children,
		NOW,
	);
}

describe("planRetention", () => {
	test("keeps a current window tag, a semver/latest tag, an off-main tag, and lock pins", () => {
		const pinnedByDigest = version({ id: 4, name: DIGEST_PIN });
		const lockTagVersion = version({
			id: 5,
			name: `sha256:${"5".repeat(64)}`,
			tags: [LOCK_TAG],
		});
		const plan = classify([
			version({
				id: 1,
				name: `sha256:${"1".repeat(64)}`,
				tags: [`git-${WINDOW_SHA}`],
			}),
			version({
				id: 2,
				name: `sha256:${"2".repeat(64)}`,
				tags: ["latest", "v1.2.3"],
			}),
			version({
				id: 3,
				name: `sha256:${"3".repeat(64)}`,
				tags: [`git-${OFF_MAIN_SHA}-arm64`],
			}),
			pinnedByDigest,
			lockTagVersion,
		]);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([
			1, 2, 3, 4, 5,
		]);
		expect(plan.delete).toHaveLength(0);
	});

	test("keeps untagged children of a kept old index and a young untagged version", () => {
		const index = version({
			id: 10,
			name: `sha256:${"a".repeat(64)}`,
			tags: ["v2.0.0"],
		});
		const semverChild = version({
			id: 11,
			name: `sha256:${"b".repeat(64)}`,
			tags: [],
		});
		const youngOrphan = version({
			id: 12,
			name: `sha256:${"c".repeat(64)}`,
			created_at: YOUNG,
		});
		const plan = classify(
			[index, semverChild, youngOrphan],
			new Map([[index.name, new Set([semverChild.name])]]),
		);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([10, 11, 12]);
		expect(plan.keep[1]?.reason).toContain("child of kept index");
		expect(plan.delete).toHaveLength(0);
	});
	test("keeps per-architecture children of an old semver index", () => {
		const index = version({
			id: 16,
			name: `sha256:${"a".repeat(64)}`,
			tags: ["v2.3.4"],
		});
		const amd64 = version({
			id: 17,
			name: `sha256:${"b".repeat(64)}`,
			tags: [`git-${OLD_MAIN_SHA}-amd64`],
		});
		const arm64 = version({
			id: 18,
			name: `sha256:${"c".repeat(64)}`,
			tags: [`git-${OLD_MAIN_SHA}-arm64`],
		});
		const plan = classify(
			[index, amd64, arm64],
			new Map([[index.name, new Set([amd64.name, arm64.name])]]),
		);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([16, 17, 18]);
		expect(plan.keep.slice(1).map(({ reason }) => reason)).toEqual([
			`child of kept index ${index.name}`,
			`child of kept index ${index.name}`,
		]);
		expect(plan.delete).toHaveLength(0);
	});
	test("keeps the in-window sha tag when a second git tag is malformed", () => {
		const plan = classify([
			version({
				id: 14,
				name: `sha256:${"8".repeat(64)}`,
				tags: [`git-${WINDOW_SHA}`, "git-custom-build"],
			}),
		]);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([14]);
		expect(plan.delete).toHaveLength(0);
	});
	test("keeps unknown non-git tags because their retention is not judged", () => {
		const plan = classify([
			version({
				id: 13,
				name: `sha256:${"7".repeat(64)}`,
				tags: ["preview-build"],
			}),
		]);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([13]);
		expect(plan.delete).toHaveLength(0);
	});
	test("keeps an unrecognized git tag rather than judging its source ref", () => {
		const plan = classify([
			version({
				id: 15,
				name: `sha256:${"9".repeat(64)}`,
				tags: ["git-custom-build"],
			}),
		]);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([15]);
		expect(plan.delete).toHaveLength(0);
	});

	test("deletes an old out-of-window index with its architecture tags", () => {
		const digest = `sha256:${"e".repeat(64)}`;
		const plan = classify([
			version({
				id: 20,
				name: digest,
				tags: [
					`git-${OLD_MAIN_SHA}`,
					`git-${OLD_MAIN_SHA}-amd64`,
					`git-${OLD_MAIN_SHA}-arm64`,
				],
			}),
		]);

		expect(plan.keep).toHaveLength(0);
		expect(plan.delete.map(({ version: item }) => item.id)).toEqual([20]);
	});

	test("deletes an old untagged orphan", () => {
		const plan = classify([
			version({ id: 30, name: `sha256:${"f".repeat(64)}` }),
		]);

		expect(plan.keep).toHaveLength(0);
		expect(plan.delete.map(({ version: item }) => item.id)).toEqual([30]);
	});

	test("keeps a version mixing an in-window and out-of-window tag", () => {
		const plan = classify([
			version({
				id: 40,
				name: `sha256:${"0".repeat(64)}`,
				tags: [`git-${WINDOW_SHA}`, `git-${OLD_MAIN_SHA}`],
			}),
		]);

		expect(plan.keep.map(({ version: item }) => item.id)).toEqual([40]);
		expect(plan.delete).toHaveLength(0);
	});
});

describe("retentionWindow", () => {
	test("uses the minimum when fewer commits are recent", () => {
		const commits = Array.from({ length: 300 }, (_, index) => ({
			sha: `${index.toString(16).padStart(12, "0")}${"a".repeat(28)}`,
			committedAt: NOW.getTime() - (index < 10 ? 1 : 31 * 24 * 60 * 60 * 1000),
		}));
		expect(retentionWindow(commits, NOW, 200)).toHaveLength(200);
	});

	test("adds 50 commits after 300 recent commits", () => {
		const commits = Array.from({ length: 400 }, (_, index) => ({
			sha: `${index.toString(16).padStart(12, "0")}${"b".repeat(28)}`,
			committedAt: NOW.getTime() - (index < 300 ? 0 : 31 * 24 * 60 * 60 * 1000),
		}));
		expect(retentionWindow(commits, NOW, 200)).toHaveLength(350);
	});

	test("includes commits exactly on the 30-day boundary", () => {
		const boundary = NOW.getTime() - 30 * 24 * 60 * 60 * 1000;
		const commits = Array.from({ length: 251 }, (_, index) => ({
			sha: `${index.toString(16).padStart(12, "0")}${"c".repeat(28)}`,
			committedAt: index < 151 ? boundary : boundary - 1,
		}));
		const window = retentionWindow(commits, NOW, 200);
		const boundaryCommit = commits[150];
		expect(window).toHaveLength(201);
		expect(boundaryCommit && window.has(boundaryCommit.sha.slice(0, 12))).toBe(
			true,
		);
	});
});

describe("recheckSkipReason", () => {
	const pins = { digests: new Set([DIGEST_PIN]), tag: LOCK_TAG };
	const mainSha12 = new Set([WINDOW_SHA, OLD_MAIN_SHA]);

	test("reports a changed tag list", () => {
		const planned = version({ id: 50, name: `sha256:${"5".repeat(64)}` });
		const current = version({
			id: 50,
			name: planned.name,
			tags: [`git-${OLD_MAIN_SHA}`],
		});
		expect(
			recheckSkipReason(
				planned,
				current,
				new Set([WINDOW_SHA]),
				mainSha12,
				pins,
				new Map(),
				NOW,
			),
		).toBe("tag list changed");
	});

	test("returns the keep reason when unchanged tags enter the window", () => {
		const current = version({
			id: 51,
			name: `sha256:${"6".repeat(64)}`,
			tags: [`git-${OLD_MAIN_SHA}`],
		});
		const planned = version({
			id: 51,
			name: current.name,
			tags: [`git-${OLD_MAIN_SHA}`],
		});
		expect(
			recheckSkipReason(
				planned,
				current,
				new Set([WINDOW_SHA, OLD_MAIN_SHA]),
				mainSha12,
				pins,
				new Map(),
				NOW,
			),
		).toBe(`window tag git-${OLD_MAIN_SHA}`);
	});

	test("allows deletion when tags are unchanged and still out of policy", () => {
		const current = version({
			id: 52,
			name: `sha256:${"7".repeat(64)}`,
			tags: [`git-${OLD_MAIN_SHA}`],
		});
		const planned = version({
			id: 52,
			name: current.name,
			tags: [`git-${OLD_MAIN_SHA}`],
		});
		expect(
			recheckSkipReason(
				planned,
				current,
				new Set([WINDOW_SHA]),
				mainSha12,
				pins,
				new Map(),
				NOW,
			),
		).toBeUndefined();
	});
});
