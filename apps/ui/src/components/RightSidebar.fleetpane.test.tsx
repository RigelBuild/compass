import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import {
	type Ask,
	STUB_ACCOUNTS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
} from "../comms-stub";
import { RIGHT_SIDEBAR_TAB_BY_ID } from "../constants";
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
// module-private): driving store.setActiveRightTab("supervisor"|"warden") makes
// the RightSidebar Switch render the FleetPane, the honest integration path.
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

// The agent id each fleet tab resolves to — read from the tab table the impl
// itself indexes (RightSidebar passes RIGHT_SIDEBAR_TAB_BY_ID.<tab> to
// FleetPane), so the test tracks whatever the table declares rather than a
// copied literal. agentId is optional on ActivityBarItem; a fleet tab must have
// one, so we assert it and narrow.
function agentIdForTab(tab: "supervisor" | "warden"): string {
	const id = RIGHT_SIDEBAR_TAB_BY_ID[tab].agentId;
	if (!id) throw new Error(`fleet tab ${tab} has no agentId in the tab table`);
	return id;
}

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

// The home-DM messages for a fleet tab, straight from the fixture. The DMs are
// flat (no parentMessageId), so each message is its own thread → one .msg row.
function homeDmMessages(tab: "supervisor" | "warden") {
	const channelId = homeChannelForAgent(agentIdForTab(tab));
	return STUB_MESSAGES.filter((m) => m.channelId === channelId);
}

const msgRoles = (container: HTMLElement): string[] =>
	[...container.querySelectorAll<HTMLElement>(".conv-stream .msg-role")].map(
		(el) => el.textContent ?? "",
	);

const FLEET_TABS = ["supervisor", "warden"] as const;

describe("RightSidebar fleet pane (compass-0.7)", () => {
	// The inline conversation leg, one case per fleet tab. Against the OLD
	// control-only FleetPane (a button, no ChannelView) there is no .conv-stream
	// and zero .msg rows — every assertion below reddens. Mutation-check: dropping
	// the inline ChannelView, or binding the wrong channel, changes the .msg count
	// and the rendered author handles, so each reddens independently.
	for (const tab of FLEET_TABS) {
		test(`${tab} tab renders the agent's home-DM conversation inline`, () => {
			const { store, container } = mountRightSidebar();
			store.setActiveRightTab(tab);

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
		store.setActiveRightTab("supervisor");

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
		store.setActiveRightTab("supervisor");

		// Precondition: we start on the board with no agent selected.
		expect(store.view()).toBe("bridge");
		expect(store.selectedAgentId()).toBeNull();

		const button = container.querySelector<HTMLButtonElement>(".r-open-agent");
		expect(button).not.toBeNull();

		fireEvent.click(button as HTMLButtonElement);

		expect(store.view()).toBe("agent");
		expect(store.selectedAgentId()).toBe(agentIdForTab("supervisor"));
	});

	// Fallback: an unresolved agent id yields the "Agent not found." empty state,
	// never a crash or an empty pane. Both real fleet tabs resolve, so this guards
	// the Show fallback arm — but we can only drive it through a real tab, so we
	// assert the resolved tabs render a fleet-pane (not the fallback), which is the
	// observable inverse and reddens if agentFor ever stops resolving.
	test("a resolved fleet tab renders the pane, not the not-found fallback", () => {
		const { store, container } = mountRightSidebar();
		store.setActiveRightTab("warden");

		expect(container.querySelector(".fleet-pane")).not.toBeNull();
		expect(container.querySelector(".term-empty")).toBeNull();
	});
});
