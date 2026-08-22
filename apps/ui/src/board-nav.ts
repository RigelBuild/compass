/**
 * Pure board cursor model for the Bridge 2-D roving-tabindex grid (RIG-2130 T3).
 *
 * No DOM, no Solid, no store/board.ts imports — this module is fed plain data
 * the Bridge already derives and returns stop lists + traversal results. Wave 2
 * (T4) constructs the {@link BoardNavInput} from `boardAgents()`, `cellItems()`
 * and `prBoardGroups()` and wires the returned ids into the roving group.
 *
 * Coordinate model (design §"The 2-D sparse-grid cursor model"): the board is a
 * sparse grid of (lane rows × columns) where each cell holds 0..N cards. Cursor
 * stops are cards + gutter heads, never cells — an empty cell contributes no
 * stop and is skipped by traversal. A stop's position is `(row, col, index)`:
 *   - `row`   — lane row: agent index in swimlane, 0 in status mode, group
 *               index in the PRs tab.
 *   - `col`   — 0-based column into BOARD_LANES/PR_LANES; the gutter is col -1
 *               (swimlane only; the PRs Unassigned group has no gutter stop).
 *   - `index` — position within the cell's card stack (0 for a gutter head).
 */

/** A single keyboard-reachable position on the board. */
export type BoardStop = {
	/** Card id (issues: `issue.id`; PRs: composite `${issueId}::${repo}#${prNumber}`)
	 *  or gutter id (`gutter:${agentId}`). Unique across the whole board. */
	id: string;
	kind: "card" | "gutter";
	/** Lane row index. */
	row: number;
	/** Column index (0-based) or -1 for the gutter. */
	col: number;
	/** Position within the cell's card stack; 0 for a gutter head. */
	index: number;
};

/**
 * The minimal pure data the Bridge derives, enough to build the stop list for
 * both tabs and both modes. A discriminated union on `tab`/`mode`:
 *
 *   - `issues`/`swimlane`: `agents` are the ordered swimlane rows
 *     (`boardAgents()`), `cells(agentId, col)` yields that cell's card ids in
 *     stack order (`cellItems(all, agentId, BOARD_LANES[col].state)`), and
 *     `laneCount` = `BOARD_LANES.length`. Each agent row gets a gutter head.
 *   - `issues`/`status`: one row (row 0), no gutter; `cells(col)` yields the
 *     status column's card ids (`cellItems(all, null, BOARD_LANES[col].state)`),
 *     `laneCount` = `BOARD_LANES.length`.
 *   - `prs`: `groups` mirror `prBoardGroups()` — each has an `agentId`
 *     (`agent?.account.id ?? null`) and its rows carrying `issueId`, `repo`,
 *     `prNumber` and the `col` (index into `PR_LANES`) the row lands in. A group
 *     with a non-null `agentId` gets a gutter head; the trailing null
 *     (Unassigned) group gets none. `laneCount` = `PR_LANES.length`.
 *
 * The shape is deliberately dependency-free: plain arrays/ids/indices, no Solid
 * accessors and no board.ts import. `laneCount` is passed in rather than read
 * from constants.ts to keep the module self-contained.
 */
export type BoardNavInput =
	| {
			tab: "issues";
			mode: "swimlane";
			agents: readonly { id: string }[];
			cells: (agentId: string, col: number) => readonly { id: string }[];
			laneCount: number;
	  }
	| {
			tab: "issues";
			mode: "status";
			cells: (col: number) => readonly { id: string }[];
			laneCount: number;
	  }
	| {
			tab: "prs";
			groups: readonly {
				agentId: string | null;
				rows: readonly {
					issueId: string;
					repo: string;
					prNumber: number;
					col: number;
				}[];
			}[];
			laneCount: number;
	  };

export type BoardDirection = "up" | "down" | "left" | "right" | "home" | "end";

/** The gutter stop id for a swimlane agent (namespaced so it cannot collide
 *  with a card id). */
export const gutterId = (agentId: string): string => `gutter:${agentId}`;

/** The composite stop id for a PRs-tab card — never the bare issue id, since an
 *  issue with two non-closed PRs yields two rows sharing `issueId`. */
export const prStopId = (
	issueId: string,
	repo: string,
	prNumber: number,
): string => `${issueId}::${repo}#${prNumber}`;

/**
 * Build the ordered stop list for the board. Row order = input order; within a
 * row, the gutter head (if any) precedes columns 0..laneCount-1; within a
 * column, cards follow their stack order. Empty cells contribute no stops.
 */
