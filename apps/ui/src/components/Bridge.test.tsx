import { describe, expect, test } from "bun:test";
import { createRouter, memoryHistory } from "@solidjs/router";
import { fireEvent, render } from "@solidjs/testing-library";
import { createRoot, flush as flushSync } from "solid-js";
import App from "../App";
import { prBoardRows, prCount } from "../board";
import { StoreContext } from "../context";
import type { CommandId } from "../keyboard/commands";
import { appRoutes } from "../routes";
import { type AppStore, createAppStore } from "../store";
import { STUB_ISSUES } from "../stub-data";
import { flush, mountApp } from "../test-router";
import { testQueryClient } from "../test-support";
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
		store = createAppStore({ queryClient: testQueryClient() });
		return (
			<StoreContext value={store}>
				<Bridge />
			</StoreContext>
		);
	});
	return { store, container };
}

// Mount the full App shell (store + StoreContext + a Router 2 instance whose
// render-prop child is the App root — the production shape mirror of `mountApp`)
// over a CALLER-PROVIDED store, returning the render's `unmount`. The
// remount-hygiene test uses one shared store across two mounts to observe the
// app-lifetime registry across a mount→unmount→remount cycle.
function mountShell(
	store: AppStore,
	initialPath: string,
): { container: HTMLElement; unmount: () => void } {
	const Router = createRouter({
		routes: appRoutes,
		history: memoryHistory(initialPath),
	});
	const { container, unmount } = render(() => (
		<StoreContext value={store}>
			<Router>{(props) => <App {...props} />}</Router>
		</StoreContext>
	));
	return { container, unmount };
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
	flushSync();
};

