import { type Component, createSignal, For, Show } from "solid-js";
import {
	activeIssues,
	boardAgents as boardAgentsOf,
	cellItems as cellItemsOf,
	laneTotal as laneTotalOf,
	type PrRow,
	prCount,
	prRowGroups,
} from "../board";
import {
	ciBadge,
	isMultiForge,
	issueKey,
	prBadge,
	reviewBadge,
} from "../board-render";
import { BOARD_LANES } from "../constants";
import { useStore } from "../context";
import type { IssueState } from "../stub-data";
import { IssueCard } from "./IssueCard";
import { StateDot } from "./StateDot";

/** How the board groups rows. Swimlane = one row per agent (default); status =
 *  a plain column board with no agent rows. */
type BoardMode = "swimlane" | "status";

/** Which artifact kind the board shows — a peer tab axis, orthogonal to
 *  `BoardMode` (which is how ISSUES group). Bridge-local, like `BoardMode`. */
type BoardTab = "issues" | "prs";

/** One PRs-tab row: the PR's state badge, forge coordinate, title, CI + review
 *  badges, resolved/total thread tally, and the owning issue's key as the
 *  cross-link chip back to the Issues tab. The row body selects the issue
 *  (without leaving the tab); the issueKey chip selects AND flips to Issues. */
const PrRowItem: Component<{
	row: PrRow;
	multiForge: boolean;
	selected: boolean;
	onSelect: () => void;
	onOpenIssue: () => void;
}> = (props) => {
	const pr = () => props.row.pr;
	const resolved = () => pr().threads.filter((t) => t.resolved).length;
	return (
		<button
			type="button"
			class="pr-row"
			classList={{ selected: props.selected }}
			onClick={props.onSelect}
		>
			<span class="pr-row-state" data-pr-state={prBadge(pr())}>
				{prBadge(pr())}
			</span>
			<span class="pr-row-coord">
				{props.multiForge ? `${pr().forge.host}/` : ""}
				{pr().repo}#{pr().number}
			</span>
			<span class="pr-row-title">{pr().title}</span>
			<Show when={pr().checks}>
				<span class="ci-badge" data-status={ciBadge(pr())} />
			</Show>
			<Show when={reviewBadge(pr())}>
				{(verdict) => <span class="review-badge" data-verdict={verdict()} />}
			</Show>
			<span class="pr-row-threads">
				{resolved()}/{pr().threads.length}
			</span>
			{/* biome-ignore lint/a11y/useSemanticElements: an <a> needs an href; this is an
			   in-app selection chip, and it already lives inside the row <button>, so a
			   nested link/button is disallowed — role="link" + keyboard is the compromise. */}
			<span
				class="pr-row-issue"
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
		const groups = prRowGroups(store.agents(), store.issues());
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
					class="swimlane"
					style={{ "grid-template-columns": gridColumns() }}
				>
					{/* Header row */}
					<Show when={mode() === "swimlane"}>
						<div class="swim-corner">Agent</div>
					</Show>
					<For each={BOARD_LANES}>
						{(lane) => (
							<div class="swim-colhead">
								<span class="lane-dot" style={{ background: lane.color }} />
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
									<div class="swim-cell">
										<For
											each={cellItems(null, lane.state)}
											fallback={<span class="term-empty">—</span>}
										>
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
										class="swim-gutter"
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
													class="swim-cell"
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
				<div class="pr-tab">
					<For
						each={prGroups()}
						fallback={<div class="pr-empty">No open PRs.</div>}
					>
						{(group) => (
							<div class="pr-group">
								<Show
									when={group.agent}
									fallback={
										<div class="pr-group-head unassigned">Unassigned</div>
									}
								>
									{(agent) => (
										<button
											type="button"
											class="pr-group-head swim-gutter"
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
								<For each={group.rows}>
									{(row) => (
										<PrRowItem
											row={row}
											multiForge={multiForge()}
											selected={row.issue.id === store.selectedIssueId()}
											onSelect={() => store.selectIssue(row.issue.id)}
											onOpenIssue={() => {
												store.selectIssue(row.issue.id);
												setTab("issues");
											}}
										/>
									)}
								</For>
							</div>
						)}
					</For>
				</div>
			</Show>
		</div>
	);
};
