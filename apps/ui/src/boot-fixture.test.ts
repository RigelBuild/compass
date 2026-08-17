// T2 unit coverage for the offline fixture boot. The happy-dom global DOM is
// installed by the bun test preload (test-setup.ts), so a real `document` and
// `HTMLElement` are available to mount the shell against.

import { afterEach, describe, expect, test } from "bun:test";
import { bootFixture, FIXTURE_SENTINEL } from "./boot-fixture";
import { STUB_ISSUES } from "./stub-data";

// import.meta.env is the process-wide Vite env object, mutable at runtime (the
// same handle provider.test.ts writes). PROD is the build-time flag the tripwire
// reads; stub it around the throw test and restore it so nothing leaks into a
// sibling suite.
type MutableEnv = Record<string, unknown>;

describe("bootFixture (offline fixture boot)", () => {
	const env = import.meta.env as MutableEnv;
	let priorProd: unknown;

	afterEach(() => {
		env.PROD = priorProd;
	});

	// Drain the microtask queue so Solid's render effects flush before a read.
	const flush = async (): Promise<void> => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};

	test("renders the board seeded from STUB_ISSUES", async () => {
		const root = document.createElement("div");
		document.body.appendChild(root);
		try {
			bootFixture(root);
			await flush();

			// The board renders IssueCards from the clientless store's STUB_ISSUES
			// seed. A `.card` element proves the shell mounted with fixture content.
			expect(root.querySelector(".card")).not.toBeNull();

			// And a known fixture issue's title is in the rendered text — the board
			// is populated from the fixtures, not merely a mounted-but-empty shell.
			const firstTitle = STUB_ISSUES[0]?.title;
			expect(firstTitle).toBeDefined();
			if (firstTitle) {
				expect(root.textContent).toContain(firstTitle);
			}
		} finally {
			root.remove();
		}
	});

	test("throws in a production build (PROD tripwire)", () => {
		priorProd = env.PROD;
		env.PROD = true;

		const root = document.createElement("div");
		expect(() => bootFixture(root)).toThrow(FIXTURE_SENTINEL);
	});
});
