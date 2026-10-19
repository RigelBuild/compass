import {
	type Component,
	createEffect,
	createMemo,
	createSignal,
	For,
	Show,
} from "solid-js";
import {
	activeIssues,
	backlogIssues,
	boardAgents as boardAgentsOf,
	cellItems as cellItemsOf,
	isAdvancing,
	laneTotal as laneTotalOf,
	nextAdvancingExpiry,
	type PrRow,
	prBoardGroups,
	prCount,
} from "../board";
import {
	type BoardDirection,
	type BoardNavInput,
	type BoardStop,
	boardStops,
	gutterId,
	moveCursor,
	prStopId,
	resolveCursor,
} from "../board-nav";
import {
	ciBadge,
	isMultiForge,
	issueKey,
	primaryPr,
	prLifecycle,
	queueChip,
	reviewBadge,
} from "../board-render";
import { BOARD_LANES, PR_LANES } from "../constants";
import { useRefLinkRules, useStore } from "../context";
import type { Command, CommandId } from "../keyboard/commands";
import { detectPlatform } from "../keyboard/dispatch";
import { shortcutForAria } from "../keyboard/keymap";
import {
	createRovingGroup,
	type RovingGroupHandle,
	type Stop,
} from "../keyboard/roving";
import { openLink } from "../open-link";
import type { RefLinkRule } from "../refs/grammar";
import type { Issue, IssueState } from "../stub-data";
import { useView } from "../view-scope";
import { BacklogList } from "./BacklogView";
import { BadgeGlyph } from "./BadgeGlyph";
import { DoneList } from "./DoneView";
import { IssueCard } from "./IssueCard";
import { hasLinkedRef, RefText } from "./RefText";
import { RuntimeMarker } from "./RuntimeMarker";
import { StateDot } from "./StateDot";

const CARD_REF_SELECTOR = ".card-title .ref-link";
function cardRefs(card: HTMLElement): HTMLElement[] {
	return [...card.querySelectorAll<HTMLElement>(CARD_REF_SELECTOR)];
}

/** How the board groups rows. Swimlane = one row per agent (default); status =
 *  a plain column board with no agent rows. */
type BoardMode = "swimlane" | "status";

/** Which artifact kind the board shows — a peer tab axis, orthogonal to
 *  `BoardMode` (which is how ISSUES group). Bridge-local, like `BoardMode`. */
type BoardTab = "issues" | "prs";

// Register group-relative commands in the main scope for palette discovery.
// Their handlers mirror onCommand.
const LIST_COMMANDS: ReadonlyArray<{
	id: string;
	title: string;
	keywords: string[];
}> = [
	{
		id: "list.movePrev",
		title: "Move up",
		keywords: ["up", "previous", "move"],
	},
	{
		id: "list.moveNext",
		title: "Move down",
		keywords: ["down", "next", "move"],
	},
	{ id: "list.moveLeft", title: "Move left", keywords: ["left", "move"] },
	{ id: "list.moveRight", title: "Move right", keywords: ["right", "move"] },
	{
		id: "list.openOrSelect",
		title: "Open or select",
		keywords: ["open", "select", "enter"],
	},
	{
		id: "list.expandOrToggle",
		title: "Expand or toggle",
		keywords: ["expand", "toggle", "space"],
	},
	{
		id: "list.moveFirst",
		title: "Move to first",
		keywords: ["first", "home", "move"],
	},
	{
		id: "list.moveLast",
		title: "Move to last",
		keywords: ["last", "end", "move"],
	},
];

/** What the board does when the cursor rests on a given stop, resolved from the
 *  board data as the stop list is built (T4). A card stop carries its
 *  select/cross-link/open-agent targets; a gutter stop only opens its agent.
 *  Positional a11y strings are derived here too (design §219-223: no ARIA grid
 *  role — the container is a "kanban board" and each stop is self-describing). */
type StopAction =
	| { kind: "gutter"; agentId: string; ariaLabel: string }
	| {
			kind: "card";
			issueId: string;
			title: string;
			assignee: string | null;
			crossLink?: () => void;
			ariaLabel: string;
			ariaDescription: string;
	  };

