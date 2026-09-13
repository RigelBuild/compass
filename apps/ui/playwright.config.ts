import { createServer } from "node:net";
import { defineConfig, devices } from "@playwright/test";

// The repo's first browser harness (RIG-2034 T1). Drives `vite dev` against the in-memory
// stub store and pixel-diffs full-page screenshots of the core surfaces. Browser resolution:
// the cached ms-playwright binaries are unpatched for NixOS (fail on libnspr4.so), so we
// point Playwright at the nix-wrapped Chromium via launchOptions.executablePath.

// The fixture dev server binds an OS-assigned ephemeral port, never a fixed one. A fixed
// port collides with any dev server already on it and `--strictPort` turns that into a
// hard failure. We ask the OS for a free one at config-load and pin it through
// `process.env` so runner, workers, and webServer launch all agree; --strictPort stays loud.
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
	snapshotPathTemplate: "{testDir}/__screens__/{arg}{ext}",
	expect: {
		toHaveScreenshot: {
			maxDiffPixelRatio: 0.001,
			// Deliberately leave threshold unset; Playwright's default is 0.2.
		},
	},
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
		// Always launch our own `--mode fixture` server; never adopt one already on the port.
		// Playwright's reuse probe only checks the URL for any 200, so it can't tell a fixture
		// server from a plain `vite dev`; reusing a foreign one would depict non-fixture data
		// in valid-looking shots the byte-identity self-test can't catch. Ephemeral port + --strictPort keep it loud.
		reuseExistingServer: false,
		timeout: 120_000,
	},
});
