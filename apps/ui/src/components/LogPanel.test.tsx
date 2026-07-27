import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { Show } from "solid-js";
import { StoreContext } from "../context";
import { foldSession } from "../session-events";
import { STUB_SESSION_EVENTS } from "../session-events-stub";
import { type AppStore, createAppStore } from "../store";
import { LogPanel } from "./LogPanel";

// Acceptance spec for T-U2 (design.md §440-478): LogPanel's `TracePane` is
// rebuilt over `foldSession(store.agentSession().events)` and renders the typed
// `SessionTrace`; `FrameRow`/`FRAME_TAG` are deleted. The panel SHELL is
// UNCHANGED — header handle, running dot, Stop (disabled when idle), minimize
// toggle, trace body removed when minimized — so the shell + no-input-box tests
// stay GREEN. The two typed-trace tests are RED until SessionTrace.tsx lands and
// store.agentSession() is re-pointed to the new AgentSession (`.events`) shape.
//
// Session subtlety (store.ts:602-604): `store.agentSession()` keys off
// `store.selectedAgentId()`, NOT off the `agent` prop. So each test must
// `store.openAgent(id)` (sets selectedAgentId AND resolves selectedAgent) and
// pass the resolved agent as the prop — both are driven below.
//
// Fixture ground truth (session-events-stub.ts STUB_SESSION_EVENTS):
//   - acc-livingstone: running:true — a thinking beat, ~10 one-word
//     assistant_text deltas sharing messageId "m-l1" (fold → ONE text item), a
//     tool_call + tool_call_update (tc-l1, diff+output+status "completed"), a
//     3-entry plan.
//   - acc-drake:  running:false — a single notice.
//   - acc-ross:   ABSENT → agentSession() undefined → empty state.

// Derived from the fixture (never hardcoded, so a fixture reshuffle can't stale
// the test): the coalesced assistant text and the tool title the typed trace
// must show for acc-livingstone. Computed via the real fold so the test asserts
// exactly what the panel renders.
const LIVINGSTONE_ITEMS = foldSession(
	STUB_SESSION_EVENTS["acc-livingstone"].events,
);
const LIVINGSTONE_COALESCED_TEXT = (() => {
	const item = LIVINGSTONE_ITEMS.find((i) => i.kind === "text");
	if (item?.kind !== "text") throw new Error("no coalesced text item");
	return item.text;
})();
const LIVINGSTONE_TOOL_TITLE = (() => {
	const item = LIVINGSTONE_ITEMS.find((i) => i.kind === "tool");
	if (item?.kind !== "tool" || !item.call)
		throw new Error("no tool item with a call");
	return item.call.title;
})();

