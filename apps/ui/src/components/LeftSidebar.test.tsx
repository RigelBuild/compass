import { afterEach, describe, expect, test } from "bun:test";
import { cleanup, fireEvent, render } from "@solidjs/testing-library";
import { flush as flushSync } from "solid-js";
import { STUB_CHANNELS, STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import type { CommandId } from "../keyboard/commands";
import { detectPlatform } from "../keyboard/dispatch";
import { shortcutFor } from "../keyboard/keymap";
import { type AppStore, createAppStore } from "../store";
import { STUB_AGENTS } from "../stub-data";
import { flush, mountApp } from "../test-router";
import { testQueryClient } from "../test-support";
import { LeftSidebar } from "./LeftSidebar";

// T7 acceptance: one Channels section contains the agent tree, channel bands,
// direct messages, and the unchanged browse/discover list.
//
// The test fixture has joined standalone channels, home DMs, and a browse channel.

// Mount LeftSidebar over a real store through the app's StoreContext (index.tsx
// wires it as `<StoreContext value={store}>`). The store is built inside
// render's reactive root so its memos are owned and disposed on the library's
// per-test cleanup; the reference is captured so tests drive store actions and
// re-query the live DOM.
function mountSidebar(initialComms = STUB_COMMS_STATE): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			initialComms,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<LeftSidebar />
			</StoreContext>
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

// The channel rows that are rail rows (not the browse-list rows) — the
// standalone set the Channels section renders.
const railRows = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>(".ch-row:not(.browse-row)"),
];

