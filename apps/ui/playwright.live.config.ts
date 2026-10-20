import { defineConfig, devices } from "@playwright/test";

// The live UI tier: the default-mode (non-fixture) Vite app dials a real stack's door.
// go/e2e TestUITierPostAndReply starts that stack and runs this config.

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") {
		throw new Error(`${name} is required for the live UI tier`);
	}
	return value;
}

const chromiumPath = requiredEnv("PLAYWRIGHT_CHROMIUM_PATH");
const fontconfigPath = requiredEnv("E2E_FONTCONFIG_FILE");
requiredEnv("VITE_COMPASS_BASE_URL");
// The door's single CORS origin is this exact host and port, so never use localhost.
const devPort = Number(requiredEnv("PLAYWRIGHT_DEV_PORT"));
const baseURL = `http://127.0.0.1:${devPort}`;

export default defineConfig({
	testDir: "./e2e-live",
	outputDir: "./e2e-live/.output",
	fullyParallel: false,
	reporter: [["list"]],
	captureGitInfo: { commit: false, diff: false },
	use: {
		baseURL,
		headless: true,
		screenshot: "off",
		reducedMotion: "reduce",
		// The fixture door serves a self-signed certificate from its own CA.
		ignoreHTTPSErrors: true,
		launchOptions: {
			executablePath: chromiumPath,
			env: { ...process.env, FONTCONFIG_FILE: fontconfigPath },
		},
	},
	projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
	webServer: {
		command: `bunx vite --host 127.0.0.1 --port ${devPort} --strictPort`,
		url: baseURL,
		reuseExistingServer: false,
		timeout: 120_000,
	},
});
