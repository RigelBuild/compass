import { afterEach, describe, expect, jest, test } from "bun:test";
import { TourOutcome } from "@compass/client";
import { cleanup, fireEvent } from "@solidjs/testing-library";
import type { TourClient } from "../store";
import { flush, mountApp } from "../test-router";
import { TOUR_STEPS } from "../tour/state";
import { ANCHOR_WAIT_MS } from "./TourOverlay";

afterEach(cleanup);

// Drains the boot read + claim chain and the serialized tour-write chain.
async function settle(): Promise<void> {
	for (let i = 0; i < 5; i++) await flush();
}

// The spotlight recomputes its cutout in one rAF callback per burst.
const nextFrame = (): Promise<void> =>
	new Promise((resolve) => requestAnimationFrame(() => resolve()));

// Kobalte attaches its outside-pointer listener in a zero-delay timer, so the
// test yields one macrotask; this gates on that attach, not on elapsed time.
const outsideListenerAttached = (): Promise<void> =>
	// biome-ignore lint/style/noRestrictedGlobals: matches the library's own setTimeout(0) attach.
	new Promise((resolve) => setTimeout(resolve, 0));

function tourClient(outcome = TourOutcome.UNSPECIFIED, stepId = "") {
	const writes: { outcome: TourOutcome; stepId: string }[] = [];
	const client: TourClient = {
		getTourState: async () => ({ outcome, stepId }),
		claimTourStart: async () => ({ claimed: true }),
		setTourState: async (write) => {
			writes.push(write);
			return {};
		},
	};
	return { client, writes };
}

async function startCallout(path = "/", options: { tour?: TourClient } = {}) {
	const mounted = mountApp(path, options);
	mounted.store.tour.start("replay");
	await flush();
	mounted.store.tour.next();
	await flush();
	return mounted;
}

function callout(container: HTMLElement): HTMLElement | null {
	return (
		container.querySelector<HTMLElement>(".cx-tour-callout") ??
		document.querySelector<HTMLElement>(".cx-tour-callout")
	);
}

