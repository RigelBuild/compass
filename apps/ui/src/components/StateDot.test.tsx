import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { AGENT_STATE_LABEL } from "../constants";
import type { AgentState } from "../stub-data";
import { StateDot } from "./StateDot";

// The eight frozen state-dot grids, transcribed INDEPENDENTLY from
// design/components.md §State dot (`#` = lit, `.` = off; 9×9). This catches a
// wrong grid even when its lit-cell count is unchanged (idle and done both have
// 9 cells; stopped and disconnected differ by only 4).
const GRIDS: Record<AgentState, readonly string[]> = {
	working: [
		".........",
		"#...#....",
		".#...#...",
		"..#...#..",
		"...#...#.",
		"..#...#..",
		".#...#...",
		"#...#....",
		".........",
	],
	idle: [
		".........",
		".........",
		".........",
		"...###...",
		"...###...",
		"...###...",
		".........",
		".........",
		".........",
	],
	waiting: [
		"..####...",
		".#....#..",
		"......#..",
		".....#...",
		"....#....",
		"....#....",
		".........",
		"....#....",
		".........",
	],
	done: [
		".........",
		".........",
		".........",
		"........#",
		".......#.",
		"#.....#..",
		".#...#...",
		"..#.#....",
		"...#.....",
	],
	paused: [
		".........",
		".........",
		"..##.##..",
		"..##.##..",
		"..##.##..",
		"..##.##..",
		"..##.##..",
		".........",
		".........",
	],
	stopped: [
		".........",
		".........",
		"..#####..",
		"..#...#..",
		"..#...#..",
		"..#...#..",
		"..#####..",
		".........",
		".........",
	],
	error: [
		".........",
		"....#....",
		"....#....",
		"....#....",
		"....#....",
		"....#....",
		".........",
		"....#....",
		".........",
	],
	disconnected: [
		".........",
		".........",
		"..##.##..",
		"..#...#..",
		".........",
		"..#...#..",
		"..##.##..",
		".........",
		".........",
	],
};

const STATES: readonly AgentState[] = [
	"working",
	"idle",
	"waiting",
	"done",
	"paused",
	"stopped",
	"error",
	"disconnected",
];

// The [x,y] set a grid's `#` cells occupy — the frozen geometry the SVG must draw.
function litCells(grid: readonly string[]): Set<string> {
	const cells = new Set<string>();
	grid.forEach((row, y) => {
		[...row].forEach((cell, x) => {
			if (cell === "#") cells.add(`${x},${y}`);
		});
	});
	return cells;
}

// The [x,y] set the rendered SVG actually paints.
function renderedCells(dot: Element): Set<string> {
	const cells = new Set<string>();
	for (const r of dot.querySelectorAll(".cx-state-dot svg rect")) {
		cells.add(`${r.getAttribute("x")},${r.getAttribute("y")}`);
	}
	return cells;
}

function mount(state: AgentState) {
	const { container } = render(() => <StateDot state={state} />);
	const dot = container.querySelector(".cx-state-dot");
	if (!dot) throw new Error("state dot did not render");
	return dot;
}

describe("StateDot", () => {
	// Each state draws its own frozen geometry. Assert exact lit-cell POSITIONS,
	// not just the count, against the independently-transcribed design grids.
	for (const state of STATES) {
		test(`${state} draws exactly its frozen grid`, () => {
			const expected = litCells(GRIDS[state]);
			const actual = renderedCells(mount(state));
			expect([...actual].sort()).toEqual([...expected].sort());
		});
	}

	test("working is alive and every other state omits data-alive", () => {
		expect(mount("working").getAttribute("data-alive")).toBe("1");
		for (const state of STATES) {
			if (state !== "working") {
				expect(mount(state).getAttribute("data-alive")).toBeNull();
			}
		}
	});

	test("every glyph rect fills on currentColor", () => {
		const rects = mount("working").querySelectorAll(".cx-state-dot svg rect");
		expect(rects.length).toBeGreaterThan(0);
		for (const r of rects) expect(r.getAttribute("fill")).toBe("currentColor");
	});

	test("the SVG is a 9×9 crispEdges decorative glyph", () => {
		const svg = mount("working").querySelector(".cx-state-dot svg");
		expect(svg?.getAttribute("viewBox")).toBe("0 0 9 9");
		expect(svg?.getAttribute("shape-rendering")).toBe("crispEdges");
		expect(svg?.getAttribute("aria-hidden")).toBe("true");
	});

	for (const state of STATES) {
		test(`${state} wrapper carries its accessible name`, () => {
			const label = AGENT_STATE_LABEL[state];
			expect(label).not.toBe("");
			const dot = mount(state);
			expect(dot.getAttribute("role")).toBe("img");
			expect(dot.getAttribute("aria-label")).toBe(label);
			expect(dot.getAttribute("title")).toBe(label);
		});
	}
});
