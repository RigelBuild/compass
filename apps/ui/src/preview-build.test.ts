// Previewable-build gate: `bunx vite build` must bake the CONFIGURED door URL and bearer into
// `dist/`, so one bundle deploys against any target (a Vite default: every build-time `VITE_*`
// is inlined). Guards the USEFUL direction — nothing else pins it, so a vite.config or
// env-precedence regression could ship a bundle dialing the wrong door. Counterpart to env-secrecy.test.ts.

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { $ } from "bun";

// Build is run from apps/ui (where vite.config.ts + index.html live). Resolve
// from this test file (apps/ui/src/) so the invocation is cwd-independent, the
// same way env-secrecy.test.ts anchors UI_DIR.
const UI_DIR = resolve(import.meta.dir, "..");

// Unique .invalid sentinels expose build-time env leaks without dialing a real service.
const PREVIEW_BASE_URL = "https://sea2011-preview-door.invalid:8443";
const PREVIEW_TOKEN = "sea2011-preview-bearer-SENTINEL";

// Production builds must not bake the ignored development door default.
const DEV_DEFAULT_BASE_URL = "127.0.0.1:50051";

// Concatenate every emitted JS chunk once so each assertion greps the whole
// bundle, not a guessed filename (the hashed `index-<hash>.js` name is not
// stable across builds).
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

describe("previewable build bakes the configured door + bearer into dist/", () => {
	let outDir: string;
	let bundle: string;

	beforeAll(async () => {
		outDir = await mkdtemp(join(tmpdir(), "compass-preview-build-"));
		// Build with preview env in a temporary output directory.
		await $`bunx vite build --outDir ${outDir} --emptyOutDir`
			.cwd(UI_DIR)
			.env({
				...process.env,
				VITE_COMPASS_BASE_URL: PREVIEW_BASE_URL,
				VITE_COMPASS_TOKEN: PREVIEW_TOKEN,
			})
			.quiet();
		bundle = await bundleText(outDir);
	}, 60_000);

	afterAll(async () => {
		await rm(outDir, { recursive: true, force: true });
	});

	test("the configured door URL is inlined into the bundle", () => {
		expect(bundle).toContain(PREVIEW_BASE_URL);
	});

	test("the configured bearer is inlined into the bundle", () => {
		expect(bundle).toContain(PREVIEW_TOKEN);
	});

	test("the tracked dev loopback default does not bleed into a production build", () => {
		// A production `vite build` reads process.env, not .env.development, so the
		// checked-in dev door must be absent — otherwise a preview built with a
		// mistyped VITE_COMPASS_BASE_URL could silently fall back to loopback.
		expect(bundle).not.toContain(DEV_DEFAULT_BASE_URL);
	});
});
