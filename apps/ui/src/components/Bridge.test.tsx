import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { prCount, prRows } from "../board";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { STUB_ISSUES } from "../stub-data";
import { Bridge } from "./Bridge";

// Render acceptance spec for the Bridge Issues/PRs tabs (Record B / DL-097). The
// board is now two peer tabs inside one view: the Issues tab is today's board
// unchanged; the PRs tab is a flat one-row-per-OPEN-PR list. The tab is a
// Bridge-local signal (like BoardMode), so it is exercised through the exported
// Bridge mounted over a real store — the same seam a click uses. These tests
// defend: the tab flip renders PR rows, the tab-label count equals prCount, the
// grouping seg is Issues-only, the cross-link chips move selection + flip tabs,
// and the card's per-check pip strip has collapsed to CI/review badges.
function mountBridge(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore();
		return (
			<StoreContext.Provider value={store}>
				<Bridge />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

// The tab buttons live in the toolbar seg labeled "Board view"; the grouping
// (Swimlanes|Status) seg is labeled "Board grouping".
function tabButtons(container: HTMLElement): HTMLButtonElement[] {
	return [
		...container.querySelectorAll<HTMLButtonElement>(
			'[aria-label="Board view"] button',
		),
	];
}
const groupingSeg = (container: HTMLElement): HTMLElement | null =>
	container.querySelector('[aria-label="Board grouping"]');
const clickTab = (container: HTMLElement, label: "Issues" | "PRs"): void => {
	const btn = tabButtons(container).find((b) =>
		(b.textContent ?? "").startsWith(label),
	);
	if (!btn) throw new Error(`tab ${label} not found`);
	fireEvent.click(btn);
};

describe("Bridge Issues/PRs tabs (DL-097)", () => {
	test("defaults to the Issues tab: grouping seg shown, no PR rows", () => {
		const { container } = mountBridge();
		expect(groupingSeg(container)).not.toBeNull();
		expect(container.querySelectorAll(".pr-row")).toHaveLength(0);
	});

	test("the PRs tab label carries the open-PR count (prCount)", () => {
		const { container } = mountBridge();
		const prTab = tabButtons(container).find((b) =>
			(b.textContent ?? "").startsWith("PRs"),
		);
		expect(prTab?.textContent).toContain(
			String(prCount(STUB_ISSUES, undefined)),
		);
	});

	test("flipping to PRs renders one row per open PR and hides the grouping seg", () => {
		const { container } = mountBridge();
		clickTab(container, "PRs");
		expect(container.querySelectorAll(".pr-row")).toHaveLength(
			prRows(STUB_ISSUES).length,
		);
		expect(groupingSeg(container)).toBeNull();
	});

	test("a PR row's issueKey chip selects the issue and flips back to Issues", () => {
		const { store, container } = mountBridge();
		clickTab(container, "PRs");
		const firstRowIssueId = prRows(STUB_ISSUES)[0]?.issue.id;
		const chip = container.querySelector<HTMLElement>(".pr-row-issue");
		if (!chip) throw new Error("no issueKey chip");
		fireEvent.click(chip);
		expect(store.selectedIssueId()).toBe(firstRowIssueId);
		expect(groupingSeg(container)).not.toBeNull();
	});

	test("a PR row body click selects its issue without leaving the PRs tab", () => {
		const { store, container } = mountBridge();
		clickTab(container, "PRs");
		const firstRowIssueId = prRows(STUB_ISSUES)[0]?.issue.id;
		const row = container.querySelector<HTMLElement>(".pr-row");
		if (!row) throw new Error("no PR row");
		fireEvent.click(row);
		expect(store.selectedIssueId()).toBe(firstRowIssueId);
		// Still on the PRs tab: rows remain, grouping seg absent.
		expect(container.querySelectorAll(".pr-row").length).toBeGreaterThan(0);
		expect(groupingSeg(container)).toBeNull();
	});
});

describe("Bridge card badges (Record B §3)", () => {
	test("issue cards render CI/review badges, not a per-check pip strip", () => {
		const { container } = mountBridge();
		// The card pip strip is gone from the board (the RightSidebar CheckRuns
		// pane, which keeps its pips, is not mounted here).
		expect(container.querySelectorAll(".check-pips")).toHaveLength(0);
		expect(container.querySelectorAll(".ci-badge").length).toBeGreaterThan(0);
		expect(container.querySelectorAll(".review-badge").length).toBeGreaterThan(
			0,
		);
	});

	test("a card PR chip is a link that selects the issue and flips to the PRs tab", () => {
		const { store, container } = mountBridge();
		const chip = container.querySelector<HTMLElement>('.card-pr[role="link"]');
		if (!chip) throw new Error("no interactive card PR chip");
		fireEvent.click(chip);
		expect(store.selectedIssueId()).not.toBeNull();
		// Flipped to the PRs tab: grouping seg hidden, PR rows shown.
		expect(groupingSeg(container)).toBeNull();
		expect(container.querySelectorAll(".pr-row").length).toBeGreaterThan(0);
	});
});
