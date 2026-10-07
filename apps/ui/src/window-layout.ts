export type ViewInstance = { id: string; path: string };
export type TabLayout =
	| { kind: "single"; view: ViewInstance }
	| {
			kind: "split";
			direction: "row" | "column";
			first: ViewInstance;
			second: ViewInstance;
			ratio: number; // share of the first pane, 0.2..0.8
			focused: "first" | "second";
	  };
/** A view tab: one entry in the window's tab strip. */
export type ViewTab = { id: string; layout: TabLayout };
export type WindowLayout = { tabs: ViewTab[]; activeTabId: string };

export const MAX_TABS = 10;
export const MIN_RATIO = 0.2;
export const MAX_RATIO = 0.8;
const STORAGE_KEY = "compass.windowLayout";
// Far above any real session's id count, far below where +1 stops working.
const MAX_SEED = 1e9;

export type LayoutAction =
	| { kind: "open"; path: string; background?: boolean }
	| { kind: "close"; tabId: string }
	| { kind: "focusTab"; tabId: string }
	| { kind: "move"; tabId: string; toIndex: number }
	| { kind: "split"; direction: "row" | "column" }
	| { kind: "closeOtherPane" }
	| { kind: "focusPane"; pane: "first" | "second" }
	| { kind: "resize"; ratio: number }
	| { kind: "navigateFocused"; path: string };
export type LayoutRefusal = { refused: "tab-cap" };

// Ids are never reused within a page session: a history entry naming a closed
// view must not resolve to a later view. Persisted with the layout for reloads.
let idSeq = 0;
function mintId(prefix: "tab" | "view"): string {
	idSeq += 1;
	return `${prefix}-${idSeq}`;
}

function newView(path: string): ViewInstance {
	return { id: mintId("view"), path };
}

function newTab(path: string): ViewTab {
	return { id: mintId("tab"), layout: { kind: "single", view: newView(path) } };
}

/** A fresh one-tab layout on `path`. */
export function singleTabLayout(path: string): WindowLayout {
	const tab = newTab(path);
	return { tabs: [tab], activeTabId: tab.id };
}

function activeTab(layout: WindowLayout): ViewTab {
	const tab =
		layout.tabs.find((item) => item.id === layout.activeTabId) ??
		layout.tabs[0];
	if (!tab) throw new Error("a window layout always holds a tab");
	return tab;
}

/** The view a tab shows focused: its single view, or its focused pane. */
export function focusedPane(tab: TabLayout): ViewInstance {
	if (tab.kind === "single") return tab.view;
	return tab.focused === "first" ? tab.first : tab.second;
}

/** Every view a tab shows: one, or both panes of a split. */
export function tabViews(tab: TabLayout): ViewInstance[] {
	return tab.kind === "single" ? [tab.view] : [tab.first, tab.second];
}

/** The ids of the views on screen: every pane of the active tab. */
export function shownViewIds(layout: WindowLayout): string[] {
	return tabViews(activeTab(layout).layout).map((view) => view.id);
}

/** The active tab's focused view: the one the URL mirrors. */
export function focusedViewOf(layout: WindowLayout): ViewInstance {
	return focusedPane(activeTab(layout).layout);
}

/** Every view instance in the layout, in tab order. */
export function layoutViews(layout: WindowLayout): ViewInstance[] {
	return layout.tabs.flatMap((tab) => tabViews(tab.layout));
}

// Each helper returns the same object when nothing changes, so a caller can
// skip a write (and the hash sync it would trigger) on a no-op.
function updateActive(
	layout: WindowLayout,
	update: (tab: TabLayout) => TabLayout,
): WindowLayout {
	const tab = activeTab(layout);
	const next = update(tab.layout);
	if (next === tab.layout) return layout;
	return {
		...layout,
		tabs: layout.tabs.map((item) =>
			item.id === tab.id ? { ...item, layout: next } : item,
		),
	};
}

function withFocusedPath(tab: TabLayout, path: string): TabLayout {
	const view = focusedPane(tab);
	if (view.path === path) return tab;
	const moved = { ...view, path };
	if (tab.kind === "single") return { ...tab, view: moved };
	return tab.focused === "first"
		? { ...tab, first: moved }
		: { ...tab, second: moved };
}

function open(
	layout: WindowLayout,
	path: string,
	background: boolean,
): WindowLayout | LayoutRefusal {
	const existing = layout.tabs.find(
		(tab) => tab.layout.kind === "single" && tab.layout.view.path === path,
	);
	if (existing) {
		return background || existing.id === layout.activeTabId
			? layout
			: { ...layout, activeTabId: existing.id };
	}
	if (layout.tabs.length >= MAX_TABS) return { refused: "tab-cap" };
	const tab = newTab(path);
	const at = layout.tabs.findIndex((item) => item.id === layout.activeTabId);
	const tabs = [...layout.tabs];
	tabs.splice(at + 1, 0, tab);
	return { tabs, activeTabId: background ? layout.activeTabId : tab.id };
}

