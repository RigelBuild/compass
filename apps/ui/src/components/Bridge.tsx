import { type Component, createSignal, For, Show } from "solid-js";
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
	ciBadge,
	isMultiForge,
	issueKey,
	prLifecycle,
	reviewBadge,
} from "../board-render";
import { BOARD_LANES, PR_LANES } from "../constants";
import { useStore } from "../context";
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
					tabIndex={0}
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
export const Bridge: Component = () => {
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
				<div
					class="bridge-grid"
					style={{ "grid-template-columns": gridColumns() }}
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

			<Show when={tab() === "prs"}>
				<div
					class="bridge-grid"
					style={{
						"grid-template-columns": `180px repeat(${PR_LANES.length}, minmax(210px, 1fr))`,
					}}
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
		</div>
	);
};
