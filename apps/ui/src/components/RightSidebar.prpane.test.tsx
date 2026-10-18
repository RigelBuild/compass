import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { RightSidebar } from "./RightSidebar";

// The verdict wrapper keeps an accessible name when its glyph is hidden.
function mountRightSidebar(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<RightSidebar />
			</StoreContext>
		);
	});
	return { store, container };
}

// Open the PR pane over a given fixture issue, then return its rendered verdict
// marks keyed by chip word (`.rv` carries the word on BOTH `data-v` and its
// aria-label, so `data-v` is a name-independent handle to the mark).
function verdictMarks(
	store: AppStore,
	container: HTMLElement,
	issueId: string,
): Map<string, HTMLElement> {
	store.selectIssue(issueId);
	store.setActiveRightTab("pr");
	flush();
	const marks = new Map<string, HTMLElement>();
	for (const el of container.querySelectorAll<HTMLElement>(
		".pr-reviews .review-chip .rv",
	)) {
		const chip = el.getAttribute("data-v");
		if (chip) marks.set(chip, el);
	}
	return marks;
}

// The observable contract a screen reader consumes: the `.rv` wrapper names the
// verdict (role="img" + aria-label = the chip word), and the inner glyph is
// hidden so the name is not doubled.
function assertNamedMark(mark: HTMLElement, expectedWord: string): void {
	expect(mark.getAttribute("role")).toBe("img");
	expect(mark.getAttribute("aria-label")).toBe(expectedWord);
	const svg = mark.querySelector("svg");
	expect(svg).not.toBeNull();
	expect(svg?.getAttribute("aria-hidden")).toBe("true");
}

describe("RightSidebar PR pane — verdict mark accessible name", () => {
	// selectIssue/pin state write through to the process-wide happy-dom
	// localStorage; clear it around every case (the sibling suite's discipline).
	beforeEach(() => globalThis.localStorage.clear());
	afterEach(() => globalThis.localStorage.clear());

	// ws-1022 (RIG-1022 / PR #453) carries bot reviews greptile→approved,
	// cubic→approved, CodeRabbit→commented — the latest-per-author collapse
	// leaves both `approved` and `commented` chips on the pane.
	test("approved and commented marks are named for a screen reader", () => {
		const { store, container } = mountRightSidebar();
		const marks = verdictMarks(store, container, "ws-1022");
		expect(marks.has("approved")).toBe(true);
		expect(marks.has("commented")).toBe(true);
		assertNamedMark(marks.get("approved") as HTMLElement, "approved");
		assertNamedMark(marks.get("commented") as HTMLElement, "commented");
	});

	// ws-1023 (RIG-1023 / PR #443) carries greptile→changes_requested and
	// CodeRabbit→commented, so it is the reachable source of the `changes` chip
	// (VERDICT_CHIP maps "changes_requested" → "changes").
	test("the changes mark is named for a screen reader", () => {
		const { store, container } = mountRightSidebar();
		const marks = verdictMarks(store, container, "ws-1023");
		expect(marks.has("changes")).toBe(true);
		assertNamedMark(marks.get("changes") as HTMLElement, "changes");
	});
});
