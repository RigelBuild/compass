import { describe, expect, test } from "bun:test";
import { createMemo, createRoot } from "solid-js";
import type { Ask, AskQuestion } from "./comms-stub";
import { STUB_CHANNELS, STUB_COMMS_STATE, STUB_MESSAGES } from "./comms-stub";
import {
	type AgentTab,
	type AppStore,
	CALLER_ID,
	CHAT_TAB_ID,
	createAppStore,
	type Pane,
	type SplitNode,
	splitPaneIds,
	splitPaneOnce,
	splitPanes,
	type View,
} from "./store";
import { STUB_AGENTS, STUB_ASSIGNED_ISSUES } from "./stub-data";

// The store exposes SolidJS `createMemo` accessors (selectedAgent,
// selectedWorkstream, agentRepos, agentSession) that only
// compute inside a reactive root. `withStore` builds a fresh store inside its own
// `createRoot`, runs the body, then disposes. Actions are synchronous and memo
// reads recompute on demand within the root, so every assertion runs inside the
// root before it is torn down — no effects, no waiting, no cross-test leakage.
//
// The store no longer boots from the comms fixture — its comms surface is fed by
// the live SubscribeComms stream. These tests are OFFLINE (no client), so they
// seed the same fixture explicitly through `initialComms`: the behavior asserted
// below (membership transitions, boot selection, ask recording, routing) is the
// store's reduction over a populated comms state, whatever its origin.
function withStore(body: (store: AppStore) => void): void {
	createRoot((dispose) => {
		const store = createAppStore({ initialComms: STUB_COMMS_STATE });
		try {
			body(store);
		} finally {
			dispose();
		}
	});
}

// The async cousin of `withStore` for the seam-backed accessors. `assignedIssues`
// loads through the TrackerSeam on a microtask (store.ts `loadAssignedIssues`), so
// its tests must await a tick INSIDE the live root before disposing. The store is
// built inside `createRoot` (createMemo needs an owner); the body then runs and
// awaits outside the callback but before dispose, so the reactive owner stays
// alive across the whole async body — and the tick is a microtask, never a timer.
async function withStoreAsync(
	body: (store: AppStore) => Promise<void>,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({ initialComms: STUB_COMMS_STATE });
	});
	try {
		await body(store);
	} finally {
		dispose();
	}
}

// The first ask in the comms fixture, with the message coordinates answerAsk
// needs and its (single) question. Derived (not hardcoded) so it survives a
// fixture reshuffle; the tests assert behavior relative to whatever it finds.
function firstCommsAsk(): {
	messageId: string;
	ask: Ask;
	question: AskQuestion;
} {
	for (const m of STUB_MESSAGES) {
		for (const b of m.blocks) {
			if (b.kind === "ask") {
				return { messageId: m.id, ask: b.ask, question: b.ask.questions[0] };
			}
		}
	}
	throw new Error("fixture has no ask block — answerAsk tests need one");
}

// Read a message's ask's (single) question (by ids) out of the store's reactive
// message list, so a test observes the post-mutation state through the public
// accessor.
function questionIn(
	store: AppStore,
	messageId: string,
	askId: string,
): AskQuestion | undefined {
	const msg = store.messages().find((m) => m.id === messageId);
	for (const b of msg?.blocks ?? []) {
		if (b.kind === "ask" && b.ask.askId === askId) return b.ask.questions[0];
	}
	return undefined;
}

// A tab's pane ids in render order, tolerant of an absent tab (returns [] rather than
// a thrown non-null assertion), so a missing tab reddens the toEqual rather than
// crashing — keeping the teeth without a `!` (clears biome noNonNullAssertion).
const paneIds = (tab: AgentTab | undefined): string[] =>
	tab ? splitPaneIds(tab.layout) : [];

describe("initial state", () => {
	// The store boots onto the bridge surface (the default view, compass-0.7
	// §421) with nothing in the agent view yet: the anchor every transition test
	// builds on. A regression that mounted the shell elsewhere or with an agent
	// already selected breaks here.
	test("boots onto the bridge surface with the agent view inert", () => {
		withStore((s) => {
			expect(s.view()).toBe("bridge");
			expect(s.selectedAgentId()).toBeNull();
			expect(s.selectedWorkstreamId()).toBe("ws-1022");
			// The seeded workstream resolves through the memo to a real object.
			expect(s.selectedWorkstream()?.id).toBe("ws-1022");
			// No agent selected -> the resolved-agent memo is empty.
			expect(s.selectedAgent()).toBeUndefined();
			// No agent selected → the repo derivations return their empty guards.
			expect(s.agentRepos()).toEqual([]);
			expect(s.activeRepo()).toBeUndefined();
		});
	});
});

describe("view routing", () => {
	// The top-bar routes swap the whole primary surface. Each case starts from a
	// DIFFERENT view so the assertion proves the transition (not the boot
	// default): a setter wired to the wrong view, or a no-op setter, leaves the
	// view at `from` and reddens.
	const routes: {
		name: string;
		from: (s: AppStore) => void;
		act: (s: AppStore) => void;
		view: View;
	}[] = [
		{
			name: "showBridge",
			from: (s) => s.showSettings(),
			act: (s) => s.showBridge(),
			view: "bridge",
		},
		{
			name: "showBacklog",
			from: (s) => s.showBridge(),
			act: (s) => s.showBacklog(),
			view: "backlog",
		},
		{
			name: "showDone",
			from: (s) => s.showBridge(),
			act: (s) => s.showDone(),
			view: "done",
		},
		{
			name: "showSettings",
			from: (s) => s.showBridge(),
			act: (s) => s.showSettings(),
			view: "settings",
		},
	];
	for (const r of routes) {
		test(`${r.name} routes to ${r.view}`, () => {
			withStore((s) => {
				r.from(s);
				r.act(s);
				expect(s.view()).toBe(r.view);
			});
		});
	}
});