function close(layout: WindowLayout, tabId: string): WindowLayout {
	const at = layout.tabs.findIndex((tab) => tab.id === tabId);
	const closing = layout.tabs[at];
	if (!closing) return layout;
	// The last tab stays: closing it sends its focused view home instead.
	if (layout.tabs.length === 1) {
		const view = focusedPane(closing.layout);
		if (closing.layout.kind === "single" && view.path === "/") return layout;
		return {
			...layout,
			tabs: [
				{
					...closing,
					layout: { kind: "single", view: { ...view, path: "/" } },
				},
			],
		};
	}
	const tabs = layout.tabs.filter((tab) => tab.id !== tabId);
	if (layout.activeTabId !== tabId) return { ...layout, tabs };
	const next = tabs[at] ?? tabs[at - 1];
	return { tabs, activeTabId: next ? next.id : layout.activeTabId };
}

function move(
	layout: WindowLayout,
	tabId: string,
	toIndex: number,
): WindowLayout {
	const from = layout.tabs.findIndex((tab) => tab.id === tabId);
	if (from < 0 || Number.isNaN(toIndex)) return layout;
	const to = Math.min(Math.max(Math.trunc(toIndex), 0), layout.tabs.length - 1);
	if (to === from) return layout;
	const tabs = [...layout.tabs];
	const [moved] = tabs.splice(from, 1);
	if (!moved) return layout;
	tabs.splice(to, 0, moved);
	return { ...layout, tabs };
}

function navigateFocused(layout: WindowLayout, path: string): WindowLayout {
	return updateActive(layout, (tab) => withFocusedPath(tab, path));
}

export function reduceLayout(
	layout: WindowLayout,
	action: LayoutAction,
): WindowLayout | LayoutRefusal {
	switch (action.kind) {
		case "open":
			return open(layout, action.path, action.background ?? false);
		case "close":
			return close(layout, action.tabId);
		case "focusTab":
			return action.tabId !== layout.activeTabId &&
				layout.tabs.some((tab) => tab.id === action.tabId)
				? { ...layout, activeTabId: action.tabId }
				: layout;
		case "move":
			return move(layout, action.tabId, action.toIndex);
		case "split":
			// Bounded to two panes: an already split tab does not split again.
			return updateActive(layout, (tab) =>
				tab.kind === "single"
					? {
							kind: "split",
							direction: action.direction,
							first: tab.view,
							second: newView(tab.view.path),
							ratio: 0.5,
							focused: "second",
						}
					: tab,
			);
		case "closeOtherPane":
			return updateActive(layout, (tab) =>
				tab.kind === "split" ? { kind: "single", view: focusedPane(tab) } : tab,
			);
		case "focusPane":
			return updateActive(layout, (tab) =>
				tab.kind === "split" && tab.focused !== action.pane
					? { ...tab, focused: action.pane }
					: tab,
			);
		case "resize":
			return updateActive(layout, (tab) => {
				if (tab.kind !== "split" || Number.isNaN(action.ratio)) return tab;
				const ratio = clampRatio(action.ratio);
				return ratio === tab.ratio ? tab : { ...tab, ratio };
			});
		case "navigateFocused":
			return navigateFocused(layout, action.path);
	}
}

function clampRatio(ratio: number): number {
	return Math.min(Math.max(ratio, MIN_RATIO), MAX_RATIO);
}

/** Focus the tab and pane holding `viewId`; unchanged for an unknown id. */
export function focusView(layout: WindowLayout, viewId: string): WindowLayout {
	const tab = layout.tabs.find((item) =>
		tabViews(item.layout).some((view) => view.id === viewId),
	);
	if (!tab) return layout;
	let next = tab.layout;
	if (next.kind === "split") {
		const pane = next.first.id === viewId ? "first" : "second";
		if (next.focused !== pane) next = { ...next, focused: pane };
	}
	if (next === tab.layout && tab.id === layout.activeTabId) return layout;
	return {
		tabs: layout.tabs.map((item) =>
			item.id === tab.id ? { ...item, layout: next } : item,
		),
		activeTabId: tab.id,
	};
}

/** Move one view, focused or not, to `path` without changing focus. */
export function setViewPath(
	layout: WindowLayout,
	viewId: string,
	path: string,
): WindowLayout {
	const move = (view: ViewInstance): ViewInstance =>
		view.id === viewId && view.path !== path ? { ...view, path } : view;
	let changed = false;
	const tabs = layout.tabs.map((tab) => {
		const current = tab.layout;
		const next: TabLayout =
			current.kind === "single"
				? { ...current, view: move(current.view) }
				: {
						...current,
						first: move(current.first),
						second: move(current.second),
					};
		if (tabViews(next).every((view, i) => view === tabViews(current)[i])) {
			return tab;
		}
		changed = true;
		return { ...tab, layout: next };
	});
	return changed ? { ...layout, tabs } : layout;
}

