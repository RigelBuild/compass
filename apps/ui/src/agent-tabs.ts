// The agent workspace's tab and split-pane model. Pure, so the store and each
// view scope share it without importing each other.

/** What a single pane in the agent view shows: the chat conversation, a
 *  terminal, or a file. A pane is the actual UI leaf — the thing rendered on
 *  screen. */
export type PaneKind = "chat" | "terminal" | "file";

/** One pane in the agent view (design D6/T7): a leaf UI. `terminalId`/`filePath`
 *  are set for the matching kind; the chat pane carries neither. */
export interface Pane {
	id: string;
	kind: PaneKind;
	title: string;
	terminalId?: string;
	filePath?: string;
}

/** A binary split tree of panes within one tab (T7): a leaf shows one pane; a
 *  split places two children row (side by side) or column (stacked), nesting
 *  recursively. "Split right" adds a row split, "split down" a column split. */
export type SplitNode =
	| { kind: "leaf"; pane: Pane }
	| {
			kind: "split";
			direction: "row" | "column";
			left: SplitNode;
			right: SplitNode;
	  };

/** A tab in the agent view: a group of panes shown together on one screen.
 *  Tabs are the top-level switcher (clicking a tab shows its panes full-screen);
 *  a tab owns its own split tree and remembers which pane is focused (the pane
 *  the split buttons act on). The first tab is always the chat (design D6). */
export interface AgentTab {
	id: string;
	title: string;
	/** The split tree of panes in this tab. */
	layout: SplitNode;
	/** The focused pane id — where "split right"/"split down" insert. */
	focusedPaneId: string;
}

/** The always-present chat tab/pane id — the home-DM conversation, which every
 *  agent view opens with and can never close (design D6). The tab and its sole
 *  starting pane share this id. */
export const CHAT_TAB_ID = "chat";

/** The chat pane — the home-DM conversation leaf every agent view opens on. */
const chatPane = (): Pane => ({
	id: CHAT_TAB_ID,
	kind: "chat",
	title: "Chat",
});

/** The default tab set: one chat tab holding the chat pane full-screen. */
export const chatTab = (): AgentTab => ({
	id: CHAT_TAB_ID,
	title: "Chat",
	layout: { kind: "leaf", pane: chatPane() },
	focusedPaneId: CHAT_TAB_ID,
});

/** Every pane id in a tab's split tree, left-to-right (the pane layout order). */
export function splitPaneIds(node: SplitNode): string[] {
	return node.kind === "leaf"
		? [node.pane.id]
		: [...splitPaneIds(node.left), ...splitPaneIds(node.right)];
}

/** Every pane in a tab's split tree, left-to-right. */
export function splitPanes(node: SplitNode): Pane[] {
	return node.kind === "leaf"
		? [node.pane]
		: [...splitPanes(node.left), ...splitPanes(node.right)];
}

/** Remove a pane from a tab's split tree, collapsing any split that loses a
 *  child to its surviving sibling. Returns null if the tree would be empty. */
export function prunePane(node: SplitNode, paneId: string): SplitNode | null {
	if (node.kind === "leaf") return node.pane.id === paneId ? null : node;
	const left = prunePane(node.left, paneId);
	const right = prunePane(node.right, paneId);
	if (left && right) return { ...node, left, right };
	return left ?? right;
}

/** Split the FIRST leaf matching `targetPaneId`, placing `newPane` beside it in
 *  `direction` (`row` = split right, `column` = split down). Recurses left-first
 *  and stops at the first match, so splitting grows the tree by exactly one pane
 *  (no pane explosion when a pane id somehow repeats). Returns the rewritten
 *  tree and whether a leaf matched. */
export function splitPaneOnce(
	node: SplitNode,
	targetPaneId: string,
	newPane: Pane,
	direction: "row" | "column",
): [SplitNode, boolean] {
	if (node.kind === "leaf") {
		return node.pane.id === targetPaneId
			? [
					{
						kind: "split",
						direction,
						left: node,
						right: { kind: "leaf", pane: newPane },
					},
					true,
				]
			: [node, false];
	}
	const [left, insertedLeft] = splitPaneOnce(
		node.left,
		targetPaneId,
		newPane,
		direction,
	);
	if (insertedLeft) return [{ ...node, left }, true];
	const [right, insertedRight] = splitPaneOnce(
		node.right,
		targetPaneId,
		newPane,
		direction,
	);
	return [{ ...node, right }, insertedRight];
}