describe("openAgent", () => {
	// Selecting a new agent routes to its view and syncs the workstream selection
	// to that agent's primary (first-assigned) workstream and the repo pick to the
	// agent's clone.
	test("opens the agent view and syncs selection to the primary workstream + repo", () => {
		withStore((s) => {
			s.openAgent("acc-cook");

			expect(s.view()).toBe("agent");
			expect(s.selectedAgentId()).toBe("acc-cook");
			// The memo resolves the actual agent object, not just an id echo.
			expect(s.selectedAgent()?.account.id).toBe("acc-cook");
			expect(s.selectedAgent()?.account.handle).toBe("cook");
			// cook owns ws-1022 (listed first = primary) then ws-965; the primary
			// drives the selection.
			expect(s.selectedWorkstreamId()).toBe("ws-1022");
			// The repo pick jumps to the agent's clone id.
			expect(s.activeRepoId()).toBe("acc-cook-repo");
		});
	});

	// Opening a fresh agent resets the tab group to the single permanent chat tab.
	// The chat pane's channel is derived from the selected agent via
	// `workspaceChannel()` (the agent's home DM), independent of the global
	// `selectedChannelId`. A terminal tab opened under the prior agent proves the
	// reset drops it.
	test("resets tabs to the chat tab and points workspaceChannel at the agent's home DM", () => {
		withStore((s) => {
			const home = STUB_AGENTS.find((a) => a.account.id === "acc-livingstone");
			expect(home?.account.homeChannelId).toBeDefined();

			// Prior workspace with an extra tab open — the switch must clear it.
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);

			s.openAgent("acc-livingstone");

			// Tabs reset to the lone chat tab, focused on it.
			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID]);
			expect(s.activeAgentTabId()).toBe(CHAT_TAB_ID);
			// The workspace pane derives its channel from the agent's home DM.
			expect(s.workspaceChannel()?.id).toBe(home?.account.homeChannelId);
		});
	});

	// The tab reset keys on the workspace-init guard (agentViewAgentId): re-opening
	// the SAME agent preserves the tabs the user opened. A guard that keyed on
	// selectedAgentId, or no guard at all, would wipe the terminal tab on re-open.
	test("re-opening the same agent preserves an opened terminal tab (init-guard)", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);

			// Re-open the same agent (clicking its tree row again).
			s.openAgent("acc-cook");

			// The init-guard returned before the reset — the terminal tab survives.
			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);
		});
	});

	// Switching to a *different* agent resets the per-agent view state: repo pick
	// and workstream selection fall back to the new agent's defaults. An agent that
	// owns nothing (warden) clears the workstream selection and exposes no clone.
	test("switching agents resets selection and clears surfaces for an agent that owns none", () => {
		withStore((s) => {
			// Build up per-agent state on cook so the switch has something to reset.
			s.openAgent("acc-cook");
			s.setActiveBranch("cook-965-client-transport");
			expect(s.activeRepo()?.currentBranch).toBe("cook-965-client-transport");

			// agent-warden is assigned zero workstreams.
			s.openAgent("acc-warden");

			expect(s.view()).toBe("agent");
			expect(s.selectedAgentId()).toBe("acc-warden");
			// No owned workstreams → no selection and no repo clone.
			expect(s.selectedWorkstreamId()).toBeNull();
			expect(s.agentRepos()).toEqual([]);
			expect(s.activeRepo()).toBeUndefined();
		});
	});

	// Re-selecting the current agent is a no-op beyond re-asserting the view: a
	// user who switched the branch must NOT have it snapped back on a re-open. The
	// `agentId === agentViewAgentId()` guard returns before any reset.
	test("re-opening the already-selected agent preserves the user's branch context", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.setActiveBranch("cook-965-client-transport");
			s.showBridge();
			expect(s.view()).toBe("bridge");

			// Re-open the same agent.
			s.openAgent("acc-cook");

			// The view is re-asserted, but the branch context is untouched.
			expect(s.view()).toBe("agent");
			expect(s.selectedAgentId()).toBe("acc-cook");
			expect(s.activeRepo()?.currentBranch).toBe("cook-965-client-transport");
		});
	});

	// A board roster move (selectWorkstream) sets selectedAgentId to a new agent
	// WITHOUT initializing that agent's view. Opening that agent must still reset
	// the selection — the reset keys on agentViewAgentId, not selectedAgentId, so a
	// move can't suppress it and leak the previous agent's selection (PR #467).
	test("opening a different agent after a roster move still resets the selection", () => {
		withStore((s) => {
			s.openAgent("acc-cook");

			// Board move to livingstone's workstream: selects the agent, no init.
			s.selectWorkstream("ws-1023");
			expect(s.selectedAgentId()).toBe("acc-livingstone");

			s.openAgent("acc-livingstone");

			// The reset keys on agentViewAgentId — the move didn't suppress it.
			expect(s.selectedWorkstreamId()).toBe("ws-1023");
		});
	});

	// A board card double-click selects the card's workstream first, then opens its
	// agent. For a NON-primary owned workstream, opening the agent must preserve
	// that pick — the anchoring keeps a currently-selected owned ws over the
	// primary fallback. Pre-fix, openAgent snapped to the primary (ws-1022).
	test("openAgent keeps a non-primary workstream picked just before opening (card jump)", () => {
		withStore((s) => {
			// Card double-click: select the card's ws first, then open its agent.
			s.selectWorkstream("ws-965"); // cook's non-primary workstream
			expect(s.selectedAgentId()).toBe("acc-cook");

			s.openAgent("acc-cook");

			// The non-primary pick survives — not snapped to ws-1022.
			expect(s.selectedWorkstreamId()).toBe("ws-965");
			expect(s.activeRepo()?.currentBranch).toBe("cook-965-client-transport");
		});
	});

	// A cross-agent roster move sets selectedAgentId to another agent WITHOUT
	// initializing the view — agentViewAgentId stays on the opened agent. Re-opening
	// the agent-view agent hits the early-return path, which must re-anchor to an
	// OWNED workstream — the other agent's ws must not leak (greptile's finding).
	test("re-opening the agent-view agent after a cross-agent roster move re-anchors the workstream", () => {
		withStore((s) => {
			s.openAgent("acc-cook"); // agentViewAgentId = cook
			expect(s.selectedWorkstreamId()).toBe("ws-1022");

			s.selectWorkstream("ws-1023"); // livingstone's ws; view still cook
			expect(s.selectedAgentId()).toBe("acc-livingstone");

			s.openAgent("acc-cook"); // early-return path (cook is still agentViewAgentId)

			// Re-anchored to a cook-owned ws — livingstone's ws-1023 did NOT leak.
			expect(s.selectedAgentId()).toBe("acc-cook");
			expect(s.selectedWorkstreamId()).toBe("ws-1022");
		});
	});

	// The workspace chat pane and the standalone channel surface are decoupled by
	// construction: the pane renders <ChannelView channel={workspaceChannel()}/>
	// (derived from the selected agent's home DM), while `selectedChannelId` is the
	// standalone surface's own state. `openAgent` no longer writes
	// `selectedChannelId` on either path — so a standalone channel opened in
	// between can never bleed into the workspace, and re-opening the already-open
	// agent (early-return path) restores the agent view without disturbing the
	// standalone selection (PR #783 / SEA-1195).
	test("re-opening the agent-view agent shows its home DM while leaving the standalone selection intact", () => {
		withStore((s) => {
			s.openAgent("acc-cook"); // agentViewAgentId = cook
			const home = STUB_AGENTS.find((a) => a.account.id === "acc-cook")?.account
				.homeChannelId;
			expect(home).toBeDefined();

			// Move the standalone channel surface to a channel distinct from cook's
			// home DM.
			s.openChannel("ch-svc-compass");
			expect(s.view()).toBe("channel");
			expect(s.selectedChannelId()).toBe("ch-svc-compass");

			// Re-open the same agent → early-return path (cook is still
			// agentViewAgentId).
			s.openAgent("acc-cook");

			// The workspace pane derives cook's home DM, and the view is restored.
			expect(s.view()).toBe("agent");
			expect(s.workspaceChannel()?.id).toBe(home);
			// The decouple's whole point: openAgent left `selectedChannelId`
			// untouched — the standalone selection persists alongside the
			// workspace's independent home-DM channel.
			expect(s.selectedChannelId()).toBe("ch-svc-compass");
		});
	});

	// The reverse independence: moving the standalone channel surface
	// (`openChannel` on a non-DM channel) cannot move the workspace pane. The
	// workspace channel stays derived from the selected agent's home DM while
	// `selectedChannel` follows the standalone pick — the two surfaces read
	// separate state.
	test("a standalone channel selection does not move the workspace channel", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			const home = STUB_AGENTS.find((a) => a.account.id === "acc-cook")?.account
				.homeChannelId;
			expect(home).toBeDefined();

			// Move the standalone surface to a channel distinct from cook's home DM.
			s.openChannel("ch-svc-compass");

			// The standalone surface moved…
			expect(s.selectedChannel()?.id).toBe("ch-svc-compass");
			// …but the workspace pane still derives cook's home DM.
			expect(s.workspaceChannel()?.id).toBe(home);
		});
	});
});

describe("agentRepos memo (T6)", () => {
	// The repo/branch dropdown derives one clone per agent from that agent's
	// assigned workstreams, in fixture order. Order is the contract: the first
	// branch is the repo's default currentBranch.
	test("derives the selected agent's clone with branches in fixture order", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			const repos = s.agentRepos();
			expect(repos).toHaveLength(1);
			const repo = repos[0];
			expect(repo?.id).toBe("acc-cook-repo");
			expect(repo?.name).toBe("sealedsecurity/sealed");
			// cook owns ws-1022 then ws-965 → their branches, primary first.
			expect(repo?.branches).toEqual([
				"cook-1022-tauri-shell",
				"cook-965-client-transport",
			]);
			// Selection is the primary workstream (ws-1022) → currentBranch is its branch, the first.
			expect(repo?.currentBranch).toBe("cook-1022-tauri-shell");
		});
	});

	// No agent selected → nothing to clone. Guards the null-agent branch.
	test("is empty when no agent is selected", () => {
		withStore((s) => {
			expect(s.selectedAgentId()).toBeNull();
			expect(s.agentRepos()).toEqual([]);
		});
	});
});

