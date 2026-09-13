import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import { STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { RightSidebar } from "./RightSidebar";

// Render spec for the activity-bar tab icon (RightSidebar.tsx, the `.r-tab-icon`
// span). The item type split at RIG-3603 into a glyph arm (a fixed 1-bit
// `<Glyph/>`) and an avatar arm (a person's initial as text + StateDot); this
// file defends that BOTH arms render as their contract says — no coverage
// existed before. FleetPane/tab loop are reached through the exported
// RightSidebar, the same seam a real activity-bar click uses.
function mountRightSidebar(): { store: AppStore; container: HTMLElement } {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<RightSidebar />
			</StoreContext>
		);
	});
	return { store, container };
}

// The tab button for a given aria-label, so a case targets one arm rather than
// reading the first `.r-tab` and hoping it is the intended one.
function tabByLabel(container: HTMLElement, label: string): HTMLButtonElement {
	const button = [
		...container.querySelectorAll<HTMLButtonElement>("nav.r-activity .r-tab"),
	].find((b) => b.getAttribute("aria-label") === label);
	if (!button) throw new Error(`no activity-bar tab labelled "${label}"`);
	return button;
}

describe("RightSidebar activity bar tab icons", () => {
	// pinAgent write-throughs to the process-wide happy-dom localStorage, so clear
	// it around every case (the fleet-pane suite's discipline).
	beforeEach(() => globalThis.localStorage.clear());
	afterEach(() => globalThis.localStorage.clear());

	// The glyph arm: a static tab draws a 1-bit `<Glyph/>` — an SVG with
	// crispEdges and lit `<rect>` cells, carrying NO text. A regression to the
	// old `{tab.icon}` string would render text and no SVG, reddening both legs.
	test("a static tab renders a crispEdges SVG glyph with no text", () => {
		const { container } = mountRightSidebar();
		const icon = tabByLabel(container, "Fleet status").querySelector(
			".r-tab-icon",
		);
		expect(icon).not.toBeNull();
		expect(icon?.getAttribute("data-kind")).toBe("glyph");
		const svg = icon?.querySelector("svg");
		expect(svg).not.toBeNull();
		expect(svg?.getAttribute("shape-rendering")).toBe("crispEdges");
		// Lit cells prove it is the real bitmap, not an empty box.
		expect(svg?.querySelectorAll("rect").length).toBeGreaterThan(0);
		// No initial leaked through — the glyph arm is textless.
		expect(icon?.textContent?.trim()).toBe("");
	});

	// The avatar arm: a resolvable fleet tab renders the handle's initial as TEXT
	// (no SVG glyph) plus the agent's StateDot badge. "compass-ui" → "C".
	test("a resolvable fleet tab renders its initial as text plus a StateDot", () => {
		const { store, container } = mountRightSidebar();
		store.pinAgent("acc-compass-ui");
		flush();
		const tab = tabByLabel(container, "compass-ui");
		const icon = tab.querySelector(".r-tab-icon");
		expect(icon?.getAttribute("data-kind")).toBe("avatar");
		expect(icon?.textContent?.trim()).toBe("C");
		// The avatar arm draws text, not a Glyph SVG.
		expect(icon?.querySelector("svg")).toBeNull();
		// The live agent badges the tab with a StateDot.
		expect(tab.querySelector(".cx-state-dot")).not.toBeNull();
	});

	// An unreachable pin (an id resolving to no fixture agent) still renders its
	// initial, but carries NO StateDot — the absent badge is the visual mark of a
	// dead pin (RIG-1645), so this reddens if the tab badges an unresolved agent.
	test("an unreachable fleet tab renders its initial but no StateDot", () => {
		globalThis.localStorage.setItem(
			"compass.pinnedAgents.acc-matt",
			JSON.stringify([{ id: "acc-ghost", handle: "ghosthandle" }]),
		);
		const { container } = mountRightSidebar();
		flush();
		const tab = tabByLabel(container, "ghosthandle (unreachable)");
		const icon = tab.querySelector(".r-tab-icon");
		expect(icon?.getAttribute("data-kind")).toBe("avatar");
		expect(icon?.textContent?.trim()).toBe("G");
		expect(tab.querySelector(".cx-state-dot")).toBeNull();
	});

	// D2 — the whole-pixel offset. happy-dom applies no stylesheet and computes
	// no layout, so real geometry is NOT observable here. This is a PROXY: it
	// parses app.css and asserts the mechanism that guarantees the offset — the
	// glyph box is an integer 11px square whose integer margins fill .r-tab's
	// 32px content box (34px − 2×1px border, box-sizing: border-box) EXACTLY on
	// each axis. With zero free space, flex centering has no slack to halve, so
	// the box's offset is its whole-pixel margin, not the 10.5px a centered 11px
	// box would take. It proves the declared geometry is whole-pixel; it does NOT
	// prove the browser rasterizes it there (that is the T6 visual baseline).
	test("the glyph box CSS pins a whole-pixel offset in the 34px tab (D2 proxy)", () => {
		const css = readFileSync(join(import.meta.dir, "../app.css"), "utf8");
		const rule = css.match(
			/\.r-tab \.r-tab-icon\[data-kind="glyph"\]\s*\{([^}]*)\}/,
		)?.[1];
		expect(rule).toBeDefined();
		const decl = (prop: string): string | undefined =>
			rule
				?.match(new RegExp(`(?:^|[;{\\s])${prop}\\s*:\\s*([^;]+);`))?.[1]
				.trim();
		const px = (v: string | undefined): number => {
			const n = Number(v?.replace("px", ""));
			expect(Number.isInteger(n)).toBe(true);
			return n;
		};
		const width = px(decl("width"));
		const height = px(decl("height"));
		expect(width).toBe(11);
		expect(height).toBe(11);
		// margin shorthand: top right bottom left.
		const margins = (decl("margin") ?? "").split(/\s+/);
		expect(margins.length).toBe(4);
		const [mt, mr, mb, ml] = margins.map((m) => px(m));
		// The 32px content box is filled exactly on each axis — no centering slack.
		expect(ml + width + mr).toBe(32);
		expect(mt + height + mb).toBe(32);
		// Seating: without `display: block` on the SVG itself the glyph rides the
		// text baseline and leaves the box the margins above just placed, so this
		// geometry would describe the span and not the pixels a user sees.
		const seat = css.match(
			/\.r-tab \.r-tab-icon\[data-kind="glyph"\]\s*>\s*svg\s*\{([^}]*)\}/,
		)?.[1];
		expect(seat).toBeDefined();
		expect(seat).toMatch(/display\s*:\s*block/);
	});
});