describe("LeftSidebar (T7)", () => {
	test("the combined Channels section collapses and expands as one tree", () => {
		const { store, container } = mountSidebar();
		expect(store.isSectionCollapsed("channels")).toBe(false);
		expect(railRows(container).length).toBeGreaterThan(0);
		expect(container.querySelectorAll(".tree-agent").length).toBeGreaterThan(0);
		const head = findToggle(container, "Channels");
		expect(head).toBeDefined();
		if (!head) throw new Error("Channels section header not rendered");
		fireEvent.click(head);
		flushSync();
		expect(store.isSectionCollapsed("channels")).toBe(true);
		expect(container.querySelectorAll(".ch-row").length).toBe(0);
		expect(container.querySelectorAll(".tree-agent").length).toBe(0);
		const expanded = findToggle(container, "Channels");
		if (!expanded) throw new Error("Channels section header vanished");
		fireEvent.click(expanded);
		flushSync();
		expect(store.isSectionCollapsed("channels")).toBe(false);
		expect(railRows(container).length).toBeGreaterThan(0);
		expect(container.querySelectorAll(".tree-agent").length).toBeGreaterThan(0);
	});

	// Contract (§611): the Channels section lists standalone channels, including
	// svc.compass with its unread badge.
	test("lists standalone channels with an unread badge", () => {
		const { container } = mountSidebar();

		const compass = railRows(container).find(
			(r) => r.querySelector(".ch-name")?.textContent === "svc.compass",
		);
		expect(compass).toBeDefined();
		// ch-svc-compass carries 5 unread — the badge shows the count.
		expect(compass?.querySelector(".ch-unread")?.textContent).toBe("5");
	});

	// Channel child-row behavior is covered by the same ChannelRow used in the rail.
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
		flushSync();

		expect(store.view()).toBe("channel");
		expect(store.selectedChannelId()).toBe("ch-svc-compass");
	});

	// Agent row click behavior remains available in the unified section.
	test("an agent-leaf click routes to the agent workspace", () => {
		const { store, container } = mountSidebar();

		expect(findToggle(container, "Channels")).toBeDefined();

		const ui = STUB_AGENTS.find((a) => a.account.id === "acc-compass-ui");
		expect(ui).toBeDefined();
		if (!ui) throw new Error("fixture missing acc-compass-ui");

		const uiLeaf = [
			...container.querySelectorAll<HTMLButtonElement>(".tree-agent"),
		].find((l) => l.querySelector(".name")?.textContent === ui.account.handle);
		expect(uiLeaf).toBeDefined();
		if (!uiLeaf) throw new Error("compass-ui agent leaf not rendered");
		fireEvent.click(uiLeaf);
		flushSync();

		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-compass-ui");
	});
	test("a childless agent with a home channel has no descendant badge", () => {
		const { container } = mountSidebar();
		const agentRow = [
			...container.querySelectorAll<HTMLElement>(".tree-agent-row"),
		].find(
			(row) => row.querySelector(".name")?.textContent === "compass-server-acp",
		);
		expect(agentRow).toBeDefined();
		expect(agentRow?.querySelector(".tree-badge")).toBeNull();
	});

	test("home channels render once under their agent and outside root bands", () => {
		const { container } = mountSidebar();
		const rows = [...container.querySelectorAll<HTMLElement>(".ch-row")].filter(
			(row) => row.querySelector(".ch-name")?.textContent === "compass-ui",
		);
		expect(rows).toHaveLength(1);
		expect(rows[0]?.closest(".tree-children")).not.toBeNull();
		expect(rows[0]?.closest(".rail-section")).toBeNull();
	});

	test("tree-membership channels render under their parent", () => {
		const treeChannel = {
			id: "ch-tree-test",
			name: "tree-test",
			kind: "channel" as const,
			memberAccountIds: [],
			membership: "joined" as const,
			postPolicy: "open" as const,
			membershipMode: "tree" as const,
			parentAgentId: "acc-compass-ui",
		};
		const state = {
			...STUB_COMMS_STATE,
			channels: [...STUB_CHANNELS, treeChannel],
		};
		const { container } = mountSidebar(state);
		const treeRow = [
			...container.querySelectorAll<HTMLElement>(".tree-children .ch-row"),
		].find((row) => row.querySelector(".ch-name")?.textContent === "tree-test");
		expect(treeRow).toBeDefined();
	});

	test("an attached channel with no membership appears in browse, not under its agent", () => {
		const treeChannel = {
			id: "ch-tree-none",
			name: "tree-none",
			kind: "channel" as const,
			memberAccountIds: [],
			membership: "none" as const,
			postPolicy: "open" as const,
			membershipMode: "tree" as const,
			parentAgentId: "acc-compass-ui",
		};
		const state = {
			...STUB_COMMS_STATE,
			channels: [...STUB_CHANNELS, treeChannel],
		};
		const { container } = mountSidebar(state);
		expect(
			[...container.querySelectorAll(".tree-children .ch-name")].some(
				(name) => name.textContent === "tree-none",
			),
		).toBe(false);
		const browseHead = findToggle(container, "browse channels");
		if (!browseHead) throw new Error("browse channels header not rendered");
		fireEvent.click(browseHead);
		flushSync();
		expect(
			[...container.querySelectorAll(".browse-row .ch-name")].some(
				(name) => name.textContent === "tree-none",
			),
		).toBe(true);
	});

	test("owner-grouped channels render in the trailing root band", () => {
		const { container } = mountSidebar();
		const coordination = railRows(container).find(
			(row) => row.querySelector(".ch-name")?.textContent === "coordination",
		);
		expect(coordination).toBeDefined();
		expect(
			coordination
				?.closest(".rail-section")
				?.querySelector(".rail-section-head")?.textContent,
		).toBe("channels");
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
		flushSync();

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
		flushSync();
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

	// Contract (§T8): the subscribe toggle is HIDDEN entirely on
	// mandatory_subscription channels — the model force-subscribes every member,
	// so any unsubscribe affordance (even a disabled one, even a fixed marker)
	// would be a lie. Fixture: ch-announcements and ch-coordination are both
	// mandatorySubscription. Mutation-check: removing the hide reddens (a toggle
	// or fixed marker reappears on a mandatory row).
	test("hides the subscribe control on mandatory_subscription channels", () => {
		const { container } = mountSidebar();

		const rowByName = (name: string): HTMLElement | undefined =>
			railRows(container).find(
				(r) => r.querySelector(".ch-name")?.textContent === name,
			);

		// Precondition: the fixture channels we assert on are actually mandatory.
		for (const id of ["ch-announcements", "ch-coordination"]) {
			const ch = STUB_CHANNELS.find((c) => c.id === id);
			expect(ch?.mandatorySubscription).toBe(true);
		}

		// Neither a toggle button nor a fixed marker renders on a mandatory row.
		for (const name of ["announcements", "coordination"]) {
			const row = rowByName(name);
			expect(row).toBeDefined();
			expect(row?.querySelectorAll(".ch-sub").length).toBe(0);
		}

		// Non-triviality: a NON-mandatory rail channel still renders its control.
		const compass = rowByName("svc.compass");
		expect(compass).toBeDefined();
		expect(compass?.querySelector(".ch-sub")).not.toBeNull();
	});

	// Contract (§T8): an agent's presence render carries its human-readable
	// activity note (Agent.activity, AgentPresenceChanged.activity) beside the
	// process-state dot. Present → a `.agent-activity` with the fixture text;
	// absent → nothing extra. Mutation-check: dropping the render reddens the
	// present leg. Fixture: supervisor has an activity; compass-server-acp has none.
	test("renders an agent's activity note when present", () => {
		const { container } = mountSidebar();

		const leafByHandle = (handle: string): HTMLElement | undefined =>
			[...container.querySelectorAll<HTMLElement>(".tree-agent-row")].find(
				(r) => r.querySelector(".name")?.textContent === handle,
			);

		const supervisor = STUB_AGENTS.find(
			(a) => a.account.id === "acc-supervisor",
		);
		expect(supervisor?.activity).toBeDefined();
		const supRow = leafByHandle("supervisor");
		expect(supRow).toBeDefined();
		expect(supRow?.querySelector(".agent-activity")?.textContent).toBe(
			supervisor?.activity,
		);

		// compass-server-acp has no activity → no `.agent-activity` on its row.
		const serverAcp = STUB_AGENTS.find(
			(a) => a.account.id === "acc-compass-server-acp",
		);
		expect(serverAcp?.activity).toBeUndefined();
		const serverAcpRow = leafByHandle("compass-server-acp");
		expect(serverAcpRow).toBeDefined();
		expect(serverAcpRow?.querySelector(".agent-activity")).toBeNull();
	});
});

