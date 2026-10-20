import { expect, test } from "@playwright/test";

// The human-shaped path against a real stack: start a topic in the agent's home
// channel, open it, and see the agent's reply render. Inputs come from go/e2e.

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") {
		throw new Error(`${name} is required for the live UI tier`);
	}
	return value;
}

test("a topic posted from the UI renders the agent's reply", async ({
	page,
}) => {
	test.setTimeout(90_000);
	const pageErrors: Error[] = [];
	page.on("pageerror", (error: Error) => {
		pageErrors.push(error);
	});

	const channelId = requiredEnv("UI_TIER_HOME_CHANNEL_ID");
	const topicName = requiredEnv("UI_TIER_TOPIC_NAME");
	const triggerText = requiredEnv("UI_TIER_TRIGGER_TEXT");
	const expectedReply = requiredEnv("UI_TIER_EXPECTED_REPLY");

	await page.goto(`/#/channel/${encodeURIComponent(channelId)}`);
	// A fresh profile gets the first-run tour, whose dialog covers the page.
	await page.getByRole("button", { name: "Skip tour" }).click();
	await page.locator(".new-topic-name").fill(topicName);
	await page.locator(".new-topic-message").fill(triggerText);
	await page.locator(".new-topic-start").click();

	const topicRow = page.locator(".topic-row", { hasText: topicName });
	await expect(topicRow).toBeVisible();
	await topicRow.click();

	// The reply needs a full agent turn (container, canned model, tool call), so
	// it gets a longer ceiling than Playwright's 5s default, under the test's 90s.
	const agentReply = page
		.locator('.conv-stream .msg[data-kind="agent"] .markdown-content')
		.filter({ hasText: expectedReply });
	await expect(agentReply).toBeVisible({ timeout: 60_000 });
	expect(pageErrors).toEqual([]);
});
