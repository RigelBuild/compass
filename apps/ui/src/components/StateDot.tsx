import type { Component } from "solid-js";
import { AGENT_STATE_LABEL } from "../constants";
import type { AgentState } from "../stub-data";

/** The agent-state dot (design D9): one glyph carrying the agent's process
 *  state through color (app.css `.state-dot[data-state=…]`). The visual
 *  vocabulary mirrors Orca's `AgentStateDot` — grey idle, yellow-ringed working,
 *  amber waiting, emerald done, amber paused, hollow-ring stopped, red error —
 *  with `done` kept visually distinct from idle so a finished-but-unopened agent
 *  reads as done, and `stopped` (terminated) distinct from a live idle session.
 *
 *  The color alone isn't accessible, so the dot carries the human label
 *  (`AGENT_STATE_LABEL`) as its `title` + `aria-label` rather than being
 *  `aria-hidden`. One component so every surface (tree, roster, agent header,
 *  board gutter) renders the state identically. */
export const StateDot: Component<{ state: AgentState }> = (props) => {
	const label = () => AGENT_STATE_LABEL[props.state];
	return (
		<span
			class="state-dot"
			data-state={props.state}
			title={label()}
			aria-label={label()}
			role="img"
		/>
	);
};
