import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { CHAT_TAB_ID, type Pane } from "../agent-tabs";
import { STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { STUB_AGENTS } from "../stub-data";
import { testQueryClient } from "../test-support";
import { ViewContext } from "../view-scope";
import { AgentView, nextFreeTerminalPane } from "./AgentView";

// The tab strip is absent until an agent is opened.

// The fixture agent whose home DM carries a visible message + an interactive ask.
const AGENT_ID = "acc-compass-ui";

// The DM pane lists this agent's home topic.
const HOME_DM_TOPIC = "general";

// This terminal's scrollback proves that it rendered.
const TERM_C1_LINE = "VITE v8.1.0  ready in 247 ms";
const termPaneC1: Pane = {
	id: "t-ui1",
	kind: "terminal",
	title: "vite dev",
	terminalId: "t-ui1",
};
const termPaneC2: Pane = {
	id: "t-ui2",
	kind: "terminal",
	title: "compass-ui:ci",
	terminalId: "t-ui2",
};

// Build the store inside render so its reactive memos are disposed with the test.
function mountAgentView(): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<ViewContext value={store.focusedView()}>
					<AgentView />
				</ViewContext>
			</StoreContext>
		);
	});
	return { store, container };
}

describe("AgentView (T3)", () => {
	// Contract: with no agent selected the workspace shows the empty prompt and
	// NO tab strip — the tab/split UI only exists once an agent is opened.
	test("empty state: shows 'Select an agent.' and renders no tab strip", () => {
		const { container } = mountAgentView();

		expect(container.textContent).toContain("Select an agent.");
		expect(container.querySelector(".av-tabs")).toBeNull();
	});

	// Contract: the chat tab is permanent (design D6). Opening an agent shows
	// exactly one tab — the chat tab, titled "Chat", id CHAT_TAB_ID — with no
	// close affordance; `closeTab(CHAT_TAB_ID)` is a no-op, so it survives.
	test("chat tab is permanent: one 'Chat' tab, no close button, closeTab is a no-op", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);
		flush();

		const tabs = container.querySelectorAll(".av-tab");
		expect(tabs.length).toBe(1);
		expect(tabs[0]?.textContent).toContain("Chat");
		// The chat tab carries no close button (tab.id === CHAT_TAB_ID guard).
		expect(tabs[0]?.querySelector(".av-tab-close")).toBeNull();

		store.focusedView().closeTab(CHAT_TAB_ID);
		flush();

		// Still exactly one tab — the chat tab can't be closed.
		expect(container.querySelectorAll(".av-tab").length).toBe(1);
	});

	// A DM renders its topic index in the chat pane.
	test("chat pane renders the home-DM topic index in the tab tree", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);
		flush();

		const tree = container.querySelector(".av-tree");
		expect(tree).not.toBeNull();

		const topics = [...container.querySelectorAll(".av-tree .topic-name")].map(
			(n) => n.textContent,
		);
		expect(topics.some((t) => t?.includes(HOME_DM_TOPIC))).toBe(true);
	});

	// Contract (Matt's ruling: steering an agent means opening/starting a topic):
	// the DM's topic index in the chat pane drills into a topic on click. Clicking
	// the home-DM topic row routes to that topic's message view (openTopic → the
	// `/channel/:channelId/topic/:topicId` route), where the ask is answered.
	test("home-DM topic row drills into the topic on click", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);
		flush();

		const tree = container.querySelector(".av-tree");
		expect(tree).not.toBeNull();

		const row = [
			...container.querySelectorAll<HTMLButtonElement>(".av-tree .topic-row"),
		].find((r) => r.textContent?.includes(HOME_DM_TOPIC));
		expect(row).toBeDefined();
		if (!row) throw new Error("home-DM topic row not rendered");

		fireEvent.click(row);
		flush();

		// The click drilled into the topic: the store now selects it and the view
		// flipped to the topic message surface.
		expect(store.view()).toBe("topic");
		expect(store.selectedTopic()?.name).toBe(HOME_DM_TOPIC);
	});

	// Contract: opening a terminal adds it as a second tab and makes it active;
	// the active tab's body renders that terminal's scrollback (`.term-body`).
	test("terminal opens as a second, active tab rendering its scrollback", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);
		flush();
		store.focusedView().openTab(termPaneC1);
		flush();

		// Chat tab + the new terminal tab.
		expect(container.querySelectorAll(".av-tab").length).toBe(2);

		// Only the active tab's tree renders, so a visible `.term-body` proves the
		// terminal tab is active — and it carries the real t-ui1 scrollback.
		const termBody = container.querySelector(".av-tree .term-body");
		expect(termBody).not.toBeNull();
		expect(termBody?.textContent).toContain(TERM_C1_LINE);
	});

	// Contract: splitting the active terminal tab's focused pane places the new
	// terminal beside it — the active tab's tree becomes a `.av-split.row` with
	// two `.av-pane` leaves.
	test("terminal opens as a split: two panes under one row split", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);
		flush();
		store.focusedView().openTab(termPaneC1);
		flush();
		store.focusedView().splitFocused(termPaneC2, "row");
		flush();

		const split = container.querySelector(".av-tree .av-split.row");
		expect(split).not.toBeNull();
		expect(split?.querySelectorAll(".av-pane").length).toBe(2);
	});
});

