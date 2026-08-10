// Start-agent dialog — the start operation (design compass-spawn-control T3,
// DL-164). The caller has already fixed the agent + card via props.spec,
// so the only field is the initial prompt (a textarea; empty = start idle, and
// so a valid submit). Pure callback component — no store. Dialog open/closed is
// the parent's concern.

import { type Component, createSignal, onMount } from "solid-js";
import type { SpawnSpec } from "../spawn";

export const StartAgentDialog: Component<{
	spec: Omit<SpawnSpec, "initialPrompt">;
	onSubmit: (spec: SpawnSpec) => void;
	onCancel: () => void;
}> = (props) => {
	const [prompt, setPrompt] = createSignal("");

	// The dialog owns its initial focus so its own Escape handler is reachable
	// before the user tabs into the field (role=dialog/aria-modal expectation).
	let dialogRef: HTMLDivElement | undefined;
	onMount(() => dialogRef?.focus());

	const submit = () => {
		props.onSubmit({ ...props.spec, initialPrompt: prompt() });
	};

	return (
		<div
			ref={dialogRef}
			class="dialog start-agent-dialog"
			role="dialog"
			aria-modal="true"
			aria-label="Start agent"
			tabindex={-1}
			onKeyDown={(e) => {
				if (e.key === "Escape") props.onCancel();
			}}
		>
			<label class="field-row">
				<span>Initial prompt</span>
				<textarea
					class="field field-prompt"
					value={prompt()}
					onInput={(e) => setPrompt(e.currentTarget.value)}
				/>
			</label>

			<div class="dialog-actions">
				<button type="button" class="cancel" onClick={() => props.onCancel()}>
					Cancel
				</button>
				<button type="button" class="submit" onClick={submit}>
					Start
				</button>
			</div>
		</div>
	);
};
