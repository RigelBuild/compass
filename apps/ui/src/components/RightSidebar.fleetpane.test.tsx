import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import {
	type Ask,
	STUB_ACCOUNTS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
} from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
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
		store = createAppStore({ initialComms: STUB_COMMS_STATE });
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

// The handle a message author renders as (the .msg-role text). Resolved through
// the fixture accounts, so a fixture reshuffle can't stale a hardcoded handle.
function handleFor(accountId: string): string {
	const account = STUB_ACCOUNTS.find((a) => a.id === accountId);
	if (!account) throw new Error(`no account for author ${accountId}`);
	return account.handle;
}

// The home-DM messages for a fleet agent, straight from the fixture. The DMs are
// flat (no parentMessageId), so each message is its own thread → one .msg row.
function homeDmMessages(agentId: string) {
	const channelId = homeChannelForAgent(agentId);
	return STUB_MESSAGES.filter((m) => m.channelId === channelId);
}

const msgRoles = (container: HTMLElement): string[] =>
	[...container.querySelectorAll<HTMLElement>(".conv-stream .msg-role")].map(
		(el) => el.textContent ?? "",
	);

describe("RightSidebar fleet pane (compass-0.7)", () => {
	// pinAgent write-throughs to the default-workspace localStorage key
	// (compass.pinnedAgents.acc-matt), and happy-dom's localStorage is
	// process-wide, so clear it around every case — otherwise pins accumulate
	// across tests and leak into other default-workspace suites (store.test.ts's
	// clearStorage discipline).
	beforeEach(() => globalThis.localStorage.clear());
	afterEach(() => globalThis.localStorage.clear());

	// The inline conversation leg, one case per fleet tab. Against the OLD
	// control-only FleetPane (a button, no ChannelView) there is no .conv-stream
	// and zero .msg rows — every assertion below reddens. Mutation-check: dropping
	// the inline ChannelView, or binding the wrong channel, changes the .msg count
	// and the rendered author handles, so each reddens independently.
	for (const tab of FLEET_TABS) {
		test(`${tab} tab renders the agent's home-DM conversation inline`, () => {
			const { store, container } = mountRightSidebar();
			store.pinAgent(tab);
			store.setActiveRightTab(`agent:${tab}`);

			const messages = homeDmMessages(tab);
			// Non-triviality: the fixture DM actually carries a multi-message
			// exchange, so an "it rendered" pass can't be an empty agreement.
			expect(messages.length).toBeGreaterThan(1);

			// The conversation surface exists and holds exactly the DM's messages
			// (flat DM → one .msg per message). Old control-only pane: 0.
			const stream = container.querySelector(".conv-stream");
			expect(stream).not.toBeNull();
			expect(container.querySelectorAll(".conv-stream .msg").length).toBe(
				messages.length,
			);

			// Every distinct author in the DM appears as a rendered message role —
			// proves the pane bound THIS agent's home DM, not some other channel.
			const roles = msgRoles(container);
			const expectedHandles = new Set(
				messages.map((m) => handleFor(m.authorAccountId)),
			);
			expect(expectedHandles.size).toBeGreaterThan(0);
			for (const handle of expectedHandles) {
				expect(roles).toContain(handle);
			}
		});
	}

	// The supervisor home DM carries a single-select ask (`ask-sup-lane`). Per
	// Matt's kill-the-gate ruling, the fleet pane renders it ANSWERABLE in place:
	// options enabled, no owner-routing hint, and a click settles it through the
	// store. This is the leg the review flagged as missing (M2). It REDDENS if a
	// read-only gate (`readonlyAsks`) is re-introduced on the fleet mount — the
	// options would go `disabled` and an `.ask-readonly-hint` would render — and
	// it reddens if the click stops reaching `store.answerAsk`.
	const SUP_ASK_ID = "ask-sup-lane";
	// The supervisor ask's block, read out of the store's reactive message list
	// (not the fixture) so a click's mutation is observable and the option order
	// matches what `<For each={ask().options}>` renders.
	const supAskIn = (store: AppStore): Ask => {
		for (const m of store.messages()) {
			for (const b of m.blocks) {
				if (b.kind === "ask" && b.ask.askId === SUP_ASK_ID) return b.ask;
			}
		}
		throw new Error(`no message in the store carries ask ${SUP_ASK_ID}`);
	};

	test("the supervisor fleet pane renders a home-DM ask answerable in place", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-supervisor");
		store.setActiveRightTab("agent:acc-supervisor");

		// The ask actually rendered inside the pane's conversation stream.
		const askBlock = container.querySelector(".conv-stream .block-ask");
		expect(askBlock).not.toBeNull();

		// Its options are present and NOT disabled — a fresh single-select ask,
		// `locked()` is false and there is no read-only gate on the fleet mount.
		const options = [
			...container.querySelectorAll<HTMLButtonElement>(
				".conv-stream .block-ask .ask-option",
			),
		];
		expect(options.length).toBeGreaterThan(0);
		for (const opt of options) {
			expect(opt.disabled).toBe(false);
		}

		// No owner-routing hint anywhere — the "answer in @X's workspace" gate is
		// gone. This is the teeth that reddens if a read-only gate returns.
		expect(container.querySelector(".ask-readonly-hint")).toBeNull();

		// Interaction teeth: clicking the first option records that option's id in
		// the store, proving the pane's ask is wired to answerAsk (not just visually
		// enabled). The rendered option order matches the store ask's options, so
		// the first button carries the first option id.
		const firstOptionId = supAskIn(store).questions[0].options[0].id;
		expect(supAskIn(store).questions[0].chosenOptionIds).toEqual([]);
		fireEvent.click(options[0]);
		expect(supAskIn(store).questions[0].chosenOptionIds).toEqual([
			firstOptionId,
		]);
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
