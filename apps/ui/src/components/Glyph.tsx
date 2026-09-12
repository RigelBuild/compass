import { type Component, For } from "solid-js";

/** A fixed chrome symbol drawn as an 11×11 1-bit pixel-art grid at one CSS px
 *  per cell (RIG-3603, design compass-glyph-primitives). Adopts the DL-150 /
 *  DL-199 technique as-is: one `<rect width="1" height="1">` per lit cell,
 *  filled on `currentColor` so the consuming control's color flows through.
 *
 *  The glyph is decorative — `aria-hidden`, no `role="img"` and no
 *  `aria-label`. It carries no meaning of its own, so a name-bearing label
 *  belongs on the consuming control (this is the deliberate difference from
 *  `BadgeGlyph`, whose glyph carries status meaning). */

export type GlyphName = "status" | "files" | "vcs" | "pr";

/** [x, y] of each lit cell (11×11, one CSS px per cell), transcribed from the
 *  frozen ASCII grids in `design/components.md` §Glyphs (`#` = lit). Keying on
 *  the exhaustive `GlyphName` union makes a name without a bitmap a compile
 *  error, not a silent runtime blank. */
const GLYPH_CELLS: Record<
	GlyphName,
	ReadonlyArray<readonly [number, number]>
> = {
	status: [
		[1, 1],
		[2, 1],
		[3, 1],
		[4, 1],
		[6, 1],
		[7, 1],
		[8, 1],
		[9, 1],
		[1, 2],
		[2, 2],
		[3, 2],
		[4, 2],
		[6, 2],
		[7, 2],
		[8, 2],
		[9, 2],
		[1, 3],
		[2, 3],
		[3, 3],
		[4, 3],
		[6, 3],
		[7, 3],
		[8, 3],
		[9, 3],
		[1, 4],
		[2, 4],
		[3, 4],
		[4, 4],
		[6, 4],
		[7, 4],
		[8, 4],
		[9, 4],
		[1, 6],
		[2, 6],
		[3, 6],
		[4, 6],
		[6, 6],
		[7, 6],
		[8, 6],
		[9, 6],
		[1, 7],
		[2, 7],
		[3, 7],
		[4, 7],
		[6, 7],
		[7, 7],
		[8, 7],
		[9, 7],
		[1, 8],
		[2, 8],
		[3, 8],
		[4, 8],
		[6, 8],
		[7, 8],
		[8, 8],
		[9, 8],
		[1, 9],
		[2, 9],
		[3, 9],
		[4, 9],
		[6, 9],
		[7, 9],
		[8, 9],
		[9, 9],
	],
	files: [
		[1, 2],
		[2, 2],
		[3, 2],
		[4, 2],
		[1, 3],
		[2, 3],
		[3, 3],
		[4, 3],
		[1, 4],
		[2, 4],
		[3, 4],
		[4, 4],
		[5, 4],
		[6, 4],
		[7, 4],
		[8, 4],
		[9, 4],
		[1, 5],
		[2, 5],
		[3, 5],
		[4, 5],
		[5, 5],
		[6, 5],
		[7, 5],
		[8, 5],
		[9, 5],
		[1, 6],
		[2, 6],
		[3, 6],
		[4, 6],
		[5, 6],
		[6, 6],
		[7, 6],
		[8, 6],
		[9, 6],
		[1, 7],
		[2, 7],
		[3, 7],
		[4, 7],
		[5, 7],
		[6, 7],
		[7, 7],
		[8, 7],
		[9, 7],
		[1, 8],
		[2, 8],
		[3, 8],
		[4, 8],
		[5, 8],
		[6, 8],
		[7, 8],
		[8, 8],
		[9, 8],
		[1, 9],
		[2, 9],
		[3, 9],
		[4, 9],
		[5, 9],
		[6, 9],
		[7, 9],
		[8, 9],
		[9, 9],
	],
	vcs: [
		[1, 1],
		[2, 1],
		[7, 1],
		[8, 1],
		[1, 2],
		[2, 2],
		[6, 2],
		[7, 2],
		[8, 2],
		[1, 3],
		[2, 3],
		[5, 3],
		[6, 3],
		[1, 4],
		[2, 4],
		[3, 4],
		[4, 4],
		[5, 4],
		[1, 5],
		[2, 5],
		[3, 5],
		[1, 6],
		[2, 6],
		[1, 7],
		[2, 7],
		[1, 8],
		[2, 8],
		[1, 9],
		[2, 9],
	],
	pr: [
		[8, 1],
		[1, 2],
		[2, 2],
		[3, 2],
		[4, 2],
		[5, 2],
		[6, 2],
		[7, 2],
		[8, 2],
		[9, 2],
		[8, 3],
		[2, 7],
		[1, 8],
		[2, 8],
		[3, 8],
		[4, 8],
		[5, 8],
		[6, 8],
		[7, 8],
		[8, 8],
		[9, 8],
		[2, 9],
	],
};

/** Every glyph name, derived from the table itself so callers that enumerate
 *  glyphs (the cell-validity test) pick up a new name automatically. */
export const GLYPH_NAMES = Object.keys(GLYPH_CELLS) as readonly GlyphName[];

export const Glyph: Component<{ name: GlyphName }> = (props) => {
	return (
		<svg
			viewBox="0 0 11 11"
			width="11"
			height="11"
			shape-rendering="crispEdges"
			aria-hidden="true"
		>
			<For each={GLYPH_CELLS[props.name]}>
				{([x, y]) => (
					<rect x={x} y={y} width="1" height="1" fill="currentColor" />
				)}
			</For>
		</svg>
	);
};