describe("selectWorkstream", () => {
	// Clicking a card on the board syncs the roster to that workstream's
	// assignee but stays on the board — it never jumps into the agent view.
	test("selects a workstream and syncs the roster without leaving the board", () => {
		withStore((s) => {
			s.showBridge();
			expect(s.view()).toBe("bridge");

			s.selectWorkstream("ws-1023");

			expect(s.selectedWorkstreamId()).toBe("ws-1023");
			// ws-1023 is assigned to livingstone; the roster follows.
			expect(s.selectedAgentId()).toBe("acc-livingstone");
			// The memo resolves the actual workstream object.
			expect(s.selectedWorkstream()?.id).toBe("ws-1023");
			expect(s.selectedWorkstream()?.assignee).toBe("acc-livingstone");
			// Crucially, the view does not change.
			expect(s.view()).toBe("bridge");
		});
	});

	// An unassigned workstream clears the roster rather than leaving a stale
	// agent highlighted.
	test("clears the roster when the workstream has no assignee", () => {
		withStore((s) => {
			// Seed a real agent selection to prove it gets cleared.
			s.selectWorkstream("ws-1023");
			expect(s.selectedAgentId()).toBe("acc-livingstone");

			// ws-1146 is in the backlog with assignee null.
			s.selectWorkstream("ws-1146");

			expect(s.selectedWorkstreamId()).toBe("ws-1146");
			expect(s.selectedAgentId()).toBeNull();
		});
	});
});

describe("pane toggles", () => {
	// Each pane toggle flips only its own accessor (the other stays open) and
	// double-toggling returns to the original — the panes are independent
	// booleans, not a shared radio group.
	const paneCases: {
		name: string;
		toggle: (s: AppStore) => void;
		read: (s: AppStore) => boolean;
		others: ((s: AppStore) => boolean)[];
	}[] = [
		{
			name: "left",
			toggle: (s) => s.toggleLeft(),
			read: (s) => s.leftOpen(),
			others: [(s) => s.rightOpen()],
		},
		{
			name: "right",
			toggle: (s) => s.toggleRight(),
			read: (s) => s.rightOpen(),
			others: [(s) => s.leftOpen()],
		},
	];

	for (const c of paneCases) {
		test(`toggle ${c.name} flips only its own pane and round-trips`, () => {
			withStore((s) => {
				const before = c.read(s);
				c.toggle(s);
				expect(c.read(s)).toBe(!before);
				// The other pane is untouched.
				for (const other of c.others) {
					expect(other(s)).toBe(true);
				}

				c.toggle(s);
				expect(c.read(s)).toBe(before);
				for (const other of c.others) {
					expect(other(s)).toBe(true);
				}
			});
		});
	}
});

describe("right sidebar tab (dock-in-sidebar T1)", () => {
	// The active-tab setter must round-trip any member of the widened union
	// (fleet + workstream). Exercising one fleet value ("warden") and one
	// workstream value ("vcs") proves the signal accepts both halves of the union
	// — a setter narrowed back to the old workstream-only type, or one stuck on
	// its boot value, would fail one leg.
	test("setActiveRightTab round-trips a fleet and a workstream value", () => {
		withStore((s) => {
			s.setActiveRightTab("warden");
			expect(s.activeRightTab()).toBe("warden");

			s.setActiveRightTab("vcs");
			expect(s.activeRightTab()).toBe("vcs");
		});
	});
});

describe("folder collapse", () => {
	// The collapse state is a Set: multiple folders collapse independently, and
	// collapsing one must not collapse a sibling. A boolean-per-nothing or a
	// single-slot implementation would fail the "both stay collapsed" assertion.
	test("collapses folders independently via the underlying Set", () => {
		withStore((s) => {
			expect(s.isFolderCollapsed("folder-a")).toBe(false);
			expect(s.isFolderCollapsed("folder-b")).toBe(false);

			s.toggleFolder("folder-a");
			expect(s.isFolderCollapsed("folder-a")).toBe(true);
			// Collapsing A leaves B expanded.
			expect(s.isFolderCollapsed("folder-b")).toBe(false);

			s.toggleFolder("folder-b");
			// Both now collapsed simultaneously — the Set holds both.
			expect(s.isFolderCollapsed("folder-a")).toBe(true);
			expect(s.isFolderCollapsed("folder-b")).toBe(true);

			s.toggleFolder("folder-a");
			// Re-toggling A expands only A; B remains collapsed.
			expect(s.isFolderCollapsed("folder-a")).toBe(false);
			expect(s.isFolderCollapsed("folder-b")).toBe(true);
		});
	});
});

describe("repo + branch selection (T6)", () => {
	// Picking a branch in the dropdown is a workstream navigator (design "Option
	// A"): it selects the workstream that OWNS that branch, so the dropdown label
	// and the Files/VCS/PR panes (which read the selected workstream) move as one.
	// There is no independent per-repo branch pick to drift from the panes.
	test("setActiveBranch selects the workstream that owns the branch, moving the panes with the dropdown", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			expect(s.activeRepo()?.currentBranch).toBe("cook-1022-tauri-shell");

			s.setActiveBranch("cook-965-client-transport");

			// The dropdown label follows the newly-selected workstream's branch.
			expect(s.activeRepo()?.currentBranch).toBe("cook-965-client-transport");
			// And the selection itself moved — this is the whole point: the pick
			// re-targets the selected workstream, not a private branch override.
			expect(s.selectedWorkstreamId()).toBe("ws-965");
			expect(s.selectedWorkstream()?.branch).toBe("cook-965-client-transport");
		});
	});

	// The anti-drift invariant (the bug three review bots flagged, now fixed):
	// the dropdown label (activeRepo().currentBranch) and the pane source
	// (selectedWorkstream().branch) resolve to the SAME workstream, so the
	// Files/VCS/PR panes can never show a different workstream than the dropdown.
	// TEETH: under the OLD per-repo override model, setActiveBranch only wrote a
	// private branchOverrides map and left selectedWorkstreamId at the primary —
	// so here it would still be "ws-1022", and the selectedWorkstreamId() /
	// selectedWorkstream().branch assertions below would fail (the dropdown shows
	// cook-965 while the panes still show ws-1022).
	test("the dropdown and the detail panes resolve to one workstream (no drift)", () => {
		withStore((s) => {
			s.openAgent("acc-cook"); // primary = ws-1022
			s.setActiveBranch("cook-965-client-transport");

			const dropdownBranch = s.activeRepo()?.currentBranch;
			const paneBranch = s.selectedWorkstream()?.branch;
			expect(dropdownBranch).toBe("cook-965-client-transport");
			expect(paneBranch).toBe("cook-965-client-transport");
			// The one-source-of-truth invariant: label === pane source.
			expect(dropdownBranch).toBe(paneBranch);
			expect(s.selectedWorkstreamId()).toBe("ws-965");
		});
	});

	// The guard: a branch no workstream of the selected agent owns is rejected,
	// leaving BOTH the dropdown label and the selection untouched — so a
	// stale/foreign branch can't move the panes.
	test("setActiveBranch is a no-op for a branch no workstream of the agent owns", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.setActiveBranch("cook-965-client-transport");

			s.setActiveBranch("not-a-branch");

			// Unchanged: the guard found no owning workstream, so both the label
			// and the selection stay on ws-965 / cook-965.
			expect(s.activeRepo()?.currentBranch).toBe("cook-965-client-transport");
			expect(s.selectedWorkstreamId()).toBe("ws-965");
		});
	});

	// A branch owned by a DIFFERENT agent's workstream is also rejected: the
	// guard requires `w.assignee === selectedAgentId()`, so picking livingstone's
	// branch while cook is selected can't hijack the selection.
	// TEETH: drop the `assignee === id` half of the guard and this would select
	// ws-1023 (livingstone's), moving cook's panes onto another agent's work —
	// selectedWorkstreamId() would become "ws-1023" and this assertion fails.
	test("setActiveBranch is a no-op for a branch owned by a different agent", () => {
		withStore((s) => {
			s.openAgent("acc-cook"); // selects ws-1022
			s.setActiveBranch("livingstone-1023-acp-session");

			// livingstone's branch belongs to agent-livingstone, not cook → ignored.
			expect(s.selectedWorkstreamId()).toBe("ws-1022");
			expect(s.activeRepo()?.currentBranch).toBe("cook-1022-tauri-shell");
		});
	});

	// setActiveRepo only accepts an id that is actually among the agent's clones.
	test("setActiveRepo is a no-op for an id not among the agent's clones", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			expect(s.activeRepoId()).toBe("acc-cook-repo");

			s.setActiveRepo("bogus-repo");

			// The pick is untouched, so activeRepo still resolves to the real clone.
			expect(s.activeRepoId()).toBe("acc-cook-repo");
			expect(s.activeRepo()?.id).toBe("acc-cook-repo");
		});
	});

	// Opening an agent resets the selection to that agent's primary (first-owned)
	// workstream — the branch follows the selected workstream, and there is no
	// remembered per-agent branch to restore. So navigating to cook-965, away to
	// livingstone, and back to cook lands on cook's primary (ws-1022), NOT the
	// previously-navigated cook-965.
	test("openAgent resets the selection to the agent's primary workstream", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			// Primary workstream first: ws-1022 / cook-1022-tauri-shell.
			expect(s.selectedWorkstreamId()).toBe("ws-1022");
			expect(s.activeRepo()?.currentBranch).toBe("cook-1022-tauri-shell");

			// Navigate to cook's other workstream via the dropdown.
			s.setActiveBranch("cook-965-client-transport");
			expect(s.selectedWorkstreamId()).toBe("ws-965");

			// Switch to livingstone: its only workstream (ws-1023) is selected.
			s.openAgent("acc-livingstone");
			expect(s.selectedWorkstreamId()).toBe("ws-1023");
			expect(s.activeRepo()?.currentBranch).toBe(
				"livingstone-1023-acp-session",
			);

			// Back to cook: openAgent re-selects the primary workstream — NOT the
			// previously-navigated cook-965.
			s.openAgent("acc-cook");
			expect(s.selectedWorkstreamId()).toBe("ws-1022");
			expect(s.activeRepo()?.currentBranch).toBe("cook-1022-tauri-shell");
		});
	});
});

