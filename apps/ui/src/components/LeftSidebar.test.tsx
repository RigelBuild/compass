import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { STUB_CHANNELS, STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { STUB_AGENTS } from "../stub-data";
import { LeftSidebar } from "./LeftSidebar";

// RED acceptance spec for T5 (design.md §578-613): the reshaped LeftSidebar —
// the Bridge/Backlog/Done/Settings links, then a collapsible **Channels**
// section (member channels + group DMs + browse/join, moved from ChannelSidebar)
// ABOVE a collapsible **Agent workspaces** section (the existing folder tree).
// It fails today because LeftSidebar renders the tree directly with no section
// chrome and no channel rows: every assertion below is an
// absence-of-section-surface / unwired-row failure, never a module-load error
// (LeftSidebar.tsx already exists and imports fine). An implementer makes it
// green next by building `ChannelsSection` / `AgentsSection` and wiring the rows
// to `openChannel` (§602-604).
//
// Fixture ground truth (grepped from comms-stub.ts / stub-data.ts, quoted here):
//   - Standalone rail channels (kind "channel", membership !== "none"):
//     ch-announcements, ch-coordination, ch-svc-compass ("svc.compass", unread
//     5), ch-svc-ci-build. ch-random is membership "none" → the browse list.
//   - Group DM dm-cook-ross ("cook, ross", kind group_dm) — lists in Channels.
//   - 1:1 agent home DMs (kind "dm", one per board agent, name = handle) — must
//     NOT list under Channels (§589); the agent workspace is their surface. cook
//     ("acc-cook") is a `.tree-agent` leaf in the Agent workspaces tree instead.

// The Channels rail lists standalone channels + group DMs but NOT 1:1 agent DMs
// (kind "dm"). Derived from the fixture so a reshuffle can't stale the count.
const RAIL_ROWS = STUB_CHANNELS.filter(
	(c) => c.membership !== "none" && c.kind !== "dm",
).length;
// The excluded set — proves the exclusion below is non-trivial (there ARE 1:1
// agent DMs in the fixture that a naive `dmChannels` render would leak in).
const AGENT_DM_ROWS = STUB_CHANNELS.filter((c) => c.kind === "dm").length;

// Mount LeftSidebar over a real store through the app's StoreContext (index.tsx
// wires it as `<StoreContext.Provider value={store}>`). The store is built inside
// render's reactive root so its memos are owned and disposed on the library's
// per-test cleanup; the reference is captured so tests drive store actions and
// re-query the live DOM.
function mountSidebar(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({ initialComms: STUB_COMMS_STATE });
		return (
			<StoreContext.Provider value={store}>
				<LeftSidebar />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

// A section/browse toggle button, located by its accessible collapse control
// (aria-expanded) + visible label — the NEW section chrome is asserted by text +
// aria, not a guessed class name (brief). Returns undefined when absent (→ RED).
const findToggle = (
	container: HTMLElement,
	label: string,
): HTMLButtonElement | undefined =>
	[
		...container.querySelectorAll<HTMLButtonElement>("button[aria-expanded]"),
	].find((b) => b.textContent?.includes(label));

// The channel rows that are rail rows (not the browse-list rows), and their
// visible names — the standalone set the Channels section renders.
const railRows = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>(".ch-row:not(.browse-row)"),
];
const railNames = (container: HTMLElement): (string | null)[] =>
	railRows(container).map(
		(r) => r.querySelector(".ch-name")?.textContent ?? null,
	);

describe("LeftSidebar (T5)", () => {
	// Contract (§610): the two sections collapse/expand independently — collapsing
	// Channels hides its rows while the agent tree stays, and vice-versa. Driven
	// through the header toggles; asserted through BOTH the store's
	// isSectionCollapsed AND the DOM. A single-slot / shared-flag section would
	// fail the "one collapsed, the other still shown" legs.
	test("both sections collapse and expand independently", () => {
		const { store, container } = mountSidebar();

		// Both expanded by default: rail rows and agent leaves both present.
		expect(store.isSectionCollapsed("channels")).toBe(false);
		expect(store.isSectionCollapsed("agents")).toBe(false);
		expect(railRows(container).length).toBeGreaterThan(0);
		expect(container.querySelectorAll(".tree-agent").length).toBeGreaterThan(0);

		// Collapse Channels — only the channel rows disappear; the tree stays.
		const chHead = findToggle(container, "Channels");
		expect(chHead).toBeDefined();
		if (!chHead) throw new Error("Channels section header not rendered");
		fireEvent.click(chHead);

		expect(store.isSectionCollapsed("channels")).toBe(true);
		expect(store.isSectionCollapsed("agents")).toBe(false);
		expect(container.querySelectorAll(".ch-row").length).toBe(0);
		expect(container.querySelectorAll(".tree-agent").length).toBeGreaterThan(0);

		// Collapse Agent workspaces too — now the tree disappears.
		const agHead = findToggle(container, "Agent workspaces");
		expect(agHead).toBeDefined();
		if (!agHead)
			throw new Error("Agent workspaces section header not rendered");
		fireEvent.click(agHead);

		expect(store.isSectionCollapsed("agents")).toBe(true);
		expect(container.querySelectorAll(".tree-agent").length).toBe(0);

		// Re-expand Channels only — channels return, agents stay collapsed.
		const chHead2 = findToggle(container, "Channels");
		expect(chHead2).toBeDefined();
		if (!chHead2) throw new Error("Channels section header vanished");
		fireEvent.click(chHead2);

		expect(store.isSectionCollapsed("channels")).toBe(false);
		expect(store.isSectionCollapsed("agents")).toBe(true);
		expect(railRows(container).length).toBeGreaterThan(0);
		expect(container.querySelectorAll(".tree-agent").length).toBe(0);
	});

	// Contract (§611): the Channels section lists the standalone set — a plain
	// channel (svc.compass) with its unread badge, and the group DM (cook, ross).
	test("lists standalone channels and the group DM with an unread badge", () => {
		const { container } = mountSidebar();

		const compass = railRows(container).find(
			(r) => r.querySelector(".ch-name")?.textContent === "svc.compass",
		);
		expect(compass).toBeDefined();
		// ch-svc-compass carries 5 unread — the badge shows the count.
		expect(compass?.querySelector(".ch-unread")?.textContent).toBe("5");

		// The group DM lists as a rail row.
		expect(railNames(container)).toContain("cook, ross");
	});

	// Contract (§589, §611): 1:1 agent home DMs do NOT list under Channels — the
	// rail row count is exactly the standalone set (channels + group DMs), and no
	// rail row's label is a bare agent handle. The same handle ("cook") DOES
	// appear as an agent leaf in the tree, proving the exclusion is about the DM
	// row, not the agent.
	test("excludes 1:1 agent DMs from the Channels section", () => {
		const { container } = mountSidebar();

		// Precondition: the fixture actually has 1:1 agent DMs to exclude, so a
		// leak would change the count.
		expect(AGENT_DM_ROWS).toBeGreaterThan(0);

		// Exactly the standalone set — not standalone + the ~10 agent home DMs.
		expect(railRows(container).length).toBe(RAIL_ROWS);

		// No rail row is the bare "cook" DM…
		expect(railNames(container)).not.toContain("cook");
		// …but "cook" IS an agent leaf in the Agent workspaces tree.
		const treeNames = [...container.querySelectorAll(".tree-agent .name")].map(
			(n) => n.textContent,
		);
		expect(treeNames).toContain("cook");
	});

	// Contract (§586-587, §602): clicking a channel row routes to the channel
	// view with that channel selected — via the new `openChannel` wiring on the
	// row's select button.
	test("a channel-row click routes to the channel view", () => {
		const { store, container } = mountSidebar();

		const compass = railRows(container).find(
			(r) => r.querySelector(".ch-name")?.textContent === "svc.compass",
		);
		expect(compass).toBeDefined();
		const select = compass?.querySelector<HTMLButtonElement>(".ch-row-select");
		expect(select).not.toBeNull();
		if (!select) throw new Error("channel-row select button not rendered");
		fireEvent.click(select);

		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe("ch-svc-compass");
	});

	// Contract (§588): clicking an agent leaf inside the Agent workspaces section
	// routes to that agent's workspace. Requires the section chrome (RED now) and
	// preserves the leaf's existing routing.
	test("an agent-leaf click routes to the agent workspace", () => {
		const { store, container } = mountSidebar();

		// The Agent workspaces section exists (RED now — no section chrome yet).
		expect(findToggle(container, "Agent workspaces")).toBeDefined();

		const cook = STUB_AGENTS.find((a) => a.account.id === "acc-cook");
		expect(cook).toBeDefined();
		if (!cook) throw new Error("fixture missing acc-cook");

		const cookLeaf = [
			...container.querySelectorAll<HTMLButtonElement>(".tree-agent"),
		].find(
			(l) => l.querySelector(".name")?.textContent === cook.account.handle,
		);
		expect(cookLeaf).toBeDefined();
		if (!cookLeaf) throw new Error("cook agent leaf not rendered");
		fireEvent.click(cookLeaf);

		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-cook");
	});

	// Matt's ruling: join/subscribe are NOT wired to the wire yet — there is no
	// join/subscribe RPC, and the old local-only mutation silently reverted the
	// moment the next SubscribeComms snapshot re-derived membership from the
	// server (store.ts adoptComms → live/adapt.ts deriveMembership). A control
	// that plainly does not work beats one that appears to and undoes itself, so
	// the join control renders DISABLED with an honest title and clicking it
	// changes nothing. Mutation-check: restoring the local-toggle mutation
	// reddens the membership leg; an enabled button reddens the disabled leg.
	test("browse/join renders disabled and does not fake membership", () => {
		const { store, container } = mountSidebar();

		const membershipOf = () =>
			store.channels().find((c) => c.id === "ch-random")?.membership;
		// The transition starts from `none` (ch-random is the unjoined channel).
		expect(membershipOf()).toBe("none");

		const browseHead = findToggle(container, "browse channels");
		expect(browseHead).toBeDefined();
		if (!browseHead) throw new Error("browse channels header not rendered");
		fireEvent.click(browseHead);

		const randomRow = [
			...container.querySelectorAll<HTMLElement>(".ch-row.browse-row"),
		].find((r) => r.querySelector(".ch-name")?.textContent === "random");
		expect(randomRow).toBeDefined();
		const join = randomRow?.querySelector<HTMLButtonElement>(".ch-join");
		expect(join).not.toBeNull();
		if (!join) throw new Error("ch-random join button not rendered");

		// The control is visibly non-functional, and says why.
		expect(join.disabled).toBe(true);
		expect(join.title).toContain("not wired up yet");

		// And nothing fakes state behind it.
		fireEvent.click(join);
		expect(membershipOf()).toBe("none");
	});

	// The same ruling on the subscribe toggle: a joined row's ◉/○ control is
	// disabled with an honest title, and a click leaves membership alone. The
	// always-subscribed rows render a non-button `.ch-sub.fixed` marker, so this
	// picks a row that actually has the toggle. Mutation-check: restoring
	// toggleSubscribe's local mutation reddens the membership leg.
	test("the subscribe toggle renders disabled and does not fake membership", () => {
		const { store, container } = mountSidebar();

		const toggles = [
			...container.querySelectorAll<HTMLButtonElement>("button.ch-sub"),
		];
		// Non-triviality: the rail really does render togglable rows.
		expect(toggles.length).toBeGreaterThan(0);

		const before = store.channels().map((c) => c.membership);
		for (const toggle of toggles) {
			expect(toggle.disabled).toBe(true);
			expect(toggle.title).toContain("not wired up yet");
			fireEvent.click(toggle);
		}

		// No membership anywhere moved.
		expect(store.channels().map((c) => c.membership)).toEqual(before);
	});
});
