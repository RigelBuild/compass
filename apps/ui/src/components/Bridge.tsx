import { type Component, createSignal, For, Show } from "solid-js";
import {
	activeIssues,
	boardAgents as boardAgentsOf,
	cellItems as cellItemsOf,
	laneTotal as laneTotalOf,
} from "../board";
import { BOARD_LANES } from "../constants";
import { useStore } from "../context";
import { type IssueState, STUB_AGENTS } from "../stub-data";
import { IssueCard } from "./IssueCard";
import { StateDot } from "./StateDot";

/** How the board groups rows. Swimlane = one row per agent (default); status =
 *  a plain column board with no agent rows. */
type BoardMode = "swimlane" | "status";

/** The Bridge: the full kanban board, swimlane-by-agent by default. Columns are
 *  the issue lifecycle states; each agent is a row; a cell holds that
 *  agent's cards in that state. Clicking an agent gutter opens the agent view. */
export const Bridge: Component = () => {
	const store = useStore();
	const [mode, setMode] = createSignal<BoardMode>("swimlane");

	// The board reads the store's reactive issue list (design "one source
	// of truth") through the pure board.ts partition, so a promote/archive shows
	// here immediately. STUB_AGENTS stays direct — agents aren't mutated here.
	const boardAgents = () => boardAgentsOf(STUB_AGENTS, store.issues());
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
			</div>

			<div class="swimlane" style={{ "grid-template-columns": gridColumns() }}>
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
										{(ws) => <IssueCard issue={ws} />}
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
													{(ws) => <IssueCard issue={ws} />}
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
		</div>
	);
};
