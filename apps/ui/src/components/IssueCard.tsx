import { type Component, For, Show } from "solid-js";
import { checkPip, isMultiForge, issueKey, primaryPr } from "../board-render";
import { useStore } from "../context";
import type { Issue } from "../stub-data";

/** A single issue card — used in the Bridge swimlane cells. Single-click selects
 *  the issue (syncing the roster) without leaving the board; double-click jumps
 *  into the assigned agent's view (design D10). A card with no assignee has no
 *  jump target, so double-click falls back to select. */
export const IssueCard: Component<{ issue: Issue }> = (props) => {
	const store = useStore();
	const openAssignedAgent = () => {
		const agentId = props.issue.assignee;
		if (agentId) store.openAgent(agentId);
	};
	const pr = () => primaryPr(props.issue);
	const key = () => issueKey(props.issue, isMultiForge(store.issues()));
	// The assignee is a trusted Compass account id; show its handle as `@handle`.
	// Only Compass artifacts are boarded, so there is no separate untrusted
	// author to reconcile (Matt's 2026-07-31 ruling).
	const assignee = () => {
		const id = props.issue.assignee;
		if (!id) return undefined;
		return store.agentView(id)?.account.handle ?? id.replace("acc-", "");
	};
	return (
		<button
			type="button"
			class="card"
			data-priority={props.issue.priority}
			classList={{ selected: props.issue.id === store.selectedIssueId() }}
			onClick={() => store.selectIssue(props.issue.id)}
			onDblClick={openAssignedAgent}
		>
			<span class="card-top">
				<span class="card-issue">{key()}</span>
				<Show when={pr()}>
					{(p) => (
						<span class="card-pr">
							<Show when={p().checks}>
								{(checks) => (
									<span class="check-pips">
										<For each={checks().checks}>
											{(c) => (
												<span
													class="check-pip"
													data-status={checkPip(c.state)}
												/>
											)}
										</For>
									</span>
								)}
							</Show>
							#{p().number}
						</span>
					)}
				</Show>
			</span>
			<span class="card-title">{props.issue.title}</span>
			<span class="card-foot">
				<span class="card-author">
					{assignee() ? `@${assignee()}` : "unassigned"}
				</span>
				<Show when={pr()?.changed} keyed>
					{(changed) => (
						<Show when={changed.files > 0}>
							<span class="card-diff">
								<span class="add">+{changed.additions}</span>
								<span class="del">−{changed.deletions}</span>
							</span>
						</Show>
					)}
				</Show>
			</span>
		</button>
	);
};
