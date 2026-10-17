import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { closureChanges } from "./retag-core.ts";

const root = join(import.meta.dir, "..", "..");
const workflow = Bun.YAML.parse(
	readFileSync(join(root, ".github/workflows/release.yml"), "utf8"),
) as { env: Record<string, string> };

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
