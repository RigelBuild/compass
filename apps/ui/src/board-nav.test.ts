import { describe, expect, test } from "bun:test";
import {
	type BoardNavInput,
	type BoardStop,
	boardStops,
	gutterId,
	moveCursor,
	prStopId,
	resolveCursor,
} from "./board-nav";

/** Build a swimlane input from a `{ [agentId]: { [col]: cardId[] } }` map. */
const swimlane = (
	agents: string[],
	grid: Record<string, Record<number, string[]>>,
	laneCount = 5,
): BoardNavInput => ({
	tab: "issues",
	mode: "swimlane",
	agents: agents.map((id) => ({ id })),
	cells: (agentId, col) => (grid[agentId]?.[col] ?? []).map((id) => ({ id })),
	laneCount,
});

const status = (
	grid: Record<number, string[]>,
	laneCount = 5,
): BoardNavInput => ({
	tab: "issues",
	mode: "status",
	cells: (col) => (grid[col] ?? []).map((id) => ({ id })),
	laneCount,
});

const ids = (stops: BoardStop[]): string[] => stops.map((s) => s.id);

describe("boardStops — issues swimlane", () => {
	test("gutter head per agent then column-ordered cards; empty cells skipped", () => {
		const input = swimlane(["a1", "a2"], {
			a1: { 0: ["i1", "i2"], 2: ["i3"] },
			a2: { 4: ["i4"] },
		});
		const stops = boardStops(input);
		expect(ids(stops)).toEqual([
			gutterId("a1"),
			"i1",
			"i2",
			"i3",
			gutterId("a2"),
			"i4",
		]);
		// Multi-card cell stack carries index positions.
		expect(stops[1]).toEqual({
			id: "i1",
			kind: "card",
			row: 0,
			col: 0,
			index: 0,
		});
		expect(stops[2]).toEqual({
			id: "i2",
			kind: "card",
			row: 0,
			col: 0,
			index: 1,
		});
		expect(stops[3]).toEqual({
			id: "i3",
			kind: "card",
			row: 0,
			col: 2,
			index: 0,
		});
		expect(stops[4]).toEqual({
			id: gutterId("a2"),
			kind: "gutter",
			row: 1,
			col: -1,
			index: 0,
		});
	});
});

describe("boardStops — issues status (no gutter)", () => {
	test("single row, no gutter, column-ordered cards", () => {
		const stops = boardStops(status({ 0: ["i1"], 1: ["i2", "i3"] }));
		expect(ids(stops)).toEqual(["i1", "i2", "i3"]);
		expect(stops.every((s) => s.kind === "card" && s.row === 0)).toBe(true);
	});
});

describe("boardStops — PRs tab", () => {
	const prInput: BoardNavInput = {
		tab: "prs",
		laneCount: 4,
		groups: [
			{
				agentId: "a1",
				rows: [
					{ issueId: "RIG-1", repo: "core", prNumber: 10, col: 0 },
					// Same issue, two non-closed PRs -> two rows, composite ids.
					{ issueId: "RIG-1", repo: "core", prNumber: 11, col: 1 },
				],
			},
			{
				agentId: null,
				rows: [{ issueId: "RIG-9", repo: "ui", prNumber: 3, col: 0 }],
			},
		],
	};

	test("gutter for non-null agent group, none for Unassigned; composite ids", () => {
		const stops = boardStops(prInput);
		expect(ids(stops)).toEqual([
			gutterId("a1"),
			prStopId("RIG-1", "core", 10),
			prStopId("RIG-1", "core", 11),
			prStopId("RIG-9", "ui", 3),
		]);
		// Duplicate issue id yields two distinct composite stops.
		expect(stops[1].id).toBe("RIG-1::core#10");
		expect(stops[2].id).toBe("RIG-1::core#11");
		expect(stops[1].id).not.toBe(stops[2].id);
		// Unassigned group (row 1) contributes no gutter.
		expect(stops.some((s) => s.kind === "gutter" && s.row === 1)).toBe(false);
	});
});

describe("moveCursor — up/down column-flattened", () => {
	const input = swimlane(["a1", "a2"], {
		a1: { 0: ["i1", "i2"] },
		a2: { 0: ["i3"] },
	});
	const stops = boardStops(input);

	test("down through stack then into next row's cell in same column", () => {
		expect(moveCursor(stops, "i1", "down")).toBe("i2");
		expect(moveCursor(stops, "i2", "down")).toBe("i3");
	});
	test("up mirrors down; clamp at column edges (no wrap)", () => {
		expect(moveCursor(stops, "i3", "up")).toBe("i2");
		expect(moveCursor(stops, "i1", "up")).toBe("i1");
		expect(moveCursor(stops, "i3", "down")).toBe("i3");
	});
	test("empty cell in a column is skipped by up/down", () => {
		const sparse = boardStops(
			swimlane(["a1", "a2", "a3"], {
				a1: { 0: ["i1"] },
				a2: {},
				a3: { 0: ["i3"] },
			}),
		);
		expect(moveCursor(sparse, "i1", "down")).toBe("i3");
	});
	test("gutter column is its own up/down track", () => {
		expect(moveCursor(stops, gutterId("a1"), "down")).toBe(gutterId("a2"));
		expect(moveCursor(stops, gutterId("a2"), "up")).toBe(gutterId("a1"));
		expect(moveCursor(stops, gutterId("a1"), "up")).toBe(gutterId("a1"));
	});
});