/** Restore the session's layout, or one tab on `hashPath` when there is none
 *  or it is unreadable. A hash no restored tab shows opens focused, so a deep
 *  link always wins. */
export function loadLayout(
	storage: Storage | undefined,
	hashPath: string,
): WindowLayout {
	const restored = readStored(storage);
	if (!restored) return singleTabLayout(hashPath);
	// Keep the saved focus when it already shows the hash: split panes can share it.
	if (focusedViewOf(restored).path === hashPath) return restored;
	const shown = layoutViews(restored).find((view) => view.path === hashPath);
	if (shown) return focusView(restored, shown.id);
	const opened = open(restored, hashPath, false);
	// At the cap there is no room for a new tab; the focused view takes the link.
	return "refused" in opened ? navigateFocused(restored, hashPath) : opened;
}

/** The `sessionStorage` handle, or undefined where reading it throws (a
 *  privacy-locked context): the layout then lives only for the page. */
export function sessionLayoutStorage(): Storage | undefined {
	try {
		return globalThis.sessionStorage;
	} catch {
		return undefined;
	}
}

export function saveLayout(
	storage: Storage | undefined,
	layout: WindowLayout,
): void {
	if (!storage) return;
	try {
		storage.setItem(STORAGE_KEY, JSON.stringify({ seq: idSeq, layout }));
	} catch {
		// Best-effort: a quota or privacy-locked write only loses the restore.
	}
}

function readStored(storage: Storage | undefined): WindowLayout | undefined {
	let raw: string | null;
	try {
		raw = storage ? storage.getItem(STORAGE_KEY) : null;
	} catch {
		return undefined;
	}
	if (raw === null) return undefined;
	let parsed: unknown;
	try {
		parsed = JSON.parse(raw);
	} catch {
		return undefined;
	}
	const layout = parseLayout(field(parsed, "layout"));
	if (!layout) return undefined;
	// Continue the stored id sequence so a new view never takes a stale id.
	const ids = [
		...layout.tabs.map((tab) => tab.id),
		...layoutViews(layout).map((view) => view.id),
	];
	const seq = field(parsed, "seq");
	const seeds = [
		seq,
		...ids.map((id) => Number(/-(\d+)$/.exec(id)?.[1] ?? 0)),
	].filter(
		(n): n is number =>
			typeof n === "number" && Number.isInteger(n) && n >= 0 && n <= MAX_SEED,
	);
	// Untrusted seeds near 2^53 stop incrementing and would mint repeat ids.
	idSeq = Math.max(idSeq, ...seeds);
	return layout;
}

// The stored value is untrusted JSON: every field is read as unknown and
// checked before it reaches a typed layout.
function field(value: unknown, key: string): unknown {
	return typeof value === "object" && value !== null && key in value
		? Reflect.get(value, key)
		: undefined;
}

function parseView(value: unknown): ViewInstance | undefined {
	const id = field(value, "id");
	const path = field(value, "path");
	if (typeof id !== "string" || id.length === 0) return undefined;
	if (typeof path !== "string" || !path.startsWith("/")) return undefined;
	return { id, path };
}

function parseTabLayout(value: unknown): TabLayout | undefined {
	const kind = field(value, "kind");
	if (kind === "single") {
		const view = parseView(field(value, "view"));
		return view ? { kind: "single", view } : undefined;
	}
	if (kind !== "split") return undefined;
	const first = parseView(field(value, "first"));
	const second = parseView(field(value, "second"));
	const direction = field(value, "direction");
	const ratio = field(value, "ratio");
	const focused = field(value, "focused");
	if (!first || !second) return undefined;
	if (direction !== "row" && direction !== "column") return undefined;
	if (typeof ratio !== "number" || Number.isNaN(ratio)) return undefined;
	if (focused !== "first" && focused !== "second") return undefined;
	return {
		kind: "split",
		direction,
		first,
		second,
		ratio: clampRatio(ratio),
		focused,
	};
}

function parseLayout(value: unknown): WindowLayout | undefined {
	const items = field(value, "tabs");
	const activeTabId = field(value, "activeTabId");
	if (!Array.isArray(items)) return undefined;
	if (items.length === 0 || items.length > MAX_TABS) return undefined;
	const tabs: ViewTab[] = [];
	for (const item of items) {
		const id = field(item, "id");
		const layout = parseTabLayout(field(item, "layout"));
		if (typeof id !== "string" || !layout) return undefined;
		tabs.push({ id, layout });
	}
	if (typeof activeTabId !== "string") return undefined;
	if (!tabs.some((tab) => tab.id === activeTabId)) return undefined;
	const restored = { tabs, activeTabId };
	const ids = [
		...tabs.map((tab) => tab.id),
		...layoutViews(restored).map((view) => view.id),
	];
	return new Set(ids).size === ids.length ? restored : undefined;
}