describe("store isolation", () => {
	// createAppStore must produce fully independent instances (no module-level
	// shared signals). Mutating one store leaves another untouched.
	test("two stores do not share state", () => {
		createRoot((dispose) => {
			const a = createAppStore({ initialComms: STUB_COMMS_STATE });
			const b = createAppStore({ initialComms: STUB_COMMS_STATE });
			try {
				a.openAgent("acc-cook");
				a.toggleLeft();
				a.toggleFolder("folder-x");

				// Store A moved into an agent view.
				expect(a.view()).toBe("agent");

				// Store B is still at its independent starting state: A's agent
				// open, pane change, and toggles did not leak across instances.
				expect(b.view()).toBe("bridge");
				expect(b.selectedAgentId()).toBeNull();
				expect(b.leftOpen()).toBe(true);
				expect(b.agentRepos()).toEqual([]);
				expect(b.isFolderCollapsed("folder-x")).toBe(false);
			} finally {
				dispose();
			}
		});
	});
});

describe("agent tab group (T7)", () => {
	// The chat tab is always first and always present once an agent is open;
	// terminals/files are opened explicitly (hidden by default, D6). Its sole
	// pane is the chat pane — `kind` moved from the tab to the pane.
	test("agentTabs leads with the permanent chat tab holding the chat pane", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			const tabs = s.agentTabs();
			expect(tabs).toHaveLength(1);
			expect(tabs[0]?.id).toBe(CHAT_TAB_ID);
			expect(tabs[0]?.title).toBe("Chat");
			expect(tabs[0]?.layout).toEqual({
				kind: "leaf",
				pane: { id: CHAT_TAB_ID, kind: "chat", title: "Chat" },
			});
		});
	});

	// Opening a tab appends it after the chat, focuses it, and shows its pane
	// full-screen (a single leaf) — a fresh tab is never pre-split.
	test("openTab appends a full-screen tab, focuses it, and shows its pane as a single leaf", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			const pane: Pane = {
				id: "term-1",
				kind: "terminal",
				title: "dev",
				terminalId: "t1",
			};

			s.openTab(pane);

			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);
			expect(s.activeAgentTabId()).toBe("term-1");
			const tab = s.activeAgentTab();
			expect(tab?.title).toBe("dev");
			expect(tab?.layout).toEqual({ kind: "leaf", pane });
			expect(tab?.focusedPaneId).toBe("term-1");
		});
	});

	// Re-opening an id already present must NOT duplicate — it only refocuses.
	test("re-opening an existing tab id refocuses without duplicating", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			s.openTab({ id: "term-2", kind: "terminal", title: "tests" });
			expect(s.activeAgentTabId()).toBe("term-2");

			// Re-open term-1 (already present): no new tab, focus moves back to it.
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });

			expect(s.agentTabs().map((t) => t.id)).toEqual([
				CHAT_TAB_ID,
				"term-1",
				"term-2",
			]);
			expect(s.activeAgentTabId()).toBe("term-1");
		});
	});

	// setActiveAgentTab only focuses a tab that exists.
	test("setActiveAgentTab is a no-op for an absent tab id", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			expect(s.activeAgentTabId()).toBe("term-1");

			s.setActiveAgentTab("does-not-exist");

			// Focus unchanged: the guard rejected the unknown id.
			expect(s.activeAgentTabId()).toBe("term-1");
		});
	});
});

describe("splitActivePane (T7)", () => {
	// Splitting the active tab's focused pane grows THAT tab's tree by one pane
	// and focuses the new pane so a follow-up split chains off it. `row` places
	// the new pane to the right (split right).
	test("splits the active tab's focused pane and focuses the new pane (row = split right)", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			const pane: Pane = { id: "term-1", kind: "terminal", title: "dev" };

			s.splitActivePane(pane, "row");

			const tab = s.activeAgentTab();
			expect(tab?.layout).toEqual({
				kind: "split",
				direction: "row",
				left: {
					kind: "leaf",
					pane: { id: CHAT_TAB_ID, kind: "chat", title: "Chat" },
				},
				right: { kind: "leaf", pane },
			});
			expect(paneIds(tab)).toEqual([CHAT_TAB_ID, "term-1"]);
			expect(tab?.focusedPaneId).toBe("term-1");
		});
	});

	// `column` stacks the new pane below (split down) — the direction is carried
	// onto the split node, not silently coerced to row.
	test("column direction stacks the new pane below (split down)", () => {
		withStore((s) => {
			s.openAgent("acc-cook");

			s.splitActivePane(
				{ id: "term-1", kind: "terminal", title: "dev" },
				"column",
			);

			expect(s.activeAgentTab()?.layout).toEqual({
				kind: "split",
				direction: "column",
				left: {
					kind: "leaf",
					pane: { id: CHAT_TAB_ID, kind: "chat", title: "Chat" },
				},
				right: {
					kind: "leaf",
					pane: { id: "term-1", kind: "terminal", title: "dev" },
				},
			});
		});
	});

	// A split touches ONLY the active tab; the chat and every other open tab
	// keep their single-pane full-screen layout. A shared-tree regression would
	// leak the new pane into the other tabs here.
	test("only mutates the active tab, leaving other tabs untouched", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			s.openTab({ id: "term-2", kind: "terminal", title: "tests" });
			// Make term-1 the active tab, then split it.
			s.setActiveAgentTab("term-1");

			s.splitActivePane(
				{ id: "term-3", kind: "terminal", title: "logs" },
				"row",
			);

			const tabs = s.agentTabs();
			const term1 = tabs.find((t) => t.id === "term-1");
			expect(paneIds(term1)).toEqual(["term-1", "term-3"]);
			expect(term1?.focusedPaneId).toBe("term-3");
			// Every other tab stayed a single-pane full-screen leaf.
			expect(paneIds(tabs.find((t) => t.id === CHAT_TAB_ID))).toEqual([
				CHAT_TAB_ID,
			]);
			expect(paneIds(tabs.find((t) => t.id === "term-2"))).toEqual(["term-2"]);
		});
	});

	// setFocusedPane moves the anchor the split buttons act on. An unknown id is
	// a no-op (the guard rejects a pane not in the active tab), and the next
	// split then anchors on the re-focused pane — proving focus, not insertion
	// order, drives where a split lands.
	test("setFocusedPane redirects where the next split anchors; an absent id is a no-op", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			// Split once: panes [chat, term-1], term-1 focused.
			s.splitActivePane(
				{ id: "term-1", kind: "terminal", title: "dev" },
				"row",
			);
			expect(s.activeAgentTab()?.focusedPaneId).toBe("term-1");

			// Focus back to the chat pane; an absent id changes nothing.
			s.setFocusedPane(CHAT_TAB_ID);
			expect(s.activeAgentTab()?.focusedPaneId).toBe(CHAT_TAB_ID);
			s.setFocusedPane("not-a-pane");
			expect(s.activeAgentTab()?.focusedPaneId).toBe(CHAT_TAB_ID);

			// The next split anchors on the chat pane, inserting term-2 beside
			// it (not beside term-1), and focuses term-2.
			s.splitActivePane(
				{ id: "term-2", kind: "terminal", title: "tests" },
				"column",
			);
			expect(paneIds(s.activeAgentTab())).toEqual([
				CHAT_TAB_ID,
				"term-2",
				"term-1",
			]);
			expect(s.activeAgentTab()?.focusedPaneId).toBe("term-2");
		});
	});
});