describe("Bridge Issues/PRs tabs (DL-097)", () => {
	test("defaults to the Issues tab: grouping seg shown, no PR cards", () => {
		const { container } = mountBridge();
		expect(groupingSeg(container)).not.toBeNull();
		// `.card-issue-link` is unique to PR cards, so its absence proves the PRs
		// board is not mounted (the Issues board's `.cx-card`s have no such chip).
		expect(container.querySelectorAll(".card-issue-link")).toHaveLength(0);
	});

	test("the PRs tab label carries the open-PR count (prCount, open-only)", () => {
		const { container } = mountBridge();
		const prTab = tabButtons(container).find((b) =>
			(b.textContent ?? "").startsWith("PRs"),
		);
		expect(prTab?.textContent).toContain(
			String(prCount(STUB_ISSUES, undefined)),
		);
	});

	test("flipping to PRs renders one card per board PR and hides the grouping seg", () => {
		const { container } = mountBridge();
		clickTab(container, "PRs");
		// The cross-link chip is 1:1 with a PR card, so its count is the PR-card
		// count — and prBoardRows includes merged rows (the Merged column).
		expect(container.querySelectorAll(".card-issue-link")).toHaveLength(
			prBoardRows(STUB_ISSUES).length,
		);
		expect(groupingSeg(container)).toBeNull();
	});

	test("a PR card's issueKey chip selects the issue and flips back to Issues", () => {
		const { store, container } = mountBridge();
		clickTab(container, "PRs");
		const firstRowIssueId = prBoardRows(STUB_ISSUES)[0]?.issue.id;
		const chip = container.querySelector<HTMLElement>(".card-issue-link");
		if (!chip) throw new Error("no issueKey chip");
		fireEvent.click(chip);
		flushSync();
		expect(store.selectedIssueId()).toBe(firstRowIssueId);
		expect(groupingSeg(container)).not.toBeNull();
	});

	test("a PR card body click selects its issue without leaving the PRs tab", () => {
		const { store, container } = mountBridge();
		clickTab(container, "PRs");
		const firstRowIssueId = prBoardRows(STUB_ISSUES)[0]?.issue.id;
		// The card body is the `.cx-card` that owns the first cross-link chip.
		const card = container
			.querySelector<HTMLElement>(".card-issue-link")
			?.closest<HTMLElement>(".cx-card");
		if (!card) throw new Error("no PR card");
		fireEvent.click(card);
		flushSync();
		expect(store.selectedIssueId()).toBe(firstRowIssueId);
		// Still on the PRs tab: cards remain, grouping seg absent.
		expect(
			container.querySelectorAll(".card-issue-link").length,
		).toBeGreaterThan(0);
		expect(groupingSeg(container)).toBeNull();
	});

	test("the issueKey chip activates on Enter (keyboard a11y, DL-097)", () => {
		const { store, container } = mountBridge();
		clickTab(container, "PRs");
		const firstRowIssueId = prBoardRows(STUB_ISSUES)[0]?.issue.id;
		const chip = container.querySelector<HTMLElement>(".card-issue-link");
		if (!chip) throw new Error("no issueKey chip");
		// Enter activates the chip: same effect as a click (select + flip to Issues).
		fireEvent.keyDown(chip, { key: "Enter" });
		flushSync();
		expect(store.selectedIssueId()).toBe(firstRowIssueId);
		expect(groupingSeg(container)).not.toBeNull();
	});

	test("the issueKey chip activates on Space and consumes the default page scroll", () => {
		const { store, container } = mountBridge();
		clickTab(container, "PRs");
		const firstRowIssueId = prBoardRows(STUB_ISSUES)[0]?.issue.id;
		const chip = container.querySelector<HTMLElement>(".card-issue-link");
		if (!chip) throw new Error("no issueKey chip");
		// Space activates AND is consumed: preventDefault makes dispatchEvent return
		// false, so the space key never scrolls the page.
		const notDefaulted = fireEvent.keyDown(chip, { key: " " });
		flushSync();
		expect(notDefaulted).toBe(false);
		expect(store.selectedIssueId()).toBe(firstRowIssueId);
		expect(groupingSeg(container)).not.toBeNull();
	});

	test("the issueKey chip ignores non-activation keys (stays on the PRs tab)", () => {
		const { container } = mountBridge();
		clickTab(container, "PRs");
		const chip = container.querySelector<HTMLElement>(".card-issue-link");
		if (!chip) throw new Error("no issueKey chip");
		// The guard early-returns on any key but Enter/Space: no activation, so no
		// flip — still on the PRs tab (grouping seg absent, cards present).
		fireEvent.keyDown(chip, { key: "a" });
		flushSync();
		expect(groupingSeg(container)).toBeNull();
		expect(
			container.querySelectorAll(".card-issue-link").length,
		).toBeGreaterThan(0);
	});
});

