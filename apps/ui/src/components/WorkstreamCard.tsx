import { type Component, For, Show } from "solid-js";
import { useStore } from "../context";
import type { Workstream } from "../stub-data";

/** A single workstream card — used in the Bridge swimlane cells. Single-click
 *  selects the workstream (syncing the roster) without leaving the board;
 *  double-click jumps into the assigned agent's view (design D10). A card with
 *  no assignee has no jump target, so double-click falls back to select. */
export const WorkstreamCard: Component<{ ws: Workstream }> = (props) => {
	const store = useStore();
	const openAssignedAgent = () => {
		const agentId = props.ws.assignee;
		if (agentId) store.openAgent(agentId);
	};
	return (
		<button
			type="button"
			class="card"
			data-priority={props.ws.priority}
			classList={{ selected: props.ws.id === store.selectedWorkstreamId() }}
			onClick={() => store.selectWorkstream(props.ws.id)}
			onDblClick={openAssignedAgent}
		>
			<span class="card-top">
				<span class="card-issue">{props.ws.issue}</span>
				<Show when={props.ws.pr}>
					{(pr) => (
						<span class="card-pr">
							<span class="check-pips">
								<For each={pr().checks}>
									{(c) => <span class="check-pip" data-status={c.status} />}
								</For>
							</span>
							#{pr().number}
						</span>
					)}
				</Show>
			</span>
			<span class="card-title">{props.ws.title}</span>
			<span class="card-foot">
				<span>{props.ws.assignee?.replace("acc-", "") ?? "unassigned"}</span>
				<Show when={props.ws.changed.files > 0}>
					<span class="card-diff">
						<span class="add">+{props.ws.changed.additions}</span>
						<span class="del">−{props.ws.changed.deletions}</span>
					</span>
				</Show>
			</span>
		</button>
	);
};
