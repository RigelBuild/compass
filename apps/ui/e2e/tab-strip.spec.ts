import { expect, test } from "@playwright/test";

// In-window view tabs: a channel tab and an agent tab, a composer draft that
// survives a switch (inactive tabs stay mounted), and both tabs restored after
// a reload (the layout persists in sessionStorage).

const TOPIC_PATH = "/channel/ch-svc-compass/topic/top-compass-acp";

test("channel and agent tabs keep a draft across a switch and survive reload", async ({
	page,
}) => {
	await page.goto(`/#${TOPIC_PATH}`);
	const tabs = page.locator('.cx-tab-strip [role="tab"]');
	const composer = page.locator(".conv-composer input.field");
	await expect(composer).toBeVisible();
	await composer.fill("half-typed reply");

	// Middle-click the agent in the sidebar tree: it opens in a new tab.
	await page
		.locator(".tree-agent", { hasText: "compass-ui" })
		.first()
		.click({ button: "middle" });
	await expect(tabs).toHaveCount(2);
	await tabs.nth(1).click();
	await expect(page.locator(".agent-view")).toBeVisible();
	await expect(composer).toBeHidden();

	await tabs.nth(0).click();
	await expect(composer).toBeVisible();
	await expect(composer).toHaveValue("half-typed reply");

	await page.reload();
	await expect(tabs).toHaveCount(2);
	await expect(tabs.nth(0)).toContainText("ACP seam review");
	await expect(tabs.nth(1)).toContainText("compass-ui");
});
test("composer keeps W text; W N opens a tab and W X closes it", async ({
	page,
}) => {
	await page.goto(`/#${TOPIC_PATH}`);
	const tabs = page.locator('.cx-tab-strip [role="tab"]');
	const composer = page.locator(".conv-composer input.field");
	await expect(composer).toBeVisible();
	await expect(tabs).toHaveCount(1);
	await composer.focus();
	await page.keyboard.type("wn");
	await expect(composer).toHaveValue("wn");
	await expect(tabs).toHaveCount(1);

	await tabs.first().click();
	await page.keyboard.press("w");
	await page.keyboard.press("n");
	await expect(tabs).toHaveCount(2);
	await expect(tabs.nth(1)).toContainText("Bridge");

	await page.keyboard.press("w");
	await page.keyboard.press("x");
	await expect(tabs).toHaveCount(1);
	await expect(tabs.nth(0)).toContainText("ACP seam review");
});
