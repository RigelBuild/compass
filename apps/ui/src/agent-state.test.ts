import { describe, expect, test } from "bun:test";
import { AgentSessionState } from "@compass/client";
import { type AgentStreamRefinement, agentDotState } from "./agent-state";
import type { AgentState } from "./stub-data";

// Pin daemon-state projection and stream refinements.

// Enumerate valid dot values so the projection guard rejects unknown states.
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

// Iterate every session enum value across the refinements.
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
	// Keep an expected row for every session enum member.
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

	// READY can also carry an open ask; that takes precedence over an unopened turn.
	test("READY with awaitingInput stays waiting even when the turn is done", () => {
		expect(
			agentDotState(AgentSessionState.READY, {
				awaitingInput: true,
				turnDoneUnopened: true,
			}),
		).toBe("waiting");
	});

	test("READY with awaitingInput is waiting", () => {
		expect(
			agentDotState(AgentSessionState.READY, { awaitingInput: true }),
		).toBe("waiting");
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

	// STARTING shares WORKING's awaiting-input refinement.
	test("STARTING + awaitingInput → waiting (shares the WORKING case)", () => {
		expect(
			agentDotState(AgentSessionState.STARTING, { awaitingInput: true }),
		).toBe("waiting");
	});
});

describe("turnDoneUnopened refinement (READY → done)", () => {
	// A completed but unopened turn maps to done, not idle.
	test("lifts READY to done", () => {
		expect(
			agentDotState(AgentSessionState.READY, { turnDoneUnopened: true }),
		).toBe("done");
	});
	// turnDoneUnopened must not change WORKING; only READY reads that flag.
	test("does NOT change WORKING (stays working)", () => {
		expect(
			agentDotState(AgentSessionState.WORKING, { turnDoneUnopened: true }),
		).toBe("working");
	});
});

describe("return type is always a valid AgentState", () => {
	// Reject dot values outside the domain union across enum/refinement combinations.
	// Reject outputs outside the domain union across state/refinement combinations.
	const isValidDot = (dot: string): boolean => Object.hasOwn(VALID_DOTS, dot);
	const refinements: readonly AgentStreamRefinement[] = [
		{},
		{ awaitingInput: true },
		{ turnDoneUnopened: true },
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
	// Proto3 can carry unknown numeric values at runtime; reject them before UI use.
	test("throws on an unmodeled AgentSessionState value", () => {
		expect(() => agentDotState(999 as AgentSessionState)).toThrow(
			"Unhandled AgentSessionState: 999",
		);
	});
});
