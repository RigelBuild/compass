import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import {
	type AppStore,
	CHAT_TAB_ID,
	createAppStore,
	type Pane,
} from "../store";
import { STUB_AGENTS } from "../stub-data";
import { AgentView, nextFreeTerminalPane } from "./AgentView";

// RED acceptance spec for T3 (design.md §504-545): the rebuilt AgentView — a
// tab strip (chat tab permanent + terminal button) over each tab's split-pane
// tree, the chat pane rendering the agent's home-DM ChannelView. It fails today
// because AgentView is a minimal shell that renders only <ChannelView/> with no
// tabs/tree — every assertion below is an absence-of-workspace-surface failure
// (no `.av-tabs`, no `.av-tree`, no `.av-split`), never a module-load error. An
// implementer makes it green next.
//
// Fixture ground truth (grepped from comms-stub.ts / stub-data.ts, quoted here):
//   - agent `acc-cook`, home DM `dm-cook`. `openAgent("acc-cook")` centers
//     `selectedChannel` on dm-cook, so the chat pane's ChannelView renders it.
//   - dm-cook message `msg-dm-f1` (text) and `msg-dm-f2` (an ask).
//   - cook terminals `t-c1` ("vite dev") and `t-c2` ("compass-ui:ci").

// The fixture agent whose home DM carries a visible message + an interactive ask.
const AGENT_ID = "acc-cook";

// dm-cook `msg-dm-f1` block text — the home-DM message the chat pane must show.
const HOME_DM_MESSAGE =
	"Comms-in-workspace shell is up — board primary, channels/DMs in the left rail, chat folded into the agent workspace. Want to walk the layout?";

// dm-cook `msg-dm-f2` ask (`ask-cook-layout`) option labels (single-select).
const ASK_OPTION_KEEP = "Keep top-bar tab";
const ASK_OPTION_MOVE = "Move to left rail";

// cook's two terminals, as the terminal panes `openTab`/`splitActivePane` place.
// `t-c1`'s scrollback carries this line — proof the right terminal rendered.
const TERM_C1_LINE = "VITE v8.1.0  ready in 247 ms";
const termPaneC1: Pane = {
	id: "t-c1",
	kind: "terminal",
	title: "vite dev",
	terminalId: "t-c1",
};
const termPaneC2: Pane = {
	id: "t-c2",
	kind: "terminal",
	title: "compass-ui:ci",
	terminalId: "t-c2",
};

// Mount AgentView over a real store through the app's StoreContext (index.tsx
// wires it as `<StoreContext.Provider value={store}>`; there is no separate
// provider wrapper). The store is built inside render's reactive root so its
// memos are owned and disposed on the library's per-test cleanup; the reference
// is captured so tests drive the store's actions and re-query the live DOM.
function mountAgentView(): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({ initialComms: STUB_COMMS_STATE });
		return (
			<StoreContext.Provider value={store}>
				<AgentView />
			</StoreContext.Provider>
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

		const tabs = container.querySelectorAll(".av-tab");
		expect(tabs.length).toBe(1);
		expect(tabs[0]?.textContent).toContain("Chat");
		// The chat tab carries no close button (tab.id === CHAT_TAB_ID guard).
		expect(tabs[0]?.querySelector(".av-tab-close")).toBeNull();

		store.closeTab(CHAT_TAB_ID);

		// Still exactly one tab — the chat tab can't be closed.
		expect(container.querySelectorAll(".av-tab").length).toBe(1);
	});

	// Contract: the chat pane renders the agent's home DM through ChannelView,
	// inside the active tab's split tree — so the fixture home-DM message text is
	// visible within `.av-tree` (not merely somewhere on screen).
	test("chat pane renders the home-DM conversation in the tab tree", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);

		const tree = container.querySelector(".av-tree");
		expect(tree).not.toBeNull();

		const shown = [...container.querySelectorAll(".av-tree .msg-text")].map(
			(n) => n.textContent,
		);
		expect(shown.some((t) => t?.includes(HOME_DM_MESSAGE))).toBe(true);
	});

	// Contract (design D3: asks are fully interactive in the workspace chat pane):
	// the home-DM ask renders its options as enabled `.ask-option` buttons inside
	// the chat pane; clicking one records the answer through the store, and the
	// re-rendered ask is `.answered` with every option now disabled.
	test("home-DM ask is answerable in the chat pane: click settles + disables it", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);

		const tree = container.querySelector(".av-tree");
		expect(tree).not.toBeNull();

		const options = () => [
			...container.querySelectorAll<HTMLButtonElement>(".av-tree .ask-option"),
		];
		const keep = options().find((o) =>
			o.textContent?.includes(ASK_OPTION_KEEP),
		);
		const move = options().find((o) =>
			o.textContent?.includes(ASK_OPTION_MOVE),
		);
		expect(keep).toBeDefined();
		expect(move).toBeDefined();
		// A single-select ask starts unsettled: its options are live.
		expect(keep?.disabled).toBe(false);

		if (!keep) throw new Error("ask option not rendered");
		fireEvent.click(keep);

		// The store mutation is reflected in the re-rendered chat pane: the ask is
		// settled (answered) and every option is now locked.
		const ask = container.querySelector(".av-tree .block-ask");
		expect(ask?.classList.contains("answered")).toBe(true);
		expect(options().length).toBeGreaterThan(0);
		expect(options().every((o) => o.disabled)).toBe(true);
	});

	// Contract: opening a terminal adds it as a second tab and makes it active;
	// the active tab's body renders that terminal's scrollback (`.term-body`).
	test("terminal opens as a second, active tab rendering its scrollback", () => {
		const { store, container } = mountAgentView();
		store.openAgent(AGENT_ID);
		store.openTab(termPaneC1);

		// Chat tab + the new terminal tab.
		expect(container.querySelectorAll(".av-tab").length).toBe(2);

		// Only the active tab's tree renders, so a visible `.term-body` proves the
		// terminal tab is active — and it carries the real t-c1 scrollback.
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
		store.openTab(termPaneC1);
		store.splitActivePane(termPaneC2, "row");

		const split = container.querySelector(".av-tree .av-split.row");
		expect(split).not.toBeNull();
		expect(split?.querySelectorAll(".av-pane").length).toBe(2);
	});
});