describe("closing tabs + panes (T7)", () => {
	// Closing the active tab drops it and falls focus back to the permanent
	// chat tab.
	test("closeTab drops the tab and falls focus back to the chat", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			s.openTab({ id: "term-2", kind: "terminal", title: "tests" });
			expect(s.activeAgentTabId()).toBe("term-2");

			s.closeTab("term-2");

			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);
			expect(s.activeAgentTabId()).toBe(CHAT_TAB_ID);
		});
	});

	// Closing a tab that ISN'T active leaves focus where it is — only closing the
	// active tab moves focus. A "always reset to chat" bug would fail here.
	test("closing a non-active tab leaves the active tab focused", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			s.openTab({ id: "term-2", kind: "terminal", title: "tests" });
			s.setActiveAgentTab("term-2");

			s.closeTab("term-1");

			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-2"]);
			expect(s.activeAgentTabId()).toBe("term-2");
		});
	});

	// The chat tab is permanent — closing it does nothing to the tabs or focus.
	test("closeTab on the chat tab is a no-op", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			expect(s.activeAgentTabId()).toBe("term-1");

			s.closeTab(CHAT_TAB_ID);

			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);
			expect(s.activeAgentTabId()).toBe("term-1");
		});
	});

	// Closing one pane of a split collapses the split around the surviving
	// sibling; the tab itself stays open (it still has a pane).
	test("closePane collapses the split around the surviving sibling", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			s.splitActivePane(
				{ id: "term-2", kind: "terminal", title: "tests" },
				"row",
			);
			expect(paneIds(s.activeAgentTab())).toEqual(["term-1", "term-2"]);

			s.closePane("term-1");

			// The tab survives; its tree collapsed to the lone surviving pane.
			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID, "term-1"]);
			expect(s.activeAgentTab()?.layout).toEqual({
				kind: "leaf",
				pane: { id: "term-2", kind: "terminal", title: "tests" },
			});
		});
	});

	// Closing the LAST pane of a non-chat tab closes the whole tab and drops
	// focus back to the chat.
	test("closing the last pane of a non-chat tab closes the tab", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			expect(s.activeAgentTabId()).toBe("term-1");

			s.closePane("term-1");

			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID]);
			expect(s.activeAgentTabId()).toBe(CHAT_TAB_ID);
		});
	});

	// Closing the FOCUSED pane of a split falls focus back to a surviving pane —
	// focus can never dangle on a pane that's gone.
	test("closing the focused pane falls focus back to a surviving pane", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.openTab({ id: "term-1", kind: "terminal", title: "dev" });
			s.splitActivePane(
				{ id: "term-2", kind: "terminal", title: "tests" },
				"row",
			);
			expect(s.activeAgentTab()?.focusedPaneId).toBe("term-2");

			s.closePane("term-2");

			expect(paneIds(s.activeAgentTab())).toEqual(["term-1"]);
			expect(s.activeAgentTab()?.focusedPaneId).toBe("term-1");
		});
	});

	// The chat pane is permanent: closing it while it's the chat tab's sole
	// pane is a no-op — the tab and its pane both survive (the UI hides the close
	// button for a chat pane, and the store's last-pane close is a no-op for
	// the permanent chat tab).
	test("closePane on the sole chat pane is a no-op (chat pane permanent)", () => {
		withStore((s) => {
			s.openAgent("acc-cook");

			s.closePane(CHAT_TAB_ID);

			expect(s.agentTabs().map((t) => t.id)).toEqual([CHAT_TAB_ID]);
			expect(s.activeAgentTabId()).toBe(CHAT_TAB_ID);
			expect(s.activeAgentTab()?.layout).toEqual({
				kind: "leaf",
				pane: { id: CHAT_TAB_ID, kind: "chat", title: "Chat" },
			});
		});
	});

	// The chat pane stays permanent even after the chat tab is SPLIT: with
	// a terminal beside it, closing the chat pane is still a no-op — both panes
	// survive and the split stays intact. Pre-fix, prunePane collapsed the split
	// down to just the terminal, evicting the permanent chat pane.
	test("closePane on the chat pane in a split chat tab is a no-op", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			// Split the chat tab: chat pane + term-1 beside it.
			s.splitActivePane(
				{ id: "term-1", kind: "terminal", title: "dev" },
				"row",
			);
			// Sanity: the chat tab now holds both panes.
			expect(paneIds(s.activeAgentTab())).toEqual([CHAT_TAB_ID, "term-1"]);

			s.closePane(CHAT_TAB_ID); // permanent — no-op

			// Both panes survive; the split is intact.
			expect(paneIds(s.activeAgentTab())).toEqual([CHAT_TAB_ID, "term-1"]);
		});
	});

	// The guard is scoped to the chat PANE, not the whole split chat tab:
	// closing the NON-chat pane (term-1) of a split chat tab still prunes
	// it, collapsing back to the lone chat pane. The permanent guard must not
	// freeze the whole tab.
	test("closePane on the non-chat pane of a split chat tab prunes it", () => {
		withStore((s) => {
			s.openAgent("acc-cook");
			s.splitActivePane(
				{ id: "term-1", kind: "terminal", title: "dev" },
				"row",
			);
			expect(paneIds(s.activeAgentTab())).toEqual([CHAT_TAB_ID, "term-1"]);

			s.closePane("term-1");

			// The split collapses back to the lone chat pane.
			expect(s.activeAgentTab()?.layout).toEqual({
				kind: "leaf",
				pane: { id: CHAT_TAB_ID, kind: "chat", title: "Chat" },
			});
		});
	});
});

describe("splitPaneIds / splitPanes (T7)", () => {
	const chat: Pane = {
		id: CHAT_TAB_ID,
		kind: "chat",
		title: "Chat",
	};
	const t1: Pane = { id: "term-1", kind: "terminal", title: "dev" };
	const t2: Pane = { id: "term-2", kind: "terminal", title: "tests" };
	const tree: SplitNode = {
		kind: "split",
		direction: "row",
		left: { kind: "leaf", pane: chat },
		right: {
			kind: "split",
			direction: "column",
			left: { kind: "leaf", pane: t1 },
			right: { kind: "leaf", pane: t2 },
		},
	};

	// The helpers flatten a tab's split tree to its leaves in left-to-right
	// order — the order the panes render. A swapped recursion would reorder them.
	test("flatten a nested split tree left-to-right", () => {
		expect(splitPaneIds(tree)).toEqual([CHAT_TAB_ID, "term-1", "term-2"]);
		// splitPanes yields the pane OBJECTS in the same order, not just ids.
		expect(splitPanes(tree)).toEqual([chat, t1, t2]);
	});

	// A bare leaf (the default layout) yields its single pane.
	test("return the single leaf for a bare leaf node", () => {
		const leaf: SplitNode = { kind: "leaf", pane: chat };
		expect(splitPaneIds(leaf)).toEqual([CHAT_TAB_ID]);
		expect(splitPanes(leaf)).toEqual([chat]);
	});
});

