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
		[1, 2],
		[4, 2],
		[2, 3],
		[5, 3],
		[3, 4],
		[6, 4],
		[2, 5],
		[5, 5],
		[1, 6],
		[4, 6],
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
		[3, 1],
		[4, 1],
		[5, 1],
		[2, 2],
		[6, 2],
		[6, 3],
		[4, 4],
		[5, 4],
		[4, 5],
		[4, 7],
	],
	done: [
		[1, 4],
		[2, 5],
		[3, 6],
		[4, 5],
		[5, 4],
		[6, 3],
		[7, 2],
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

// Integer scales only, so every cell stays a whole pixel block: 1× (9px) in
// dense spots, 2× (18px) in tree rows.
export const StateDot: Component<{ state: AgentState; scale?: 1 | 2 }> = (
	props,
) => {
	const label = () => AGENT_STATE_LABEL[props.state];
	return (
		<span
			class="cx-state-dot"
			data-state={props.state}
			data-scale={props.scale === 2 ? "2" : undefined}
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
