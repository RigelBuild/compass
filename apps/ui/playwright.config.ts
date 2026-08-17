import { defineConfig, devices } from "@playwright/test";

// The repo's first browser harness (SEA-2034 T1). Drives `vite dev` against the
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
export default defineConfig({
	testDir: "./e2e",
	outputDir: "./e2e/.output",
	fullyParallel: false,
	reporter: [["list"]],
	use: {
		baseURL: "http://localhost:5173",
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
		command: "bunx vite --port 5173 --strictPort --mode fixture",
		url: "http://localhost:5173",
		reuseExistingServer: true,
		timeout: 120_000,
	},
});