export function boardStops(input: BoardNavInput): BoardStop[] {
	const stops: BoardStop[] = [];
	if (input.tab === "issues") {
		if (input.mode === "swimlane") {
			input.agents.forEach((agent, row) => {
				stops.push({
					id: gutterId(agent.id),
					kind: "gutter",
					row,
					col: -1,
					index: 0,
				});
				for (let col = 0; col < input.laneCount; col++) {
					input.cells(agent.id, col).forEach((card, index) => {
						stops.push({ id: card.id, kind: "card", row, col, index });
					});
				}
			});
		} else {
			for (let col = 0; col < input.laneCount; col++) {
				input.cells(col).forEach((card, index) => {
					stops.push({ id: card.id, kind: "card", row: 0, col, index });
				});
			}
		}
		return stops;
	}
	input.groups.forEach((group, row) => {
		if (group.agentId !== null) {
			stops.push({
				id: gutterId(group.agentId),
				kind: "gutter",
				row,
				col: -1,
				index: 0,
			});
		}
		for (let col = 0; col < input.laneCount; col++) {
			let index = 0;
			for (const pr of group.rows) {
				if (pr.col !== col) continue;
				stops.push({
					id: prStopId(pr.issueId, pr.repo, pr.prNumber),
					kind: "card",
					row,
					col,
					index: index++,
				});
			}
		}
	});
	return stops;
}

/** Stops in one column, ordered top-to-bottom (row then index) — the flattened
 *  Up/Down track for a column (the gutter track is col -1). */
const columnTrack = (stops: BoardStop[], col: number): BoardStop[] =>
	stops
		.filter((s) => s.col === col)
		.sort((a, b) => (a.row === b.row ? a.index - b.index : a.row - b.row));

/** The card stack for one cell, ordered by index. */
const cell = (stops: BoardStop[], row: number, col: number): BoardStop[] =>
	stops
		.filter((s) => s.row === row && s.col === col)
		.sort((a, b) => a.index - b.index);

/**
 * Move the cursor from `cursorId` in `dir` and return the destination stop id,
 * or the same id when the move clamps (no wrap). Returns the first stop id when
 * `cursorId` is absent from `stops` (minimal vanished-cursor fallback; callers
 * wanting same-column recovery use {@link resolveCursor}). Returns null only
 * when `stops` is empty.
 */
export function moveCursor(
	stops: BoardStop[],
	cursorId: string,
	dir: BoardDirection,
): string | null {
	if (stops.length === 0) return null;
	const cur = stops.find((s) => s.id === cursorId);
	if (!cur) return stops[0].id;

	if (dir === "up" || dir === "down" || dir === "home" || dir === "end") {
		const track = columnTrack(stops, cur.col);
		const pos = track.findIndex((s) => s.id === cur.id);
		if (dir === "home") return track[0].id;
		if (dir === "end") return track[track.length - 1].id;
		const next = pos + (dir === "down" ? 1 : -1);
		if (next < 0 || next >= track.length) return cur.id;
		return track[next].id;
	}

	// left / right: nearest non-empty column in that direction within the row.
	const colsInRow = Array.from(
		new Set(stops.filter((s) => s.row === cur.row).map((s) => s.col)),
	).sort((a, b) => a - b);
	const at = colsInRow.indexOf(cur.col);
	const targetCol = dir === "right" ? colsInRow[at + 1] : colsInRow[at - 1];
	if (targetCol === undefined) return cur.id; // clamp, no wrap
	const targetCell = cell(stops, cur.row, targetCol);
	if (targetCell.length === 0) return cur.id;
	// Land on the matching index, clamped to the target stack length. The clamp
	// is a NAMED non-reversible asymmetry: moving into a shorter stack then back
	// does not restore the original index.
	const idx = Math.min(cur.index, targetCell.length - 1);
	return targetCell[idx].id;
}

/**
 * Resolve the cursor id after a board rebuild, given the previous stop (or
 * null on first entry). If the previous stop's id still exists, keep it. Else
 * drop to the nearest remaining stop in the same column — the next card in the
 * flattened column order, else the previous, else the first stop on the board.
 * Returns null only when `stops` is empty.
 */
export function resolveCursor(
	stops: BoardStop[],
	prev: BoardStop | null,
): string | null {
	if (stops.length === 0) return null;
	if (prev && stops.some((s) => s.id === prev.id)) return prev.id;
	if (prev) {
		const track = columnTrack(stops, prev.col);
		if (track.length > 0) {
			// The first surviving stop at or past the vanished position, else the
			// last stop above it.
			const below = track.find(
				(s) =>
					s.row > prev.row || (s.row === prev.row && s.index >= prev.index),
			);
			if (below) return below.id;
			return track[track.length - 1].id;
		}
	}
	return stops[0].id;
}