function setCardShortcuts(
	el: HTMLElement,
	action: StopAction,
	isCursor: boolean,
	spaceShortcut: string | undefined,
	refShortcut: string | undefined,
	rules: readonly RefLinkRule[],
): void {
	if (action.kind !== "card" || !isCursor) {
		el.removeAttribute("aria-keyshortcuts");
		return;
	}
	const shortcuts: string[] = [];
	if (action.crossLink && spaceShortcut) shortcuts.push(spaceShortcut);
	if (hasLinkedRef(action.title, rules) && refShortcut) {
		shortcuts.push(refShortcut);
	}
	if (shortcuts.length > 0) {
		el.setAttribute("aria-keyshortcuts", shortcuts.join(" "));
	} else {
		el.removeAttribute("aria-keyshortcuts");
	}
}

const CARD_REF_EDGES: Partial<Record<string, "first" | "last">> = {
	"board.focusCardRef": "first",
	"board.focusCardRefPrev": "last",
};

/** One PRs-board card — mirrors the IssueCard anatomy (IssueCard.tsx:49-115)
 *  with the PR's own facts. The card body selects the owning issue (staying on
 *  the PRs tab); a `.card-issue-link` chip in the card top selects AND flips to
 *  the Issues tab. Badges are `compact` (glyph-only): the board card is the same
 *  cramped gutter as the issue card, and the design's reference render shows
 *  glyph-only PR badges — deliberate, not an accident. */
const PrCard: Component<{
	row: PrRow;
	multiForge: boolean;
	selected: boolean;
	onSelect: () => void;
	onOpenIssue: () => void;
	/** Board wiring (T4): when the card is a stop in the roving group, its
	 *  issue-link chip drops to `tabindex={-1}` (the board owns keyboard nav;
	 *  the cross-link moves to `board.openCardCrossLink`, Space). */
	inRovingGroup?: boolean;
	/** The board collects each stop's element for the roving group; the
	 *  positional a11y attributes are applied centrally from the stop list. */
	cardRef?: (el: HTMLButtonElement) => void;
}> = (props) => {
	const store = useStore();
	const pr = () => props.row.pr;
	const resolved = () => pr().threads.filter((t) => t.resolved).length;
	const coord = () =>
		`${props.multiForge ? `${pr().forge.host}/` : ""}${pr().repo}#${pr().number}`;
	// Mirror IssueCard.tsx:44-48: the assignee is a trusted Compass account id, so
	// agentView resolves it; a miss surfaces the raw id rather than a fake handle.
	const assignee = () => {
		const id = props.row.issue.assignee;
		if (!id) return undefined;
		return store.agentView(id)?.account.handle ?? id;
	};
	return (
		<button
			type="button"
			class="cx-card"
			data-selected={props.selected ? "" : undefined}
			onClick={props.onSelect}
			ref={(el) => props.cardRef?.(el)}
		>
			<span class="card-top">
				<span class="card-issue">{coord()}</span>
				<span class="card-pr">
					<Show when={ciBadge(pr())}>
						{(status) => <BadgeGlyph axis="ci" status={status()} compact />}
					</Show>
					<Show when={reviewBadge(pr())}>
						{(verdict) => (
							<BadgeGlyph axis="review" status={verdict()} compact />
						)}
					</Show>
					<Show when={queueChip(pr())}>
						{(s) => (
							<span class="card-queue" data-state={s()}>
								{s() === "failed" ? "queue failed" : "queue removed"}
							</span>
						)}
					</Show>
				</span>
				{/* biome-ignore lint/a11y/useSemanticElements: an <a> needs an href; this is an
				   in-app selection chip, and it already lives inside the card <button>, so a
				   nested link/button is disallowed — role="link" + keyboard is the compromise.
				   biome-ignore lint/a11y/useFocusableInteractive: Solid 2's JSX types only admit
				   the lowercase `tabindex` prop (camelCase `tabIndex` is a tsc error under
				   @solidjs/web), which this rule does not recognize as conferring focusability —
				   but the element IS keyboard-focusable at runtime via the lowercase prop below. */}
				<span
					class="card-issue-link"
					role="link"
					tabindex={props.inRovingGroup ? -1 : 0}
					onClick={(e) => {
						e.stopPropagation();
						props.onOpenIssue();
					}}
					onKeyDown={(e) => {
						if (e.key !== "Enter" && e.key !== " ") return;
						e.preventDefault();
						e.stopPropagation();
						props.onOpenIssue();
					}}
				>
					{issueKey(props.row.issue, props.multiForge)}
				</span>
			</span>
			<span class="card-title">
				<RefText
					text={pr().title}
					inControl
					inRovingGroup={props.inRovingGroup}
				/>
			</span>
			<span class="card-foot">
				<span class="card-author">
					{assignee() ? `@${assignee()}` : "unassigned"}
				</span>
				<span class="card-threads">
					{resolved()}/{pr().threads.length} threads
				</span>
			</span>
		</button>
	);
};

