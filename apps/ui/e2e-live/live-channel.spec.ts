import { expect, test } from "@playwright/test";

// A channel that first appears while the page is open: go/e2e adds the caller
// after this spec prints the ready marker, so the channel is not in the snapshot.

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") {
		throw new Error(`${name} is required for the live UI tier`);
	}
	return value;
}

test("a channel that appears live shows its earlier history", async ({
	page,
}) => {
	test.setTimeout(90_000);
	const pageErrors: Error[] = [];
	page.on("pageerror", (error: Error) => {
		pageErrors.push(error);
	});

	const channelName = requiredEnv("UI_TIER_LIVE_CHANNEL");
	const topicName = requiredEnv("UI_TIER_LIVE_TOPIC");
	const earlierText = requiredEnv("UI_TIER_LIVE_EARLIER_TEXT");
	const readyMarker = requiredEnv("UI_TIER_READY_MARKER");
	const agentHandle = requiredEnv("UI_TIER_LIVE_AGENT");

	await page.goto("/");
	await page.getByRole("button", { name: "Skip tour" }).click();
	// The seeded supervisor is listed, so the snapshot has loaded without our channel.
	await expect(page.locator(".tree-agent-row .name").first()).toBeVisible();
	const channelRow = page.locator(".ch-row .ch-name", { hasText: channelName });
	await expect(channelRow).toHaveCount(0);
	await expect(
		page.locator(".tree-agent-row .name", { hasText: agentHandle }),
	).toHaveCount(0);
	// go/e2e reads this line from stdout and only then adds the caller.
	process.stdout.write(`${readyMarker}\n`);

	await expect(channelRow).toBeVisible({ timeout: 30_000 });
	await channelRow.click();
	// The channel row lands before its async history backfill completes.
	const topicRow = page.locator(".topic-row", { hasText: topicName });
	await expect(topicRow).toBeVisible({ timeout: 15_000 });
	await topicRow.click();
	await expect(
		page.locator(".msg .markdown-content", { hasText: earlierText }),
	).toBeVisible({ timeout: 15_000 });

	// An agent created while the page is open gets its home channel without a reload.
	const agentFolder = page.locator(".folder", {
		has: page.locator(".tree-agent-row .name", { hasText: agentHandle }),
	});
	await expect(
		agentFolder.locator(".ch-row .ch-name", { hasText: agentHandle }),
	).toBeVisible({ timeout: 30_000 });
	expect(pageErrors).toEqual([]);
});
