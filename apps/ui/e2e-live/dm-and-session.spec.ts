import { expect, test } from "@playwright/test";

// A DM between two of the caller's agents, posted to before the page loads, and
// the Session Log header of an agent whose session has settled to READY.

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") {
		throw new Error(`${name} is required for the live UI tier`);
	}
	return value;
}

test("a DM shows earlier history and a READY session reads idle", async ({
	page,
}) => {
	test.setTimeout(90_000);
	const pageErrors: Error[] = [];
	page.on("pageerror", (error: Error) => {
		pageErrors.push(error);
	});

	const agentHandle = requiredEnv("UI_TIER_DM_AGENT");
	const topicName = requiredEnv("UI_TIER_DM_TOPIC");
	const earlierText = requiredEnv("UI_TIER_DM_TEXT");

	await page.goto("/");
	await page.getByRole("button", { name: "Skip tour" }).click();

	// The DM's other parties are both agents, so it lists under direct messages.
	const dmRow = page
		.locator(".rail-section", {
			has: page.locator(".rail-section-head", { hasText: "direct messages" }),
		})
		.locator(".ch-row .ch-name", { hasText: agentHandle });
	await expect(dmRow).toBeVisible({ timeout: 15_000 });
	await dmRow.click();
	const topicRow = page.locator(".topic-row", { hasText: topicName });
	await expect(topicRow).toBeVisible({ timeout: 15_000 });
	await topicRow.click();
	await expect(
		page.locator(".msg .markdown-content", { hasText: earlierText }),
	).toBeVisible({ timeout: 15_000 });

	await page
		.locator(".tree-agent-row .name", { hasText: agentHandle })
		.first()
		.click();
	// A session that has not loaded also reads idle; the enabled Stop proves it has.
	await expect(page.locator(".log-panel .obs-stop")).toBeEnabled({
		timeout: 15_000,
	});
	await expect(page.locator(".log-panel .obs-run")).toHaveText("idle");
	expect(pageErrors).toEqual([]);
});
