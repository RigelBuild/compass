import { test } from "@playwright/test";

// Visual-smoke harness (SEA-2034 T1). Navigates the HashRouter surfaces of the
// stub-data app and writes one full-page PNG per surface into e2e/__screens__/
// for Matt's before/after review. Each capture awaits a stable per-surface
// selector (never a fixed sleep) so the shot is taken after content renders.
//
// A real stub agent id (acc-cook) drives the /#/agent/:agentId route; it is
// defined in src/stub-data.ts (STUB_AGENTS).

const SCREENS = "e2e/__screens__";
const AGENT_ID = "acc-cook";

test.describe("visual smoke — legacy-palette baseline", () => {
	test("bridge board", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		await page.locator(".state-dot").first().waitFor({ state: "visible" });
		await page.screenshot({ path: `${SCREENS}/bridge.png`, fullPage: true });
	});

	test("right sidebar — PR pane", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		const sidebar = page.locator("aside.right");
		await sidebar.waitFor({ state: "visible" });
		// Cropped clip of the right sidebar region so the PR pane reads clearly.
		await sidebar.screenshot({ path: `${SCREENS}/right-sidebar.png` });
	});

	test("agent view — trace + composer", async ({ page }) => {
		await page.goto(`/#/agent/${AGENT_ID}`);
		await page.locator(".agent-view").waitFor({ state: "visible" });
		await page.locator(".av-body").waitFor({ state: "visible" });
		await page.screenshot({ path: `${SCREENS}/agent.png`, fullPage: true });
	});

	test("backlog", async ({ page }) => {
		await page.goto("/#/backlog");
		await page.locator(".backlog-view").waitFor({ state: "visible" });
		await page.screenshot({ path: `${SCREENS}/backlog.png`, fullPage: true });
	});

	test("done", async ({ page }) => {
		await page.goto("/#/done");
		await page.locator(".done-view").waitFor({ state: "visible" });
		await page.screenshot({ path: `${SCREENS}/done.png`, fullPage: true });
	});

	test("settings", async ({ page }) => {
		await page.goto("/#/settings");
		await page.locator(".settings-view").waitFor({ state: "visible" });
		await page.screenshot({ path: `${SCREENS}/settings.png`, fullPage: true });
	});

	test("state-dot close-up", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		const dot = page.locator(".state-dot").first();
		await dot.waitFor({ state: "visible" });
		// Cropped close-up clip of a single state dot.
		await dot.screenshot({ path: `${SCREENS}/state-dot.png` });
	});
});