describe("splitPaneOnce (PR #467)", () => {
	const newPane: Pane = { id: "b", kind: "terminal", title: "B" };

	// `row` wraps the matched leaf in a split with the new pane to its RIGHT and
	// reports a match.
	test("row splits the matching leaf with the new pane to the right", () => {
		const tree: SplitNode = {
			kind: "leaf",
			pane: { id: "a", kind: "terminal", title: "A" },
		};

		const [result, matched] = splitPaneOnce(tree, "a", newPane, "row");

		expect(matched).toBe(true);
		expect(result).toEqual({
			kind: "split",
			direction: "row",
			left: tree,
			right: { kind: "leaf", pane: newPane },
		});
		expect(splitPaneIds(result)).toEqual(["a", "b"]);
	});

	// `column` produces a column-direction split (split down), not a row one.
	test("column splits the matching leaf stacking the new pane below", () => {
		const tree: SplitNode = {
			kind: "leaf",
			pane: { id: "a", kind: "terminal", title: "A" },
		};

		const [result, matched] = splitPaneOnce(tree, "a", newPane, "column");

		expect(matched).toBe(true);
		expect(result).toEqual({
			kind: "split",
			direction: "column",
			left: tree,
			right: { kind: "leaf", pane: newPane },
		});
	});

	// Splitting wraps ONLY the first matching leaf, even when the same pane id
	// appears twice. The old recursion rewrote every matching leaf, so one split
	// of a doubly-shown pane inserted the new leaf twice — a pane explosion.
	test("splits exactly the first matching leaf when a pane id repeats", () => {
		const tree: SplitNode = {
			kind: "split",
			direction: "row",
			left: { kind: "leaf", pane: { id: "a", kind: "terminal", title: "A1" } },
			right: { kind: "leaf", pane: { id: "a", kind: "terminal", title: "A2" } },
		};

		const [result, matched] = splitPaneOnce(tree, "a", newPane, "row");

		expect(matched).toBe(true);
		// One "b" beside the FIRST "a" only. Old code -> ["a","b","a","b"].
		expect(splitPaneIds(result)).toEqual(["a", "b", "a"]);
	});

	// No leaf matches -> the tree is returned structurally unchanged and matched
	// is false, so callers can fall back instead of silently mutating.
	test("returns [tree, false] when no leaf matches", () => {
		const tree: SplitNode = {
			kind: "leaf",
			pane: { id: "a", kind: "terminal", title: "A" },
		};

		const [result, matched] = splitPaneOnce(tree, "zzz", newPane, "row");

		expect(matched).toBe(false);
		expect(result).toEqual(tree);
		expect(splitPaneIds(result)).toEqual(["a"]);
	});
});

describe("log panel (D2)", () => {
	// The log panel opens by default; toggleLog flips it closed then open again.
	// A setter that ignored its prior state, or a one-way close, fails a leg.
	test("toggleLog flips logOpen and round-trips", () => {
		withStore((s) => {
			expect(s.logOpen()).toBe(true);

			s.toggleLog();
			expect(s.logOpen()).toBe(false);

			s.toggleLog();
			expect(s.logOpen()).toBe(true);
		});
	});

	// Entering a workspace re-opens the log panel: after the user minimized it,
	// opening an agent resets logOpen to true (per-workspace-entry reset). A reset
	// that never fired would leave it closed and redden here.
	test("openAgent resets logOpen to true", () => {
		withStore((s) => {
			s.toggleLog();
			expect(s.logOpen()).toBe(false);

			s.openAgent("acc-cook");

			expect(s.logOpen()).toBe(true);
		});
	});
});

describe("sidebar section collapse", () => {
	// The two sidebar sections collapse independently and each round-trips: both
	// default to not-collapsed, collapsing one leaves the other expanded, and both
	// can be collapsed at once. A single-slot or shared-flag implementation would
	// fail the "channels collapsed, agents still expanded" leg.
	test("collapses channels and agents independently", () => {
		withStore((s) => {
			expect(s.isSectionCollapsed("channels")).toBe(false);
			expect(s.isSectionCollapsed("agents")).toBe(false);

			s.toggleSection("channels");
			expect(s.isSectionCollapsed("channels")).toBe(true);
			// Collapsing channels leaves agents expanded.
			expect(s.isSectionCollapsed("agents")).toBe(false);

			s.toggleSection("agents");
			// Both collapsed simultaneously.
			expect(s.isSectionCollapsed("channels")).toBe(true);
			expect(s.isSectionCollapsed("agents")).toBe(true);

			s.toggleSection("channels");
			// Re-toggling channels expands only channels; agents stay collapsed.
			expect(s.isSectionCollapsed("channels")).toBe(false);
			expect(s.isSectionCollapsed("agents")).toBe(true);
		});
	});
});

// ── Comms surface, folded onto the unified store (was comms-store.test.ts) ────
// The comms accessors + mutations now live on AppStore (createAppStore). These
// assert logical behavior + invariants over the fixture ("boots onto A subscribed
// channel", not "boots onto ch-announcements"), so a stub edit can't falsely
// redden them.

describe("comms boot selection", () => {
	// Boots onto A subscribed channel (not a hardcoded id, not the first channel
	// unconditionally). Bites a regression that selected the first channel
	// regardless of membership, or booted into the empty state.
	test("selects a subscribed channel on construction", () => {
		withStore((s) => {
			const id = s.selectedChannelId();
			expect(id).not.toBeNull();
			const selected = s.channels().find((c) => c.id === id);
			expect(selected?.membership).toBe("subscribed");
			// selectedChannel resolves the id to the same channel object.
			expect(s.selectedChannel()?.id).toBe(selected?.id);
		});
	});
});

// Matt's ruling: joining/subscribing has NO wire RPC yet. The store used to
// mutate `membership` locally, which was harmless against a fixture but a lie
// against the live stream — `adoptComms` replaces the state wholesale and
// `deriveMembership` re-derives membership from the server's member lists, so a
// local toggle silently reverted mid-use (the composer enabling, then disabling
// under a half-typed draft). Until the RPCs land the store MUST NOT fake
// membership state, and the rail's controls render disabled
// (LeftSidebar.test.tsx). These tests pin the store half: both functions keep
// their shape (the rail's onClick seam still type-checks) and are inert.
describe("joinChannel", () => {
	// The transition the old local-only path produced (none → joined) must NOT
	// happen: it is a state the server never agreed to. Mutation-check:
	// restoring `setMembership(id, m => m === "none" ? "joined" : m)` reddens.
	test("does not fake a join — membership is unchanged (no wire RPC yet)", () => {
		withStore((s) => {
			const target = s.channels().find((c) => c.membership === "none");
			expect(target).toBeDefined();
			if (!target) return;

			s.joinChannel(target.id);

			expect(s.channels().find((c) => c.id === target.id)?.membership).toBe(
				"none",
			);
		});
	});

	// Inert everywhere, not just on the `none` row — and it leaves the channels
	// array REFERENCE untouched, so it cannot even churn the rail's memo.
	test("leaves every channel (and the channels reference) untouched", () => {
		withStore((s) => {
			const before = s.channels();
			for (const channel of before) s.joinChannel(channel.id);
			s.joinChannel("ch-nope");

			expect(s.channels()).toBe(before);
		});
	});
});