// Terminal controls must remain usable after fixture terminals run out.
describe("AgentView always-open '+'/split (regression)", () => {
	// Assert the fixture facts this suite leans on straight from the source, so a
	// fixture reshuffle reddens here rather than silently weakening the tests.
	const agentTerminalCount = (id: string): number => {
		const agent = STUB_AGENTS.find((a) => a.account.id === id);
		if (!agent) throw new Error(`fixture missing ${id}`);
		return agent.terminals.length;
	};

	test("fixture ground truth: supervisor has zero terminals, compass-ui has two", () => {
		expect(agentTerminalCount("acc-supervisor")).toBe(0);
		expect(agentTerminalCount("acc-compass-ui")).toBe(2);
	});

	// A zero-fixture agent exposes the disabled-button bug on its first click.
	test("zero-fixture agent: '+' is enabled and clicking it opens a placeholder tab", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-supervisor");
		flush();

		// Only the permanent chat tab to start (no fixture terminals to auto-open).
		expect(container.querySelectorAll(".av-tab").length).toBe(1);

		const newBtn = container.querySelector<HTMLButtonElement>(".av-tab-new");
		expect(newBtn).not.toBeNull();
		if (!newBtn) throw new Error("'+' new-tab button not rendered");
		// The catch: old code left this disabled for a zero-terminal agent.
		expect(newBtn.disabled).toBe(false);

		fireEvent.click(newBtn);
		flush();

		// A second tab appeared — the minted placeholder terminal tab.
		const tabs = container.querySelectorAll(".av-tab");
		expect(tabs.length).toBe(2);
		expect(tabs[1]?.textContent).toContain("Terminal 1");

		// The active tab's tree renders the placeholder's muted "starting" body,
		// never an error — and never a real fixture `.term-body`.
		const empty = container.querySelector(".av-tree .av-leaf-empty");
		expect(empty).not.toBeNull();
		expect(empty?.textContent).toContain("Terminal starting…");
		expect(container.querySelector(".av-tree .term-body")).toBeNull();
	});

	// After fixture exhaustion, opening another tab must create a placeholder.
	test("fixture exhaustion (compass-ui): '+' stays enabled and mints unique placeholder tabs", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-compass-ui");
		flush();

		// Exhaust both fixture terminals through the real store action.
		store.focusedView().openTab(termPaneC1);
		flush();
		store.focusedView().openTab(termPaneC2);
		flush();
		expect(container.querySelectorAll(".av-tab").length).toBe(3);
		// No unplaced fixture terminal remains — the fallback path is now live.
		const selected = store.selectedAgent();
		if (!selected) throw new Error("compass-ui not selected");
		expect(
			nextFreeTerminalPane(selected, store.focusedView().agentTabs()),
		).toBe(undefined);

		const newBtn = (): HTMLButtonElement => {
			const btn = container.querySelector<HTMLButtonElement>(".av-tab-new");
			if (!btn) throw new Error("'+' new-tab button not rendered");
			return btn;
		};
		// The catch: old code disabled "+" once fixtures were spent.
		expect(newBtn().disabled).toBe(false);

		fireEvent.click(newBtn());
		flush();
		expect(container.querySelectorAll(".av-tab").length).toBe(4);
		expect(newBtn().disabled).toBe(false);
		fireEvent.click(newBtn());
		flush();
		expect(container.querySelectorAll(".av-tab").length).toBe(5);

		// The two minted tabs carry distinct ids (the store's monotonic counter),
		// distinct from the fixture tabs — no collision, no dedupe-swallow.
		const ids = store
			.focusedView()
			.agentTabs()
			.map((t) => t.id);
		expect(new Set(ids).size).toBe(ids.length);
		const minted = ids.filter((id) => id.startsWith("term-acc-compass-ui-"));
		expect(minted.length).toBe(2);
		expect(new Set(minted).size).toBe(2);
	});

	// A zero-fixture agent's first split must use a minted placeholder.
	test("split stays open after fixtures: clicking a split button grows the tree with a placeholder pane", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-supervisor");
		flush();

		// Open the supervisor's first (minted) terminal tab so its pane shows split
		// buttons — supervisor has zero fixtures, so this pane is itself a placeholder.
		const newTab = container.querySelector<HTMLButtonElement>(".av-tab-new");
		if (!newTab) throw new Error("'+' new-tab button not rendered");
		fireEvent.click(newTab);
		flush();
		expect(container.querySelectorAll(".av-tree .av-pane").length).toBe(1);

		const splitRight = container.querySelector<HTMLButtonElement>(
			'.av-tree .av-pane-btn[aria-label="Split right"]',
		);
		expect(splitRight).not.toBeNull();
		if (!splitRight) throw new Error("split-right button not rendered");
		// The catch: old code disabled the split buttons with no fixture to place.
		expect(splitRight.disabled).toBe(false);

		fireEvent.click(splitRight);
		flush();

		// The tab's tree is now a row split with two panes — the split did work.
		const rowSplit = container.querySelector(".av-tree .av-split.row");
		expect(rowSplit).not.toBeNull();
		expect(rowSplit?.querySelectorAll(".av-pane").length).toBe(2);
		// Both leaves are minted placeholders → two "starting" bodies, no error.
		const empties = container.querySelectorAll(".av-tree .av-leaf-empty");
		expect(empties.length).toBe(2);
		expect(
			[...empties].every((e) => e.textContent?.includes("Terminal starting…")),
		).toBe(true);
	});

	// Minted placeholders render the starting state, never a missing-terminal error.
	test("minted placeholder renders the 'Terminal starting…' empty state, never an error", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-supervisor");
		flush();
		const newTab = container.querySelector<HTMLButtonElement>(".av-tab-new");
		if (!newTab) throw new Error("'+' new-tab button not rendered");
		fireEvent.click(newTab);
		flush();

		const empty = container.querySelector(".av-tree .av-leaf-empty");
		expect(empty).not.toBeNull();
		expect(empty?.textContent).toContain("Terminal starting…");
		expect(container.textContent).not.toContain("Terminal not found.");
	});
});