/** The Bridge: the full kanban board, swimlane-by-agent by default. Columns are
 *  the issue lifecycle states; each agent is a row; a cell holds that
 *  agent's cards in that state. Clicking an agent gutter opens the agent view. */
export const Bridge: Component = () => {
	const store = useStore();
	const view = useView();
	const [mode, setMode] = createSignal<BoardMode>("swimlane");
	const refLinkRules = useRefLinkRules();
	// The active artifact tab — a Bridge-local view axis, peer to `mode`.
	const [tab, setTab] = createSignal<BoardTab>("issues");
	// Backlog and Done are routed segments (DL-438); Issues/PRs stay local.
	const segment = (): BoardTab | "backlog" | "done" => {
		const routed = view.route().view;
		return routed === "backlog" || routed === "done" ? routed : tab();
	};
	const onGrid = () => segment() === "issues" || segment() === "prs";
	const showGrid = (next: BoardTab): void => {
		view.navigate("/");
		setTab(next);
	};

	// Subtree scoping is not wired; rows and counts currently use the full set.
	const scope = (): ReadonlySet<string> | undefined => undefined;
	const multiForge = () => isMultiForge(store.issues());
	const prGroups = () => {
		const groups = prBoardGroups(store.agents(), store.issues());
		const active = scope();
		if (!active) return groups;
		return groups.filter(
			(g) => g.agent !== null && active.has(g.agent.account.id),
		);
	};

	// The board reads the store's reactive fleet and issue list (design "one
	// source of truth") through the pure board.ts partition, so a promote/archive
	// or a roster change shows here immediately.
	const boardAgents = () => boardAgentsOf(store.agents(), store.issues());
	const cellItems = (agentId: string | null, state: IssueState) =>
		cellItemsOf(store.issues(), agentId, state);
	const laneTotal = (state: IssueState) => laneTotalOf(store.issues(), state);
	const inFlight = () =>
		activeIssues(store.issues()).filter((w) => w.state !== "done").length;
	const agentItemCount = (agentId: string) =>
		store.issues().filter((w) => w.assignee === agentId).length;
	// The board clock for the advancing card. It re-reads at the next card's
	// expiry, so the chase-light ends on time with no polling timer.
	const [nowMs, setNowMs] = createSignal(Date.now());
	createEffect(
		() => nextAdvancingExpiry(store.issues(), nowMs()),
		(expiry) => {
			if (expiry === null) return;
			const timer = window.setTimeout(
				() => setNowMs(Date.now()),
				Math.max(0, expiry - Date.now()),
			);
			return () => window.clearTimeout(timer);
		},
	);
	createEffect(
		() => store.issues(),
		() => {
			setNowMs(Date.now());
		},
		{ defer: true },
	);
	const advancing = (issue: Issue) => isAdvancing(issue, nowMs());
	// Bridge's aria-label overrides the card's content, so the advancing cue for
	// screen readers goes in the label.
	const cardLabel = (issue: Issue) =>
		`Issue ${issueKey(issue, multiForge())}${advancing(issue) ? ", advancing" : ""}`;

	// Grid columns: the agent gutter (only in swimlane mode) + one per lane.
	const gridColumns = () =>
		mode() === "swimlane"
			? `180px repeat(${BOARD_LANES.length}, minmax(210px, 1fr))`
			: `repeat(${BOARD_LANES.length}, minmax(210px, 1fr))`;

	// Derive stops and actions from the same board snapshot.
	const navInput = createMemo<BoardNavInput>(() => {
		if (tab() === "prs") {
			return {
				tab: "prs",
				laneCount: PR_LANES.length,
				groups: prGroups().map((g) => ({
					agentId: g.agent?.account.id ?? null,
					rows: g.rows.map((r) => ({
						issueId: r.issue.id,
						repo: r.pr.repo,
						prNumber: r.pr.number,
						col: PR_LANES.findIndex((l) => l.state === prLifecycle(r.pr)),
					})),
				})),
			};
		}
		if (mode() === "swimlane") {
			return {
				tab: "issues",
				mode: "swimlane",
				laneCount: BOARD_LANES.length,
				agents: boardAgents().map((a) => ({ id: a.account.id })),
				cells: (agentId, col) =>
					cellItems(agentId, BOARD_LANES[col].state).map((i) => ({ id: i.id })),
			};
		}
		return {
			tab: "issues",
			mode: "status",
			laneCount: BOARD_LANES.length,
			cells: (col) =>
				cellItems(null, BOARD_LANES[col].state).map((i) => ({ id: i.id })),
		};
	});
	const stops = createMemo<BoardStop[]>(() => boardStops(navInput()));

	// The per-stop action + a11y map, keyed by stop id, rebuilt with the stops.
	// Positional strings name the stop's place on the board so the grid is
	// self-describing WITHOUT an ARIA grid role (design §219-223).
	const actions = createMemo<Map<string, StopAction>>(() => {
		const map = new Map<string, StopAction>();
		const laneLabel = (col: number): string =>
			tab() === "prs" ? PR_LANES[col].label : BOARD_LANES[col].label;
		const handleFor = (id: string): string | undefined =>
			store.agentView(id)?.account.handle ?? id;
		for (const stop of stops()) {
			if (stop.kind === "gutter") {
				const agentId = stop.id.slice("gutter:".length);
				map.set(stop.id, {
					kind: "gutter",
					agentId,
					ariaLabel: `${handleFor(agentId)} lane`,
				});
				continue;
			}
			if (tab() === "prs") {
				const row = prGroups()
					.flatMap((g) => g.rows)
					.find(
						(r) => prStopId(r.issue.id, r.pr.repo, r.pr.number) === stop.id,
					);
				if (!row) continue;
				map.set(stop.id, {
					kind: "card",
					issueId: row.issue.id,
					title: row.pr.title,
					assignee: row.issue.assignee,
					crossLink: () => {
						store.selectIssue(row.issue.id);
						setTab("issues");
					},
					ariaLabel: `PR ${row.pr.repo}#${row.pr.number}`,
					ariaDescription: `${laneLabel(stop.col)} column, row ${stop.row + 1}${
						queueChip(row.pr) ? `, queue ${queueChip(row.pr)}` : ""
					}`,
				});
				continue;
			}
			const issue = store.issues().find((w) => w.id === stop.id);
			if (!issue) continue;
			const openPr = primaryPr(issue)
				? () => {
						store.selectIssue(issue.id);
						setTab("prs");
					}
				: undefined;
			map.set(stop.id, {
				kind: "card",
				issueId: issue.id,
				title: issue.title,
				assignee: issue.assignee,
				crossLink: openPr,
				ariaLabel: cardLabel(issue),
				ariaDescription: `${laneLabel(stop.col)} column, row ${stop.row + 1}`,
			});
		}
		return map;
	});

	// Seed the cursor to the selected card if it is a stop, else the first stop —
	// the tab's "selected/first" landing (design §211, §248). Used at init and on
	// a tab switch, which rebuilds the whole stop set with fresh ids.
	const seedCursor = (list: BoardStop[]): string | null => {
		const selected = store.selectedIssueId();
		if (selected && list.some((s) => s.id === selected)) return selected;
		return list[0]?.id ?? null;
	};
	const [cursorId, setCursorId] = createSignal<string | null>(
		seedCursor(stops()),
	);
	let prevStop: BoardStop | null =
		stops().find((s) => s.id === cursorId()) ?? null;
	let prevTab = tab();
	// Tab changes reset the cursor; in-tab changes preserve surviving stops.
	// Defer rebuilding because the signal already has its seeded cursor.
	createEffect(
		() => stops(),
		(list: BoardStop[]) => {
			const curTab = tab();
			const next =
				curTab !== prevTab ? seedCursor(list) : resolveCursor(list, prevStop);
			prevTab = curTab;
			setCursorId(next);
			prevStop = list.find((s) => s.id === next) ?? null;
		},
		{ defer: true },
	);
	// Keep `prevStop` tracking the live cursor so a rebuild recovers from where
	// the cursor actually is (a move updates the signal, not `prevStop`).
	createEffect(
		() => cursorId(),
		(id: string | null) => {
			prevStop = stops().find((s) => s.id === id) ?? prevStop;
		},
	);

	// Map mutation is untracked, so bump elsVersion after refs fill.
	const els = new Map<string, HTMLElement>();
	const [elsVersion, bumpElsVersion] = createSignal(0, { equals: false });
	const setStopEl = (id: string) => (el: HTMLElement | undefined) => {
		if (el) els.set(id, el);
		else els.delete(id);
		bumpElsVersion(0);
	};
	const rovingStops = (): Stop[] => {
		elsVersion();
		return stops().flatMap((s) => {
			const el = els.get(s.id);
			return el ? [{ id: s.id, el }] : [];
		});
	};
	const focusedStop = (): Stop | undefined =>
		rovingStops().find((stop) => stop.el === document.activeElement);
	const enterRefs = (edge: "first" | "last"): boolean => {
		const stop =
			focusedStop() ??
			rovingStops().find((candidate) => candidate.id === cursorId());
		if (!stop) return true;
		const action = actions().get(stop.id);
		if (action?.kind !== "card") return true;
		const refs = cardRefs(stop.el);
		(edge === "first" ? refs[0] : refs.at(-1))?.focus();
		return true;
	};

	// The single chord→command→direction table (design §481-483). The dispatcher
	// routes a group-relative id here; we return true when the board claims it
	// (the dispatcher then suppresses native activation), false to fall through.
	const DIRECTIONS: Partial<Record<string, BoardDirection>> = {
		"list.movePrev": "up",
		"list.moveNext": "down",
		"list.moveLeft": "left",
		"list.moveRight": "right",
		"list.moveFirst": "home",
		"list.moveLast": "end",
	};
	const openCrossLink = (): boolean => {
		const cur = cursorId();
		const action = cur ? actions().get(cur) : undefined;
		// A chip-less card (no cross-link) STILL claims the chord — no scroll, no
		// fall-through (design §505-507). A gutter has no cross-link either.
		if (action?.kind === "card") action.crossLink?.();
		return true;
	};
	const onCommand = (id: CommandId): boolean => {
		const dir = DIRECTIONS[id];
		if (dir) {
			const cur = cursorId();
			if (!cur) return true;
			const next = moveCursor(stops(), cur, dir);
			if (next) setCursorId(next);
			return true;
		}
		const refEdge = CARD_REF_EDGES[id];
		if (refEdge) return enterRefs(refEdge);
		if (id === "list.openOrSelect") {
			const cur = cursorId();
			const action = cur ? actions().get(cur) : undefined;
			if (!action) return true;
			if (action.kind === "gutter") store.openAgent(action.agentId);
			else store.selectIssue(action.issueId);
			return true;
		}
		if (id === "board.openAssignedAgent") {
			const cur = cursorId();
			const action = cur ? actions().get(cur) : undefined;
			if (!action) return true;
			if (action.kind === "gutter") store.openAgent(action.agentId);
			else if (action.assignee) store.openAgent(action.assignee);
			else store.selectIssue(action.issueId); // no-assignee falls back to select
			return true;
		}
		// Space maps to the Lists-block expand command for cross-links.
		if (id === "board.openCardCrossLink" || id === "list.expandOrToggle") {
			return openCrossLink();
		}
		return false;
	};

	// Register board commands and publish its roving group on the shared keyboard spine.
	// Cleanup retracts both to prevent dispatch after unmount.
	const registry = store.keyboard.registry;
	const rovingGroup = createRovingGroup({
		group: { zone: "main", id: "bridge-board" },
		stops: rovingStops,
		cursor: cursorId,
		setCursor: setCursorId,
		onCommand,
	});
	const focusedRef = (): { ref: HTMLElement; owner: Stop } | undefined => {
		const active = document.activeElement;
		if (
			!(active instanceof HTMLElement) ||
			!active.matches(CARD_REF_SELECTOR)
		) {
			return undefined;
		}
		const owner = rovingStops().find((stop) => stop.el.contains(active));
		return owner ? { ref: active, owner } : undefined;
	};
	const stepRef = (
		at: { ref: HTMLElement; owner: Stop },
		delta: 1 | -1,
	): void => {
		const refs = cardRefs(at.owner.el);
		const index = refs.indexOf(at.ref);
		refs[(index + delta + refs.length) % refs.length]?.focus();
	};
	const exitRef = (owner: Stop, dir?: BoardDirection): void => {
		const target = dir
			? (moveCursor(stops(), owner.id, dir) ?? owner.id)
			: owner.id;
		setCursorId(target);
		(els.get(target) ?? owner.el).focus();
	};
	const onRefCommand = (id: CommandId): boolean => {
		const active = focusedRef();
		if (!active) return false;
		if (id === "board.focusCardRef") {
			stepRef(active, 1);
			return true;
		}
		if (id === "board.focusCardRefPrev") {
			stepRef(active, -1);
			return true;
		}
		if (id === "board.exitCardRef") {
			exitRef(active.owner);
			return true;
		}
		const dir = DIRECTIONS[id];
		if (dir) {
			exitRef(active.owner, dir);
			return true;
		}
		return false;
	};
	// A ref is not a stop, so this peer handle claims keys only while a title ref has focus.
	const refGroup: RovingGroupHandle = {
		group: { zone: "main", id: "bridge-card-refs" },
		isFocused: () => focusedRef() !== undefined,
		handleCommand: onRefCommand,
		focus: () => rovingGroup.focus(),
	};
	// Several tabs can mount a Bridge, so only the focused view's Bridge holds the
	// shared board/list command ids; a cleanup removes only the entry it added.
	const commands: Command[] = [
		{
			id: "board.openAssignedAgent" as CommandId,
			title: "Open assigned agent",
			keywords: ["agent", "workspace", "open"],
			scope: "main",
			run: () => onCommand("board.openAssignedAgent" as CommandId),
		},
		{
			id: "board.openCardCrossLink" as CommandId,
			title: "Open card cross-link",
			keywords: ["pr", "issue", "cross-link"],
			scope: "main",
			run: () => onCommand("board.openCardCrossLink" as CommandId),
		},
		{
			id: "board.focusCardRef" as CommandId,
			title: "Focus next link in card title",
			keywords: ["ref", "link", "title", "issue", "pr"],
			scope: "main",
			run: () =>
				onRefCommand("board.focusCardRef" as CommandId) ||
				onCommand("board.focusCardRef" as CommandId),
		},
		{
			id: "board.focusCardRefPrev" as CommandId,
			title: "Focus previous link in card title",
			keywords: ["ref", "link", "title", "issue", "pr"],
			scope: "main",
			run: () =>
				onRefCommand("board.focusCardRefPrev" as CommandId) ||
				onCommand("board.focusCardRefPrev" as CommandId),
		},
		{
			id: "board.exitCardRef" as CommandId,
			title: "Return to card",
			keywords: ["escape", "card", "return", "link"],
			scope: "main",
			run: () =>
				onRefCommand("board.exitCardRef" as CommandId) ||
				onCommand("board.exitCardRef" as CommandId),
		},
		...LIST_COMMANDS.map((spec) => ({
			id: spec.id as CommandId,
			title: spec.title,
			keywords: spec.keywords,
			scope: "main" as const,
			run: () => onCommand(spec.id as CommandId),
		})),
	];
	createEffect(
		() => store.focusedView().id === view.id && onGrid(),
		(active) => {
			if (!active) return;
			// Take the ids over from the previous owner, whose cleanup may run
			// after this; its identity check then leaves these entries alone.
			for (const cmd of commands) {
				registry.unregister(cmd.id);
				registry.register(cmd);
			}
			return () => {
				for (const cmd of commands) {
					if (registry.get(cmd.id) === cmd) registry.unregister(cmd.id);
				}
			};
		},
	);
	// The board group exists only on the grid segments; the lists are not a board.
	createEffect(onGrid, (on) => {
		if (!on) return;
		store.keyboard.registerGroup(rovingGroup);
		store.keyboard.registerGroup(refGroup);
		return () => {
			store.keyboard.unregisterGroup(refGroup);
			store.keyboard.unregisterGroup(rovingGroup);
		};
	});

	// Apply positional labels and announce only shortcuts with a target here.
	const platform = detectPlatform();
	const spaceShortcut = shortcutForAria(
		"list.expandOrToggle" as CommandId,
		platform,
	);
	const refShortcut = shortcutForAria(
		"board.focusCardRef" as CommandId,
		platform,
	);
	createEffect(
		() => [stops(), cursorId(), actions(), refLinkRules()] as const,
		([list, cursor, actionMap, rules]: readonly [
			BoardStop[],
			string | null,
			Map<string, StopAction>,
			readonly RefLinkRule[],
		]) => {
			for (const stop of list) {
				const el = els.get(stop.id);
				const action = actionMap.get(stop.id);
				if (!el || !action) continue;
				el.setAttribute("aria-label", action.ariaLabel);
				if (action.kind === "card") {
					el.setAttribute("aria-description", action.ariaDescription);
				}
				setCardShortcuts(
					el,
					action,
					stop.id === cursor,
					spaceShortcut,
					refShortcut,
					rules,
				);
			}
		},
	);
	return (
		<div class="bridge">
			<div class="bridge-toolbar">
				<span class="heading">Bridge</span>
				<div class="seg bridge-tabs" role="toolbar" aria-label="Board view">
					<button
						type="button"
						class={{ active: segment() === "issues" }}
						onClick={() => showGrid("issues")}
					>
						Issues
					</button>
					<button
						type="button"
						class={{ active: segment() === "prs" }}
						onClick={() => showGrid("prs")}
					>
						PRs · {prCount(store.issues(), scope())}
					</button>
					<button
						type="button"
						class={{ active: segment() === "backlog" }}
						onClick={() => view.navigate("/backlog")}
					>
						Backlog ·{" "}
						{backlogIssues(store.issues()).length +
							store.assignedIssues().length}
					</button>
					<button
						type="button"
						class={{ active: segment() === "done" }}
						onClick={() => view.navigate("/done")}
					>
						Done
					</button>
				</div>
				{/* After the tabs, so the subtitle alone gives way to the grouping seg. */}
				<span class="sub">
					{boardAgents().length} agents · {inFlight()} in-flight issues
				</span>
				<Show when={segment() === "issues"}>
					<div class="seg" role="toolbar" aria-label="Board grouping">
						<button
							type="button"
							class={{ active: mode() === "swimlane" }}
							onClick={() => setMode("swimlane")}
						>
							Swimlanes
						</button>
						<button
							type="button"
							class={{ active: mode() === "status" }}
							onClick={() => setMode("status")}
						>
							Status
						</button>
					</div>
				</Show>
			</div>

			<Show when={segment() === "backlog"}>
				<BacklogList />
			</Show>
			<Show when={segment() === "done"}>
				<DoneList />
			</Show>

			<Show when={segment() === "issues"}>
				<Show
					when={stops().length > 0}
					fallback={
						<div class="bridge-empty">
							No issues on the board yet — promote work from the Backlog to see
							it here.
						</div>
					}
				>
					{/* biome-ignore lint/a11y/useSemanticElements: the board is a labeled
				   "kanban board" group (design §219-221), NOT a <fieldset> form group —
				   the roving-tabindex keyboard model carries the grid semantics and the
				   ARIA grid/row/gridcell roles are deliberately refused (DL-220). */}
					<div
						class="bridge-grid"
						style={{ "grid-template-columns": gridColumns() }}
						data-tour="board-grid"
						role="group"
						aria-roledescription="kanban board"
						aria-label="Board grid"
					>
						{/* Header row */}
						<Show when={mode() === "swimlane"}>
							<div class="bridge-corner">Agent</div>
						</Show>
						<For each={BOARD_LANES}>
							{(lane) => (
								<div
									class="bridge-col-head"
									style={{ "--lane-tint": lane.color }}
								>
									{lane.label}
									<span class="lane-count">{laneTotal(lane.state)}</span>
								</div>
							)}
						</For>

						{/* Body */}
						<Show
							when={mode() === "swimlane"}
							fallback={
								<For each={BOARD_LANES}>
									{(lane) => (
										<div class="bridge-cell">
											<For each={cellItems(null, lane.state)}>
												{(ws) => (
													<IssueCard
														issue={ws}
														inRovingGroup
														advancing={advancing(ws)}
														cardRef={setStopEl(ws.id)}
														onOpenPr={() => {
															store.selectIssue(ws.id);
															setTab("prs");
														}}
													/>
												)}
											</For>
										</div>
									)}
								</For>
							}
						>
							<For each={boardAgents()}>
								{(agent) => (
									<>
										<button
											type="button"
											class="bridge-lane"
											ref={setStopEl(gutterId(agent.account.id))}
											{...openLink(
												store,
												() => `/agent/${agent.account.id}`,
												() => store.openAgent(agent.account.id),
											)}
										>
											<StateDot state={agent.lifecycle ?? "idle"} />
											<Show when={agent.runtime}>
												{(m) => <RuntimeMarker marker={m()} />}
											</Show>
											<span>
												<span class="g-name">{agent.account.handle}</span>
												<br />
												<span class="g-meta">
													{agentItemCount(agent.account.id)} items
												</span>
											</span>
											<span class="g-open" aria-hidden="true">
												→
											</span>
										</button>
										<For each={BOARD_LANES}>
											{(lane) => {
												const items = cellItems(agent.account.id, lane.state);
												return (
													<div
														class={["bridge-cell", { dim: items.length === 0 }]}
													>
														<For each={items}>
															{(ws) => (
																<IssueCard
																	issue={ws}
																	inRovingGroup
																	advancing={advancing(ws)}
																	cardRef={setStopEl(ws.id)}
																	onOpenPr={() => {
																		store.selectIssue(ws.id);
																		setTab("prs");
																	}}
																/>
															)}
														</For>
													</div>
												);
											}}
										</For>
									</>
								)}
							</For>
						</Show>
					</div>
				</Show>
			</Show>

			<Show when={segment() === "prs"}>
				<Show
					when={stops().length > 0}
					fallback={
						<div class="bridge-empty">
							No open PRs yet — cards appear here when an agent opens one.
						</div>
					}
				>
					{/* biome-ignore lint/a11y/useSemanticElements: same "kanban board" group
				   as the Issues grid — a labeled ARIA group, not a <fieldset> (DL-220). */}
					<div
						class="bridge-grid"
						style={{
							"grid-template-columns": `180px repeat(${PR_LANES.length}, minmax(210px, 1fr))`,
						}}
						role="group"
						aria-roledescription="kanban board"
						aria-label="Board grid"
					>
						<div class="bridge-corner">Agent</div>
						<For each={PR_LANES}>
							{(lane) => (
								<div
									class="bridge-col-head"
									style={{ "--lane-tint": lane.color }}
								>
									{lane.label}
								</div>
							)}
						</For>
						<For each={prGroups()}>
							{(group) => (
								<>
									<Show
										when={group.agent}
										fallback={
											<div class="bridge-lane unassigned">
												<span class="g-name">Unassigned</span>
											</div>
										}
									>
										{(agent) => (
											<button
												type="button"
												class="bridge-lane"
												ref={setStopEl(gutterId(agent().account.id))}
												{...openLink(
													store,
													() => `/agent/${agent().account.id}`,
													() => store.openAgent(agent().account.id),
												)}
											>
												<StateDot state={agent().lifecycle ?? "idle"} />
												<Show when={agent().runtime}>
													{(m) => <RuntimeMarker marker={m()} />}
												</Show>
												<span class="g-name">{agent().account.handle}</span>
												<span class="g-open" aria-hidden="true">
													→
												</span>
											</button>
										)}
									</Show>
									<For each={PR_LANES}>
										{(lane) => {
											const cards = group.rows.filter(
												(r) => prLifecycle(r.pr) === lane.state,
											);
											return (
												<div
													class={["bridge-cell", { dim: cards.length === 0 }]}
												>
													<For each={cards}>
														{(row) => (
															<PrCard
																row={row}
																multiForge={multiForge()}
																inRovingGroup
																cardRef={setStopEl(
																	prStopId(
																		row.issue.id,
																		row.pr.repo,
																		row.pr.number,
																	),
																)}
																selected={
																	row.issue.id === store.selectedIssueId()
																}
																onSelect={() => store.selectIssue(row.issue.id)}
																onOpenIssue={() => {
																	store.selectIssue(row.issue.id);
																	setTab("issues");
																}}
															/>
														)}
													</For>
												</div>
											);
										}}
									</For>
								</>
							)}
						</For>
					</div>
				</Show>
			</Show>
		</div>
	);
};
