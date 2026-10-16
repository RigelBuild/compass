import { CommsService } from "@compass/client";
import { skipToken } from "@tanstack/solid-query";
import { type Component, createEffect, For, Show, untrack } from "solid-js";
import { useStore } from "../context";
import { adaptAgentBoard } from "../live/adapt";
import { createConnectQuery } from "../live/query";
import { openExternal } from "../open-external";
import { safeHref } from "../safe-url";
import type {
	AgentBoard,
	BoardAsk,
	BoardRef,
	BoardTask,
	BoardWaiting,
} from "../stub-data";

/** AgentBoardView renders one agent's board details without performing a board read. */
export const AgentBoardView: Component<{
	board: AgentBoard;
	onOpenAsk?: (ask: BoardAsk) => void;
}> = (props) => {
	const tasks = () => props.board.tasks;
	const focus = () =>
		props.board.tasks.find((task) => task.key === props.board.focusTaskKey);
	const asks = () =>
		[...props.board.asks].sort(
			(left, right) => Number(left.answered) - Number(right.answered),
		);
	const ageMinutes = () =>
		Math.max(
			0,
			Math.floor((Date.now() - props.board.writtenAtUnixMs) / 60_000),
		);
	const refsFor = (task: BoardTask) =>
		task.refs.flatMap((ref) => {
			const boardRef = props.board.refs.find((item) => item.ref === ref);
			return boardRef ? [boardRef] : [];
		});
	const primaryRef = (task: BoardTask): BoardRef | undefined =>
		task.primaryRef
			? props.board.refs.find((ref) => ref.ref === task.primaryRef)
			: undefined;
	const stackSize = (ref: BoardRef) => {
		if (!ref.stackTipRef) return undefined;
		const members = props.board.refs.filter(
			(item) => item.stackTipRef === ref.stackTipRef && item.stackPosition,
		);
		return Math.max(
			ref.stackPosition ?? 0,
			...members.map((item) => item.stackPosition ?? 0),
		);
	};
	const waitingLabel = (waiting: BoardWaiting) =>
		waiting.on === "agent"
			? `@${waiting.agentHandle ?? "agent"}`
			: (waiting.on ?? "waiting");
	return (
		<section class="agent-board" aria-label="Agent board">
			<div class="agent-board-topline">
				{props.board.halt && (
					<span class="agent-board-chip agent-board-halt">
						{props.board.halt.kind}
						{props.board.halt.ref && <> · {props.board.halt.ref}</>}
						{props.board.halt.reason && <> · {props.board.halt.reason}</>}
					</span>
				)}
				<Show when={focus()}>
					{(task) => <span class="agent-board-focus">{task().title}</span>}
				</Show>
				<span class="agent-board-updated">updated {ageMinutes()} min ago</span>
				<Show when={props.board.stale}>
					<span class="agent-board-chip agent-board-stale">Stale</span>
				</Show>
			</div>
			<div class="agent-board-tasks">
				<For each={tasks()}>
					{(task) => {
						const primary = () => primaryRef(task);
						const primaryHref = () => safeHref(primary()?.url);
						return (
							<article class="agent-board-task">
								<div class="agent-board-task-head">
									<Show when={primaryHref()}>
										{(href) => (
											<a
												class="agent-board-ref-primary"
												href={href()}
												target="_blank"
												rel="noreferrer noopener"
												onClick={(event) => {
													event.preventDefault();
													openExternal(href());
												}}
											>
												{task.primaryRef}
											</a>
										)}
									</Show>
									<Show when={!primaryHref() && task.primaryRef}>
										{(ref) => (
											<span class="agent-board-ref-primary">{ref()}</span>
										)}
									</Show>
									<strong class="agent-board-task-title">{task.title}</strong>
									<span class="agent-board-chip agent-board-state">
										{task.state ?? "unknown"}
									</span>
								</div>
								<Show when={refsFor(task).length > 0}>
									<div class="agent-board-refs">
										<For each={refsFor(task)}>
											{(ref) => (
												<div class="agent-board-ref-row">
													<span class="agent-board-chip">{ref.ref}</span>
													<Show when={ref.gate}>
														{(gate) => (
															<span class="agent-board-chip">
																{gate().replaceAll("_", " ")}
															</span>
														)}
													</Show>
													<Show when={ref.checksState}>
														{(state) => (
															<span class="agent-board-chip">
																checks: {state()}
															</span>
														)}
													</Show>
													<Show when={ref.draft}>
														<span class="agent-board-chip">draft</span>
													</Show>
													<Show when={ref.stackPosition}>
														{(position) => (
															<span class="agent-board-chip">
																stack {position()}/{stackSize(ref)}
															</span>
														)}
													</Show>
													<Show when={ref.stackTipRef}>
														{(tip) => (
															<span class="agent-board-chip">tip {tip()}</span>
														)}
													</Show>
													<Show when={ref.staleApproval}>
														<span class="agent-board-chip">stale approval</span>
													</Show>
												</div>
											)}
										</For>
									</div>
								</Show>
								<Show when={task.note}>
									{(note) => <p class="agent-board-note">{note()}</p>}
								</Show>
								<Show when={task.waiting.length > 0}>
									<div class="agent-board-waiting">
										<For each={task.waiting}>
											{(waiting) => (
												<div class="agent-board-wait-row">
													<span class="agent-board-chip agent-board-wait-chip">
														{waitingLabel(waiting)}
													</span>
													<Show when={waiting.what}>
														{(what) => <span>{what()}</span>}
													</Show>
													<For each={waiting.blocks}>
														{(ref) => (
															<span class="agent-board-block-ref">
																blocks {ref}
															</span>
														)}
													</For>
												</div>
											)}
										</For>
									</div>
								</Show>
								<Show when={task.subagents.length > 0}>
									<ul class="agent-board-subagents" aria-label="Subagents">
										<For each={task.subagents}>
											{(subagent) => (
												<li>
													<span class="agent-board-chip">
														{subagent.status}
													</span>
													<strong>{subagent.agent}</strong>{" "}
													{subagent.description}
												</li>
											)}
										</For>
									</ul>
								</Show>
							</article>
						);
					}}
				</For>
			</div>
			<Show when={asks().length > 0}>
				<section class="agent-board-section" aria-label="Asks">
					<h3>Asks</h3>
					<div class="agent-board-asks">
						<For each={asks()}>
							{(ask) => (
								<button
									type="button"
									class="agent-board-ask"
									data-answered={ask.answered}
									data-channel-id={ask.channelId}
									data-message-id={ask.messageId}
									onClick={() => props.onOpenAsk?.(ask)}
								>
									<span>{ask.header}</span>
									<Show when={ask.ref}>
										{(ref) => <span class="agent-board-ask-ref">{ref()}</span>}
									</Show>
								</button>
							)}
						</For>
					</div>
				</section>
			</Show>
			<Show when={props.board.untrackedRefs.length > 0}>
				<section class="agent-board-section" aria-label="Untracked references">
					<h3>Untracked</h3>
					<div class="agent-board-untracked">
						<For each={props.board.untrackedRefs}>
							{(ref) => <span class="agent-board-chip">{ref}</span>}
						</For>
					</div>
				</section>
			</Show>
		</section>
	);
};

