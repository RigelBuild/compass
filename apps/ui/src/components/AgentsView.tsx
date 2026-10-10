import { type Component, createMemo, For, Show } from "solid-js";
import { agentIssueChip } from "../board";
import { isMultiForge } from "../board-render";
import { AGENT_STATE_LABEL } from "../constants";
import { useStore } from "../context";
import { openLink } from "../open-link";
import { type Agent, type AgentTreeNode, agentTree } from "../stub-data";
import { StateDot } from "./StateDot";
import "../design/components/agent-card.css";

interface TreeRow {
	agent: Agent;
	depth: number;
}

// Depth-first, parent above children, so each row's spine reaches up to its parent.
function flatten(nodes: readonly AgentTreeNode[], depth = 0): TreeRow[] {
	return nodes.flatMap((node) => [
		{ agent: node.agent, depth },
		...flatten(node.children, depth + 1),
	]);
}

const AgentCard: Component<{ agent: Agent; root: boolean }> = (props) => {
	const store = useStore();
	const id = () => props.agent.account.id;
	const state = () => props.agent.lifecycle ?? "idle";
	const chip = () =>
		agentIssueChip(id(), store.issues(), isMultiForge(store.issues()));
	return (
		<button
			type="button"
			class="agent-card"
			data-root={props.root ? "1" : undefined}
			{...openLink(
				store,
				() => `/agent/${id()}`,
				() => store.openAgent(id()),
			)}
			onKeyDown={(event) => {
				if (event.key !== "Enter") return;
				// Own the activation so the native Enter click cannot fire it twice.
				event.preventDefault();
				store.openAgent(id());
			}}
		>
			<StateDot state={state()} scale={2} />
			<span class="ac-body">
				<span class="ac-name">{props.agent.account.handle}</span>
				<span class="ac-meta">
					<span class="ac-state" data-state={state()}>
						{AGENT_STATE_LABEL[state()]}
					</span>
				</span>
			</span>
			<Show when={chip()}>
				{(text) => <span class="ac-issue">{text()}</span>}
			</Show>
		</button>
	);
};

/** The agent tree as a main view: the site's Manager tree, one card per agent,
 *  indented under its parent with an elbow spine. */
export const AgentsView: Component = () => {
	const store = useStore();
	const rows = createMemo(() => flatten(agentTree(store.agents())));
	return (
		<section class="agents-view" aria-label="Agents">
			<h2 class="heading">Agents</h2>
			<Show
				when={rows().length > 0}
				fallback={<p class="agents-empty">No agents yet.</p>}
			>
				<ul class="agent-tree">
					<For each={rows()}>
						{(row) => (
							<li class="tree-row" style={{ "--depth": String(row.depth) }}>
								<Show when={row.depth > 0}>
									<span
										class="tree-spine"
										data-flow={
											row.agent.lifecycle === "working" ? "1" : undefined
										}
										aria-hidden="true"
									/>
								</Show>
								<AgentCard agent={row.agent} root={row.depth === 0} />
							</li>
						)}
					</For>
				</ul>
			</Show>
		</section>
	);
};
