import { describe, expect, test } from "bun:test";
import { TOUR_STEPS } from "./state";

describe("TOUR_STEPS", () => {
	// The ids are the server's resume cursor and the analytics dimension, so a
	// rename or reorder strands every stored row.
	test("step ids are frozen in order", () => {
		expect(TOUR_STEPS.map((s) => s.id)).toEqual([
			"welcome",
			"board",
			"sidebar-tree",
			"agent-workspace",
			"keyboard",
			"finale",
		]);
	});

	test("callouts name a unique anchor and dialogs name none", () => {
		const anchors = TOUR_STEPS.filter((s) => s.kind === "callout").map(
			(s) => s.anchor,
		);
		expect(anchors.every((a) => typeof a === "string" && a.length > 0)).toBe(
			true,
		);
		expect(new Set(anchors).size).toBe(anchors.length);
		expect(
			TOUR_STEPS.filter((s) => s.kind === "dialog").every(
				(s) => s.anchor === undefined,
			),
		).toBe(true);
	});

	test("the tour opens and closes on a dialog", () => {
		expect(TOUR_STEPS[0]?.kind).toBe("dialog");
		expect(TOUR_STEPS.at(-1)?.kind).toBe("dialog");
	});
});