describe("Bridge card badges (Record B §3)", () => {
	test("issue cards render CI/review badges, not a per-check pip strip", () => {
		const { container } = mountBridge();
		// The card pip strip is gone from the board (the RightSidebar CheckRuns
		// pane, which keeps its pips, is not mounted here).
		expect(container.querySelectorAll(".check-pips")).toHaveLength(0);
		expect(
			container.querySelectorAll('.cx-axis-badge[data-axis="ci"]').length,
		).toBeGreaterThan(0);
		expect(
			container.querySelectorAll('.cx-axis-badge[data-axis="review"]').length,
		).toBeGreaterThan(0);
	});

	test("card badges are compact (glyph-only) on both boards", () => {
		const { container } = mountBridge();
		// T3 contract: IssueCard passes `compact` (cramped card gutter → the CI/RV
		// code is hidden, glyph only). The PRs board reuses the same card anatomy,
		// so its PR cards are compact too — defend the attribute on BOTH, a
		// dropped/inverted `compact` on either consumer would otherwise ship green.
		const cardBadge = container.querySelector(".cx-card .cx-axis-badge");
		if (!cardBadge) throw new Error("no card axis badge");
		expect(cardBadge.hasAttribute("data-compact")).toBe(true);
		clickTab(container, "PRs");
		// A PR card = the `.cx-card` owning a `.card-issue-link` chip; its badge is
		// compact too.
		const prCard = container
			.querySelector<HTMLElement>(".card-issue-link")
			?.closest<HTMLElement>(".cx-card");
		if (!prCard) throw new Error("no PR card");
		const prBadge = prCard.querySelector(".cx-axis-badge");
		if (!prBadge) throw new Error("no PR-card axis badge");
		expect(prBadge.hasAttribute("data-compact")).toBe(true);
	});

	test("the selected card carries data-selected; others do not (presence toggle)", () => {
		// T3 encodes selection solely as `.cx-card[data-selected]` (IssueCard.tsx:
		// `sel ? "" : undefined`), which owns the accent left rule now that the
		// priority stripe is gone. Defend the toggle against inversion / a dropped
		// attribute: exactly one card is selected (the store seeds STUB_ISSUES[0]),
		// and selecting a different issue moves the attribute to exactly one card.
		const { store, container } = mountBridge();
		const selectedCards = () =>
			container.querySelectorAll(".cx-card[data-selected]");
		const allCards = container.querySelectorAll(".cx-card");
		expect(allCards.length).toBeGreaterThan(1);
		expect(selectedCards()).toHaveLength(1);

		// Move the selection to a different fixture issue that the board renders.
		const otherId = STUB_ISSUES.find(
			(w) => w.id !== store.selectedIssueId(),
		)?.id;
		if (!otherId) throw new Error("need a second fixture issue");
		store.selectIssue(otherId);
		expect(selectedCards()).toHaveLength(1);
	});

	test("a card PR chip is a link that selects the issue and flips to the PRs tab", () => {
		const { store, container } = mountBridge();
		const chip = container.querySelector<HTMLElement>('.card-pr[role="link"]');
		if (!chip) throw new Error("no interactive card PR chip");
		fireEvent.click(chip);
		flushSync();
		expect(store.selectedIssueId()).not.toBeNull();
		// Flipped to the PRs tab: grouping seg hidden, PR rows shown.
		expect(groupingSeg(container)).toBeNull();
		expect(
			container.querySelectorAll(".card-issue-link").length,
		).toBeGreaterThan(0);
	});

	test("a card PR chip activates on Enter (keyboard a11y, DL-097)", () => {
		const { store, container } = mountBridge();
		const chip = container.querySelector<HTMLElement>('.card-pr[role="link"]');
		if (!chip) throw new Error("no interactive card PR chip");
		// Enter activates: same effect as the click test (select + flip to PRs).
		fireEvent.keyDown(chip, { key: "Enter" });
		flushSync();
		expect(store.selectedIssueId()).not.toBeNull();
		expect(groupingSeg(container)).toBeNull();
		expect(
			container.querySelectorAll(".card-issue-link").length,
		).toBeGreaterThan(0);
	});

	test("a card PR chip activates on Space and consumes the default page scroll", () => {
		const { store, container } = mountBridge();
		const chip = container.querySelector<HTMLElement>('.card-pr[role="link"]');
		if (!chip) throw new Error("no interactive card PR chip");
		// Space activates AND is consumed: preventDefault makes dispatchEvent return
		// false (no page scroll), and the chip selects + flips to the PRs tab.
		const notDefaulted = fireEvent.keyDown(chip, { key: " " });
		flushSync();
		expect(notDefaulted).toBe(false);
		expect(store.selectedIssueId()).not.toBeNull();
		expect(groupingSeg(container)).toBeNull();
		expect(
			container.querySelectorAll(".card-issue-link").length,
		).toBeGreaterThan(0);
	});

	test("a card PR chip ignores non-activation keys", () => {
		const { store, container } = mountBridge();
		const chip = container.querySelector<HTMLElement>('.card-pr[role="link"]');
		if (!chip) throw new Error("no interactive card PR chip");
		// The guard early-returns on any key but Enter/Space: selection unchanged
		// (the store seeds a selection) and no flip to the PRs tab.
		const before = store.selectedIssueId();
		fireEvent.keyDown(chip, { key: "a" });
		flushSync();
		expect(store.selectedIssueId()).toBe(before);
		expect(groupingSeg(container)).not.toBeNull();
		expect(container.querySelectorAll(".card-issue-link")).toHaveLength(0);
	});
});