describe("toggleSubscribe", () => {
	// The joined ⇆ subscribed round-trip the old local path produced is gone: a
	// subscription the server does not hold must never render as held.
	// Mutation-check: restoring the local toggle reddens the first assertion.
	test("does not fake a subscribe — membership is unchanged (no wire RPC yet)", () => {
		withStore((s) => {
			const target = s
				.channels()
				.find((c) => c.membership === "joined" && !c.alwaysSubscribed);
			expect(target).toBeDefined();
			if (!target) return;

			s.toggleSubscribe(target.id);

			expect(s.channels().find((c) => c.id === target.id)?.membership).toBe(
				"joined",
			);
		});
	});

	// Inert across every row — including the two the old implementation already
	// refused (a `none` channel, and the alwaysSubscribed one whose subscription
	// is implicit and non-togglable, design.md:416). The fixture pins
	// ch-announcements as alwaysSubscribed + subscribed.
	test("leaves every channel (and the channels reference) untouched", () => {
		withStore((s) => {
			// Non-triviality: the fixture really carries all three shapes, so an
			// "unchanged" pass is not an empty-set pass.
			expect(s.channels().some((c) => c.membership === "none")).toBe(true);
			expect(s.channels().some((c) => c.alwaysSubscribed)).toBe(true);
			expect(
				s
					.channels()
					.some((c) => c.membership === "joined" && !c.alwaysSubscribed),
			).toBe(true);

			const before = s.channels();
			for (const channel of before) s.toggleSubscribe(channel.id);
			s.toggleSubscribe("ch-nope");

			expect(s.channels()).toBe(before);
		});
	});
});

describe("answerAsk", () => {
	// First-responder-wins: a single-select ask settles on its FIRST answer, and
	// a second answer is a NO-OP — the winner's chosenOptionIds is unchanged
	// (design.md §202-217). This is the seam contract the @compass/client
	// swap depends on (a second single-select answer matches CodeAlreadyExists
	// without a rewrite). Mutation-check: the pre-first-wins "replace" behavior
	// (second answer sets [second]) reddens the second toEqual; a guard that
	// wrongly no-ops the FIRST answer too reddens the first toEqual.
	test("single-select is first-wins: a second answer is a no-op", () => {
		const { messageId, ask, question } = firstCommsAsk();
		expect(question.allowMultiple).toBe(false);
		expect(question.options.length).toBeGreaterThanOrEqual(2);
		const [first, second] = question.options;

		withStore((s) => {
			s.answerAsk(messageId, ask.askId, question.questionId, first.id);
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([
				first.id,
			]);

			s.answerAsk(messageId, ask.askId, question.questionId, second.id);
			// first-wins: the second answer does nothing, the first choice stands.
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([
				first.id,
			]);
		});
	});

	// Answering does not mutate the shared fixture object in place — the store
	// maps to fresh objects, so the module-level STUB_MESSAGES ask stays pending.
	test("does not mutate the shared fixture in place (immutability)", () => {
		const { messageId, ask, question } = firstCommsAsk();
		const before = [...question.chosenOptionIds];

		withStore((s) => {
			s.answerAsk(
				messageId,
				ask.askId,
				question.questionId,
				question.options[0].id,
			);
			// the store's copy changed …
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([
				question.options[0].id,
			]);
		});

		// … but the original fixture object did not.
		expect(question.chosenOptionIds).toEqual(before);
	});

	// No-op for an unknown message / ask / option id — none of the three miss
	// paths may alter the recorded choice. The option miss-path is the
	// referential-integrity contract in produced form: answerAsk only ever
	// records an id that names a real option, so a chosen id can never dangle.
	test("is a no-op for an unknown message, ask, or option id", () => {
		const { messageId, ask, question } = firstCommsAsk();
		const good = question.options[0].id;
		const qid = question.questionId;

		withStore((s) => {
			s.answerAsk("msg-nope", ask.askId, qid, good);
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([]);

			s.answerAsk(messageId, "ask-nope", qid, good);
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([]);

			s.answerAsk(messageId, ask.askId, qid, "opt-nope");
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([]);
		});
	});

	// No-op for an unknown questionId — naming a question the ask doesn't carry
	// leaves every question's chosenOptionIds untouched. The per-question seam
	// (answerAsk keys on questionId) must never record a choice
	// against a phantom question. Mutation-check: dropping the questionId guard
	// (so the mapper answers the first/only question regardless) reddens this.
	test("is a no-op for an unknown questionId", () => {
		const { messageId, ask, question } = firstCommsAsk();
		const good = question.options[0].id;

		withStore((s) => {
			s.answerAsk(messageId, ask.askId, "question-nope", good);
			expect(questionIn(s, messageId, ask.askId)?.chosenOptionIds).toEqual([]);
		});
	});
});

describe("caller identity", () => {
	// The store reads the same CALLER_ID the fixture pins its owner to, so the
	// caller resolves to a real account rather than the synthetic {id,id,id}
	// fallback. Guards against the caller memo falling through to the placeholder.
	test("resolves the caller to the fixture account for CALLER_ID", () => {
		withStore((s) => {
			expect(s.caller().id).toBe(CALLER_ID);
			const known = s.accounts().find((a) => a.id === CALLER_ID);
			expect(known).toBeDefined();
			expect(known?.handle).toBe(s.caller().handle);
		});
	});
});

describe("comms fixture preconditions", () => {
	// Guard the fixture preconditions the comms tests lean on, so if a stub edit
	// removes them the failure names the missing precondition rather than
	// mystifying a behavior test above. (Not defaults — these are the
	// equivalence-class inputs the behavior tests need to exist.)
	test("the stub exercises every membership tier and an alwaysSubscribed channel", () => {
		const memberships = new Set(STUB_CHANNELS.map((c) => c.membership));
		expect(memberships.has("none")).toBe(true);
		expect(memberships.has("joined")).toBe(true);
		expect(memberships.has("subscribed")).toBe(true);
		expect(STUB_CHANNELS.some((c) => c.alwaysSubscribed)).toBe(true);
		// a joined, non-always-subscribed channel exists for the round-trip test.
		expect(
			STUB_CHANNELS.some(
				(c) => c.membership === "joined" && !c.alwaysSubscribed,
			),
		).toBe(true);
	});
});

describe("promoteToTodo (PR3)", () => {
	// The one contract of promote: a workstream that is *currently* `backlog`
	// moves to `todo`. ws-1146 is the fixture's only backlog row. A regression
	// that promoted to the wrong state, or moved the wrong row, reddens here.
	test("moves a backlog workstream to todo", () => {
		withStore((s) => {
			const before = s.workstreams().find((w) => w.id === "ws-1146");
			expect(before?.state).toBe("backlog");

			s.promoteToTodo("ws-1146");

			expect(s.workstreams().find((w) => w.id === "ws-1146")?.state).toBe(
				"todo",
			);
		});
	});

	// The no-op branch (`w.state === "backlog"` guard). Promoting a workstream
	// that is NOT backlog must leave it untouched — this is what makes a
	// double-click on an already-promoted card safe. ws-965 is `in_progress`.
	test("is a no-op on a non-backlog workstream", () => {
		withStore((s) => {
			expect(s.workstreams().find((w) => w.id === "ws-965")?.state).toBe(
				"in_progress",
			);

			s.promoteToTodo("ws-965");

			// State unchanged: the guard rejected the transition.
			expect(s.workstreams().find((w) => w.id === "ws-965")?.state).toBe(
				"in_progress",
			);
		});
	});

	// Idempotency against a double-click: the second promote finds the row
	// already `todo` (no longer `backlog`) and the guard makes it a no-op, so it
	// stays `todo` rather than flipping onward or throwing.
	test("is idempotent — a second promote keeps it at todo", () => {
		withStore((s) => {
			s.promoteToTodo("ws-1146");
			expect(s.workstreams().find((w) => w.id === "ws-1146")?.state).toBe(
				"todo",
			);

			s.promoteToTodo("ws-1146");
			expect(s.workstreams().find((w) => w.id === "ws-1146")?.state).toBe(
				"todo",
			);
		});
	});

	// An unknown id matches no row, so promote changes nothing — it must not
	// throw or corrupt the list length.
	test("leaves the list intact for an unknown id", () => {
		withStore((s) => {
			const len = s.workstreams().length;
			const snapshot = s.workstreams().map((w) => `${w.id}:${w.state}`);

			s.promoteToTodo("ws-does-not-exist");

			expect(s.workstreams().length).toBe(len);
			expect(s.workstreams().map((w) => `${w.id}:${w.state}`)).toEqual(
				snapshot,
			);
		});
	});
});

