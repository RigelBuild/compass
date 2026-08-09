// The agent-state projection — the pure core of the agent axis (design D9/D11).
//
// The agent dot is a UI presentation over the daemon's coarse, authoritative
// `AgentSessionState` (compass.v1, #443) plus the `session/update` event stream.
// It is NOT a parallel enum: the daemon enum stays coarse by design, and the
// UI derives the fine-grained dot client-side. `waiting`/`done`/`paused` are
// UI-only refinements the wire contract does not carry (D9) — never written
// back as new `AgentSessionState` variants.
//
// Consuming the generated enum here (not a hand-copied union) is the whole
// point: if #443's contract changes, this projection is the one place the UI
// reconciles it, and TypeScript's exhaustiveness check flags an unhandled case.

import { AgentSessionState } from "@compass/client";
import type { AgentState } from "./stub-data";

/**
 * The client-observed refinements the coarse `AgentSessionState` doesn't carry,
 * derived from the `session/update` stream (design D9). All optional: with none
 * set, the projection falls back to the pure enum mapping.
 */
export interface AgentStreamRefinement {
	/** An ACP permission / `ask` request is open on the stream — the agent has
	 *  asked for input. Refines `WORKING` → `waiting` (Matt's "ask tool" state). */
	awaitingInput?: boolean;
	/** The agent completed a turn and no human has opened its view yet. Refines
	 *  `READY` → `done` (emerald check, deliberately not idle grey). */
	turnDoneUnopened?: boolean;
}

/**
 * Project the daemon's `AgentSessionState` (+ optional stream refinements) onto
 * the UI dot (design D9). The daemon enum is coarse; the refinements add the
 * three UI-only states:
 *
 * | AgentSessionState | UI dot                                        |
 * | ----------------- | --------------------------------------------- |
 * | STARTING          | working (spinner), or `waiting` if awaitingInput |
 * | WORKING           | working, or `waiting` if awaitingInput        |
 * | READY             | idle, or `done` if turnDoneUnopened           |
 * | STOPPED           | stopped (terminated; distinct from live idle)  |
 * | DISCONNECTED      | disconnected (Runner link dropped; awaiting reattach) |
 * | UNSPECIFIED       | idle (defensive; a well-behaved daemon never   |
 * |                   | sends it as a live state)                     |
 *
 */
export function agentDotState(
	sessionState: AgentSessionState,
	refinement: AgentStreamRefinement = {},
): AgentState {
	switch (sessionState) {
		case AgentSessionState.STARTING:
		case AgentSessionState.WORKING:
			return refinement.awaitingInput ? "waiting" : "working";
		case AgentSessionState.READY:
			return refinement.turnDoneUnopened ? "done" : "idle";
		case AgentSessionState.ERRORED:
			return "error";
		case AgentSessionState.STOPPED:
			return "stopped";
		case AgentSessionState.DISCONNECTED:
			// The owning Runner's link dropped — live truth is temporarily
			// unreachable but the session is not terminated (compass.proto: a
			// bounded reattach window governs recovery; only expiry falls to
			// ERRORED). Its own dot, never collapsed into `error` (that would
			// erase the recoverable-vs-fatal distinction the wire enum draws) or
			// `stopped` (that state is a deliberate teardown).
			return "disconnected";
		case AgentSessionState.UNSPECIFIED:
			return "idle";
		default: {
			// Exhaustiveness guard: a new AgentSessionState variant (an additive
			// #443 delta) reddens here at type-check. At runtime the coarse enum
			// is proto3-open, so a version-skewed daemon can send an unmodeled
			// numeric variant that bypasses the `never` check — throw rather than
			// return a raw enum that would break downstream Record<AgentState> lookups.
			const _exhaustive: never = sessionState;
			throw new Error(`Unhandled AgentSessionState: ${_exhaustive}`);
		}
	}
}
