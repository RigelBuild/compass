import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { BadgeGlyph } from "./BadgeGlyph";

// BadgeGlyph's observable contract (SEA-2121 / SEA-2117 Option B): each badge is
// a wrapper carrying the axis+status as data-attributes (the single source of
// truth for color routing), a 2-char axis code, and a 9×9 crispEdges SVG glyph
// whose lit `<rect>` count is the frozen glyph geometry. The tests pin the
// axis→code mapping, the per-status glyph geometry, the wrapper attributes the
// CSS color selectors key on, and the compact toggle — behaviour a consumer or
// a CSS-selector change could silently break.

// The six frozen glyph grids, transcribed verbatim from the ASCII art in
// docs/designs/product/compass-badge-clarity/design.md §"The six glyph grids"
// (`#` = lit, `.` = off; 9×9). This is an INDEPENDENT copy of the contract —
// the component transcribes the same grids into [x,y] coordinate arrays, so
// comparing the rendered rects against these grids catches a wrong grid even
// when its lit-cell count is unchanged (three grids share a count of 17).
const GRIDS = {
	ci: {
		success: [
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
		pending: [
			".........",
			".........",
			".........",
			".........",
			"##.##.##.",
			"##.##.##.",
			".........",
			".........",
			".........",
		],
		failure: [
			"#.......#",
			".#.....#.",
			"..#...#..",
			"...#.#...",
			"....#....",
			"...#.#...",
			"..#...#..",
			".#.....#.",
			"#.......#",
		],
	},
	review: {
		approved: [
			".........",
			".........",
			"........#",
			".......##",
			"##....##.",
			".##..##..",
			"..####...",
			"...##....",
			".........",
		],
		changes: [
			".........",
			"....#....",
			"....#....",
			"...#.#...",
			"...#.#...",
			"..#...#..",
			"..#...#..",
			".#######.",
			".........",
		],
		commented: [
			".........",
			".#######.",
			".#.....#.",
			".#.....#.",
			".#######.",
			"...#.....",
			"..#......",
			".........",
			".........",
		],
	},
} as const;

// The screen-reader-observable label for each variant (BadgeGlyph.tsx ARIA_LABEL).
const ARIA = {
	ci: {
		success: "CI: passing",
		pending: "CI: pending",
		failure: "CI: failing",
	},
	review: {
		approved: "Review: approved",
		changes: "Review: changes requested",
		commented: "Review: commented",
	},
} as const;

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
function renderedCells(badge: Element): Set<string> {
	const cells = new Set<string>();
	for (const r of badge.querySelectorAll(".glyph rect")) {
		cells.add(`${r.getAttribute("x")},${r.getAttribute("y")}`);
	}
	return cells;
}

function mount(props: Parameters<typeof BadgeGlyph>[0]) {
	const { container } = render(() => <BadgeGlyph {...props} />);
	const badge = container.querySelector(".cx-axis-badge");
	if (!badge) throw new Error("badge did not render");
	return badge;
}

describe("BadgeGlyph", () => {
	test("CI axis renders the CI code; review axis renders RV", () => {
		expect(
			mount({ axis: "ci", status: "success" }).querySelector(".cx-axis-code")
				?.textContent,
		).toBe("CI");
		expect(
			mount({ axis: "review", status: "approved" }).querySelector(
				".cx-axis-code",
			)?.textContent,
		).toBe("RV");
	});

	test("the wrapper carries data-axis + data-status (the color-routing source of truth)", () => {
		const badge = mount({ axis: "ci", status: "failure" });
		expect(badge.getAttribute("data-axis")).toBe("ci");
		expect(badge.getAttribute("data-status")).toBe("failure");
		// The inner glyph does NOT restate the status — one attribute to keep in sync.
		const glyph = badge.querySelector(".glyph");
		expect(glyph?.getAttribute("data-status")).toBeNull();
		expect(glyph?.getAttribute("data-verdict")).toBeNull();
	});

	// Each status draws its own frozen geometry. Assert the exact lit-cell
	// POSITIONS (not just the count): three grids share a count of 17, so a
	// count check alone can't tell a swapped-but-same-count grid from the
	// contract. Comparing [x,y] sets against the independently-transcribed
	// frozen grids catches that.
	for (const axis of ["ci", "review"] as const) {
		for (const status of Object.keys(GRIDS[axis])) {
			test(`${axis}/${status} draws exactly its frozen grid`, () => {
				const badge = mount({
					axis,
					status,
				} as Parameters<typeof BadgeGlyph>[0]);
				const expected = litCells(
					GRIDS[axis][status as keyof (typeof GRIDS)[typeof axis]],
				);
				const actual = renderedCells(badge);
				expect([...actual].sort()).toEqual([...expected].sort());
			});
		}
	}

	test("every glyph rect fills on currentColor (so the wrapper's color paints it)", () => {
		const rects = mount({ axis: "review", status: "changes" }).querySelectorAll(
			".glyph rect",
		);
		expect(rects.length).toBeGreaterThan(0);
		for (const r of rects) expect(r.getAttribute("fill")).toBe("currentColor");
	});

	test("the SVG is a 9×9 crispEdges role=img", () => {
		const glyph = mount({ axis: "ci", status: "success" }).querySelector(
			".glyph",
		);
		expect(glyph?.getAttribute("viewBox")).toBe("0 0 9 9");
		expect(glyph?.getAttribute("shape-rendering")).toBe("crispEdges");
		expect(glyph?.getAttribute("role")).toBe("img");
	});

	// The aria-label is the screen-reader-observable name for each variant, and
	// each is a hand-written string — assert all six so a typo or copy-paste
	// swap in any label (e.g. "Review: changes requested") turns the suite red.
	for (const axis of ["ci", "review"] as const) {
		for (const status of Object.keys(ARIA[axis])) {
			test(`${axis}/${status} is labelled "${ARIA[axis][status as keyof (typeof ARIA)[typeof axis]]}"`, () => {
				const glyph = mount({
					axis,
					status,
				} as Parameters<typeof BadgeGlyph>[0]).querySelector(".glyph");
				expect(glyph?.getAttribute("aria-label")).toBe(
					ARIA[axis][status as keyof (typeof ARIA)[typeof axis]],
				);
			});
		}
	}

	test("compact sets data-compact (CSS then hides the code); default omits it", () => {
		expect(
			mount({ axis: "ci", status: "success", compact: true }).getAttribute(
				"data-compact",
			),
		).toBe("");
		expect(
			mount({ axis: "ci", status: "success" }).getAttribute("data-compact"),
		).toBeNull();
	});
});