// Regression suite for the always-open "+"/split fix (impl in the worktree):
// AgentView's "+" new-tab button and each pane's split buttons are ALWAYS
// enabled for every agent. When no unplaced FIXTURE terminal remains,
// `openTerminalTab`/`splitWith` fall back to `store.newTerminalPane(agent)`,
// which mints a fresh placeholder pane (`term-<accountId>-<n>`, title
// "Terminal <n>", terminalId matching no fixture → the muted "Terminal starting…"
// body). The OLD code carried `disabled={!nextFreeTerminalPane(...)}` on these
// buttons, so once fixtures were exhausted (immediately, for a zero-terminal
// agent) the buttons went dead and a click was a no-op. Each test below asserts
// observable behavior — button enabled AND a click grows the tab/split tree with
// a rendered placeholder — so every one would FAIL against the disabled variant.
//
// Fixture ground truth (verified from stub-data.ts STUB_AGENTS, quoted here):
//   - acc-supervisor / acc-warden: `terminals: []` — ZERO fixture terminals, so
//     `nextFreeTerminalPane` returns undefined from the first "+" click. The old
//     code disabled "+" immediately for them → the cleanest always-open proof.
//   - acc-cook: terminals `t-c1` + `t-c2` (2) — exhausted after two opens.
describe("AgentView always-open '+'/split (regression)", () => {
	// Assert the fixture facts this suite leans on straight from the source, so a
	// fixture reshuffle reddens here rather than silently weakening the tests.
	const agentTerminalCount = (id: string): number => {
		const agent = STUB_AGENTS.find((a) => a.account.id === id);
		if (!agent) throw new Error(`fixture missing ${id}`);
		return agent.terminals.length;
	};

	test("fixture ground truth: supervisor/warden have zero terminals, cook has two", () => {
		expect(agentTerminalCount("acc-supervisor")).toBe(0);
		expect(agentTerminalCount("acc-warden")).toBe(0);
		expect(agentTerminalCount("acc-cook")).toBe(2);
	});

	// PRIMARY REGRESSION. A zero-fixture-terminal agent (supervisor): the "+"
	// button is enabled from the first render, and clicking it opens a new
	// terminal tab whose pane is a minted placeholder — titled "Terminal 1", its
	// body the "Terminal starting…" empty state. The OLD `disabled={!next…}` code
	// disabled "+" immediately here (no fixture terminal to place) → the
	// `newBtn.disabled` assertion below is exactly what catches the old bug, and
	// the tab-count-grows + placeholder-title assertions confirm the click did
	// real work rather than being swallowed by a dead button.
	test("zero-fixture agent: '+' is enabled and clicking it opens a placeholder tab", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-supervisor");

		// Only the permanent chat tab to start (no fixture terminals to auto-open).
		expect(container.querySelectorAll(".av-tab").length).toBe(1);

		const newBtn = container.querySelector<HTMLButtonElement>(".av-tab-new");
		expect(newBtn).not.toBeNull();
		if (!newBtn) throw new Error("'+' new-tab button not rendered");
		// The catch: old code left this disabled for a zero-terminal agent.
		expect(newBtn.disabled).toBe(false);

		fireEvent.click(newBtn);

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

	// Fixture EXHAUSTION. cook has two fixture terminals. Open both as tabs, then
	// assert `nextFreeTerminalPane` is spent (undefined) yet "+" stays enabled and
	// a further click opens ANOTHER tab — a minted placeholder. Assert the tab
	// count keeps growing and each new tab id is unique (no collision with the
	// fixtures or with each other, no dedupe-swallow). The old code disabled "+"
	// at this exact point.
	test("fixture exhaustion (cook): '+' stays enabled and mints unique placeholder tabs", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-cook");

		// Exhaust both fixture terminals through the real store action.
		store.openTab(termPaneC1);
		store.openTab(termPaneC2);
		expect(container.querySelectorAll(".av-tab").length).toBe(3);
		// No unplaced fixture terminal remains — the fallback path is now live.
		const cook = store.selectedAgent();
		if (!cook) throw new Error("cook not selected");
		expect(nextFreeTerminalPane(cook, store.agentTabs())).toBe(undefined);

		const newBtn = (): HTMLButtonElement => {
			const btn = container.querySelector<HTMLButtonElement>(".av-tab-new");
			if (!btn) throw new Error("'+' new-tab button not rendered");
			return btn;
		};
		// The catch: old code disabled "+" once fixtures were spent.
		expect(newBtn().disabled).toBe(false);

		fireEvent.click(newBtn());
		expect(container.querySelectorAll(".av-tab").length).toBe(4);
		expect(newBtn().disabled).toBe(false);
		fireEvent.click(newBtn());
		expect(container.querySelectorAll(".av-tab").length).toBe(5);

		// The two minted tabs carry distinct ids (the store's monotonic counter),
		// distinct from the fixture tabs — no collision, no dedupe-swallow.
		const ids = store.agentTabs().map((t) => t.id);
		expect(new Set(ids).size).toBe(ids.length);
		const minted = ids.filter((id) => id.startsWith("term-acc-cook-"));
		expect(minted.length).toBe(2);
		expect(new Set(minted).size).toBe(2);
	});

	// Split always-open. Inside a terminal tab whose fixture pane is placed, the
	// pane's split buttons stay enabled and clicking one grows the split tree with
	// a placeholder pane (rather than a disabled no-op). Uses a zero-fixture agent
	// so the first split already lands on the minted fallback — the old code
	// disabled the split buttons the moment `nextFreeTerminalPane` was undefined.
	test("split stays open after fixtures: clicking a split button grows the tree with a placeholder pane", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-warden");

		// Open the warden's first (minted) terminal tab so its pane shows split
		// buttons — warden has zero fixtures, so this pane is itself a placeholder.
		const newTab = container.querySelector<HTMLButtonElement>(".av-tab-new");
		if (!newTab) throw new Error("'+' new-tab button not rendered");
		fireEvent.click(newTab);
		expect(container.querySelectorAll(".av-tree .av-pane").length).toBe(1);

		const splitRight = container.querySelector<HTMLButtonElement>(
			'.av-tree .av-pane-btn[aria-label="Split right"]',
		);
		expect(splitRight).not.toBeNull();
		if (!splitRight) throw new Error("split-right button not rendered");
		// The catch: old code disabled the split buttons with no fixture to place.
		expect(splitRight.disabled).toBe(false);

		fireEvent.click(splitRight);

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

	// Graceful placeholder render: a minted placeholder never surfaces an error
	// string — its body is the muted "Terminal starting…" empty state, and the
	// text "Terminal not found." appears nowhere. Guards the TerminalBody fallback
	// that makes the always-open buttons safe to click past the fixture supply.
	test("minted placeholder renders the 'Terminal starting…' empty state, never an error", () => {
		const { store, container } = mountAgentView();
		store.openAgent("acc-supervisor");
		const newTab = container.querySelector<HTMLButtonElement>(".av-tab-new");
		if (!newTab) throw new Error("'+' new-tab button not rendered");
		fireEvent.click(newTab);

		const empty = container.querySelector(".av-tree .av-leaf-empty");
		expect(empty).not.toBeNull();
		expect(empty?.textContent).toContain("Terminal starting…");
		expect(container.textContent).not.toContain("Terminal not found.");
	});
});