describe("TourOverlay", () => {
	test("a successful first-run claim opens the welcome dialog", async () => {
		const fake = tourClient();
		const { store, container } = mountApp("/", {
			tour: fake.client,
			claimFirstRun: true,
		});
		await settle();
		expect(store.tour.shouldAutoStart()).toBe(false);
		expect(store.tour.open()).toBe(true);
		expect(
			container.querySelector('[role="dialog"][aria-label="Compass tour"] h2')
				?.textContent,
		).toBe("Welcome to Compass");
	});

	test("dialog traps focus and Escape restores it without a permanent write", async () => {
		const fake = tourClient();
		const { store, container } = mountApp("/", { tour: fake.client });
		const returnFocus = container.querySelector<HTMLElement>('[role="tab"]');
		if (!returnFocus) throw new Error("no view tab");
		returnFocus.focus();
		store.tour.start("replay");
		await flush();
		const dialog = container.querySelector<HTMLElement>(
			'[role="dialog"][aria-label="Compass tour"]',
		);
		if (!dialog) throw new Error("no tour dialog");
		expect(dialog.getAttribute("aria-modal")).toBe("true");
		const controls = [...dialog.querySelectorAll<HTMLElement>("button")];
		const first = controls[0];
		const last = controls[controls.length - 1];
		if (!first || !last) throw new Error("tour dialog has no controls");
		last.focus();
		fireEvent.keyDown(dialog, { key: "Tab" });
		expect(document.activeElement).toBe(first);
		fireEvent.keyDown(dialog, { key: "Escape" });
		await settle();
		expect(store.tour.open()).toBe(false);
		expect(document.activeElement).toBe(returnFocus);
		expect(
			fake.writes.some(
				(write) =>
					write.outcome === TourOutcome.DISMISSED ||
					write.outcome === TourOutcome.COMPLETED,
			),
		).toBe(false);
	});

	test("callout anchors to its element and positions the spotlight cutout", async () => {
		const { container } = await startCallout();
		const anchor = container.querySelector<HTMLElement>(
			'[data-tour="board-grid"]',
		);
		if (!anchor) throw new Error("board anchor did not render");
		anchor.getBoundingClientRect = () => new DOMRect(20, 30, 400, 200);
		fireEvent(window, new Event("resize"));
		await nextFrame();
		const layer = document.querySelector<HTMLElement>(".cx-tour-spotlight");
		expect(callout(container)).not.toBeNull();
		expect(layer?.style.getPropertyValue("--cx-tour-cutout-x")).toBe("12px");
		expect(layer?.style.getPropertyValue("--cx-tour-cutout-y")).toBe("22px");
		expect(layer?.style.getPropertyValue("--cx-tour-cutout-width")).toBe(
			"416px",
		);
		expect(layer?.style.getPropertyValue("--cx-tour-cutout-height")).toBe(
			"216px",
		);
	});

	test("a callout waits for its anchor after navigation", async () => {
		const { store, container } = await startCallout("/backlog");
		expect(store.view()).toBe("bridge");
		expect(container.querySelector('[data-tour="board-grid"]')).not.toBeNull();
		expect(callout(container)).not.toBeNull();
	});

	test("a missing anchor skips only after the bounded wait", async () => {
		const { client } = tourClient(TourOutcome.STARTED, "sidebar-tree");
		const { store } = mountApp("/", { tour: client });
		await flush();
		store.toggleLeft();
		jest.useFakeTimers();
		try {
			store.tour.start("resume");
			await flush();
			jest.advanceTimersByTime(ANCHOR_WAIT_MS - 1);
			await flush();
			expect(store.tour.stepIndex()).toBe(2);
			jest.advanceTimersByTime(1);
			await flush();
			expect(store.tour.stepIndex()).toBe(3);
		} finally {
			jest.useRealTimers();
		}
	});

	test("Skip tour dismisses and persists DISMISSED", async () => {
		const fake = tourClient();
		const { store, container } = mountApp("/", { tour: fake.client });
		store.tour.start("replay");
		await flush();
		const skip = [...container.querySelectorAll("button")].find((button) =>
			button.textContent?.includes("Skip tour"),
		);
		if (!skip) throw new Error("no Skip tour control");
		fireEvent.click(skip);
		await settle();
		expect(store.tour.open()).toBe(false);
		expect(fake.writes.map((write) => write.outcome)).toContain(
			TourOutcome.DISMISSED,
		);
	});

	test("Escape closes from app focus without a permanent write", async () => {
		const fake = tourClient();
		const { store, container } = await startCallout("/", { tour: fake.client });
		const anchor = container.querySelector<HTMLElement>(
			'[data-tour="board-grid"]',
		);
		if (!anchor) throw new Error("no board anchor");
		anchor.focus();
		fireEvent.keyDown(anchor, { key: "Escape" });
		await settle();
		expect(store.tour.open()).toBe(false);
		expect(
			fake.writes.some(
				(write) =>
					write.outcome === TourOutcome.DISMISSED ||
					write.outcome === TourOutcome.COMPLETED,
			),
		).toBe(false);
	});

	test("outside pointer interaction leaves the callout open", async () => {
		const { store, container } = await startCallout();
		await outsideListenerAttached();
		fireEvent.pointerDown(document.body);
		await flush();
		expect(store.tour.open()).toBe(true);
		expect(callout(container)).not.toBeNull();
	});

	test("shortcuts hides the callout and Escape closes only shortcuts", async () => {
		const { store, container } = await startCallout();
		store.toggleShortcuts();
		await flush();
		expect(callout(container)).toBeNull();
		const shortcuts = container.querySelector<HTMLElement>(
			'[role="dialog"][aria-label="Keyboard shortcuts"]',
		);
		if (!shortcuts) throw new Error("no shortcuts dialog");
		fireEvent.keyDown(shortcuts, { key: "Escape" });
		await flush();
		expect(store.shortcutsOpen()).toBe(false);
		expect(store.tour.open()).toBe(true);
		expect(callout(container)).not.toBeNull();
	});

	test("arrow keys move the tour only from the callout", async () => {
		const { store, container } = await startCallout();
		fireEvent.keyDown(document.body, { key: "ArrowRight" });
		await flush();
		expect(store.tour.stepIndex()).toBe(1);
		const content = callout(container);
		if (!content) throw new Error("no tour callout");
		fireEvent.keyDown(content, { key: "ArrowRight" });
		await flush();
		expect(store.tour.stepIndex()).toBe(2);
		const moved = callout(container);
		if (!moved) throw new Error("no tour callout after advance");
		fireEvent.keyDown(moved, { key: "ArrowLeft" });
		await flush();
		expect(store.tour.stepIndex()).toBe(1);
	});

	test("spotlight is non-interactive and reduced motion preserves content", async () => {
		const { store, container } = await startCallout();
		const layer = document.querySelector<HTMLElement>(".cx-tour-spotlight");
		expect(layer).not.toBeNull();
		expect(layer?.style.pointerEvents).toBe("none");
		document.documentElement.dataset.reduce = "on";
		store.tour.back();
		await flush();
		expect(
			container.querySelector('[role="dialog"][aria-label="Compass tour"]'),
		).not.toBeNull();
		delete document.documentElement.dataset.reduce;
	});

	test("each callout step has a live data-tour anchor", async () => {
		for (const [index, step] of TOUR_STEPS.entries()) {
			if (step.kind !== "callout" || !step.anchor) continue;
			const { store, container } = mountApp(step.route ?? "/");
			store.tour.start("replay");
			await flush();
			for (let i = 0; i < index; i++) {
				store.tour.next();
				await flush();
			}
			expect(
				container.querySelector(`[data-tour="${step.anchor}"]`),
			).not.toBeNull();
			cleanup();
		}
	});

	test("demo agent, issue card, and channel row carry Demo badges", async () => {
		const { store, container } = mountApp();
		store.tour.start("replay");
		await flush();
		expect(
			container.querySelector('[data-tour="demo-agent"] .cx-tour-demo-badge')
				?.textContent,
		).toBe("Demo");
		expect(
			container.querySelector(".cx-card .cx-tour-demo-badge")?.textContent,
		).toBe("Demo");
		expect(
			[...container.querySelectorAll(".ch-row")].some((row) =>
				row.querySelector(".cx-tour-demo-badge"),
			),
		).toBe(true);
	});
});