/** AgentBoardPanel loads an agent's board from the shared live query cache. */
export const AgentBoardPanel: Component<{ agentId: string }> = (props) => {
	const store = useStore();
	// An ask opens the topic holding its message; an unloaded message opens its channel.
	const openAsk = (ask: BoardAsk) => {
		const message = store.messages().find((item) => item.id === ask.messageId);
		if (message) store.openTopic(message.topicId, ask.channelId);
		else store.openChannel(ask.channelId);
	};
	const deps = store.connectQueryDeps;
	if (!deps) return null;
	const query = createConnectQuery(
		CommsService.method.listAgentBoards,
		() => {
			const agent = store
				.agents()
				.find((item) => item.account.id === props.agentId);
			return agent
				? { agentHandles: [agent.account.handle], limit: 1 }
				: skipToken;
		},
		{ ...deps, refetchInterval: 60_000 },
	);
	// The mount read covers the events so far; each later AgentBoardChanged re-reads.
	const events = () => store.agentView(props.agentId)?.boardEvents ?? 0;
	let seen = untrack(events);
	createEffect(events, (next) => {
		if (next === seen) return;
		seen = next;
		void query.refetch();
	});
	return (
		<Show when={query.data?.boards[0]}>
			{(board) => (
				<AgentBoardView board={adaptAgentBoard(board())} onOpenAsk={openAsk} />
			)}
		</Show>
	);
};