describe("archiveWorkstream (PR3)", () => {
	// Archive stamps an ISO `archivedAt` on a `done` row — and, crucially, the
	// row STAYS in workstreams() with state still `done` (a marker, not a
	// delete; the Done view still lists it). ws-1145 is a `done` fixture row.
	test("stamps archivedAt on a done workstream without removing it or changing state", () => {
		withStore((s) => {
			const lenBefore = s.workstreams().length;
			const before = s.workstreams().find((w) => w.id === "ws-1145");
			expect(before?.state).toBe("done");
			expect(before?.archivedAt).toBeUndefined();

			s.archiveWorkstream("ws-1145");

			const after = s.workstreams().find((w) => w.id === "ws-1145");
			// Still present, still done — archive is a marker, not a delete.
			expect(s.workstreams().length).toBe(lenBefore);
			expect(after?.state).toBe("done");
			// Stamped with a real ISO-8601 instant that round-trips through Date.
			expect(typeof after?.archivedAt).toBe("string");
			expect(after?.archivedAt).toBe(
				new Date(after?.archivedAt as string).toISOString(),
			);
		});
	});

	// The no-op branch (`w.state === "done"` guard). A non-done workstream is
	// never archivable; ws-965 (`in_progress`) must gain no stamp.
	test("is a no-op on a non-done workstream", () => {
		withStore((s) => {
			s.archiveWorkstream("ws-965");

			const ws = s.workstreams().find((w) => w.id === "ws-965");
			expect(ws?.state).toBe("in_progress");
			expect(ws?.archivedAt).toBeUndefined();
		});
	});

	// Idempotency: re-archiving an already-archived row keeps the ORIGINAL
	// stamp (the `!w.archivedAt` guard rejects the second stamp), so a
	// double-click can't rewrite the archive instant.
	test("preserves the original stamp when archived twice", () => {
		withStore((s) => {
			s.archiveWorkstream("ws-1145");
			const first = s.workstreams().find((w) => w.id === "ws-1145")?.archivedAt;
			expect(first).toBeDefined();

			s.archiveWorkstream("ws-1145");
			const second = s
				.workstreams()
				.find((w) => w.id === "ws-1145")?.archivedAt;
			expect(second).toBe(first);
		});
	});
});

describe("reactive workstreams (PR3)", () => {
	// The core PR3 reactivity contract: workstreams() is a signal, so a derived
	// read (createMemo) recomputes after promote/archive within the same root.
	// If the list were a frozen array captured once, the memo would keep the
	// pre-mutation value and these re-reads would fail.
	test("a derived memo observes a promote", () => {
		withStore((s) => {
			const state1146 = createMemo(
				() => s.workstreams().find((w) => w.id === "ws-1146")?.state,
			);
			expect(state1146()).toBe("backlog");

			s.promoteToTodo("ws-1146");

			// The memo recomputed off the new signal value.
			expect(state1146()).toBe("todo");
		});
	});

	test("a derived memo observes an archive stamp", () => {
		withStore((s) => {
			const archivedCount = createMemo(
				() => s.workstreams().filter((w) => w.archivedAt).length,
			);
			expect(archivedCount()).toBe(0);

			s.archiveWorkstream("ws-1145");
			expect(archivedCount()).toBe(1);

			// A second, distinct done row archives independently — the memo
			// tracks the growing set, not a one-shot flag.
			s.archiveWorkstream("ws-1130");
			expect(archivedCount()).toBe(2);
		});
	});
});

describe("assignedIssues async load (PR3)", () => {
	// At init the store fires loadAssignedIssues; after the seam's microtask
	// settles, assignedIssues() holds the fixture queue for the default
	// (non-empty) handle. Before the tick it is []; this pins the resolution.
	test("loads the fixture queue for the default handle after a tick", async () => {
		await withStoreAsync(async (s) => {
			// Synchronously (pre-microtask) the signal is still its empty seed.
			expect(s.assignedIssues()).toEqual([]);

			await Promise.resolve();

			expect(s.assignedIssues().map((w) => w.id)).toEqual(
				STUB_ASSIGNED_ISSUES.map((w) => w.id),
			);
		});
	});

	// Reconfiguring to an empty handle reloads through the seam, which yields []
	// for a blank handle (tracker-not-configured). Proves setTrackerConfig
	// rebuilds the seam AND reloads, not just swaps the config signal.
	test("clears the queue after setTrackerConfig with an empty handle", async () => {
		await withStoreAsync(async (s) => {
			await Promise.resolve();
			expect(s.assignedIssues().length).toBeGreaterThan(0);

			s.setTrackerConfig({ ...s.trackerConfig(), handle: "" });
			await Promise.resolve();

			expect(s.assignedIssues()).toEqual([]);
		});
	});
});

describe("setTrackerConfig (PR3)", () => {
	// trackerConfig() reflects the config just set — the accessor the Settings
	// view reads back. Uses a non-default handle so the assertion can only pass
	// if the setter actually wrote the signal (not a default echo).
	test("trackerConfig reflects a newly set config", () => {
		withStore((s) => {
			const next = { ...s.trackerConfig(), handle: "someone@else" };

			s.setTrackerConfig(next);

			expect(s.trackerConfig().handle).toBe("someone@else");
		});
	});
});

describe("openChannel (T5)", () => {
	// A standalone channel routes to the channel view and centers the selection
	// on that channel. Starts from the boot bridge view so the assertion proves
	// the transition, not the default. `ch-svc-compass` is a plain `channel`
	// (comms-stub.ts:232) the caller is subscribed to.
	test("routes a standalone channel to the channel view", () => {
		withStore((s) => {
			expect(s.view()).toBe("bridge");

			s.openChannel("ch-svc-compass");

			expect(s.view()).toBe("channel");
			expect(s.selectedChannelId()).toBe("ch-svc-compass");
		});
	});

	// A 1:1 agent home DM is the agent's workspace surface, not a dead-end DM
	// view (record §603-604): openChannel delegates to openAgent, so the id
	// resolves to the agent view with that agent selected. The DM id is derived
	// from cook's homeChannelId so a fixture reshuffle can't stale it.
	test("routes a 1:1 agent DM to the agent workspace via openAgent", () => {
		withStore((s) => {
			const cook = STUB_AGENTS.find((a) => a.account.id === "acc-cook");
			const dmId = cook?.account.homeChannelId ?? "dm-cook";

			s.openChannel(dmId);

			expect(s.view()).toBe("agent");
			expect(s.selectedAgentId()).toBe("acc-cook");
		});
	});

	// A group DM has more than one other party — it is not a single-agent
	// workspace, so it routes to the channel view like any channel (not to an
	// agent). `dm-cook-ross` is a `group_dm` of matt + cook + ross
	// (comms-stub.ts:262-268).
	test("routes a group DM to the channel view, not an agent workspace", () => {
		withStore((s) => {
			s.openChannel("dm-cook-ross");

			expect(s.view()).toBe("channel");
			expect(s.selectedChannelId()).toBe("dm-cook-ross");
		});
	});

	// An unknown channel id is guarded (`if (!chan) return`): neither the view
	// nor the selection moves. Captured-before/after proves the no-op rather
	// than asserting a fixed boot state.
	test("is a no-op on an unknown channel id", () => {
		withStore((s) => {
			const view = s.view();
			const selected = s.selectedChannelId();

			s.openChannel("ch-does-not-exist");

			expect(s.view()).toBe(view);
			expect(s.selectedChannelId()).toBe(selected);
		});
	});
});
