import { expect, test } from "@playwright/test";

// Dev-boot smoke gate. `vite dev` can serve a broken app (blank page, render() never runs)
// while build/unit/typecheck stay green, because they exercise the production path, not the
// dev-serving path where the defect lives (the `development` export condition only `vite dev`
// applies). This spec boots the real fixture-mode dev server and requires the app to come up.

// Two clauses: (a) the `.bridge` root surface becomes visible — the sound mount check, since
// renderBootError paints into #root so "#root non-empty" false-greens; (b) zero pageerror
// over the load. The listener attaches BEFORE goto and the wait polls `.bridge`-visible OR a
// captured error, so the defect this exists for (graph dies before mount) names its cause.
test("dev boot — app mounts with no page errors", async ({ page }) => {
	const pageErrors: Error[] = [];
	page.on("pageerror", (error: Error) => {
		pageErrors.push(error);
	});

	await page.goto("/#/");

	// Wait for mount, failing fast (and by name) the instant a pageerror is
	// captured — see the failure-path note above. The poll ceiling matches a
	// plain `.bridge` wait so a slow cold dev-server compile is not a false red.
	await expect
		.poll(
			async () => {
				if (pageErrors.length > 0) return "errored";
				return (await page.locator(".bridge").isVisible())
					? "mounted"
					: "pending";
			},
			{
				timeout: 30_000,
				message: "app did not mount (.bridge never became visible)",
			},
		)
		.not.toBe("pending");
	if (pageErrors.length > 0) {
		throw new Error(
			`dev boot failed before mount — page error(s):\n${pageErrors
				.map((error) => error.message)
				.join("\n")}`,
		);
	}

	const rootHtml = await page.locator("#root").innerHTML();
	expect(rootHtml).not.toBe("");
	// Belt-and-suspenders: re-assert no errors after the post-poll `#root` read.
	// The throw guard above already fires on anything captured before the poll
	// resolved; this catches the narrow case of a pageerror landing DURING the
	// innerHTML await on the line above.
	expect(pageErrors).toEqual([]);
});
