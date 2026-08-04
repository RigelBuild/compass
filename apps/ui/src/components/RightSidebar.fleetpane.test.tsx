import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import {
	STUB_ACCOUNTS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
	STUB_TOPICS,
} from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { RightSidebar } from "./RightSidebar";

// Render acceptance spec for the compass-0.7 fleet pane (design compass-0.7,
// FleetPane in RightSidebar.tsx). A fleet tab (Supervisor · Warden) used to
// render CONTROL-ONLY — just a button into the agent's workspace. It now renders
// the agent's home-DM conversation INLINE above a compact "Open workspace"
// control, and — per Matt's kill-the-gate ruling (2026-07-20) — any ask in that
// DM renders ANSWERABLE in place (first-responder-wins is the sole settlement;
// no read-only gate, no rerouting to the owner's workspace). These tests defend
// that contract:
//   1. the inline home-DM conversation actually renders (the DM's messages show
//      up in the pane's .conv-stream) — the leg that would REDDEN against the old
//      control-only pane, which had no conversation at all;
//   2. a home-DM ask renders answerable in place: options enabled, no read-only
//      hint, and a click records the choice through the store (answerAsk);
//   3. the "Open workspace" button routes via the store (openAgent's observable
//      effect: view → "agent", selectedAgentId → the tab's agent).
// The pane is exercised through the exported RightSidebar (FleetPane is
// module-private): driving store.setActiveRightTab("agent:<accountId>") makes
// the RightSidebar Switch render the FleetPane for any resolvable pin, the
// honest integration path.
//
// FleetPane is module-PRIVATE, so we mount the exported RightSidebar and drive
// the store's tab signal — the same seam a real click on the activity bar uses.
// The store is built inside render's reactive root (owned + auto-disposed on the
// library's per-test cleanup) and captured so tests can drive setActiveRightTab
// and re-read view()/selectedAgentId().
function mountRightSidebar(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext.Provider value={store}>
				<RightSidebar />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

// The two visible fixture agents whose home-DM the fleet pane renders. The fleet
// tabs are CONFIGURABLE PINS keyed `agent:${accountId}` (Record A §T2), not a
// hardcoded Supervisor · Warden pair. The pane arm reads the active tab's item
// out of `rightTabGroups()` (SEA-1645 P2), which emits only PINNED agents, so a
// test must pin the agent before activating its tab. Both ids resolve in the
// fixture, so once pinned the pane renders their home-DM inline.
const FLEET_TABS = ["acc-supervisor", "acc-warden"] as const;

// The agent account's home-DM channel id — resolved through the SAME account set
// the store exposes (STUB_ACCOUNTS carries agent accounts with homeChannelId),
// the same coordinate FleetPane's homeDm() derives from a().account.homeChannelId.
function homeChannelForAgent(agentId: string): string {
	const account = STUB_ACCOUNTS.find((a) => a.id === agentId);
	if (!account?.homeChannelId) {
		throw new Error(`agent ${agentId} has no homeChannelId in the fixture`);
	}
	return account.homeChannelId;
}

// The home-DM home topic name a fleet agent's DM renders as a topic row. A DM is
// topics too (Matt's ruling): one home topic, named "general" in the fixture.
const HOME_DM_TOPIC = "general";

// The home-DM messages for a fleet agent, straight from the fixture — used only
// for the non-triviality check that the DM is a real, active conversation.
function homeDmMessages(agentId: string) {
	const channelId = homeChannelForAgent(agentId);
	const topicIds = new Set(
		STUB_TOPICS.filter((t) => t.channelId === channelId).map((t) => t.id),
	);
	return STUB_MESSAGES.filter((m) => topicIds.has(m.topicId));
}

describe("RightSidebar fleet pane (compass-0.7)", () => {
	// pinAgent write-throughs to the default-workspace localStorage key
	// (compass.pinnedAgents.acc-matt), and happy-dom's localStorage is
	// process-wide, so clear it around every case — otherwise pins accumulate
	// across tests and leak into other default-workspace suites (store.test.ts's
	// clearStorage discipline).
	beforeEach(() => globalThis.localStorage.clear());
	afterEach(() => globalThis.localStorage.clear());

	// The inline topic-index leg, one case per fleet tab. In the uniform
	// two-level model a DM is topics too (Matt's ruling), so the pane renders the
	// DM's ChannelView TOPIC INDEX — the home topic as a row — not a flat message
	// stream. Against the OLD control-only FleetPane (a button, no ChannelView)
	// there is no `.topic-index` and zero `.topic-row`. Mutation-check: dropping
	// the inline ChannelView, or binding the wrong channel, changes the rendered
	// topic rows so each reddens independently.
	for (const tab of FLEET_TABS) {
		test(`${tab} tab renders the agent's home-DM topic index inline`, () => {
			const { store, container } = mountRightSidebar();
			store.pinAgent(tab);
			store.setActiveRightTab(`agent:${tab}`);

			// Non-triviality: the fixture DM actually carries messages, so the home
			// topic is a real, active topic — an "it rendered" pass can't be an
			// empty agreement.
			expect(homeDmMessages(tab).length).toBeGreaterThan(1);

			// The topic index exists and holds the DM's home topic row. Old
			// control-only pane: 0.
			const index = container.querySelector(".topic-index");
			expect(index).not.toBeNull();
			const rows = [
				...container.querySelectorAll<HTMLElement>(".topic-index .topic-row"),
			];
			expect(rows.length).toBeGreaterThan(0);
			// The home topic ("general") is one of the rendered rows — proves the
			// pane bound THIS agent's home DM, not some other channel.
			const names = rows.map(
				(r) => r.querySelector(".topic-name")?.textContent ?? "",
			);
			expect(names).toContain(HOME_DM_TOPIC);
		});
	}

	// The supervisor home DM carries a single-select ask (`ask-sup-lane`) in its
	// home topic. In the uniform two-level model the fleet pane shows the DM's
	// TOPIC INDEX (Matt's ruling: steering = open/start a topic), so the ask is
	// answered by drilling into its topic, not inline in the pane. This leg pins
	// that the pane's topic row drills into the ask's topic. It REDDENS if the
	// pane stops rendering the topic index or the row stops routing to the topic.
	test("the supervisor fleet pane's home-DM topic row drills into the topic", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-supervisor");
		store.setActiveRightTab("agent:acc-supervisor");

		// The topic index rendered inside the pane, with the home topic row.
		const row = [
			...container.querySelectorAll<HTMLButtonElement>(
				".topic-index .topic-row",
			),
		].find((r) => r.textContent?.includes(HOME_DM_TOPIC));
		expect(row).toBeDefined();
		if (!row) throw new Error("home-DM topic row not rendered");

		// No owner-routing hint anywhere — the "answer in @X's workspace" gate is
		// gone. This reddens if a read-only gate returns.
		expect(container.querySelector(".ask-readonly-hint")).toBeNull();

		// Clicking the row drills into the topic's message view (openTopic), where
		// the ask is answered.
		fireEvent.click(row);
		expect(store.view()).toBe("topic");
		expect(store.selectedTopic()?.name).toBe(HOME_DM_TOPIC);
	});

	// The "Open workspace" control routes via the store. openAgent's observable
	// contract (store.ts openAgent): view → "agent", selectedAgentId → the tab's
	// agent. We assert those store effects, not that a handler fired. Old pane had
	// the button too, so the button's presence alone isn't the new contract — the
	// inline-conversation legs above carry the regression teeth; this leg pins the
	// routing so a refactor of the button can't silently break navigation.
	test("the Open workspace button routes to the agent's workspace", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-supervisor");
		store.setActiveRightTab("agent:acc-supervisor");

		// Precondition: we start on the board with no agent selected.
		expect(store.view()).toBe("bridge");
		expect(store.selectedAgentId()).toBeNull();

		const button = container.querySelector<HTMLButtonElement>(".r-open-agent");
		expect(button).not.toBeNull();

		fireEvent.click(button as HTMLButtonElement);

		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-supervisor");
	});

	// A resolved fleet tab renders the live pane, not the unreachable block: the
	// pane arm resolves reachability first (SEA-1645). Both real fleet tabs
	// resolve, so this asserts the resolved tab renders a fleet-pane with no
	// in-pane unpin control — the observable inverse that reddens if the arm ever
	// stops resolving a live agent.
	test("a resolved fleet tab renders the live pane, not the unreachable block", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-warden");
		store.setActiveRightTab("agent:acc-warden");

		expect(container.querySelector(".fleet-pane")).not.toBeNull();
		expect(container.querySelector(".fleet-unreachable")).toBeNull();
		expect(container.querySelector(".r-unpin-agent")).toBeNull();
	});

	// An active GHOST pin (an id resolving to no fixture agent) renders the "agent
	// unreachable" pane — the message and a working in-pane unpin control — not
	// FleetPane and not StatusPane (SEA-1645 P2/P6). The pin must exist for the
	// pane arm to read its item out of rightTabGroups(), so pin then activate.
	test("an active ghost pin renders the unreachable pane with a working unpin", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-ghost");
		store.setActiveRightTab("agent:acc-ghost");

		// The unreachable block renders — message + unpin control.
		const block = container.querySelector(".fleet-unreachable");
		expect(block).not.toBeNull();
		expect(block?.querySelector(".term-empty")).not.toBeNull();
		const unpin = container.querySelector<HTMLButtonElement>(".r-unpin-agent");
		expect(unpin).not.toBeNull();

		// It is NOT the live fleet pane (no conversation) and NOT the status pane.
		expect(container.querySelector(".conv-stream")).toBeNull();
		expect(container.querySelector(".r-status")).toBeNull();

		// The unpin control works: it drops the pin and falls the active tab back
		// to status, so the unreachable pane is gone.
		fireEvent.click(unpin as HTMLButtonElement);
		expect(store.isPinned("acc-ghost")).toBe(false);
		expect(store.activeRightTab()).toBe("status");
		expect(container.querySelector(".fleet-unreachable")).toBeNull();
	});

	// The unreachable pane shows the HUMAN HANDLE cached at pin time (OQ-2), not
	// the opaque id — a regression that dropped item().title from the pane (or
	// rendered the raw id) would stay green at store level but reddens here. Seed
	// a {id,handle} pin with a DISTINCTIVE handle into the default-workspace key
	// before mount so the store hydrates it (mountRightSidebar has no workspaceKey
	// hook), then assert the handle reaches the pane's unpin control.
	test("the unreachable pane renders the cached handle, not the raw id", () => {
		globalThis.localStorage.setItem(
			"compass.pinnedAgents.acc-matt",
			JSON.stringify([{ id: "acc-ghost", handle: "ghosthandle" }]),
		);
		const { store, container } = mountRightSidebar();
		store.setActiveRightTab("agent:acc-ghost");

		// Anchor to the unreachable pane so the assertion can't drift onto a
		// same-classed control added elsewhere later.
		expect(container.querySelector(".fleet-unreachable")).not.toBeNull();
		const unpin = container.querySelector<HTMLButtonElement>(".r-unpin-agent");
		expect(unpin).not.toBeNull();
		expect(unpin?.textContent).toContain("ghosthandle");
		expect(unpin?.textContent).not.toContain("acc-ghost");
	});
});
