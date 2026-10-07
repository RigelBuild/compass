import { describe, expect, test } from "bun:test";
import {
	createRouter,
	type LocationChange,
	type RouterHistory,
} from "@solidjs/router";
import { render } from "@solidjs/testing-library";
import App from "./App";
import { STUB_COMMS_STATE } from "./comms-stub";
import { StoreContext } from "./context";
import { appRoutes } from "./routes";
import { type AppStore, createAppStore } from "./store";
import { flush } from "./test-router";
import { testQueryClient } from "./test-support";

type Entry = { value: string; state?: unknown };

// The router's memoryHistory keeps paths only, so this one also keeps each
// entry's state: back and forward must see the view id a navigation stamped.
function stateMemoryHistory(initial: readonly Entry[]) {
	const entries: Entry[] = [...initial];
	let index = entries.length - 1;
	let notify: ((value?: string | LocationChange) => void) | undefined;
	const current = (): Entry => {
		const entry = entries[index];
		if (!entry) throw new Error("history index out of range");
		return entry;
	};
	const go = (delta: number): void => {
		index = Math.max(0, Math.min(index + delta, entries.length - 1));
		notify?.();
	};
	const history: RouterHistory = {
		get: current,
		set: (next) => {
			const entry = { value: next.value, state: next.state };
			if (next.replace) entries[index] = entry;
			else {
				entries.splice(index + 1, entries.length, entry);
				index += 1;
			}
		},
		init: (listener) => {
			notify = listener;
			return () => {
				notify = undefined;
			};
		},
		utils: { go },
	};
	return {
		history,
		back: () => go(-1),
		forward: () => go(1),
		length: () => entries.length,
	};
}

function mountWithHistory(initial: readonly Entry[]) {
	const memory = stateMemoryHistory(initial);
	const Router = createRouter({ routes: appRoutes, history: memory.history });
	let store!: AppStore;
	render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<Router>{(props) => <App {...props} />}</Router>
			</StoreContext>
		);
	});
	return { store, memory };
}

function pathOfTab(store: AppStore, tabId: string): string | undefined {
	const tab = store.layout().tabs.find((item) => item.id === tabId);
	return tab?.layout.kind === "single" ? tab.layout.view.path : undefined;
}

describe("window layout over one linear history (record A2)", () => {
	test("back walks a view's navigations, then crosses a replaced tab switch", async () => {
		const { store, memory } = mountWithHistory([{ value: "/" }]);
		await flush();
		const start = memory.length();
		const tabA = store.layout().activeTabId;

		store.showDone();
		await flush();
		store.dispatchLayout({ kind: "open", path: "/" });
		await flush();
		const tabB = store.layout().activeTabId;
		expect(tabB).not.toBe(tabA);
		store.showBacklog();
		await flush();
		expect(pathOfTab(store, tabB)).toBe("/backlog");
		// The tab switch replaced an entry; only the two navigations pushed.
		expect(memory.length()).toBe(start + 2);

		memory.back();
		await flush();
		expect(store.layout().activeTabId).toBe(tabB);
		expect(pathOfTab(store, tabB)).toBe("/");
		expect(pathOfTab(store, tabA)).toBe("/done");
		expect(store.view()).toBe("bridge");

		// The switch replaced A's /done entry, so the next entry back is A on /.
		memory.back();
		await flush();
		expect(store.layout().activeTabId).toBe(tabA);
		expect(pathOfTab(store, tabA)).toBe("/");
		expect(pathOfTab(store, tabB)).toBe("/");
	});

	test("back onto an entry whose view was closed applies to the focused view", async () => {
		const { store, memory } = mountWithHistory([{ value: "/" }]);
		await flush();
		const tabA = store.layout().activeTabId;
		store.showDone();
		await flush();
		store.dispatchLayout({ kind: "open", path: "/" });
		await flush();
		const tabB = store.layout().activeTabId;
		store.showBacklog();
		await flush();
		// Focusing A replaces B's /backlog entry; B's / entry stays behind it.
		store.dispatchLayout({ kind: "focusTab", tabId: tabA });
		await flush();
		store.dispatchLayout({ kind: "close", tabId: tabB });
		await flush();
		expect(pathOfTab(store, tabA)).toBe("/done");

		memory.back();
		await flush();
		expect(store.layout().tabs).toHaveLength(1);
		expect(store.layout().activeTabId).toBe(tabA);
		expect(pathOfTab(store, tabA)).toBe("/");
	});

	test("back onto an entry with no view id applies to the focused view", async () => {
		const { store, memory } = mountWithHistory([
			{ value: "/settings" },
			{ value: "/" },
		]);
		await flush();
		const tabA = store.layout().activeTabId;
		store.dispatchLayout({ kind: "open", path: "/backlog" });
		await flush();
		const tabB = store.layout().activeTabId;
		expect(tabB).not.toBe(tabA);

		memory.back();
		await flush();
		expect(store.layout().activeTabId).toBe(tabB);
		expect(pathOfTab(store, tabB)).toBe("/settings");
		expect(pathOfTab(store, tabA)).toBe("/");
		expect(store.view()).toBe("settings");
	});
});
