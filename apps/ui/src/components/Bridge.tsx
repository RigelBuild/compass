import {
	type Component,
	createEffect,
	createMemo,
	createSignal,
	For,
	on,
	onCleanup,
	Show,
} from "solid-js";
import {
	activeIssues,
	boardAgents as boardAgentsOf,
	cellItems as cellItemsOf,
	laneTotal as laneTotalOf,
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
	reviewBadge,
} from "../board-render";
import { BOARD_LANES, PR_LANES } from "../constants";
import { useStore } from "../context";
import type { CommandId, CommandRegistry } from "../keyboard/commands";
import { installKeymap } from "../keyboard/dispatch";
import { createCommandRegistry } from "../keyboard/registry";
import { createRovingGroup, type Stop } from "../keyboard/roving";
import type { IssueState } from "../stub-data";
import { BadgeGlyph } from "./BadgeGlyph";
import { IssueCard } from "./IssueCard";
import { StateDot } from "./StateDot";

/** How the board groups rows. Swimlane = one row per agent (default); status =
 *  a plain column board with no agent rows. */
type BoardMode = "swimlane" | "status";

/** Which artifact kind the board shows — a peer tab axis, orthogonal to
 *  `BoardMode` (which is how ISSUES group). Bridge-local, like `BoardMode`. */
type BoardTab = "issues" | "prs";

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
			assignee: string | null;
			crossLink?: () => void;
			ariaLabel: string;
			ariaDescription: string;
	  };

/** One PRs-board card — mirrors the IssueCard anatomy (IssueCard.tsx:49-115)
 *  with the PR's own facts. The card body selects the owning issue (staying on
 *  the PRs tab); a `.card-issue-link` chip in the card top selects AND flips to
 *  the Issues tab. Badges are `compact` (glyph-only): the board card is the same
 *  cramped gutter as the issue card, and the frozen reference render shows
 *  glyph-only PR badges — deliberate, not an accident. */