// Coaching-tooltip adoption sweep (RIG-2530 T2). The view buttons convert from
// a native `title=` to a CoachTip;
// the new-folder button stays native (no registered command → nothing to
// coach, the A4/D4 boundary). These assert the observable adoption contract.

// Kobalte portals its tooltip content on a macrotask.
async function settle(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	// biome-ignore lint/style/noRestrictedGlobals: deterministic macrotask yield (setTimeout(0)) to observe Kobalte's portalled tooltip; not a timed wait
	setTimeout(resolve, 0);
	await promise;
}

const viewButtons = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>("button.bridge-link"),
];

describe("LeftSidebar coaching tooltips (RIG-2530 T2)", () => {
	test("the Bridge button opens a coaching tooltip on focus with label + chord", async () => {
		const { container } = mountSidebar();
		const bridge = viewButtons(container).find((b) =>
			b.textContent?.includes("Bridge"),
		);
		expect(bridge).toBeDefined();

		bridge?.focus();
		await settle();

		const tooltip =
			document.body.querySelector<HTMLElement>('[role="tooltip"]');
		expect(tooltip).not.toBeNull();
		expect(tooltip?.textContent).toContain("Bridge");
		const chip = tooltip?.querySelector(".cx-palette-shortcut");
		const kbds = Array.from(chip?.querySelectorAll("kbd") ?? []).map(
			(k) => k.textContent,
		);
		expect(shortcutFor("view.bridge" as CommandId, detectPlatform())).toBe(
			"Ctrl+B",
		);
		expect(kbds).toEqual(["Ctrl", "B"]);
	});

	test("every converted view button drops `title`, keeps `aria-keyshortcuts` where a chord exists, and has a text accessible name", () => {
		const { container } = mountSidebar();
		const buttons = viewButtons(container);
		expect(buttons.length).toBe(3);
		for (const b of buttons) {
			expect(b.hasAttribute("title")).toBe(false);
			// Text-labelled buttons carry their accessible name from visible text —
			// no aria-label needed.
			expect(b.hasAttribute("aria-label")).toBe(false);
			expect(b.textContent?.trim()).not.toBe("");
		}
		// Bridge + Settings have keymap rows → aria-keyshortcuts present.
		const bridge = buttons.find((b) => b.textContent?.includes("Bridge"));
		expect(bridge?.getAttribute("aria-keyshortcuts")).toBeTruthy();
	});

	test("Agents is the first view link and opens the agent tree", () => {
		const { store, container } = mountSidebar();
		const first = viewButtons(container)[0];
		expect(first?.textContent).toContain("Agents");
		if (!first) throw new Error("no view link rendered");
		fireEvent.click(first);
		flushSync();
		expect(store.view()).toBe("agents");
	});

	test("the view buttons are Agents, Bridge, and Settings; Backlog and Done live in the Bridge", () => {
		const { container } = mountSidebar();
		expect(
			viewButtons(container).map((b) => b.textContent?.trim().split(/\d/)[0]),
		).toEqual(["Agents", "Bridge", "Settings"]);
	});
});

