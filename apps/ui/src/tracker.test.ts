import { describe, expect, test } from "bun:test";
import type { WorkstreamState } from "./stub-data";
import {
	fromTrackerStatus,
	LINEAR_STATUS_MAPPING,
	toTrackerStatus,
} from "./tracker";

// tracker.ts is the pure D2 projection between Compass state (canonical) and a
// tracker's native status, through a user-editable mapping. These tests defend
// the projection's two structural invariants — `toTracker` is TOTAL over the
// WorkstreamState union, `fromTracker` is MANY-TO-ONE with a `backlog` fallback
// — independent of any reactive root (the functions are pure).

// The complete lifecycle (stub-data WorkstreamState). Enumerated on purpose: a
// future 8th union member reddens this literal against the exhaustiveness type,
// forcing the new state to be given a tracker projection.
const ALL_STATES: readonly WorkstreamState[] = [
	"backlog",
	"todo",
	"queued",
	"blocked",
	"in_progress",
	"in_review",
	"done",
];

describe("toTrackerStatus (D2 totality)", () => {
	// Invariant — totality. Every Compass state projects to a NON-EMPTY tracker
	// status under the Linear map; there is no state without a target. A map
	// with a missing/blank entry (e.g. a new union member left unmapped) fails
	// here rather than surfacing `undefined` on a tracker write.
	test("maps every WorkstreamState to a non-empty status", () => {
		for (const state of ALL_STATES) {
			const status = toTrackerStatus(state, LINEAR_STATUS_MAPPING);
			expect(typeof status).toBe("string");
			expect(status.length).toBeGreaterThan(0);
		}
		// Guards the enumeration itself: exactly 7 distinct states.
		expect(new Set(ALL_STATES).size).toBe(7);
	});

	// The named collapse: Linear has no `Queued`, so both `todo` and `queued`
	// project to `Todo`. This is the forward half of the many-to-one — a
	// regression that gave `queued` its own status would break the round-trip
	// asymmetry the design relies on.
	test("collapses todo and queued onto the same Todo status", () => {
		expect(toTrackerStatus("todo", LINEAR_STATUS_MAPPING)).toBe("Todo");
		expect(toTrackerStatus("queued", LINEAR_STATUS_MAPPING)).toBe("Todo");
		// blocked likewise shares In Progress with in_progress.
		expect(toTrackerStatus("blocked", LINEAR_STATUS_MAPPING)).toBe(
			"In Progress",
		);
		expect(toTrackerStatus("in_progress", LINEAR_STATUS_MAPPING)).toBe(
			"In Progress",
		);
	});
});

describe("fromTrackerStatus (D2 many-to-one + fallback)", () => {
	// Invariant — many-to-one. Distinct tracker statuses read back to the SAME
	// Compass state: Done, Cancelled, and Duplicate all become `done` (the
	// design's named case). A map that dropped Cancelled/Duplicate would send
	// them through the fallback to `backlog` and fail here.
	test("reads Done, Cancelled, and Duplicate all back to done", () => {
		expect(fromTrackerStatus("Done", LINEAR_STATUS_MAPPING)).toBe("done");
		expect(fromTrackerStatus("Cancelled", LINEAR_STATUS_MAPPING)).toBe("done");
		expect(fromTrackerStatus("Duplicate", LINEAR_STATUS_MAPPING)).toBe("done");
	});

	// The mapped statuses each resolve to their canonical Compass state.
	test("resolves each mapped status to its Compass state", () => {
		const cases: [string, WorkstreamState][] = [
			["Backlog", "backlog"],
			["Todo", "todo"],
			["In Progress", "in_progress"],
			["In Review", "in_review"],
		];
		for (const [status, expected] of cases) {
			expect(fromTrackerStatus(status, LINEAR_STATUS_MAPPING)).toBe(expected);
		}
	});

	// The fallback branch (`?? "backlog"`): an unmapped status — a column the
	// user's org has that this map doesn't know — resolves to `backlog` rather
	// than dropping the issue or returning undefined. This is the edge the `??`
	// exists for; without it the function would surface `undefined`.
	test("falls back to backlog for an unmapped status", () => {
		expect(fromTrackerStatus("Triage", LINEAR_STATUS_MAPPING)).toBe("backlog");
		expect(fromTrackerStatus("", LINEAR_STATUS_MAPPING)).toBe("backlog");
	});
});
