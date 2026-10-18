// Production bundles must omit the dynamically loaded fixture-only chunk.

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { $ } from "bun";
import { FIXTURE_SENTINEL } from "./boot-fixture";

// Resolve the Vite project directory from this test file, independent of cwd.
const UI_DIR = resolve(import.meta.dir, "..");

// Use a non-resolving door URL so production build does not dial a service.
const WALL_BASE_URL = "https://fixture-wall.invalid:8443";

// Require a live-app literal as a positive control against vacuous bundle scans.
const LIVE_APP_LITERAL = "missing #root element";

// Concatenate every emitted JS chunk once so each assertion greps the whole
// bundle, not a guessed hashed filename.
async function bundleText(distDir: string): Promise<string> {
	const assetsDir = join(distDir, "assets");
	const entries = await readdir(assetsDir);
	const chunks = await Promise.all(
		entries
			.filter((name) => name.endsWith(".js"))
			.map((name) => readFile(join(assetsDir, name), "utf8")),
	);
	return chunks.join("\n");
}

describe("the fixture boot path is absent from a production build (hard wall)", () => {
	let outDir: string;
	let bundle: string;

	beforeAll(async () => {
		outDir = await mkdtemp(join(tmpdir(), "compass-fixture-wall-"));
		// A default (production) `vite build` — MODE is "production", so the inline
		// `MODE === "fixture"` branch folds away and the boot-fixture chunk is never
		// emitted. `--emptyOutDir` because outDir is outside the project root.
		await $`bunx vite build --outDir ${outDir} --emptyOutDir`
			.cwd(UI_DIR)
			.env({
				...process.env,
				VITE_COMPASS_BASE_URL: WALL_BASE_URL,
			})
			.quiet();
		bundle = await bundleText(outDir);
	}, 60_000);

	afterAll(async () => {
		await rm(outDir, { recursive: true, force: true });
	});

	test("the fixture sentinel is NOT emitted into a production bundle", () => {
		expect(bundle).not.toContain(FIXTURE_SENTINEL);
	});

	test("a known live-app literal IS present (positive control against a vacuous scan)", () => {
		expect(bundle).toContain(LIVE_APP_LITERAL);
	});
});
