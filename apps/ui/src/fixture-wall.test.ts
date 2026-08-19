// T3 — the AUTHORITATIVE hard-wall gate. The fixture boot path must be
// structurally incapable of shipping in a production bundle: because
// `boot-fixture.ts` is reached ONLY via a dynamic `import()` inside index.tsx's
// inline `import.meta.env.MODE === "fixture"` branch, a non-fixture `vite build`
// dead-code-eliminates that branch and never emits the boot-fixture chunk. This
// gate proves that structurally, the way preview-build.test.ts already asserts
// on emitted bundle text.
//
// It is red→green by construction: a single static `import … from "./boot-fixture"`
// anywhere in the shipped `src` graph re-emits the chunk and FIXTURE_SENTINEL
// reappears in dist/, failing the negative assertion. Importing FIXTURE_SENTINEL
// here does NOT count — a test module is excluded from the production build, so
// it is not a static importer in the shipped graph.

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { $ } from "bun";
import { FIXTURE_SENTINEL } from "./boot-fixture";

// Build is run from apps/ui (where vite.config.ts + index.html live). Resolve
// from this test file (apps/ui/src/) so the invocation is cwd-independent, the
// same way preview-build.test.ts anchors UI_DIR.
const UI_DIR = resolve(import.meta.dir, "..");

// A valid-shaped door URL (a `.invalid` TLD, RFC 2606 — never resolves) so the
// production build satisfies connection.ts's required-var without dialing
// anything, matching preview-build.test.ts's sentinel discipline.
const WALL_BASE_URL = "https://fixture-wall.invalid:8443";

// A known live-app literal — the #root guard at index.tsx:20. The positive
// control: it MUST be present, so a scan that read nothing (assets relocated out
// of dist/assets by a future vite.config change) fails loudly instead of passing
// vacuously on the negative assertion alone.
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
