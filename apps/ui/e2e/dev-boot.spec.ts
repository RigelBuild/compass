import { expect, test } from "@playwright/test";

// Dev-boot smoke gate. `vite dev` can serve a completely broken app — blank
// page, render() never runs — while every build/unit/typecheck task stays
// green, because those exercise the production build path, not the dev-serving
// path where the defect class lives (the `development` export condition that
// only `vite dev` applies). This spec is the one instrument that reads the
// SERVED module graph: it boots the real fixture-mode dev server (the shared
// webServer block in playwright.config.ts), loads the page, and requires the
// app to actually come up.
//
// Two clauses, both load-bearing:
//  (a) the `.bridge` root surface becomes visible — the same mount selector the
//      visual harness keys on (e2e/visual-smoke.spec.ts:22). This is the sound
//      mount check: renderBootError paints the caught-error screen INTO #root,
//      so a bare "#root is non-empty" test false-greens on a caught boot
//      failure, whereas `.bridge` visibility only ever means the app mounted.
//  (b) zero pageerror events over the whole load — catches a mount that renders
//      and then throws asynchronously (a deferred chunk explodes after mount),
//      which clause (a) alone would miss.
// `#root` non-empty is asserted too, but only as a cheap redundant secondary
// guard given (a) — never a co-equal clause.
//
// Failure-path ordering: the pageerror listener attaches BEFORE goto (so an
// error from initial module evaluation is captured), and the mount wait below
// polls for `.bridge` visible OR a captured pageerror — whichever comes first.
// This matters because the exact defect class this gate exists for — the module
// graph dies before mount, so `.bridge` never appears — throws its pageerror
// DURING module evaluation (before the wait), so a plain `.bridge` wait would
// surface an undiagnostic 30s locator timeout that never names the broken
// module. Failing the wait on the captured error instead makes every red name
// the cause.
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
	expect(pageErrors).toEqual([]);
});