// T4 — the whole board is ONE roving-tabindex group with a 2-D cursor (RIG-2130,
// DL-220/221; design §471-509). The board mounts its own keymap + roving group,
// so these drive real window `keydown`s (the same seam the dispatcher listens
// on) and assert against the live DOM + store. The fixture has no swimlane
// multi-card cell, so the multi-card traversal is exercised in status mode (the
// in_review column stacks SEA-1022/1085/847) and the empty-cell skip in swimlane
// mode (compass-ui's queued/blocked cells are empty between its cards).
const boardGrouping = (container: HTMLElement): HTMLElement | null =>
	container.querySelector('[aria-label="Board grouping"]');
const clickGrouping = (
	container: HTMLElement,
	label: "Swimlanes" | "Status",
) => {
	const btn = [
		...container.querySelectorAll<HTMLButtonElement>(
			'[aria-label="Board grouping"] button',
		),
	].find((b) => b.textContent === label);
	if (!btn) throw new Error(`grouping ${label} not found`);
	fireEvent.click(btn);
	flushSync();
};
// The cursor is the sole `tabindex="0"` stop; its positional aria-label names it.
const cursorLabel = (container: HTMLElement): string | null =>
	container
		.querySelector<HTMLElement>('.bridge-grid [tabindex="0"]')
		?.getAttribute("aria-label") ?? null;
// A real window keydown — the dispatcher's one listener resolves it.
const press = (init: KeyboardEventInit): KeyboardEvent => {
	const event = new KeyboardEvent("keydown", {
		bubbles: true,
		cancelable: true,
		...init,
	});
	window.dispatchEvent(event);
	flushSync();
	return event;
};
// Enter the board group by focusing the cursor stop (the sole `tabindex="0"`) —
// the same landing native Tab produces. The dispatcher claims a group-relative
// chord ONLY while focus is on a board stop (focus-exclusivity, design §401-405),
// so a keydown test must enter first; a within-group cursor move then keeps focus
// on the board (the roving effect refocuses), but a grouping/tab switch rebuilds
// the stop elements and drops focus, so re-enter after one.
const enterBoard = (container: HTMLElement): void => {
	container.querySelector<HTMLElement>('.bridge-grid [tabindex="0"]')?.focus();
};

