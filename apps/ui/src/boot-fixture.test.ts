// T2 unit coverage for the offline fixture boot. The happy-dom global DOM is
// installed by the bun test preload (test-setup.ts), so a real `document` and
// `HTMLElement` are available to mount the shell against.

import { describe, expect, test } from "bun:test";
import { TourOutcome } from "@compass/client";
import {
	bootFixture,
	createMemoryTourClient,
	FIXTURE_SENTINEL,
} from "./boot-fixture";
import { STUB_ISSUES } from "./stub-data";

// import.meta.env is the process-wide Vite env object, mutable at runtime (the
// same handle provider.test.ts writes). PROD is the build-time flag the tripwire
// reads; the throw test stubs it inside a local try/finally that restores the
// true original, so nothing leaks into a sibling suite.
type MutableEnv = Record<string, unknown>;

describe("bootFixture (offline fixture boot)", () => {
	const env = import.meta.env as MutableEnv;

	// Drain the microtask queue so Solid's render effects flush before a read.
	const flush = async (): Promise<void> => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};

	test("renders the board seeded from STUB_ISSUES", async () => {
		const root = document.createElement("div");
		document.body.appendChild(root);
		let dispose: (() => void) | undefined;
		try {
			dispose = bootFixture(root);
			await flush();

			// The board renders IssueCards from the clientless store's STUB_ISSUES
			// seed. A `.cx-card` element proves the shell mounted with fixture content.
			expect(root.querySelector(".cx-card")).not.toBeNull();

			// And a known fixture issue's title is in the rendered text — the board
			// is populated from the fixtures, not merely a mounted-but-empty shell.
			const firstTitle = STUB_ISSUES[0]?.title;
			expect(firstTitle).toBeDefined();
			if (firstTitle) {
				expect(root.textContent).toContain(firstTitle);
			}
		} finally {
			// Dispose the reactive root so App's onCleanup runs the keymap
			// uninstaller — otherwise the App-root window `keydown` listener leaks
			// onto the shared happy-dom window and fires during a sibling suite.
			dispose?.();
			root.remove();
		}
	});

	test("throws in a production build (PROD tripwire)", () => {
		const priorProd = env.PROD;
		env.PROD = true;
		try {
			const root = document.createElement("div");
			expect(() => bootFixture(root)).toThrow(FIXTURE_SENTINEL);
		} finally {
			env.PROD = priorProd;
		}
	});
});

describe("createMemoryTourClient (fixture tour state)", () => {
	// One load claims once; a second claim in the same load loses, so the
	// fixture shows the tour at most once per page load.
	test("claims once, then reads back what was written", async () => {
		const client = createMemoryTourClient();
		expect((await client.getTourState({})).outcome).toBe(
			TourOutcome.UNSPECIFIED,
		);
		expect(await client.claimTourStart({ stepId: "welcome" })).toEqual({
			claimed: true,
		});
		expect(await client.claimTourStart({ stepId: "welcome" })).toEqual({
			claimed: false,
		});
		await client.setTourState({
			outcome: TourOutcome.DISMISSED,
			stepId: "board",
		});
		expect(await client.getTourState({})).toEqual({
			outcome: TourOutcome.DISMISSED,
			stepId: "board",
		});
	});
});
