import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { flush } from "solid-js";
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

// Exercise the fleet pane through exported RightSidebar because FleetPane is private.
// Verify home-DM topic rows and store routing through the same tab seam used by the activity bar.
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

// Pin agents before activating tabs; only pinned agents appear in rightTabGroups().
const FLEET_TABS = ["acc-supervisor", "acc-compass-ui"] as const;

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

describe("RightSidebar fleet pane", () => {
	// Clear process-wide localStorage because pinAgent writes there.
	beforeEach(() => globalThis.localStorage.clear());
	afterEach(() => globalThis.localStorage.clear());

	// Each pinned fleet tab shows its home-DM topic index; the old pane had no topic rows.
	for (const tab of FLEET_TABS) {
		test(`${tab} tab renders the agent's home-DM topic index inline`, () => {
			const { store, container } = mountRightSidebar();
			store.pinAgent(tab);
			store.setActiveRightTab(`agent:${tab}`);
			flush();

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

	// The home-DM topic row routes to the supervisor's ask topic; it is not an inline stream.
	test("the supervisor fleet pane's home-DM topic row drills into the topic", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-supervisor");
		store.setActiveRightTab("agent:acc-supervisor");
		flush();

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
		flush();
		expect(store.view()).toBe("topic");
		expect(store.selectedTopic()?.name).toBe(HOME_DM_TOPIC);
	});

	// Check workspace routing through store effects; inline topic legs cover pane rendering.
	test("the Open workspace button routes to the agent's workspace", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-supervisor");
		store.setActiveRightTab("agent:acc-supervisor");
		flush();

		// Precondition: we start on the board with no agent selected.
		expect(store.view()).toBe("bridge");
		expect(store.selectedAgentId()).toBeNull();

		const button = container.querySelector<HTMLButtonElement>(".r-open-agent");
		expect(button).not.toBeNull();

		fireEvent.click(button as HTMLButtonElement);
		flush();

		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe("acc-supervisor");
	});

	// A resolved pin shows the live pane, not the unreachable block or unpin control.
	test("a resolved fleet tab renders the live pane, not the unreachable block", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-compass-ui");
		store.setActiveRightTab("agent:acc-compass-ui");
		flush();

		expect(container.querySelector(".fleet-pane")).not.toBeNull();
		expect(container.querySelector(".fleet-unreachable")).toBeNull();
		expect(container.querySelector(".r-unpin-agent")).toBeNull();
	});

	// An active GHOST pin (an id resolving to no fixture agent) renders the "agent
	// unreachable" pane — the message and a working in-pane unpin control — not
	// FleetPane and not StatusPane (RIG-1645 P2/P6). The pin must exist for the
	// pane arm to read its item out of rightTabGroups(), so pin then activate.
	test("an active ghost pin renders the unreachable pane with a working unpin", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-ghost");
		store.setActiveRightTab("agent:acc-ghost");
		flush();

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
		flush();
		expect(store.isPinned("acc-ghost")).toBe(false);
		expect(store.activeRightTab()).toBe("status");
		expect(container.querySelector(".fleet-unreachable")).toBeNull();
	});

	// Seed a cached handle so the unreachable pane shows a label instead of an opaque id.
	test("the unreachable pane renders the cached handle, not the raw id", () => {
		globalThis.localStorage.setItem(
			"compass.pinnedAgents.acc-matt",
			JSON.stringify([{ id: "acc-ghost", handle: "ghosthandle" }]),
		);
		const { store, container } = mountRightSidebar();
		store.setActiveRightTab("agent:acc-ghost");
		flush();

		// Anchor to the unreachable pane so the assertion can't drift onto a
		// same-classed control added elsewhere later.
		expect(container.querySelector(".fleet-unreachable")).not.toBeNull();
		const unpin = container.querySelector<HTMLButtonElement>(".r-unpin-agent");
		expect(unpin).not.toBeNull();
		expect(unpin?.textContent).toContain("ghosthandle");
		expect(unpin?.textContent).not.toContain("acc-ghost");
	});
});
