import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { closureChanges, mainTag } from "./retag-core.ts";

const root = join(import.meta.dir, "..", "..");
const workflow = Bun.YAML.parse(
	readFileSync(join(root, ".github/workflows/release.yml"), "utf8"),
) as {
	env: Record<string, string>;
	jobs: Record<
		string,
		{
			needs?: string | string[];
			if?: string;
			outputs?: Record<string, string>;
			strategy?: {
				matrix?: { include?: { arch: string; runner: string }[] };
			};
			permissions?: Record<string, string>;
			concurrency?: {
				group: string;
				"cancel-in-progress": boolean;
				queue: string;
			};
			steps?: { uses?: string; with?: Record<string, unknown> }[];
		}
	>;
};

function needsList(job: (typeof workflow.jobs)[string] | undefined): string[] {
	const needs = job?.needs;
	return needs === undefined ? [] : Array.isArray(needs) ? needs : [needs];
}

// One file per top-level go/ dir that `go list -deps ./cmd/compass-runner`
// reports. flake.nix builds from all of ./go, so each must republish the image.
const RUNNER_GO_SOURCES = [
	"go/cmd/compass-runner/main.go",
	"go/gen/compass/v1/compass.pb.go",
	"go/internal/auth/admin_gate.go",
	"go/go.mod",
	"go/go.sum",
];

describe("RUNNER_IMAGE_CLOSURE_PATHS in release.yml", () => {
	test("gates every Go source the runner is built from", () => {
		expect(
			closureChanges(
				workflow.env.RUNNER_IMAGE_CLOSURE_PATHS ?? "",
				RUNNER_GO_SOURCES,
				"runner",
			),
		).toEqual(RUNNER_GO_SOURCES);
	});
});

describe("P6 paired main tags", () => {
	test("generated Go source changes both image closures", () => {
		const source = "go/gen/compass/v1/compass.pb.go";
		expect(
			closureChanges(
				workflow.env.RUNNER_IMAGE_CLOSURE_PATHS ?? "",
				[source],
				"runner",
			),
		).toEqual([source]);
		expect(
			closureChanges(
				workflow.env.SERVER_IMAGE_CLOSURE_PATHS ?? "",
				[source],
				"server",
			),
		).toEqual([source]);
	});
	test("tag-main-images waits for both indexes and checks out full history", () => {
		const job = workflow.jobs["tag-main-images"];
		expect(needsList(job)).toEqual(
			expect.arrayContaining(["publish-runner-index", "publish-server-index"]),
		);
		expect(job?.permissions).toEqual({ contents: "read", packages: "write" });
		expect(job?.concurrency).toEqual({
			group: "tag-main-images",
			"cancel-in-progress": false,
			queue: "max",
		});
		expect(
			job?.steps?.some(
				(step) =>
					step.uses?.startsWith("actions/checkout@") &&
					step.with?.["fetch-depth"] === 0,
			),
		).toBe(true);
	});

	test("D8 pattern matches and its Flux extract yields n", () => {
		const tag = mainTag(1638, "abcdef0123456789abcdef0123456789abcdef01");
		// D8's Flux ImagePolicy pattern, verbatim in Go RE2 syntax.
		const goPattern = "^main-(?P<n>[1-9][0-9]*)-[0-9a-f]{12}$";
		const pattern = new RegExp(goPattern.replace("(?P<", "(?<"));
		const extract = "$n";
		expect(tag).toMatch(pattern);
		expect(tag.replace(pattern, extract.replace(/\$(\w+)/g, "$<$1>"))).toBe(
			"1638",
		);
		expect("main-0-abcdef012345").not.toMatch(pattern);
		expect("main-1638-abcdefg12345").not.toMatch(pattern);
	});
});

describe("P7 multi-arch release publishers", () => {
	const lanes = ["runner", "server", "guest", "gateway"] as const;
	test("each lane gates once, builds native members, then composes an index", () => {
		for (const lane of lanes) {
			const gate = workflow.jobs[`publish-${lane}-gate`];
			const publisher = workflow.jobs[`publish-${lane}-image`];
			const index = workflow.jobs[`publish-${lane}-index`];
			expect(needsList(publisher)).toEqual([`publish-${lane}-gate`]);
			expect(publisher?.strategy?.matrix?.include).toEqual(
				expect.arrayContaining([
					expect.objectContaining({ arch: "amd64", runner: "ubuntu-latest" }),
					expect.objectContaining({
						arch: "arm64",
						runner: "ubuntu-24.04-arm",
					}),
				]),
			);
			expect(needsList(index)).toEqual([
				`publish-${lane}-gate`,
				`publish-${lane}-image`,
			]);
			expect(gate?.outputs).toBeDefined();
		}
	});
	test("tag-main-images needs all publisher stages and gates their results", () => {
		const job = workflow.jobs["tag-main-images"];
		expect(needsList(job)).toEqual([
			"publish-runner-gate",
			"publish-runner-image",
			"publish-runner-index",
			"publish-server-gate",
			"publish-server-image",
			"publish-server-index",
		]);
		expect(job?.if).toContain("!cancelled()");
		expect(job?.if).toContain(
			"contains(fromJSON('[\"push\"]'), github.event_name)",
		);
		expect(job?.if).toContain(
			"contains(fromJSON('[\"refs/heads/main\"]'), github.ref)",
		);
		for (const lane of ["runner", "server"]) {
			expect(job?.if).toContain(
				`needs.publish-${lane}-gate.result == 'success'`,
			);
			for (const stage of ["image", "index"]) {
				expect(job?.if).toContain(
					`contains(fromJSON('[\"success\", \"skipped\"]'), needs.publish-${lane}-${stage}.result)`,
				);
			}
		}
	});
});

describe("P7 arm64 guest CI gate", () => {
	test("runs guest build-only on native arm64 behind microvm_affected and requires it in rollup", () => {
		const ci = Bun.YAML.parse(
			readFileSync(join(root, ".github/workflows/ci.yml"), "utf8"),
		) as {
			jobs: Record<
				string,
				{
					needs?: string | string[];
					if?: string;
					"runs-on"?: string;
					steps?: { run?: string }[];
				}
			>;
		};
		const guest = ci.jobs["guest-arm64"];
		expect(guest?.["runs-on"]).toBe("ubuntu-24.04-arm");
		expect(guest?.if).toContain("microvm_affected");
		expect(
			guest?.steps?.some((step) =>
				step.run?.includes("tools/microvm-boot-test/run.ts --build-only"),
			),
		).toBe(true);
		const rollupNeeds = ci.jobs.rollup?.needs;
		expect(Array.isArray(rollupNeeds) ? rollupNeeds : [rollupNeeds]).toContain(
			"guest-arm64",
		);
	});
});

describe("runner-image build inputs in moon.yml", () => {
	test("hash all of go/ except the ignored lint caches and dev binary", () => {
		const moon = Bun.YAML.parse(
			readFileSync(join(root, "runner-image/moon.yml"), "utf8"),
		) as { tasks: { build: { inputs: string[] } } };
		const inputs = moon.tasks.build.inputs;
		expect(inputs).toContain("/go/**");
		// Matches the go/ entries in .gitignore; moon's walker does not skip them.
		for (const ignored of [
			"!/go/.golangci-cache/**",
			"!/go/.golangci-cache-review/**",
			"!/go/compass",
		]) {
			expect(inputs).toContain(ignored);
		}
	});
});
