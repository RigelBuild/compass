import { describe, expect, test } from "bun:test";
import { AgentSessionState } from "@compass/client";
import { type AgentStreamRefinement, agentDotState } from "./agent-state";
import type { AgentState } from "./stub-data";

// agent-state.ts is the pure D9 projection: it maps the daemon's coarse,
// authoritative `AgentSessionState` (compass.v1, #443) plus optional stream
// refinements onto the fine-grained UI dot. These tests pin the projection
// row-by-row so a future edit to the switch, a leaked refinement, or a reorder
// of the `wardenPaused` override can't silently render the wrong dot.

// The complete UI dot union (stub-data AgentState) as a membership table.
// Enumerated on purpose: the return-type guard below asserts every branch lands
// inside it, so an edit returning an off-union string reddens instead of shipping.
const VALID_DOTS: Record<AgentState, true> = {
	working: true,
	idle: true,
	waiting: true,
	done: true,
	paused: true,
	stopped: true,
	error: true,
	disconnected: true,
};

// Every live `AgentSessionState` variant. Iterated by the override and
// return-type invariants so they hold across the whole enum, not one value.
const ALL_SESSION_STATES: readonly AgentSessionState[] = [
	AgentSessionState.UNSPECIFIED,
	AgentSessionState.STARTING,
	AgentSessionState.READY,
	AgentSessionState.WORKING,
	AgentSessionState.STOPPED,
	AgentSessionState.ERRORED,
	AgentSessionState.DISCONNECTED,
];

describe("agentDotState — base enum mapping (no refinement)", () => {
	// The D9 mapping table, one row per enum value. This is the core contract:
	// flipping any row (e.g. READY→working, ERRORED→idle) reddens exactly here.
	// STOPPED now has its own distinct `stopped` dot (a terminated process, not a
	// live idle one). Only UNSPECIFIED falls to idle — the defensive case a
	// well-behaved daemon never sends as a live state.
	const rows: readonly [
		name: string,
		state: AgentSessionState,
		dot: AgentState,
	][] = [
		["STARTING → working", AgentSessionState.STARTING, "working"],
		["WORKING → working", AgentSessionState.WORKING, "working"],
		["READY → idle", AgentSessionState.READY, "idle"],
		["STOPPED → stopped", AgentSessionState.STOPPED, "stopped"],
		["ERRORED → error", AgentSessionState.ERRORED, "error"],
		[
			"DISCONNECTED → disconnected",
			AgentSessionState.DISCONNECTED,
			"disconnected",
		],
		["UNSPECIFIED → idle (defensive)", AgentSessionState.UNSPECIFIED, "idle"],
	];

	for (const [name, state, dot] of rows) {
		test(name, () => {
			expect(agentDotState(state)).toBe(dot);
		});
	}
});

describe("awaitingInput refinement (WORKING → waiting)", () => {
	// The `ask`/permission state: a working agent that has asked for input is
	// `waiting`, not `working`. If the branch stopped reading awaitingInput this
	// reddens.
	test("lifts WORKING to waiting", () => {
		expect(
			agentDotState(AgentSessionState.WORKING, { awaitingInput: true }),
		).toBe("waiting");
	});

	// Negative row — the refinement must NOT leak into READY. READY ignores
	// awaitingInput entirely (its only refinement is turnDoneUnopened), so a
	// READY agent with an open ask is still idle. A `case READY:` that started
	// honoring awaitingInput would redden here.
	test("does NOT change READY (stays idle)", () => {
		expect(
			agentDotState(AgentSessionState.READY, { awaitingInput: true }),
		).toBe("idle");
	});

	// Negative rows — terminal/defensive states are untouched by awaitingInput.
	test("does NOT change STOPPED, ERRORED, or UNSPECIFIED", () => {
		expect(
			agentDotState(AgentSessionState.STOPPED, { awaitingInput: true }),
		).toBe("stopped");
		expect(
			agentDotState(AgentSessionState.ERRORED, { awaitingInput: true }),
		).toBe("error");
		expect(
			agentDotState(AgentSessionState.UNSPECIFIED, { awaitingInput: true }),
		).toBe("idle");
	});

	// Source behavior: STARTING shares the WORKING case, so STARTING+awaitingInput
	// is ALSO waiting. This diverges slightly from the D9 doc table (which
	// documents the refinement only on WORKING) — asserted here to match SOURCE
	// and to pin the grouping: splitting STARTING into its own case that ignores
	// awaitingInput would redden this.
	test("STARTING + awaitingInput → waiting (shares the WORKING case)", () => {
		expect(
			agentDotState(AgentSessionState.STARTING, { awaitingInput: true }),
		).toBe("waiting");
	});
});

