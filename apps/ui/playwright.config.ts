import { createServer } from "node:net";
import { defineConfig, devices } from "@playwright/test";

// The repo's first browser harness (RIG-2034 T1). Drives `vite dev` against the
// in-memory stub store (the app boots fully on stub-data.ts — no daemon, no
// Tauri IPC) and captures full-page screenshots of the core surfaces for Matt's
// human review. This is a smoke harness: no pixel-diff gating, no computed-style
// assertions.
// Browser resolution (approach (b)): the browsers cached at
// ~/.cache/ms-playwright (chromium-1234 / chromium_headless_shell-1234, the
// revision @playwright/test 1.62.1 bundles) are the upstream prebuilt binaries
// and are NOT patched for NixOS — they fail to load libnspr4.so. So we point
// Playwright at the nix-provided, properly-wrapped Chromium instead via
// launchOptions.executablePath. Version stays pinned to rev 1234 so the
// bundled protocol matches, were the cache ever usable.

// The fixture dev server binds an OS-assigned ephemeral port, never a fixed one.
// A fixed port (vite's 5173 default) collides with any dev server already on it
// — Matt's long-running review vite, or a second agent running this harness on
// the same box — and `--strictPort` turns every such clash into a hard launch
// failure. Under `vite dev` the port carries no meaning (the specs navigate via
// relative `page.goto` off `baseURL`), so we ask the OS for a free one at
// config-load and pin it in the environment. Playwright re-imports this config
// in each worker process; pinning through `process.env` (set once in the parent,
// inherited by every child) makes the runner, the workers, and the webServer
// launch all agree on the same port. `--strictPort` stays: if the tiny window
// between probe-close and vite-bind ever loses the port, we want a loud failure,
// not a silent drift to 5174.
async function pickFreePort(): Promise<number> {
	const { promise, resolve, reject } = Promise.withResolvers<number>();
	const probe = createServer();
	probe.once("error", reject);
	probe.listen(0, "127.0.0.1", () => {
		const address = probe.address();
		if (address === null || typeof address === "string") {
			probe.close();
			reject(new Error("could not resolve an ephemeral port"));
			return;
		}
		const { port } = address;
		probe.close(() => resolve(port));
	});
	return promise;
}

if (process.env.PLAYWRIGHT_DEV_PORT === undefined) {
	process.env.PLAYWRIGHT_DEV_PORT = String(await pickFreePort());
}
const devPort = Number(process.env.PLAYWRIGHT_DEV_PORT);
const baseURL = `http://localhost:${devPort}`;

export default defineConfig({
	testDir: "./e2e",
	outputDir: "./e2e/.output",
	fullyParallel: false,
	reporter: [["list"]],
	use: {
		baseURL,
		headless: true,
		screenshot: "off",
		reducedMotion: "reduce",
		deviceScaleFactor: 1,
		launchOptions: {
			// Env-overridable so this config carries no box-specific path in the
			// shared tree. Default is the nix-wrapped Chromium on Matt's dev box
			// (the cached ms-playwright binaries are unpatched for NixOS — see
			// above); CI or another box exports PLAYWRIGHT_CHROMIUM_PATH.
			executablePath:
				process.env.PLAYWRIGHT_CHROMIUM_PATH ??
				"/etc/profiles/per-user/mattw/bin/chromium",
		},
	},
	projects: [
		{
			name: "chromium",
			use: { ...devices["Desktop Chrome"] },
		},
	],
	webServer: {
		command: `bunx vite --port ${devPort} --strictPort --mode fixture`,
		url: baseURL,
		// Always launch our own `--mode fixture` server; never adopt a server
		// already on the port. The command is mode-specific, but Playwright's
		// reuse probe only checks the URL for any 200 — it can't tell a fixture
		// server from a plain `vite dev`. Reusing a foreign server (a dev server
		// wired to a live daemon, or any non-fixture build) makes the per-surface
		// selectors resolve against wrong-but-plausible content, so the shots look
		// valid while depicting non-fixture data — and the same-box byte-identity
		// self-test can't catch it (both runs reuse the same wrong server →
		// identical wrong shots). With the ephemeral port a foreign server can no
		// longer occupy it, and --strictPort keeps a residual clash loud rather
		// than adopting anything.
		reuseExistingServer: false,
		timeout: 120_000,
	},
});
