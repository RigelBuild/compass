import { test } from "@playwright/test";

// Visual-smoke harness (SEA-2034 T1). Navigates the HashRouter surfaces of the
// app and writes one full-page PNG per surface into e2e/__screens__/ for Matt's
// before/after review. The webServer boots the app under `--mode fixture`, so
// the app boots fully on the in-memory fixture store (stub-data.ts) with no
// daemon on :50051 and no VITE_COMPASS_BASE_URL — offline by construction, not
// by accident of un-wired live paths. Each capture awaits a stable per-surface
// selector (never a fixed sleep) so the shot is taken after content renders,
// and pins determinism (animations disabled, css-scaled raster, fonts settled)
// so the artifact is byte-stable across same-box runs.
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
		await page.evaluate(() => document.fonts.ready);
		await page.screenshot({
			path: `${SCREENS}/bridge.png`,
			fullPage: true,
			animations: "disabled",
			scale: "css",
		});
	});

	test("right sidebar — PR pane", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		const sidebar = page.locator("aside.right");
		await sidebar.waitFor({ state: "visible" });
		// Cropped clip of the right sidebar region so the PR pane reads clearly.
		await page.evaluate(() => document.fonts.ready);
		await sidebar.screenshot({
			path: `${SCREENS}/right-sidebar.png`,
			animations: "disabled",
			scale: "css",
		});
	});

	test("agent view — trace + composer", async ({ page }) => {
		await page.goto(`/#/agent/${AGENT_ID}`);
		await page.locator(".agent-view").waitFor({ state: "visible" });
		await page.locator(".av-body").waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		await page.screenshot({
			path: `${SCREENS}/agent.png`,
			fullPage: true,
			animations: "disabled",
			scale: "css",
		});
	});

	test("backlog", async ({ page }) => {
		await page.goto("/#/backlog");
		await page.locator(".backlog-view").waitFor({ state: "visible" });
		// Wait on a row inside the "Assigned to me" section, not the container or a
		// bare row: Todo/Backlog rows render synchronously, but "Assigned to me"
		// gates on the assignedIssues query microtask (§A3). Only this row
		// guarantees the populated state is captured.
		await page
			.locator("#backlog-section-assigned-to-me .backlog-row")
			.first()
			.waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		await page.screenshot({
			path: `${SCREENS}/backlog.png`,
			fullPage: true,
			animations: "disabled",
			scale: "css",
		});
	});

	test("done", async ({ page }) => {
		await page.goto("/#/done");
		await page.locator(".done-view").waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		await page.screenshot({
			path: `${SCREENS}/done.png`,
			fullPage: true,
			animations: "disabled",
			scale: "css",
		});
	});

	test("settings", async ({ page }) => {
		await page.goto("/#/settings");
		await page.locator(".settings-view").waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		await page.screenshot({
			path: `${SCREENS}/settings.png`,
			fullPage: true,
			animations: "disabled",
			scale: "css",
		});
	});

	test("state-dot close-up", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		const dot = page.locator(".state-dot").first();
		await dot.waitFor({ state: "visible" });
		// Cropped close-up clip of a single state dot.
		await page.evaluate(() => document.fonts.ready);
		await dot.screenshot({
			path: `${SCREENS}/state-dot.png`,
			animations: "disabled",
			scale: "css",
		});
	});

	test("bridge — PRs board", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		// The PRs seg button (Bridge.tsx:154-158) — label "PRs · N", matched on
		// its stable prefix so the live count doesn't perturb the selector.
		await page.getByRole("button", { name: /^PRs/ }).click();
		// Wait for the PRs surface to render (Bridge.tsx:276 `.pr-tab`).
		await page.locator(".pr-tab").waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		await page.screenshot({
			path: `${SCREENS}/bridge-prs.png`,
			fullPage: true,
			animations: "disabled",
			scale: "css",
		});
	});

	test("bridge — column-head strip", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		const heads = page.locator(".swim-colhead");
		await heads.first().waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		// Cropped clip of the column-head strip (tint review): union the bounding
		// boxes of the individual `.swim-colhead` cells into one row rect.
		const boxes = await heads.evaluateAll((els) =>
			els
				.map((el) => el.getBoundingClientRect())
				.map((r) => ({ x: r.x, y: r.y, right: r.right, bottom: r.bottom })),
		);
		const x = Math.min(...boxes.map((b) => b.x));
		const y = Math.min(...boxes.map((b) => b.y));
		const width = Math.max(...boxes.map((b) => b.right)) - x;
		const height = Math.max(...boxes.map((b) => b.bottom)) - y;
		await page.screenshot({
			path: `${SCREENS}/bridge-colheads.png`,
			clip: { x, y, width, height },
			animations: "disabled",
			scale: "css",
		});
	});

	test("bridge — single card close-up", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		const card = page.locator(".card").first();
		await card.waitFor({ state: "visible" });
		await page.evaluate(() => document.fonts.ready);
		// Cropped close-up clip of a single issue card (IssueCard.tsx:47 `.card`).
		await card.screenshot({
			path: `${SCREENS}/bridge-card.png`,
			animations: "disabled",
			scale: "css",
		});
	});
});
