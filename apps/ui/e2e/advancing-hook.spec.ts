import type { Page } from "@playwright/test";
import { expect, test } from "@playwright/test";

// Advancing-card dormant hook (RIG-2111 T5; design record G10 / D2 / F12).
//
// The board paints a blue chase-light on the one card mid-advance via
// `.bridge-cell > .cx-card[data-advancing="1"]::after`. No live data sets the
// flag yet (the Issue model carries no transition timestamp), so the hook ships
// dormant — this spec is the ONLY thing that exercises it, by toggling the
// attribute the way a future store accessor will.
//
// These are computed-style + stylesheet assertions, not screenshots: the
// affordance is an animation, and a committed pixel of a moving bar is timing-
// dependent (it would break the smoke harness's byte-stable contract). What is
// deterministic — and what actually regresses if someone strips the F12 guard —
// is (a) the ::after renders and runs `card-advance` under normal motion, and
// (b) it is display:none under BOTH reduced-motion triggers. That guard is
// load-bearing: `--cx-pulse-period` is zeroed under reduced motion, so without
// hiding ::after a 0s-infinite animation would spin in place.
//
// NOTE: this suite runs via the `test:visual` playwright harness, not the moon
// `compass-ui:ci` battery (which is typecheck/build/test/stylelint) — so it is a
// local / on-demand guard, not part of the required GHA `CI` gate.

const SEL = ".bridge-cell > .cx-card";

async function markFirstCardAdvancing(page: Page): Promise<void> {
	await page.goto("/#/");
	await page.locator(".bridge").waitFor({ state: "visible" });
	await page.locator(SEL).first().waitFor({ state: "visible" });
	await page.evaluate((sel) => {
		document.querySelector(sel)?.setAttribute("data-advancing", "1");
	}, SEL);
}

// The committed config sets reducedMotion: "reduce" globally; override to
// no-preference so the OS-media path does not pre-empt the render assertion.
test.describe("advancing hook — normal motion", () => {
	test.use({ reducedMotion: "no-preference" });

	test("renders the chase-light and runs the card-advance keyframe", async ({
		page,
	}) => {
		await markFirstCardAdvancing(page);
		const after = await page.evaluate((sel) => {
			const el = document.querySelector(sel);
			if (!el) return null;
			const cs = getComputedStyle(el, "::after");
			return {
				content: cs.content,
				display: cs.display,
				name: cs.animationName,
			};
		}, SEL);
		expect(after).not.toBeNull();
		// ::after exists (content is set, not "none") and is not hidden.
		expect(after?.content).not.toBe("none");
		expect(after?.display).not.toBe("none");
		// The chase-light keyframe is wired (tokenized period/easing, no literal).
		expect(after?.name).toBe("card-advance");
	});

	test("a card without the flag paints no chase-light", async ({ page }) => {
		await page.goto("/#/");
		await page.locator(".bridge").waitFor({ state: "visible" });
		await page.locator(SEL).first().waitFor({ state: "visible" });
		const name = await page.evaluate((sel) => {
			const el = document.querySelector(sel);
			return el ? getComputedStyle(el, "::after").animationName : null;
		}, SEL);
		// No data-advancing → no ::after animation (dormant by default).
		expect(name).not.toBe("card-advance");
	});
});

test.describe("advancing hook — reduced motion (F12 guard)", () => {
	test("the manual data-reduce=on mirror hides the chase-light", async ({
		page,
	}) => {
		await markFirstCardAdvancing(page);
		const display = await page.evaluate((sel) => {
			document.documentElement.setAttribute("data-reduce", "on");
			const el = document.querySelector(sel);
			return el ? getComputedStyle(el, "::after").display : null;
		}, SEL);
		// Load-bearing: the period is zeroed here, so ::after MUST be hidden or a
		// 0s-infinite animation spins in place. Do not strip this guard.
		expect(display).toBe("none");
	});

	test.describe("under prefers-reduced-motion emulation", () => {
		test.use({ reducedMotion: "reduce" });
		test("the OS media-query guard hides the chase-light", async ({ page }) => {
			await markFirstCardAdvancing(page);
			// Skip if the browser did not actually apply the emulation (the media
			// feature is what the guard keys on; asserting against a non-emulated
			// browser would be meaningless).
			const emulated = await page.evaluate(
				() => matchMedia("(prefers-reduced-motion: reduce)").matches,
			);
			test.skip(!emulated, "reduced-motion media emulation not applied");
			const display = await page.evaluate((sel) => {
				const el = document.querySelector(sel);
				return el ? getComputedStyle(el, "::after").display : null;
			}, SEL);
			expect(display).toBe("none");
		});
	});
});