// Mount LogPanel over a real store through the app's StoreContext. The store is
// built inside render's reactive root (its memos are owned + disposed on the
// library's per-test cleanup); `openAgent` runs BEFORE the JSX resolves, and a
// `<Show when={store.selectedAgent()} keyed>` narrows the agent for the prop —
// re-opening another agent re-keys the Show, re-driving both prop and session.
function mountLogPanel(agentId: string): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore();
		store.openAgent(agentId);
		return (
			<StoreContext.Provider value={store}>
				<Show when={store.selectedAgent()} keyed>
					{(agent) => <LogPanel agent={agent} />}
				</Show>
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

describe("LogPanel (T-U2)", () => {
	// Contract: an agent with a session renders its TYPED trace. The fold must
	// have run — the ~10 one-word streaming deltas coalesce into ONE `.block-text`
	// (not 10 rows), the tool title shows, and the plan block renders.
	test("renders the agent's typed trace", () => {
		const { container } = mountLogPanel("acc-livingstone");

		const trace = container.querySelector(".obs-trace");
		expect(trace).not.toBeNull();

		// The fold ran: the streaming deltas are ONE coalesced text block carrying
		// the full joined sentence — not one block per delta.
		const textBlocks = [
			...container.querySelectorAll(".obs-trace .block-text"),
		];
		const coalesced = textBlocks.filter((n) =>
			n.textContent?.includes(LIVINGSTONE_COALESCED_TEXT),
		);
		expect(coalesced.length).toBe(1);

		// The tool row shows the call title.
		const toolTitles = [
			...container.querySelectorAll(".obs-trace .block-tool .tool-title"),
		].map((n) => n.textContent);
		expect(toolTitles.some((t) => t?.includes(LIVINGSTONE_TOOL_TITLE))).toBe(
			true,
		);

		// The plan block renders.
		expect(container.querySelector(".obs-trace .block-plan")).not.toBeNull();
	});

	// Contract: an agent with no STUB_SESSION_EVENTS entry resolves
	// agentSession() to undefined — the TracePane shows the `.obs-empty` fallback
	// and there is no `.obs-trace` body. (The fallback text is the TracePane's to
	// pick; assert the empty-state contract via `.obs-empty` presence + no body.)
	test("empty state for an agent without a session", () => {
		const { container } = mountLogPanel("acc-ross");

		expect(container.querySelector(".obs-empty")).not.toBeNull();
		expect(container.querySelector(".obs-trace")).toBeNull();
	});

	// SHELL INVARIANT (stays GREEN): minimizing collapses the panel — the trace
	// body is gone from the DOM — while the running dot stays visible (liveness at
	// a glance). Expanding restores the body. Driven via the toggle by aria-label.
	test("minimize hides the trace body but keeps the running dot; expand restores it", () => {
		const { container } = mountLogPanel("acc-livingstone");

		// Expanded: body present, running dot present.
		expect(container.querySelector(".obs-body")).not.toBeNull();
		expect(container.querySelector(".obs-run-dot")).not.toBeNull();

		const minimize = container.querySelector<HTMLButtonElement>(
			'button[aria-label="Minimize"]',
		);
		expect(minimize).not.toBeNull();
		if (!minimize) throw new Error("minimize toggle not rendered");
		fireEvent.click(minimize);

		// Minimized: trace body gone, running dot still shown.
		expect(container.querySelector(".obs-body")).toBeNull();
		expect(container.querySelector(".obs-trace")).toBeNull();
		expect(container.querySelector(".obs-run-dot")).not.toBeNull();

		// Expand restores the body.
		const expand = container.querySelector<HTMLButtonElement>(
			'button[aria-label="Expand"]',
		);
		expect(expand).not.toBeNull();
		if (!expand) throw new Error("expand toggle not rendered");
		fireEvent.click(expand);

		expect(container.querySelector(".obs-body")).not.toBeNull();
	});

	// SHELL INVARIANT (stays GREEN): Stop is disabled when the agent is idle and
	// enabled while it is running. Re-opening a running agent on the same mount
	// re-drives the session and re-enables Stop.
	test("Stop is disabled when idle, enabled when running", () => {
		const { store, container } = mountLogPanel("acc-drake");

		// acc-drake: running:false → Stop disabled.
		const idleStop = container.querySelector<HTMLButtonElement>(".obs-stop");
		expect(idleStop).not.toBeNull();
		expect(idleStop?.disabled).toBe(true);

		// Re-open a running agent: the reactive prop + session update, Stop enables.
		store.openAgent("acc-livingstone");

		const liveStop = container.querySelector<HTMLButtonElement>(".obs-stop");
		expect(liveStop).not.toBeNull();
		expect(liveStop?.disabled).toBe(false);
	});

	// HARD INVARIANT (stays GREEN, design grounding line 17): the trace surface is
	// observation-only — the panel carries NO input box. A composer creeping into
	// the observation panel would redden this.
	test("the panel contains no input box (observation-only)", () => {
		const { container } = mountLogPanel("acc-livingstone");

		expect(container.querySelector("input, textarea")).toBeNull();
	});
});
