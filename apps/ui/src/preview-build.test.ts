// Previewable-build gate: `bunx vite build` must bake the CONFIGURED door URL
// and bearer into `dist/`, so one bundle can be deployed against any target
// (the isolated PR-preview environment, a staging door, a local dogfood door).
// The capability rests on a Vite default — every `import.meta.env.VITE_*` value
// present at build time is inlined into the emitted bundle as a string literal
// (the same mechanism `env-secrecy.test.ts` guards the DANGEROUS direction of:
// a committed secret ships in `dist/`). This gate guards the USEFUL direction:
// the previewable build is only previewable if the door+bearer it was built
// with actually reach the browser.
//
// Nothing else pins this. `resolveConnection` (live/connection.ts) is unit-
// tested over plain env objects, but that proves the RESOLVER is pure — not that
// `vite build` delivers the env to it. A `vite.config.ts` change (an
// `envPrefix` override, a stray `define`, a plugin that strips env) or an
// env-precedence regression could silently ship a bundle that ignores
// VITE_COMPASS_BASE_URL and dials the wrong door — a preview pointed at the
// wrong server looks like a working preview until someone reads the network
// tab. This test builds the real bundle with a configured door+bearer and
// asserts both are baked in, and that the tracked dev loopback default does NOT
// bleed into a production build.
//
// It is the positive counterpart to env-secrecy.test.ts: that one asserts no
// secret-bearing env file is tracked (nothing dangerous bakes in); this one
// asserts the configured target DOES bake in (the preview capability works).
// No connection-resolution code change backs this capability — it is a Vite
// build-time property — so this gate is the deliverable that keeps it true.

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readdir, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { $ } from "bun";

// Build is run from apps/ui (where vite.config.ts + index.html live). Resolve
// from this test file (apps/ui/src/) so the invocation is cwd-independent, the
// same way env-secrecy.test.ts anchors UI_DIR.
const UI_DIR = resolve(import.meta.dir, "..");

// Sentinels chosen to be impossible to collide with any real string in the
// source tree: a `.invalid` TLD (RFC 2606, never resolves) and a unique token
// literal. If either shows up in dist/, it came from the build env, not the
// source. A `.invalid` door also means an accidental boot never dials anywhere.
const PREVIEW_BASE_URL = "https://sea2011-preview-door.invalid:8443";
const PREVIEW_TOKEN = "sea2011-preview-bearer-SENTINEL";

// The tracked dev default (.env.development, VITE_COMPASS_BASE_URL). Vite ignores
// .env.development in build mode, so a production `vite build` must NOT carry it.
// This is a cheap tripwire, not the load-bearing assertion: absent a new
// hardcoded loopback fallback (in connection.ts or a vite.config `define`) its
// absence follows from the mode-loading rule the two positive assertions already
// exercise. It stays as a regression guard against exactly that wrong-default bake.
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
		// The real production build, parameterized by the two preview env keys —
		// exactly what a preview-hosting environment runs. `--outDir` keeps it out
		// of the tracked `dist/` so a dev's working tree is untouched;
		// `--emptyOutDir` because the outDir is outside the project root (Vite
		// otherwise refuses to clear it without confirmation).
		await $`bunx vite build --outDir ${outDir} --emptyOutDir`
			.cwd(UI_DIR)
			.env({
				...process.env,
				VITE_COMPASS_BASE_URL: PREVIEW_BASE_URL,
				VITE_COMPASS_TOKEN: PREVIEW_TOKEN,
			})
			.quiet();
		bundle = await bundleText(outDir);
	});

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