// Open modes: a plain click navigates the focused view
// in place; Mod+click and middle-click open the destination in a new tab.
describe("LeftSidebar open modes", () => {
	const setPlatform = (platform: "mac" | "other"): void => {
		Object.defineProperty(navigator, "platform", {
			value: platform === "mac" ? "MacIntel" : "X11; Linux x64",
			configurable: true,
		});
	};
	const paths = (store: AppStore): string[] =>
		store
			.layout()
			.tabs.map((tab) =>
				tab.layout.kind === "single" ? tab.layout.view.path : "split",
			);
	const agentRow = (c: HTMLElement): HTMLElement => {
		const row = [...c.querySelectorAll<HTMLElement>("button.tree-agent")].find(
			(b) => b.textContent?.includes("compass-ui"),
		);
		if (!row) throw new Error("no compass-ui tree row");
		return row;
	};
	const channelRow = (c: HTMLElement): HTMLElement => {
		const row = [...c.querySelectorAll<HTMLElement>(".ch-row-select")].find(
			(b) => b.textContent?.includes("svc.compass"),
		);
		if (!row) throw new Error("no svc.compass channel row");
		return row;
	};
	const middleClick = (el: HTMLElement): void => {
		el.dispatchEvent(
			new MouseEvent("auxclick", {
				button: 1,
				bubbles: true,
				cancelable: true,
			}),
		);
	};

	afterEach(() => {
		cleanup();
		setPlatform("other");
	});

	test("a plain click navigates the focused view in place", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		fireEvent.click(agentRow(container));
		await flush();
		expect(paths(store)).toEqual(["/agent/acc-compass-ui"]);
	});

	test("Mod+click opens a new focused tab after the active one", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		fireEvent.click(channelRow(container), { ctrlKey: true });
		await flush();
		expect(paths(store)).toEqual(["/", "/channel/ch-svc-compass"]);
		expect(store.view()).toBe("channel");
	});

	test("on a Mac, Meta is the tab modifier and Ctrl+click stays in place", async () => {
		setPlatform("mac");
		const { store, container } = mountApp("/");
		fireEvent.click(agentRow(container), { ctrlKey: true });
		await flush();
		expect(paths(store)).toEqual(["/agent/acc-compass-ui"]);
		fireEvent.click(channelRow(container), { metaKey: true });
		await flush();
		expect(paths(store)).toEqual([
			"/agent/acc-compass-ui",
			"/channel/ch-svc-compass",
		]);
	});

	test("a middle-click opens a new tab; other aux buttons do nothing", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		agentRow(container).dispatchEvent(
			new MouseEvent("auxclick", {
				button: 2,
				bubbles: true,
				cancelable: true,
			}),
		);
		await flush();
		expect(paths(store)).toEqual(["/"]);

		middleClick(agentRow(container));
		await flush();
		expect(paths(store)).toEqual(["/", "/agent/acc-compass-ui"]);
	});

	test("a middle mousedown is cancelled so it cannot start autoscroll", () => {
		const { container } = mountApp("/");
		const down = (button: number): MouseEvent => {
			const event = new MouseEvent("mousedown", {
				button,
				bubbles: true,
				cancelable: true,
			});
			agentRow(container).dispatchEvent(event);
			return event;
		};
		expect(down(1).defaultPrevented).toBe(true);
		expect(down(0).defaultPrevented).toBe(false);
	});

	test("a view link opens its view in a new tab, and an open path refocuses its tab", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		const settings = viewButtons(container).find((b) =>
			b.textContent?.includes("Settings"),
		);
		if (!settings) throw new Error("no Settings view button");
		middleClick(settings);
		await flush();
		expect(paths(store)).toEqual(["/", "/settings/tracker"]);

		const bridge = viewButtons(container).find((b) =>
			b.textContent?.includes("Bridge"),
		);
		if (!bridge) throw new Error("no Bridge view button");
		fireEvent.click(bridge, { ctrlKey: true });
		await flush();
		expect(paths(store)).toEqual(["/", "/settings/tracker"]);
		expect(store.view()).toBe("bridge");
	});

	test("a DM row opened in a tab lands on the agent workspace", async () => {
		setPlatform("other");
		const { store, container } = mountApp("/");
		const dmRow = [
			...container.querySelectorAll<HTMLElement>(".ch-row-select"),
		].find((b) => b.querySelector(".ch-glyph")?.textContent === "@");
		if (!dmRow) throw new Error("no DM row");
		middleClick(dmRow);
		await flush();
		const opened = paths(store)[1];
		expect(opened?.startsWith("/agent/")).toBe(true);
	});
});