describe("moveCursor — home/end within current column", () => {
	const stops = boardStops(
		swimlane(["a1", "a2"], { a1: { 0: ["i1", "i2"] }, a2: { 0: ["i3"] } }),
	);
	test("home = first stop of column, end = last", () => {
		expect(moveCursor(stops, "i2", "home")).toBe("i1");
		expect(moveCursor(stops, "i2", "end")).toBe("i3");
	});
});

describe("moveCursor — left/right along row", () => {
	test("nearest non-empty cell; empty cells skipped", () => {
		const stops = boardStops(
			swimlane(["a1"], { a1: { 0: ["i1"], 2: ["i3"] } }),
		);
		expect(moveCursor(stops, "i1", "right")).toBe("i3");
		expect(moveCursor(stops, "i3", "left")).toBe("i1");
	});

	test("left from first non-empty column reaches gutter; right from gutter enters first cell", () => {
		const stops = boardStops(swimlane(["a1"], { a1: { 1: ["i1"] } }));
		expect(moveCursor(stops, "i1", "left")).toBe(gutterId("a1"));
		expect(moveCursor(stops, gutterId("a1"), "right")).toBe("i1");
	});

	test("no wrap: right at last stop clamps, left at gutter clamps", () => {
		const stops = boardStops(swimlane(["a1"], { a1: { 0: ["i1"] } }));
		expect(moveCursor(stops, "i1", "right")).toBe("i1");
		expect(moveCursor(stops, gutterId("a1"), "left")).toBe(gutterId("a1"));
	});

	test("indexInCell clamp: into a shorter stack lands on last card", () => {
		const stops = boardStops(
			swimlane(["a1"], { a1: { 0: ["a", "b", "c"], 1: ["x"] } }),
		);
		// From index 2 in col 0 -> col 1 has one card -> clamp to index 0.
		expect(moveCursor(stops, "c", "right")).toBe("x");
	});

	test("indexInCell clamp is non-reversible (named asymmetry)", () => {
		const stops = boardStops(
			swimlane(["a1"], { a1: { 0: ["a", "b", "c"], 1: ["x"] } }),
		);
		const right = moveCursor(stops, "c", "right"); // "x", clamped to index 0
		expect(right).toBe("x");
		// Going back left lands on index 0 of col 0, NOT the departing "c".
		expect(moveCursor(stops, "x", "left")).toBe("a");
	});
});

describe("moveCursor — status mode (no gutter)", () => {
	test("left at first column clamps (no gutter to reach)", () => {
		const stops = boardStops(status({ 0: ["i1"], 1: ["i2"] }));
		expect(moveCursor(stops, "i1", "left")).toBe("i1");
		expect(moveCursor(stops, "i2", "left")).toBe("i1");
	});
});

describe("moveCursor — PRs Unassigned row gutter hole", () => {
	const stops = boardStops({
		tab: "prs",
		laneCount: 4,
		groups: [
			{
				agentId: null,
				rows: [{ issueId: "RIG-9", repo: "ui", prNumber: 3, col: 1 }],
			},
		],
	});
	test("no gutter stop; left within Unassigned row clamps at first card column", () => {
		expect(stops.some((s) => s.kind === "gutter")).toBe(false);
		expect(moveCursor(stops, prStopId("RIG-9", "ui", 3), "left")).toBe(
			prStopId("RIG-9", "ui", 3),
		);
	});
});

describe("moveCursor — vanished cursor fallback", () => {
	const stops = boardStops(status({ 0: ["i1", "i2"] }));
	test("absent cursor id returns first stop; empty board returns null", () => {
		expect(moveCursor(stops, "gone", "down")).toBe("i1");
		expect(moveCursor([], "anything", "down")).toBe(null);
	});
});

describe("resolveCursor — rebuild recovery", () => {
	const before = boardStops(status({ 0: ["i1", "i2", "i3"] }));
	test("keeps a surviving cursor id", () => {
		expect(resolveCursor(before, before[1])).toBe("i2");
	});
	test("drops to next stop in same column when the cursor vanished", () => {
		const gone = before[1]; // i2 at row 0 col 0 index 1
		const after = boardStops(status({ 0: ["i1", "i3"] }));
		// i3 now sits at index 1 -> the first surviving stop at/past the position.
		expect(resolveCursor(after, gone)).toBe("i3");
	});
	test("falls back to previous stop when nothing at/below survives", () => {
		const gone = before[2]; // i3 at index 2
		const after = boardStops(status({ 0: ["i1"] }));
		expect(resolveCursor(after, gone)).toBe("i1");
	});
	test("first entry (null prev) picks the first stop; empty board null", () => {
		expect(resolveCursor(before, null)).toBe("i1");
		expect(resolveCursor([], null)).toBe(null);
	});
});
