import { expect, test } from "@playwright/test";

// Two-pane split: W V splits the channel tab, the second pane navigates to an
// agent, and sending from the first pane's composer leaves the second alone.

const TOPIC_PATH = "/channel/ch-svc-compass/topic/top-compass-acp";

test("W V splits a tab; a send in the first pane leaves the second pane put", async ({
	page,
}) => {
	await page.goto(`/#${TOPIC_PATH}`);
	const tabs = page.locator('.cx-tab-strip [role="tab"]');
	const panels = page.locator(".cx-split-pane:not([hidden]) > .view-panel");
	await expect(page.locator(".conv-composer input.field")).toBeVisible();

	await tabs.first().click();
	await page.keyboard.press("w");
	await page.keyboard.press("v");
	await expect(panels).toHaveCount(2);
	await expect(tabs).toHaveCount(1);
	await expect(page.locator('[role="separator"]')).toBeVisible();
	const [first, second] = [panels.nth(0), panels.nth(1)];
	const firstBox = await first.boundingBox();
	const secondBox = await second.boundingBox();
	expect(firstBox?.y).toBe(secondBox?.y);
	expect(firstBox?.x ?? 0).toBeLessThan(secondBox?.x ?? 0);
	// The two panes and the 1px splitter fill main between them.
	const fill = await page.evaluate(() => {
		const box = document.querySelector(".cx-split-pane:not([hidden])");
		const parts = [...(box?.children ?? [])].map(
			(el) => el.getBoundingClientRect().width,
		);
		return {
			sum: parts.reduce((a, b) => a + b, 0),
			main: document.querySelector(".main")?.clientWidth ?? 0,
		};
	});
	expect(Math.abs(fill.sum - fill.main)).toBeLessThanOrEqual(2);

	// The new second pane holds focus, so the sidebar navigates it alone.
	await expect(second).toHaveAttribute("data-focused", "");
	await page.locator(".tree-agent", { hasText: "compass-ui" }).first().click();
	await expect(second.locator(".agent-view")).toBeVisible();
	await expect(first.locator(".conv-composer input.field")).toBeVisible();

	const composer = first.locator(".conv-composer input.field");
	await composer.click();
	await expect(first).toHaveAttribute("data-focused", "");
	await composer.fill("from the first pane");
	await composer.press("Enter");

	await expect(first.locator(".conv-composer")).toBeVisible();
	await expect(second.locator(".agent-view")).toBeVisible();
	await expect(second.locator(".conv-composer")).toHaveCount(0);
	await expect(first).toHaveAttribute("data-focused", "");
});
