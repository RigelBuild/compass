// Dogfood-layout base-contract gate (RIG-1362 Task 3): the tailscale Serve host
// owns `/` for the gRPC-Web door and mounts the UI under `/ui`
// (`tailscale serve --set-path=/ui <dist>`), so the dogfood bundle's asset URLs
// MUST be emitted under `/ui/`. A plain `vite build` emits root-relative
// `/assets/…` URLs (vite.config.ts sets no `base`), which under the Serve layout
// hit the door's `/` proxy and 404 — the UI loads a blank page. The
// `apps/ui:build-dogfood` moon task passes `--base=/ui/` to fix that; this gate
// is the deliverable that keeps the base contract T4 consumes from silently
// regressing (a dropped `--base` flag, or a vite.config `base` override).
//
// It is the base-layout counterpart to preview-build.test.ts (which guards the
// build-time env baking): both build the real bundle and assert on emitted
// output, never a mock. The subject here is the rewritten asset URL in the
// emitted index.html — where vite writes the base-prefixed `src`/`href` — so
// this reads index.html, not the JS chunks preview-build.test.ts greps.

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { $ } from "bun";

// Build is run from apps/ui (where vite.config.ts + index.html live). Resolve
// from this test file (apps/ui/src/) so the invocation is cwd-independent, the
// same way preview-build.test.ts anchors UI_DIR.
const UI_DIR = resolve(import.meta.dir, "..");

// A valid-shaped door URL (a `.invalid` TLD, RFC 2606 — never resolves), set
// only to mirror preview-build.test.ts's sentinel discipline and keep the
// emitted bundle deployment-shaped. It is NOT required for this gate: `vite
// build` only inlines `import.meta.env.VITE_COMPASS_BASE_URL` as a string
// literal — it never calls resolveConnection, whose required-var throw fires at
// runtime (browser boot), not at build time — so the base contract asserted
// below is independent of the door URL and the build succeeds without it.
const DOGFOOD_BASE_URL = "https://rig1944-dogfood-door.invalid:8443";

async function indexHtml(distDir: string): Promise<string> {
	return readFile(join(distDir, "index.html"), "utf8");
}

describe("the dogfood build emits assets under the /ui/ base (T4 mount contract)", () => {
	let baseOutDir: string;
	let plainOutDir: string;
	let dogfoodIndex: string;
	let plainIndex: string;

	beforeAll(async () => {
		baseOutDir = await mkdtemp(join(tmpdir(), "compass-dogfood-build-"));
		plainOutDir = await mkdtemp(join(tmpdir(), "compass-plain-build-"));
		// The dogfood build the `build-dogfood` moon task runs: `--base=/ui/`. Built
		// to a temp outDir (not the tracked dist-dogfood/) so a dev's working tree is
		// untouched; `--emptyOutDir` because outDir is outside the project root.
		await $`bunx vite build --base=/ui/ --outDir ${baseOutDir} --emptyOutDir`
			.cwd(UI_DIR)
			.env({ ...process.env, VITE_COMPASS_BASE_URL: DOGFOOD_BASE_URL })
			.quiet();
		// A plain build (no `--base`) is the negative control: it must emit
		// root-relative assets, so the positive assertion below is proven to be the
		// flag's doing, not a vacuous match on a path both layouts share.
		await $`bunx vite build --outDir ${plainOutDir} --emptyOutDir`
			.cwd(UI_DIR)
			.env({ ...process.env, VITE_COMPASS_BASE_URL: DOGFOOD_BASE_URL })
			.quiet();
		dogfoodIndex = await indexHtml(baseOutDir);
		plainIndex = await indexHtml(plainOutDir);
	}, 120_000);

	afterAll(async () => {
		await rm(baseOutDir, { recursive: true, force: true });
		await rm(plainOutDir, { recursive: true, force: true });
	});

	test("dogfood asset URLs are prefixed with the /ui/ base", () => {
		// Every emitted module/stylesheet ref in index.html carries the base. The
		// module script is the load-bearing one — a wrong base here is the blank
		// page under the Serve `/ui` mount.
		expect(dogfoodIndex).toContain('src="/ui/assets/');
		expect(dogfoodIndex).toContain('href="/ui/assets/');
	});

	test("dogfood emits NO root-relative /assets/ URL (would 404 under the /ui mount)", () => {
		// The failure mode this gate exists to catch: a root-relative `/assets/…`
		// hits the door's `/` proxy, not the UI mount. `/ui/assets/` does not match
		// `"/assets/` (the leading quote anchors it to a bare-root ref), so this is
		// a real regression tripwire, not a tautology.
		expect(dogfoodIndex).not.toContain('="/assets/');
	});

	test("a plain build emits root-relative assets (negative control)", () => {
		// Proves the /ui/ prefix above is the `--base` flag's effect: the same
		// source, built without the flag, emits the bare-root layout that would
		// break under Serve.
		expect(plainIndex).toContain('="/assets/');
		expect(plainIndex).not.toContain("/ui/assets/");
	});
});
