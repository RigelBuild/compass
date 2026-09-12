import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { GLYPH_NAMES, Glyph } from "./Glyph";

// The <Glyph/> invariants named by the frozen record (compass-glyph-primitives
// §"The <Glyph/> primitive"): every glyph name yields a non-empty cell list,
// and every cell lies within the 11×11 grid. We reach the geometry through the
// rendered SVG so the assertions bind the observable output, not the table.

function renderedCells(root: Element): Array<[number, number]> {
	const cells: Array<[number, number]> = [];
	for (const rect of root.querySelectorAll("rect")) {
		cells.push([
			Number(rect.getAttribute("x")),
			Number(rect.getAttribute("y")),
		]);
	}
	return cells;
}

describe("Glyph", () => {
	test("is decorative — aria-hidden, no role or label", () => {
		const { container } = render(() => <Glyph name="status" />);
		const svg = container.querySelector("svg");
		expect(svg?.getAttribute("aria-hidden")).toBe("true");
		expect(svg?.getAttribute("role")).toBeNull();
		expect(svg?.getAttribute("aria-label")).toBeNull();
	});

	for (const name of GLYPH_NAMES) {
		test(`${name} lights cells, all within the 11×11 grid`, () => {
			const { container } = render(() => <Glyph name={name} />);
			const cells = renderedCells(container);
			expect(cells.length).toBeGreaterThan(0);
			for (const [x, y] of cells) {
				expect(Number.isInteger(x)).toBe(true);
				expect(Number.isInteger(y)).toBe(true);
				expect(x).toBeGreaterThanOrEqual(0);
				expect(x).toBeLessThanOrEqual(10);
				expect(y).toBeGreaterThanOrEqual(0);
				expect(y).toBeLessThanOrEqual(10);
			}
		});
	}
});
