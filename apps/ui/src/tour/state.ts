// The first-run tour's step table. Pure data: no DOM and no store import, so the
// controller (store.ts) and the overlay read one definition.

/** One tour step: a centered dialog or a callout anchored to a real element. */
export interface TourStep {
	/** Stable id: the analytics dimension and the server resume cursor. */
	readonly id: string;
	readonly kind: "dialog" | "callout";
	/** The `data-tour` value of the element a callout attaches to. */
	readonly anchor?: string;
	/** The static view the step needs; the controller navigates there on entry. */
	readonly route?: "/" | "/backlog" | "/done" | "/settings";
	readonly title: string;
	readonly body: string;
}

/** The step ids are frozen: the server stores them as the resume cursor.
 *  Copy stays chord-free; the overlay renders any chord through `shortcutFor`. */
export const TOUR_STEPS: readonly TourStep[] = [
	{
		id: "welcome",
		kind: "dialog",
		title: "Welcome to Compass",
		body: "Compass is where you run a fleet of coding agents. This short tour uses a few demo agents so there is something to look at. Demo rows are marked and disappear when the tour ends.",
	},
	{
		id: "board",
		kind: "callout",
		anchor: "board-grid",
		route: "/",
		title: "The Bridge",
		body: "Each row is an agent and each column is a stage. Cards move right as agents pick up, build, and ship their issues.",
	},
	{
		id: "sidebar-tree",
		kind: "callout",
		anchor: "agent-tree",
		title: "Your agents",
		body: "The left sidebar lists your agents as a tree. A supervisor sits above the workers it hands work to, and each agent's channels sit under it.",
	},
	{
		id: "agent-workspace",
		kind: "callout",
		anchor: "demo-agent",
		title: "An agent's workspace",
		body: "Click an agent to open its workspace: its chat, its terminals, and what it is doing right now.",
	},
	{
		id: "keyboard",
		kind: "callout",
		anchor: "view-tabs",
		title: "Move by keyboard",
		body: "Every view has a shortcut. Hover a control to see its shortcut, and use the command palette to run any action by name.",
	},
	{
		id: "finale",
		kind: "dialog",
		title: "You're set",
		body: "Open the shortcuts sheet any time for the full keymap. You can replay this tour from the command palette.",
	},
];
