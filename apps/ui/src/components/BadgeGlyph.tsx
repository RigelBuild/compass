import { type Component, For } from "solid-js";

/** The axis+status badge (SEA-2117 / SEA-2121, design compass-badge-clarity
 *  Option B): a fixed 2-char axis code (`CI` / `RV`) in the DS mono UI face,
 *  followed by a 9×9 1-bit pixel-art glyph carrying the status. Code and glyph
 *  share the status color — the wrapper's `data-axis`+`data-status` route
 *  `color` through `.cx-axis-badge` (badge-glyph.css), and the glyph fills on
 *  `currentColor`, so there is one source of truth and no attribute to keep in
 *  sync on the inner SVG.
 *
 *  The glyph geometry is the six frozen grids transcribed in
 *  `design/components.md` §Badge — one `<rect>` per lit cell. `compact` hides
 *  the code span (the Option A glyph-only fallback for cramped surfaces via
 *  `data-compact`).
 *
 *  Consumers (IssueCard / Bridge / DoneView) are flipped in a separate slice
 *  (SEA-2122); this component is the primitive they will adopt. */

type CiStatus = "success" | "pending" | "failure";
type ReviewStatus = "approved" | "changes" | "commented";

type BadgeGlyphProps =
	| { axis: "ci"; status: CiStatus; compact?: boolean }
	| { axis: "review"; status: ReviewStatus; compact?: boolean };

/** [x, y] of each lit cell (9×9, one CSS px per cell), transcribed from the
 *  frozen ASCII grids in `design/components.md` §Badge (`#` = lit). */
const GLYPH_CELLS: Record<string, ReadonlyArray<readonly [number, number]>> = {
	"ci-success": [
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
	"ci-pending": [
		[0, 4],
		[1, 4],
		[3, 4],
		[4, 4],
		[6, 4],
		[7, 4],
		[0, 5],
		[1, 5],
		[3, 5],
		[4, 5],
		[6, 5],
		[7, 5],
	],
	"ci-failure": [
		[0, 0],
		[8, 0],
		[1, 1],
		[7, 1],
		[2, 2],
		[6, 2],
		[3, 3],
		[5, 3],
		[4, 4],
		[3, 5],
		[5, 5],
		[2, 6],
		[6, 6],
		[1, 7],
		[7, 7],
		[0, 8],
		[8, 8],
	],
	"review-approved": [
		[8, 2],
		[7, 3],
		[8, 3],
		[0, 4],
		[1, 4],
		[6, 4],
		[7, 4],
		[1, 5],
		[2, 5],
		[5, 5],
		[6, 5],
		[2, 6],
		[3, 6],
		[4, 6],
		[5, 6],
		[3, 7],
		[4, 7],
	],
	"review-changes": [
		[4, 1],
		[4, 2],
		[3, 3],
		[5, 3],
		[3, 4],
		[5, 4],
		[2, 5],
		[6, 5],
		[2, 6],
		[6, 6],
		[1, 7],
		[2, 7],
		[3, 7],
		[4, 7],
		[5, 7],
		[6, 7],
		[7, 7],
	],
	"review-commented": [
		[1, 1],
		[2, 1],
		[3, 1],
		[4, 1],
		[5, 1],
		[6, 1],
		[7, 1],
		[1, 2],
		[7, 2],
		[1, 3],
		[7, 3],
		[1, 4],
		[2, 4],
		[3, 4],
		[4, 4],
		[5, 4],
		[6, 4],
		[7, 4],
		[3, 5],
		[2, 6],
	],
};

const AXIS_CODE = { ci: "CI", review: "RV" } as const;

const ARIA_LABEL: Record<string, string> = {
	"ci-success": "CI: passing",
	"ci-pending": "CI: pending",
	"ci-failure": "CI: failing",
	"review-approved": "Review: approved",
	"review-changes": "Review: changes requested",
	"review-commented": "Review: commented",
};

export const BadgeGlyph: Component<BadgeGlyphProps> = (props) => {
	const key = () => `${props.axis}-${props.status}`;
	return (
		<span
			class="cx-axis-badge"
			data-axis={props.axis}
			data-status={props.status}
			data-compact={props.compact ? "" : undefined}
		>
			<span class="cx-axis-code">{AXIS_CODE[props.axis]}</span>
			<svg
				class="glyph"
				viewBox="0 0 9 9"
				width="9"
				height="9"
				shape-rendering="crispEdges"
				role="img"
				aria-label={ARIA_LABEL[key()]}
			>
				<For each={GLYPH_CELLS[key()]}>
					{([x, y]) => (
						<rect x={x} y={y} width="1" height="1" fill="currentColor" />
					)}
				</For>
			</svg>
		</span>
	);
};
