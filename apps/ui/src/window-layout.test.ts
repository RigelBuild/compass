import { describe, expect, test } from "bun:test";
import {
	focusedViewOf,
	type LayoutAction,
	layoutViews,
	loadLayout,
	MAX_TABS,
	reduceLayout,
	saveLayout,
	singleTabLayout,
	type WindowLayout,
} from "./window-layout";

// A refusal is a test failure everywhere except the tab-cap case.
function reduce(layout: WindowLayout, action: LayoutAction): WindowLayout {
	const next = reduceLayout(layout, action);
	if ("refused" in next) throw new Error(`refused: ${next.refused}`);
	return next;
}

function openAll(paths: readonly string[]): WindowLayout {
	const [first = "/", ...rest] = paths;
	return rest.reduce(
		(layout, path) => reduce(layout, { kind: "open", path }),
		singleTabLayout(first),
	);
}

function tabPaths(layout: WindowLayout): string[] {
	return layout.tabs.map((tab) =>
		tab.layout.kind === "single" ? tab.layout.view.path : "split",
	);
}

function activePath(layout: WindowLayout): string | undefined {
	const tab = layout.tabs.find((item) => item.id === layout.activeTabId);
	return tab?.layout.kind === "single" ? tab.layout.view.path : undefined;
}

function memoryStorage(seed: Record<string, string> = {}): Storage {
	const items = new Map(Object.entries(seed));
	return {
		get length() {
			return items.size;
		},
		clear: () => items.clear(),
		getItem: (key) => items.get(key) ?? null,
		key: (index) => [...items.keys()][index] ?? null,
		removeItem: (key) => {
			items.delete(key);
		},
		setItem: (key, value) => {
			items.set(key, value);
		},
	};
}

describe("reduceLayout", () => {
	test("opening a path a single-view tab shows focuses that tab", () => {
		const layout = openAll(["/", "/backlog", "/done"]);
		const backlog = layout.tabs[1];
		const next = reduce(layout, { kind: "open", path: "/backlog" });
		expect(next.tabs).toHaveLength(3);
		expect(next.activeTabId).toBe(backlog?.id ?? "");
	});

	test("a fresh open skips the dedupe and opens a second tab on the same path", () => {
		const layout = openAll(["/", "/backlog"]);
		const onBridge = reduce(layout, {
			kind: "focusTab",
			tabId: layout.tabs[0]?.id ?? "",
		});
		const next = reduce(onBridge, { kind: "open", path: "/", fresh: true });
		expect(tabPaths(next)).toEqual(["/", "/", "/backlog"]);
		expect(next.activeTabId).toBe(next.tabs[1]?.id ?? "");
	});

	test("a fresh open at the tab cap is refused even when the path is open", () => {
		const paths = Array.from({ length: MAX_TABS }, (_, i) => `/agent/a-${i}`);
		const full = openAll(paths);
		expect(
			reduceLayout(full, { kind: "open", path: "/agent/a-0", fresh: true }),
		).toEqual({ refused: "tab-cap" });
	});

	test("a new tab opens after the active one", () => {
		const layout = openAll(["/", "/backlog"]);
		const focused = reduce(layout, {
			kind: "focusTab",
			tabId: layout.tabs[0]?.id ?? "",
		});
		const next = reduce(focused, { kind: "open", path: "/done" });
		expect(tabPaths(next)).toEqual(["/", "/done", "/backlog"]);
		expect(activePath(next)).toBe("/done");
	});

	test("closing the active tab focuses the tab to its right, else its left", () => {
		const layout = openAll(["/", "/backlog", "/done"]);
		const [first, middle, last] = layout.tabs;
		const onMiddle = reduce(layout, {
			kind: "focusTab",
			tabId: middle?.id ?? "",
		});
		const right = reduce(onMiddle, { kind: "close", tabId: middle?.id ?? "" });
		expect(right.activeTabId).toBe(last?.id ?? "");
		const left = reduce(right, { kind: "close", tabId: last?.id ?? "" });
		expect(left.activeTabId).toBe(first?.id ?? "");
		expect(tabPaths(left)).toEqual(["/"]);
	});

	test("closing an inactive tab keeps the focus", () => {
		const layout = openAll(["/", "/backlog", "/done"]);
		const first = layout.tabs[0];
		const next = reduce(layout, { kind: "close", tabId: first?.id ?? "" });
		expect(next.activeTabId).toBe(layout.activeTabId);
		expect(tabPaths(next)).toEqual(["/backlog", "/done"]);
	});

	test("the last tab cannot close; closing it navigates it to /", () => {
		const layout = singleTabLayout("/done");
		const tab = layout.tabs[0];
		const next = reduce(layout, { kind: "close", tabId: tab?.id ?? "" });
		expect(next.tabs).toHaveLength(1);
		expect(next.activeTabId).toBe(tab?.id ?? "");
		expect(activePath(next)).toBe("/");
	});

	test("an eleventh tab is refused, but a dedupe at the cap still focuses", () => {
		const paths = Array.from({ length: MAX_TABS }, (_, i) => `/agent/a-${i}`);
		const full = openAll(paths);
		expect(full.tabs).toHaveLength(MAX_TABS);
		expect(reduceLayout(full, { kind: "open", path: "/done" })).toEqual({
			refused: "tab-cap",
		});
		const deduped = reduce(full, { kind: "open", path: "/agent/a-0" });
		expect(activePath(deduped)).toBe("/agent/a-0");
	});

	test("move clamps the target index to the tab bounds", () => {
		const layout = openAll(["/", "/backlog", "/done"]);
		const first = layout.tabs[0];
		const last = layout.tabs[2];
		expect(
			tabPaths(
				reduce(layout, { kind: "move", tabId: first?.id ?? "", toIndex: 99 }),
			),
		).toEqual(["/backlog", "/done", "/"]);
		expect(
			tabPaths(
				reduce(layout, { kind: "move", tabId: last?.id ?? "", toIndex: -5 }),
			),
		).toEqual(["/done", "/", "/backlog"]);
		expect(
			reduce(layout, { kind: "move", tabId: "tab-missing", toIndex: 0 }),
		).toBe(layout);
	});

	test("a split whose other pane closes turns back into a single view", () => {
		const layout = singleTabLayout("/backlog");
		const split = reduce(layout, { kind: "split", direction: "row" });
		const tab = split.tabs[0]?.layout;
		if (tab?.kind !== "split") throw new Error("expected a split tab");
		expect(tab.first.path).toBe("/backlog");
		expect(tab.second.id).not.toBe(tab.first.id);
		const onFirst = reduce(split, { kind: "focusPane", pane: "first" });
		const navigated = reduce(onFirst, {
			kind: "navigateFocused",
			path: "/done",
		});
		const single = reduce(navigated, { kind: "closeOtherPane" });
		expect(single.tabs[0]?.layout).toEqual({
			kind: "single",
			view: { id: tab.first.id, path: "/done" },
		});
	});

	test("resize keeps the split ratio within 0.2..0.8", () => {
		const split = reduce(singleTabLayout("/"), {
			kind: "split",
			direction: "column",
		});
		const ratioOf = (layout: WindowLayout): number | undefined => {
			const tab = layout.tabs[0]?.layout;
			return tab?.kind === "split" ? tab.ratio : undefined;
		};
		expect(ratioOf(reduce(split, { kind: "resize", ratio: 0.05 }))).toBe(0.2);
		expect(ratioOf(reduce(split, { kind: "resize", ratio: 0.95 }))).toBe(0.8);
	});
});

