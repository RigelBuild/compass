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

// Lit-cell counts transcribed from the six frozen ASCII grids in
// docs/designs/product/compass-badge-clarity/design.md §Badge (`#` = lit).
const LIT_CELLS = {
	ci: { success: 9, pending: 12, failure: 17 },
	review: { approved: 17, changes: 17, commented: 20 },
} as const;

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

	// Each status draws its own frozen geometry: the lit-rect count is the glyph's
	// fingerprint, so a wrong grid (or a swapped status) fails here.
	for (const [status, count] of Object.entries(LIT_CELLS.ci)) {
		test(`ci/${status} draws ${count} lit cells`, () => {
			const badge = mount({
				axis: "ci",
				status: status as "success" | "pending" | "failure",
			});
			expect(badge.querySelectorAll(".glyph rect").length).toBe(count);
		});
	}
	for (const [status, count] of Object.entries(LIT_CELLS.review)) {
		test(`review/${status} draws ${count} lit cells`, () => {
			const badge = mount({
				axis: "review",
				status: status as "approved" | "changes" | "commented",
			});
			expect(badge.querySelectorAll(".glyph rect").length).toBe(count);
		});
	}

	test("every glyph rect fills on currentColor (so the wrapper's color paints it)", () => {
		const rects = mount({ axis: "review", status: "changes" }).querySelectorAll(
			".glyph rect",
		);
		expect(rects.length).toBeGreaterThan(0);
		for (const r of rects) expect(r.getAttribute("fill")).toBe("currentColor");
	});

	test("the SVG is a labelled 9×9 crispEdges image", () => {
		const glyph = mount({ axis: "ci", status: "success" }).querySelector(
			".glyph",
		);
		expect(glyph?.getAttribute("viewBox")).toBe("0 0 9 9");
		expect(glyph?.getAttribute("shape-rendering")).toBe("crispEdges");
		expect(glyph?.getAttribute("role")).toBe("img");
		expect(glyph?.getAttribute("aria-label")).toBe("CI: passing");
	});

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
