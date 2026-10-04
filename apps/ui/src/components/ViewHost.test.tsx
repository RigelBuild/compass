import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import type { Pane } from "../agent-tabs";
import { STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { createViewScope, type ViewScope } from "../view-scope";
import { ViewHost } from "./ViewHost";

const AGENT_A = "acc-compass-ui";
const AGENT_B = "acc-compass-server";
const termPane: Pane = {
	id: "t-ui1",
	kind: "terminal",
	title: "vite dev",
	terminalId: "t-ui1",
};

// Two views in one store: each must own its agent and its agent-workspace tabs.
function mountTwoViews(): {
	a: ViewScope;
	b: ViewScope;
	paneA: HTMLElement;
	paneB: HTMLElement;
} {
	let a!: ViewScope;
	let b!: ViewScope;
	const { container } = render(() => {
		const store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		a = createViewScope(store, "view-a", `/agent/${AGENT_A}`);
		b = createViewScope(store, "view-b", `/agent/${AGENT_B}`);
		return (
			<StoreContext value={store}>
				<section data-view="a">
					<ViewHost scope={a} />
				</section>
				<section data-view="b">
					<ViewHost scope={b} />
				</section>
			</StoreContext>
		);
	});
	flush();
	const pane = (id: string): HTMLElement => {
		const el = container.querySelector<HTMLElement>(`[data-view="${id}"]`);
		if (!el) throw new Error(`view ${id} not rendered`);
		return el;
	};
	return { a, b, paneA: pane("a"), paneB: pane("b") };
}

const tabCount = (pane: HTMLElement): number =>
	pane.querySelectorAll(".av-tab").length;

describe("ViewHost: per-view agent workspace", () => {
	test("each view renders its own agent", () => {
		const { paneA, paneB } = mountTwoViews();

		expect(paneA.querySelector(".av-name")?.textContent).toBe("compass-ui");
		expect(paneB.querySelector(".av-name")?.textContent).toBe("compass-server");
	});

	test("opening a terminal tab in one view leaves the other unchanged", () => {
		const { a, b, paneA, paneB } = mountTwoViews();
		expect(tabCount(paneA)).toBe(1);
		expect(tabCount(paneB)).toBe(1);

		a.openTab(termPane);
		flush();

		expect(a.agentTabs().map((t) => t.id)).toEqual(["chat", "t-ui1"]);
		expect(b.agentTabs().map((t) => t.id)).toEqual(["chat"]);
		expect(tabCount(paneA)).toBe(2);
		expect(tabCount(paneB)).toBe(1);
		expect(paneB.querySelector(".term-body")).toBeNull();
	});
});