describe("loadLayout / saveLayout", () => {
	test("a saved layout restores, focused on the tab the hash shows", () => {
		const storage = memoryStorage();
		const saved = openAll(["/", "/backlog", "/done"]);
		saveLayout(storage, saved);
		const restored = loadLayout(storage, "/backlog");
		expect(tabPaths(restored)).toEqual(["/", "/backlog", "/done"]);
		expect(restored.tabs.map((tab) => tab.id)).toEqual(
			saved.tabs.map((tab) => tab.id),
		);
		expect(activePath(restored)).toBe("/backlog");
	});

	test("a corrupt stored value falls back to one Bridge tab", () => {
		for (const raw of ["{not json", '{"layout":{"tabs":[]}}', "null"]) {
			const restored = loadLayout(
				memoryStorage({ "compass.windowLayout": raw }),
				"/",
			);
			expect(tabPaths(restored)).toEqual(["/"]);
		}
	});

	test("a deep-link hash not in the restored layout opens as the focused tab", () => {
		const storage = memoryStorage();
		saveLayout(storage, openAll(["/", "/backlog"]));
		const restored = loadLayout(storage, "/settings/general");
		expect(tabPaths(restored)).toEqual(["/", "/backlog", "/settings/general"]);
		expect(activePath(restored)).toBe("/settings/general");
	});

	test("with no storage the hash opens as a single view", () => {
		const restored = loadLayout(undefined, "/done");
		expect(tabPaths(restored)).toEqual(["/done"]);
	});

	test("a split whose panes share the hash keeps its saved focused pane", () => {
		const storage = memoryStorage();
		const split = reduce(singleTabLayout("/backlog"), {
			kind: "split",
			direction: "row",
		});
		const tab = split.tabs[0]?.layout;
		if (tab?.kind !== "split") throw new Error("expected a split tab");
		saveLayout(storage, split);
		expect(focusedViewOf(loadLayout(storage, "/backlog")).id).toBe(
			tab.second.id,
		);
	});

	test("an out-of-range stored sequence never mints duplicate view ids", () => {
		for (const seed of ["1e309", String(Number.MAX_SAFE_INTEGER)]) {
			const stored = JSON.stringify({ seq: 1, layout: singleTabLayout("/") });
			const raw = stored.replace('"seq":1', `"seq":${seed}`);
			const restored = loadLayout(
				memoryStorage({ "compass.windowLayout": raw }),
				"/",
			);
			const a = reduce(restored, { kind: "open", path: "/backlog" });
			const b = reduce(a, { kind: "open", path: "/done" });
			const ids = layoutViews(b).map((view) => view.id);
			expect(new Set(ids).size).toBe(ids.length);
		}
	});
});
