import { createServer } from "node:net";
import { defineConfig, devices } from "@playwright/test";

// The repo's first browser harness (RIG-2034 T1). Drives `vite dev` against the in-memory
// stub store and pixel-diffs full-page screenshots of the core surfaces.

// Both of these come from the dev shell (devenv.nix) or CI, which realize them from the
// same pinned helper. There is no fallback on purpose: development is devenv-only, and
// every default here is silently wrong. The cached ms-playwright binaries are unpatched
// for NixOS (they fail on libnspr4.so), and an unpinned browser or font universe makes the
// committed baselines a function of the box rather than the repo.
function requirePinnedEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") {
		throw new Error(
			`${name} is not set. The visual gate needs the pinned browser and font ` +
				`config from the dev shell; run this under devenv (\`direnv exec . moon run ` +
				`compass-ui:visual-gate\`) rather than a bare shell.`,
		);
	}
	return value;
}

const chromiumPath = requirePinnedEnv("PLAYWRIGHT_CHROMIUM_PATH");
requirePinnedEnv("FONTCONFIG_FILE");

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
		launchOptions: { executablePath: chromiumPath },
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
