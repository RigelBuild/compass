import { type Component, For } from "solid-js";
import { AGENT_STATE_LABEL } from "../constants";
import type { AgentState } from "../stub-data";

/** The agent-state indicator: a fixed 9×9 1-bit pixel-art glyph carrying the
 * process state through its geometry and the wrapper's state color. The glyph
 * fills on `currentColor`, with color and the working pulse supplied by
 * `state-dot.css`. The wrapper carries the human-readable state label as its
 * title and aria label. */

const STATE_CELLS: Record<
	AgentState,
	ReadonlyArray<readonly [number, number]>
> = {
	working: [
		[0, 1],
		[4, 1],
		[1, 2],
		[5, 2],
		[2, 3],
		[6, 3],
		[3, 4],
		[7, 4],
		[2, 5],
		[6, 5],
		[1, 6],
		[5, 6],
		[0, 7],
		[4, 7],
	],
	idle: [
		[3, 3],
		[4, 3],
		[5, 3],
		[3, 4],
		[4, 4],
		[5, 4],
		[3, 5],
		[4, 5],
		[5, 5],
	],
	waiting: [
		[2, 0],
		[3, 0],
		[4, 0],
		[5, 0],
		[1, 1],
		[6, 1],
		[6, 2],
		[5, 3],
		[4, 4],
		[4, 5],
		[4, 7],
	],
	done: [
		[8, 3],
		[7, 4],
		[0, 5],
		[6, 5],
		[1, 6],
		[5, 6],
		[2, 7],
		[4, 7],
		[3, 8],
	],
	paused: [
		[2, 2],
		[3, 2],
		[5, 2],
		[6, 2],
		[2, 3],
		[3, 3],
		[5, 3],
		[6, 3],
		[2, 4],
		[3, 4],
		[5, 4],
		[6, 4],
		[2, 5],
		[3, 5],
		[5, 5],
		[6, 5],
		[2, 6],
		[3, 6],
		[5, 6],
		[6, 6],
	],
	stopped: [
		[2, 2],
		[3, 2],
		[4, 2],
		[5, 2],
		[6, 2],
		[2, 3],
		[6, 3],
		[2, 4],
		[6, 4],
		[2, 5],
		[6, 5],
		[2, 6],
		[3, 6],
		[4, 6],
		[5, 6],
		[6, 6],
	],
	error: [
		[4, 1],
		[4, 2],
		[4, 3],
		[4, 4],
		[4, 5],
		[4, 7],
	],
	disconnected: [
		[2, 2],
		[3, 2],
		[5, 2],
		[6, 2],
		[2, 3],
		[6, 3],
		[2, 5],
		[6, 5],
		[2, 6],
		[3, 6],
		[5, 6],
		[6, 6],
	],
};

export const StateDot: Component<{ state: AgentState }> = (props) => {
	const label = () => AGENT_STATE_LABEL[props.state];
	return (
		<span
			class="cx-state-dot"
			data-state={props.state}
			data-alive={props.state === "working" ? "1" : undefined}
			title={label()}
			aria-label={label()}
			role="img"
		>
			<svg
				viewBox="0 0 9 9"
				width="9"
				height="9"
				shape-rendering="crispEdges"
				aria-hidden="true"
			>
				<For each={STATE_CELLS[props.state]}>
					{([x, y]) => (
						<rect x={x} y={y} width="1" height="1" fill="currentColor" />
					)}
				</For>
			</svg>
		</span>
	);
};