describe("turnDoneUnopened refinement (READY → done)", () => {
	// A completed-but-unopened turn is `done` (emerald check), deliberately not
	// idle grey. Dropping the refinement read reddens here.
	test("lifts READY to done", () => {
		expect(
			agentDotState(AgentSessionState.READY, { turnDoneUnopened: true }),
		).toBe("done");
	});

	// Negative row — turnDoneUnopened must NOT leak into WORKING. The WORKING
	// case only reads awaitingInput, so a working agent with the flag set is
	// still working. A `case WORKING:` that started honoring turnDoneUnopened
	// would redden here.
	test("does NOT change WORKING (stays working)", () => {
		expect(
			agentDotState(AgentSessionState.WORKING, { turnDoneUnopened: true }),
		).toBe("working");
	});
});

describe("wardenPaused override (precedence)", () => {
	// The strongest contract: a Warden pause is a Compass overlay that wins over
	// the enum regardless of what the session was doing. `wardenPaused` is the
	// first check in the function; moving it below the switch (so a WORKING/READY
	// session returned early) would redden every row here.
	for (const state of ALL_SESSION_STATES) {
		test(`paused wins for ${AgentSessionState[state]}`, () => {
			expect(agentDotState(state, { wardenPaused: true })).toBe("paused");
		});
	}

	// Precedence over the other refinements: pause beats an open ask and an
	// unopened-done turn. If the override were reordered after the refinement
	// branches, these would resolve to waiting/done instead.
	test("paused beats awaitingInput", () => {
		expect(
			agentDotState(AgentSessionState.WORKING, {
				wardenPaused: true,
				awaitingInput: true,
			}),
		).toBe("paused");
	});

	test("paused beats turnDoneUnopened", () => {
		expect(
			agentDotState(AgentSessionState.READY, {
				wardenPaused: true,
				turnDoneUnopened: true,
			}),
		).toBe("paused");
	});

	// A falsy wardenPaused must NOT trigger the override — the enum mapping still
	// governs. Guards against `if (refinement.wardenPaused !== undefined)` or a
	// truthiness slip that treats an explicit `false` as a pause.
	test("wardenPaused:false does not override the enum mapping", () => {
		expect(
			agentDotState(AgentSessionState.WORKING, { wardenPaused: false }),
		).toBe("working");
		expect(
			agentDotState(AgentSessionState.READY, { wardenPaused: false }),
		).toBe("idle");
	});
});

describe("return type is always a valid AgentState", () => {
	// Totality guard: across every enum value and a spread of refinement combos,
	// the result is one of the seven union members. Catches a future branch that
	// returns an off-union string (a typo'd dot name) before it reaches the UI.
	const isValidDot = (dot: string): boolean => Object.hasOwn(VALID_DOTS, dot);
	const refinements: readonly AgentStreamRefinement[] = [
		{},
		{ awaitingInput: true },
		{ turnDoneUnopened: true },
		{ wardenPaused: true },
		{ awaitingInput: true, turnDoneUnopened: true, wardenPaused: true },
	];

	for (const state of ALL_SESSION_STATES) {
		for (const refinement of refinements) {
			test(`${AgentSessionState[state]} × ${JSON.stringify(refinement)}`, () => {
				expect(isValidDot(agentDotState(state, refinement))).toBe(true);
			});
		}
	}
});

describe("default branch (proto3-open enum)", () => {
	// The daemon enum is proto3-open: a version-skewed daemon can send a numeric
	// variant not in the modeled set, which slips past the compile-time `never`
	// guard at runtime. The default branch throws rather than return the raw
	// number, which would poison downstream Record<AgentState> lookups. Cast is
	// required precisely because the typed enum makes this unreachable statically.
	test("throws on an unmodeled AgentSessionState value", () => {
		expect(() => agentDotState(999 as AgentSessionState)).toThrow(
			"Unhandled AgentSessionState: 999",
		);
	});
});