describe("Bridge board roving group (T4, DL-220/221)", () => {
	// Non-keyboard DOM-shape tests keep their direct `<Bridge/>` mount — they
	// assert the roving tabindex structure, not the installed keymap (RD-3).
	test("the mounted board is one roving group: exactly one tabindex=0 stop", () => {
		const { container } = mountBridge();
		const tabbable = container.querySelectorAll('[tabindex="0"]');
		expect(tabbable).toHaveLength(1);
		// The single stop is a board card/gutter, not a nested chip.
		expect(tabbable[0].classList.contains("cx-card")).toBe(true);
		// Every other card + gutter is demoted to -1 (no stray native Tab stop).
		const stops = container.querySelectorAll(".cx-card, .bridge-lane");
		expect(stops.length).toBeGreaterThan(1);
		for (const s of stops) {
			if (s === tabbable[0]) continue;
			expect((s as HTMLElement).tabIndex).toBe(-1);
		}
	});

	test("the cursor seeds to the selected card (store seeds STUB_ISSUES[0])", () => {
		const { store, container } = mountBridge();
		expect(store.selectedIssueId()).toBe(STUB_ISSUES[0].id); // ws-1022
		expect(cursorLabel(container)).toBe("Issue SEA-1022");
	});

	// Keyboard-driving tests re-host onto the full shell (`mountApp`) so the
	// App-root spine is the installer (RD-3 / A6): they dispatch real window
	// keydowns and the one root listener resolves them. Cursor moves + selection
	// are synchronous signal writes; opening an agent navigates (real router), so
	// those cases await `flush()` before reading the routed view.
	test("Up/Down traverse a multi-card column (status mode in_review stack)", () => {
		const { container } = mountApp("/");
		clickGrouping(container, "Status");
		// The in_review column stacks three cards; the cursor rests on SEA-1022.
		expect(cursorLabel(container)).toBe("Issue SEA-1022");
		enterBoard(container);
		press({ key: "ArrowDown" });
		expect(cursorLabel(container)).toBe("Issue SEA-1085");
		press({ key: "ArrowDown" });
		expect(cursorLabel(container)).toBe("Issue SEA-847");
		// No wrap: Down at the column end clamps.
		press({ key: "ArrowDown" });
		expect(cursorLabel(container)).toBe("Issue SEA-847");
		press({ key: "ArrowUp" });
		expect(cursorLabel(container)).toBe("Issue SEA-1085");
	});

	test("Left skips empty cells and lands on the row gutter (swimlane)", () => {
		const { container } = mountApp("/");
		// compass-ui's row: SEA-1022 (in_review) and SEA-965 (in_progress) are the
		// only cards; queued/blocked cells are empty, and the gutter is column -1.
		expect(cursorLabel(container)).toBe("Issue SEA-1022");
		enterBoard(container);
		press({ key: "ArrowLeft" });
		expect(cursorLabel(container)).toBe("Issue SEA-965"); // in_progress
		press({ key: "ArrowLeft" });
		// The empty queued/blocked cells are skipped — straight to the gutter head.
		expect(cursorLabel(container)).toBe("compass-ui lane");
		// No wrap: Left at the gutter clamps.
		press({ key: "ArrowLeft" });
		expect(cursorLabel(container)).toBe("compass-ui lane");
	});

	test("Enter selects the cursor card and suppresses native activation", () => {
		const { store, container } = mountApp("/");
		clickGrouping(container, "Status");
		enterBoard(container);
		press({ key: "ArrowDown" }); // cursor → SEA-1085 (ws-1085)
		const event = press({ key: "Enter" });
		expect(store.selectedIssueId()).toBe("ws-1085");
		expect(event.defaultPrevented).toBe(true);
	});

	test("Shift+Enter opens the assigned agent — tier-1 board claim beats when:main comms", async () => {
		// The load-bearing precedence test, re-hosted onto the App-root spine (RD-3):
		// a competing `comms.newline`/`comms.send` is seeded into the SAME shared
		// registry, in the SAME `main` zone; `Shift+Enter → comms.newline
		// {when:"main"}` is a frozen keymap entry. The board's `board.openAssignedAgent`
		// is group-relative, so the dispatcher's tier 1 claims it AHEAD of the scoped
		// comms entry.
		const { store, container } = mountApp("/");
		let commsNewlineRan = 0;
		let commsSendRan = 0;
		store.keyboard.registry.register({
			id: "comms.newline" as CommandId,
			title: "Insert newline",
			keywords: [],
			scope: "main",
			run: () => commsNewlineRan++,
		});
		store.keyboard.registry.register({
			id: "comms.send" as CommandId,
			title: "Send",
			keywords: [],
			scope: "main",
			run: () => commsSendRan++,
		});
		// Cursor on SEA-1022 (assignee acc-compass-ui). Shift+Enter opens the agent.
		enterBoard(container);
		const event = press({ key: "Enter", shiftKey: true });
		expect(event.defaultPrevented).toBe(true);
		await flush();
		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-compass-ui");
		// The comms entry NEVER fired — the board won at tier 1.
		expect(commsNewlineRan).toBe(0);
		expect(commsSendRan).toBe(0);
	});

	test("Space fires the cursor card's cross-link (Issues → PRs)", () => {
		const { store, container } = mountApp("/");
		// Cursor SEA-1022 has a PR chip; Space is its cross-link (select + flip).
		enterBoard(container);
		const event = press({ key: " " });
		expect(event.defaultPrevented).toBe(true);
		expect(store.selectedIssueId()).toBe("ws-1022");
		expect(boardGrouping(container)).toBeNull(); // flipped to the PRs tab
	});

	test("Space on a chip-less card is still claimed (no select, no scroll, no fall-through)", () => {
		const { store, container } = mountApp("/");
		// Move to SEA-965 (compass-ui, in_progress) — an issue with no PR, so no
		// cross-link. Space must STILL be claimed: the handler reports handled, the
		// dispatcher preventDefaults, and nothing selects / scrolls / falls through.
		enterBoard(container);
		press({ key: "ArrowLeft" });
		expect(cursorLabel(container)).toBe("Issue SEA-965");
		const selectedBefore = store.selectedIssueId();
		const event = press({ key: " " });
		expect(event.defaultPrevented).toBe(true); // claimed → native scroll suppressed
		expect(store.selectedIssueId()).toBe(selectedBefore); // no select
		expect(boardGrouping(container)).not.toBeNull(); // no flip — still Issues
	});

	test("Enter on a gutter opens the agent", async () => {
		const { store, container } = mountApp("/");
		// Land on compass-ui's gutter head (Left past the empty cells).
		enterBoard(container);
		press({ key: "ArrowLeft" });
		press({ key: "ArrowLeft" });
		expect(cursorLabel(container)).toBe("compass-ui lane");
		press({ key: "Enter" });
		await flush();
		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-compass-ui");
	});

	test("the cursor stop carries a positional aria-label and names its Space shortcut", () => {
		const { container } = mountBridge();
		const cursor = container.querySelector<HTMLElement>('[tabindex="0"]');
		if (!cursor) throw new Error("no cursor stop");
		expect(cursor.getAttribute("aria-label")).toBe("Issue SEA-1022");
		expect(cursor.getAttribute("aria-description")).toContain("column");
		// The cursor card names the Space cross-link (design §491).
		expect(cursor.getAttribute("aria-keyshortcuts")).toBe("Space");
	});

	test("the container is a kanban board without an ARIA grid role (DL-220)", () => {
		const { container } = mountBridge();
		const grid = container.querySelector<HTMLElement>(".bridge-grid");
		if (!grid) throw new Error("no grid");
		expect(grid.getAttribute("aria-roledescription")).toBe("kanban board");
		expect(grid.getAttribute("aria-label")).toBe("Board grid");
		// A group with a kanban roledescription — never the forbidden role="grid".
		expect(grid.getAttribute("role")).toBe("group");
	});

	test("switching tabs resets the cursor to the new tab's first stop", () => {
		// design §248: a tab switch rebuilds the stop set and resets the cursor to
		// the tab's selected/first stop — NOT a same-column recovery onto whatever
		// PR row happens to share the old numeric column (the ids change wholesale
		// across tabs, so that landing would be arbitrary). The Issues-tab selection
		// (an issue id) is not a PRs-tab stop (composite `issueId::repo#n`), so the
		// reset lands on the first stop. Board stops are <button>s (`.cx-card` cards
		// + `.bridge-lane` gutters); the Unassigned lane is a non-interactive <div>,
		// so `button.…` selects only real stops in DOM order.
		const { container } = mountBridge();
		expect(cursorLabel(container)).toBe("Issue SEA-1022");
		clickTab(container, "PRs");
		const stops = container.querySelectorAll<HTMLElement>(
			"button.cx-card, button.bridge-lane",
		);
		expect(stops.length).toBeGreaterThan(0);
		// The cursor (sole tabindex=0) is the first stop, and it is the only one.
		expect(stops[0]?.tabIndex).toBe(0);
		expect(container.querySelectorAll('[tabindex="0"]')).toHaveLength(1);
	});

	test("pointer click-select is unregressed by the roving wiring", () => {
		const { store, container } = mountBridge();
		const otherId = STUB_ISSUES.find(
			(w) => w.id !== store.selectedIssueId() && w.state !== "backlog",
		)?.id;
		if (!otherId) throw new Error("need a second board issue");
		// A plain click still selects, exactly as before the roving group.
		const cards = [...container.querySelectorAll<HTMLElement>(".cx-card")];
		const target = cards.find((c) =>
			c.getAttribute("aria-label")?.includes("SEA-965"),
		);
		if (!target) throw new Error("no SEA-965 card");
		fireEvent.click(target);
		flushSync();
		expect(store.selectedIssueId()).toBe("ws-965");
	});

	test("focus-exclusivity: a focused non-board button keeps its native keys; the tier-3 Shift+Enter escape is scope-gated out (RD-4 closed, RIG-2529)", async () => {
		// The regression guard for the focus-exclusivity contract (design §401-405),
		// re-hosted onto the App-root spine: the board claims a group-relative chord
		// in tier 1 ONLY while focus is on a board stop. With focus on a toolbar
		// segmented button (a native control outside the board group), Enter/Space/
		// Arrows must NOT be claimed — the board neither preventDefaults nor moves
		// its cursor/selection, so the button keeps its native activation. (This
		// fails if the spine's active-group accessor were unconditional — the board
		// would trap every non-editable key.)
		const { store, container } = mountApp("/");
		const cursorBefore = cursorLabel(container);
		const selectedBefore = store.selectedIssueId();
		const toolbarButton = tabButtons(container)[0];
		if (!toolbarButton) throw new Error("no toolbar tab button");
		toolbarButton.focus();
		for (const key of ["Enter", " ", "ArrowDown", "ArrowLeft"]) {
			const event = press({ key });
			expect(event.defaultPrevented).toBe(false); // board did not claim it
		}
		expect(cursorLabel(container)).toBe(cursorBefore); // cursor unmoved
		expect(store.selectedIssueId()).toBe(selectedBefore); // selection unchanged

		// RD-4 CLOSED (RIG-2529): tier 3 is now scope-gated. `board.openAssignedAgent`
		// is `scope:'main'` (Bridge.tsx:437) and the keymap binds it unscoped
		// (Shift+Enter, keymap.ts:98). With the toolbar button holding focus, no
		// roving group is active and no zone is active, so `activeZone()` is null —
		// the scope gate's `command.scope === zone` is `"main" === null` → false, and
		// the chord falls out of tier 3 UN-consumed. The button keeps its native
		// activation and the assigned-agent escape does NOT fire. This is the
		// production-wiring proof (real App, real chord, real store) that the frozen
		// dispatch contract changed deliberately.
		const viewBefore = store.view();
		const escapeEvent = press({ key: "Enter", shiftKey: true });
		expect(escapeEvent.defaultPrevented).toBe(false); // native activation survives
		await flush();
		expect(store.view()).toBe(viewBefore); // the assigned-agent escape did NOT fire
	});

	// Lifecycle (A6 bullet 4): a mount→unmount→remount cycle leaves exactly ONE
	// window listener and NO stale `board.*` commands. Uses ONE shared store across
	// both shell mounts so the same app-lifetime registry is observed: after
	// unmount the Bridge's cleanup unregisters both `board.*` ids and App's
	// cleanup removes the keymap listener; after remount both are present exactly
	// once. The single-listener property is proven functionally — one `Ctrl+B`
	// runs the `view.bridge` command exactly once (a stacked listener would run it
	// twice).
	test("mount → unmount → remount leaves one listener and no stale board.*/list.* commands", async () => {
		// The shared store outlives both mounts, so it can't live inside either
		// render root; build it in its own `createRoot` (v2 createAppStore's useQuery
		// needs a reactive owner) and dispose after the second unmount — the
		// store.test.ts withStoreAsync idiom.
		let disposeStore!: () => void;
		const store = createRoot((d) => {
			disposeStore = d;
			return createAppStore({ queryClient: testQueryClient() });
		});
		// Widened to collect the ten `scope:'main'` board commands: the two `board.*`
		// ids and the eight group-relative `list.*` ids (RIG-2529). All ten must
		// retract on unmount — a stale `scope:'main'` registration can never outlive
		// its surface.
		const boardCommandIds = (): string[] =>
			store.keyboard.registry
				.all()
				.map((c) => c.id as string)
				.filter((id) => id.startsWith("board.") || id.startsWith("list."));
		const ALL_TEN = [
			"board.openAssignedAgent",
			"board.openCardCrossLink",
			"list.expandOrToggle",
			"list.moveFirst",
			"list.moveLast",
			"list.moveLeft",
			"list.moveNext",
			"list.movePrev",
			"list.moveRight",
			"list.openOrSelect",
		];

		const first = mountShell(store, "/");
		expect(boardCommandIds().sort()).toEqual(ALL_TEN);
		first.unmount();
		expect(boardCommandIds()).toEqual([]); // retracted on cleanup

		const second = mountShell(store, "/");
		expect(boardCommandIds().sort()).toEqual(ALL_TEN);

		// Exactly one listener: spy the shared `view.bridge` command and fire one
		// Ctrl+B. Two stacked App listeners over the one registry would run it twice.
		let viewBridgeRuns = 0;
		store.keyboard.registry.register({
			id: "view.bridge" as CommandId,
			title: "Go to Bridge",
			keywords: [],
			scope: "global",
			run: () => viewBridgeRuns++,
		});
		press({ key: "b", ctrlKey: true });
		await flush();
		expect(viewBridgeRuns).toBe(1);
		second.unmount();
		disposeStore();
	});
});

