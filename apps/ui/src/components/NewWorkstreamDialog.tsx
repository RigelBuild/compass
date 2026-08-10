// New-workstream dialog — the board add-a-card operation (design
// compass-spawn-control T3, DL-164). Adding a workstream and starting an agent
// are two operations, so this dialog only assembles a WorkstreamSpec (agent +
// title + priority) and hands it to onSubmit — no initial prompt, no lifecycle
// effect, no store. Dialog open/closed is the parent's concern.

import { type Component, createMemo, createSignal, For, Show } from "solid-js";
import type { WorkstreamSpec } from "../spawn";
import type { Agent, Priority } from "../stub-data";

const PRIORITIES: Priority[] = ["urgent", "high", "medium", "low"];

/** The sentinel option value that reveals the new-agent handle field. */
const NEW_AGENT = "__new__";

export const NewWorkstreamDialog: Component<{
	agents: Agent[];
	onSubmit: (spec: WorkstreamSpec) => void;
	onCancel: () => void;
}> = (props) => {
	// Empty = nothing chosen yet; NEW_AGENT = the "＋ new agent" path.
	const [agentValue, setAgentValue] = createSignal("");
	const [handle, setHandle] = createSignal("");
	const [title, setTitle] = createSignal("");
	const [priority, setPriority] = createSignal<Priority>("medium");

	const isNewAgent = () => agentValue() === NEW_AGENT;

	const agentReady = () =>
		isNewAgent() ? handle().trim().length > 0 : agentValue().length > 0;
	const canSubmit = createMemo(() => agentReady() && title().trim().length > 0);

	const submit = () => {
		if (!canSubmit()) return;
		const agent: WorkstreamSpec["agent"] = isNewAgent()
			? { kind: "new", handle: handle().trim() }
			: { kind: "existing", agentAccountId: agentValue() };
		props.onSubmit({ agent, title: title().trim(), priority: priority() });
	};

	return (
		<div
			class="dialog new-workstream-dialog"
			role="dialog"
			aria-modal="true"
			aria-label="New workstream"
			onKeyDown={(e) => {
				if (e.key === "Escape") props.onCancel();
			}}
		>
			<label class="field-row">
				<span>Agent</span>
				<select
					class="field field-agent"
					value={agentValue()}
					onInput={(e) => setAgentValue(e.currentTarget.value)}
				>
					<option value="" disabled>
						Select an agent…
					</option>
					<For each={props.agents}>
						{(agent) => (
							<option value={agent.account.id}>{agent.account.handle}</option>
						)}
					</For>
					<option value={NEW_AGENT}>＋ new agent</option>
				</select>
			</label>

			<Show when={isNewAgent()}>
				<label class="field-row">
					<span>Handle</span>
					<input
						class="field field-handle"
						type="text"
						value={handle()}
						onInput={(e) => setHandle(e.currentTarget.value)}
					/>
				</label>
			</Show>

			<label class="field-row">
				<span>Title</span>
				<input
					class="field field-title"
					type="text"
					value={title()}
					onInput={(e) => setTitle(e.currentTarget.value)}
				/>
			</label>

			<label class="field-row">
				<span>Priority</span>
				<select
					class="field field-priority"
					value={priority()}
					onInput={(e) => setPriority(e.currentTarget.value as Priority)}
				>
					<For each={PRIORITIES}>{(p) => <option value={p}>{p}</option>}</For>
				</select>
			</label>

			<div class="dialog-actions">
				<button type="button" class="cancel" onClick={() => props.onCancel()}>
					Cancel
				</button>
				<button
					type="button"
					class="submit"
					disabled={!canSubmit()}
					onClick={submit}
				>
					Add workstream
				</button>
			</div>
		</div>
	);
};