const PrCard: Component<{
	row: PrRow;
	multiForge: boolean;
	selected: boolean;
	onSelect: () => void;
	onOpenIssue: () => void;
	/** Board wiring (T4): when the card is a stop in the roving group, its
	 *  issue-link chip drops to `tabIndex={-1}` (the board owns keyboard nav;
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
				</span>
				{/* biome-ignore lint/a11y/useSemanticElements: an <a> needs an href; this is an
				   in-app selection chip, and it already lives inside the card <button>, so a
				   nested link/button is disallowed — role="link" + keyboard is the compromise. */}
				<span
					class="card-issue-link"
					role="link"
					tabIndex={props.inRovingGroup ? -1 : 0}
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
			<span class="card-title">{pr().title}</span>
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
export const Bridge: Component<{
	/** Board wiring (T4): the command registry the board installs its keymap
	 *  against. Group-relative `board.*`/`list.*` chords resolve through the
	 *  active roving group (tier 1) regardless of the registry; the registry
	 *  backs the palette + the scoped/global tiers. Injectable so a test can seed
	 *  a competing `comms.*` binding and prove the board's tier-1 claim wins.
	 *  Defaults to a fresh registry (the standalone board window, design §430). */
	registry?: CommandRegistry;
}> = (props) => {
	const store = useStore();
	const [mode, setMode] = createSignal<BoardMode>("swimlane");
	// The active artifact tab — a Bridge-local view axis, peer to `mode`.
	const [tab, setTab] = createSignal<BoardTab>("issues");

	// SEAM (subtree-scope): Record C's subtree filter has no store accessor yet,
	// so the board is always unscoped here — `scope()` is `undefined`, and both
	// the PRs-tab rows and the tab-badge count read the full set. When C wires a
	// `subtreeAgentIds` accessor into the store, feed it through this one seam and
	// the count + row filter track it together.
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

	// Grid columns: the agent gutter (only in swimlane mode) + one per lane.
	const gridColumns = () =>
		mode() === "swimlane"
			? `180px repeat(${BOARD_LANES.length}, minmax(210px, 1fr))`
			: `repeat(${BOARD_LANES.length}, minmax(210px, 1fr))`;

	// ── Keyboard: the whole board is ONE roving group (T4, DL-220) ────────────
	// The board derives a pure `BoardNavInput` from tab()/mode() (T3 owns the 2-D
	// cursor math). `stops()` is the ordered stop list; alongside it we build a
	// per-stop action map (select/open/cross-link + positional a11y), resolved
	// from the same board data so the dispatcher stays a pure id→behavior table.
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
					assignee: row.issue.assignee,
					crossLink: () => {
						store.selectIssue(row.issue.id);
						setTab("issues");
					},
					ariaLabel: `PR ${row.pr.repo}#${row.pr.number}`,
					ariaDescription: `${laneLabel(stop.col)} column, row ${stop.row + 1}`,
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
				assignee: issue.assignee,
				crossLink: openPr,
				ariaLabel: `Issue ${issueKey(issue, multiForge())}`,
				ariaDescription: `${laneLabel(stop.col)} column, row ${stop.row + 1}`,
			});
		}
		return map;
	});

	// The cursor: a card/gutter stop id. Seeded to the selected card if it is a
	// stop, else the first stop. Rebuilt when the stop set changes (tab/mode
	// switch): a surviving cursor id stays put, a vanished one recovers via T3's
	// `resolveCursor`. `on` with an explicit dep keeps this a static-dep effect
	// (no read-after-set — the recompute reads `prevStop`, a plain closure var).
	const [cursorId, setCursorId] = createSignal<string | null>(
		(() => {
			const list = stops();
			const selected = store.selectedIssueId();
			if (selected && list.some((s) => s.id === selected)) return selected;
			return list[0]?.id ?? null;
		})(),
	);
	let prevStop: BoardStop | null =
		stops().find((s) => s.id === cursorId()) ?? null;
	createEffect(
		on(
			stops,
			(list) => {
				const next = resolveCursor(list, prevStop);
				setCursorId(next);
				prevStop = list.find((s) => s.id === next) ?? null;
			},
			{ defer: true },
		),
	);
	// Keep `prevStop` tracking the live cursor so a rebuild recovers from where
	// the cursor actually is (a move updates the signal, not `prevStop`).
	createEffect(
		on(cursorId, (id) => {
			prevStop = stops().find((s) => s.id === id) ?? prevStop;
		}),
	);

	// The board's element refs, keyed by stop id — collected as each card/gutter
	// button renders. The roving group pairs a live stop id with its element.
	const els = new Map<string, HTMLElement>();
	const setStopEl = (id: string) => (el: HTMLElement | undefined) => {
		if (el) els.set(id, el);
		else els.delete(id);
	};
	const rovingStops = (): Stop[] =>
		stops().flatMap((s) => {
			const el = els.get(s.id);
			return el ? [{ id: s.id, el }] : [];
		});

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
		// Space: the board's cross-link rides the Lists-block `list.expandOrToggle`
		// (keymap:84) — the dispatcher's first group-relative match on Space, and
		// the board has no expand/toggle of its own. `board.openCardCrossLink` has
		// no keymap row (it would be dead behind list.expandOrToggle); it stays a
		// registered command (palette + a future Space remap, OQ-2), so map both.
		if (id === "board.openCardCrossLink" || id === "list.expandOrToggle") {
			return openCrossLink();
		}
		return false;
	};

	// Install the keymap once (component scope) against the injected/own registry
	// and this board as the sole active group. Register the two board commands so
	// the palette can surface them; their keyboard path is `onCommand` above (the
	// dispatcher routes group-relative ids to the group, not `registry.get`), so
	// `run` mirrors the group behavior for the palette's sake.
	const registry = props.registry ?? createCommandRegistry();
	const rovingGroup = createRovingGroup({
		group: { zone: "main", id: "bridge-board" },
		stops: rovingStops,
		cursor: cursorId,
		setCursor: setCursorId,
		onCommand,
	});
	registry.register({
		id: "board.openAssignedAgent" as CommandId,
		title: "Open assigned agent",
		keywords: ["agent", "workspace", "open"],
		scope: "main",
		run: () => onCommand("board.openAssignedAgent" as CommandId),
	});
	registry.register({
		id: "board.openCardCrossLink" as CommandId,
		title: "Open card cross-link",
		keywords: ["pr", "issue", "cross-link"],
		scope: "main",
		run: () => onCommand("board.openCardCrossLink" as CommandId),
	});
	// The board IS the main-view focus zone (design §236), so declare it: the
	// scoped tier (tier 2) can then resolve `when:"main"` entries — which is
	// exactly what the board's tier-1 claim of `board.openAssignedAgent` must
	// beat over the frozen `Shift+Enter → comms.newline {when:"main"}` entry.
	const uninstall = installKeymap(
		registry,
		() => rovingGroup,
		() => "main",
	);
	onCleanup(uninstall);

	// Apply the positional a11y strings to each stop element, and name the Space
	// cross-link on the cursor card (design §219-223, §491). Static-dep effect
	// over (stops, cursor, actions) — the roving group owns tabindex/focus; this
	// owns only the descriptive attributes, kept off IssueCard's prop surface.
	createEffect(
		on([stops, cursorId, actions], ([list, cursor, actionMap]) => {
			for (const stop of list) {
				const el = els.get(stop.id);
				const action = actionMap.get(stop.id);
				if (!el || !action) continue;
				el.setAttribute("aria-label", action.ariaLabel);
				if (action.kind === "card") {
					el.setAttribute("aria-description", action.ariaDescription);
				}
				if (stop.id === cursor && action.kind === "card") {
					el.setAttribute("aria-keyshortcuts", "Space");
				} else {
					el.removeAttribute("aria-keyshortcuts");
				}
			}
		}),
	);
	return (
		<div class="bridge">
			<div class="bridge-toolbar">
				<span class="heading">Bridge</span>
				<span class="sub">
					{boardAgents().length} agents · {inFlight()} in-flight issues
				</span>
				<div class="seg" role="toolbar" aria-label="Board view">
					<button
						type="button"
						classList={{ active: tab() === "issues" }}
						onClick={() => setTab("issues")}
					>
						Issues
					</button>
					<button
						type="button"
						classList={{ active: tab() === "prs" }}
						onClick={() => setTab("prs")}
					>
						PRs · {prCount(store.issues(), scope())}
					</button>
				</div>
				<Show when={tab() === "issues"}>
					<div class="seg" role="toolbar" aria-label="Board grouping">
						<button
							type="button"
							classList={{ active: mode() === "swimlane" }}
							onClick={() => setMode("swimlane")}
						>
							Swimlanes
						</button>
						<button
							type="button"
							classList={{ active: mode() === "status" }}
							onClick={() => setMode("status")}
						>
							Status
						</button>
					</div>
				</Show>
			</div>

			<Show when={tab() === "issues"}>
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
											onClick={() => store.openAgent(agent.account.id)}
										>
											<StateDot state={agent.lifecycle ?? "idle"} />
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
														class="bridge-cell"
														classList={{ dim: items.length === 0 }}
													>
														<For each={items}>
															{(ws) => (
																<IssueCard
																	issue={ws}
																	inRovingGroup
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

			<Show when={tab() === "prs"}>
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
												onClick={() => store.openAgent(agent().account.id)}
											>
												<StateDot state={agent().lifecycle ?? "idle"} />
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
													class="bridge-cell"
													classList={{ dim: cards.length === 0 }}
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
