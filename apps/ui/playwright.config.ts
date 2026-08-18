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
		// Always launch our own `--mode fixture` server; never adopt a server
		// already on :5173. The command is mode-specific, but Playwright's reuse
		// probe only checks the URL for any 200 — it can't tell a fixture server
		// from a plain `vite dev`. Reusing a foreign server (a dev server wired to
		// a live daemon, or any non-fixture build) makes the per-surface selectors
		// resolve against wrong-but-plausible content, so the shots look valid
		// while depicting non-fixture data — and the same-box byte-identity
		// self-test can't catch it (both runs reuse the same wrong server →
		// identical wrong shots). With --strictPort a port clash now fails the
		// launch loudly instead. CI has no pre-existing server, so this is
		// behavior-identical there.
		reuseExistingServer: false,
		timeout: 120_000,
	},
});