// T5 — the empty-board centered message (RIG-2130, design §251-282, §523-540).
// When the built stop list is empty, each tab swaps its grid for a single
// centered `.bridge-empty` line; the toolbar + segmented controls (counts → 0)
// render unchanged, and the board registers ZERO roving stops.
function mountEmptyBridge(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			queryClient: testQueryClient(),
			initialIssues: [],
		});
		return (
			<StoreContext value={store}>
				<Bridge />
			</StoreContext>
		);
	});
	return { store, container };
}

describe("Bridge empty board (T5, RIG-2130)", () => {
	test("Issues tab shows the empty message, no grid, no roving stops", () => {
		const { container } = mountEmptyBridge();
		const empty = container.querySelector(".bridge-empty");
		expect(empty?.textContent).toBe(
			"No issues on the board yet — promote work from the Backlog to see it here.",
		);
		expect(container.querySelector(".bridge-grid")).toBeNull();
		// No roving stops: no cursor tabindex, no cards, no gutters.
		expect(container.querySelectorAll('[tabindex="0"]')).toHaveLength(0);
		expect(container.querySelector(".cx-card")).toBeNull();
		expect(container.querySelector(".bridge-lane")).toBeNull();
	});

	test("toolbar + tab labels with (zero) counts stay intact", () => {
		const { container } = mountEmptyBridge();
		expect(container.querySelector(".bridge-toolbar")).not.toBeNull();
		expect(groupingSeg(container)).not.toBeNull();
		const tabs = tabButtons(container);
		expect(tabs).toHaveLength(2);
		expect((tabs[0].textContent ?? "").startsWith("Issues")).toBe(true);
		expect(tabs[1].textContent).toContain("PRs · 0");
	});

	test("flipping to the PRs tab shows the PRs empty copy", () => {
		const { container } = mountEmptyBridge();
		clickTab(container, "PRs");
		const empty = container.querySelector(".bridge-empty");
		expect(empty?.textContent).toBe(
			"No open PRs yet — cards appear here when an agent opens one.",
		);
		expect(container.querySelector(".bridge-grid")).toBeNull();
		expect(container.querySelectorAll('[tabindex="0"]')).toHaveLength(0);
	});
});
