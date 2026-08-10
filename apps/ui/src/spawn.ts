// The spawn/stop phase machine — the store-internal core of the spawn axis
// (design compass-spawn-control T1).
//
// A `SessionBinding` holds one started workstream's wire-lifecycle bookkeeping
// (the SpawnAgent request id, the resolved session id, and the phase). It is
// store-internal, keyed by workstreamId (= Issue.id) per DL-164 — never a
// fixture shape (stub-data.ts Issue/Agent stay frozen). The composite
// SpawnAgent RPC (DL-166) means the client sees spawn as one call, so the phase
// machine has no provisioning-vs-starting split.
//
// These reducers are pure functions of the binding (immutable `{...b}`
// updates), with no Solid imports, so the phase machine is unit-testable in
// isolation. `bindingDotState` is the pre-reconcile dot the board reads until an
// attributed live `AgentSessionState` arrives (Board state model precedence).

import type { AgentSessionState } from "@compass/client";
import type { AgentState, Priority } from "./stub-data";

/** One started workstream's wire-lifecycle bookkeeping. Store-internal —
 *  never a fixture shape (stub-data.ts Issue/Agent stay frozen). Keyed by
 *  workstreamId (= Issue.id) in the store map (per DL-164). A binding exists
 *  only for a card the agent was actually started on. */
export interface SessionBinding {
	/** The card this start targets — the store map key (Issue.id). */
	readonly workstreamId: string;
	/** The agent account the start is for — the SpawnAgent input, and the join
	 *  key to a pushed AgentSessionStatus.agent_account_id (per DL-167). */
	readonly agentAccountId: string;
	/** Set once SpawnAgent resolves; the cursor for Stop/Reload/status. */
	readonly sessionId?: string;
	/** The SpawnAgent idempotency key (one per spawn; a retry re-sends it
	 *  verbatim). */
	readonly clientRequestId: string;
	readonly phase: SpawnPhase;
	/** Captured from `SpawnSpec.initialPrompt` at `beginSpawn`, so
	 *  `bindingDotState` stays a pure function of the binding alone (an empty
	 *  prompt = "start idle"). */
	readonly initialPrompt: string;
	/** Human-readable failure, set only in the two failure phases. */
	readonly error?: string;
}

export type SpawnPhase =
	| "spawning" // SpawnAgent in flight (server runs Provision→Start)
	| "running" // sessionId live; live AgentSessionState now wins the dot
	| "spawn-failed" // SpawnAgent errored; retry re-sends the same request id
	| "stopping" // Stop in flight
	| "stop-failed" // Stop returned an error; session still held
	| "stopped";

export interface SpawnSpec {
	/** The target agent account — the card's assignee. */
	readonly agentAccountId: string;
	/** Empty = start idle. */
	readonly initialPrompt: string;
	/** The existing card to start the agent on (per DL-164). */
	readonly workstreamId: string;
}

/** The board-only add-a-workstream input — no prompt, no lifecycle fields. */
export interface WorkstreamSpec {
	/** The agent to assign the card to. The "＋ new agent" path carries a handle
	 *  (CreateAgent's input) instead of an account id (its output), per DL-164. */
	readonly agent:
		| { readonly kind: "existing"; readonly agentAccountId: string }
		| {
				readonly kind: "new";
				readonly handle: string;
				readonly displayName?: string;
		  };
	readonly title: string;
	readonly priority: Priority;
}

/** Mint a fresh binding in `spawning`, capturing the spec's prompt so the dot
 *  stays pure over the binding. */
export function beginSpawn(spec: SpawnSpec, requestId: string): SessionBinding {
	return {
		workstreamId: spec.workstreamId,
		agentAccountId: spec.agentAccountId,
		clientRequestId: requestId,
		initialPrompt: spec.initialPrompt,
		phase: "spawning",
	};
}

/** `spawning` → `running` (the composite SpawnAgent resolved). */
export function applySpawned(
	b: SessionBinding,
	sessionId: string,
): SessionBinding {
	if (b.phase !== "spawning") {
		throw new Error(
			`applySpawned: illegal transition from ${b.phase} (requires spawning)`,
		);
	}
	return { ...b, phase: "running", sessionId };
}

/** `running` | `stop-failed` → `stopping`. The `stop-failed` arm re-enters
 *  stopping from the still-held session (a retry, not a new spawn — DL-169). */
export function beginStop(b: SessionBinding): SessionBinding {
	if (b.phase !== "running" && b.phase !== "stop-failed") {
		throw new Error(
			`beginStop: illegal transition from ${b.phase} (requires running or stop-failed)`,
		);
	}
	return { ...b, phase: "stopping" };
}

/** Both failure sites map to a real phase: `spawn` → `spawn-failed`,
 *  `stopping` → `stop-failed`. */
export function applySpawnError(
	b: SessionBinding,
	at: "spawn" | "stopping",
	error: string,
): SessionBinding {
	return {
		...b,
		phase: at === "spawn" ? "spawn-failed" : "stop-failed",
		error,
	};
}

/** `running` | `stopping` | `stop-failed` → `stopped`. Never clears the
 *  binding — a stopped card stays restartable (DL-168). */
export function applyStopped(b: SessionBinding): SessionBinding {
	if (
		b.phase !== "running" &&
		b.phase !== "stopping" &&
		b.phase !== "stop-failed"
	) {
		throw new Error(
			`applyStopped: illegal transition from ${b.phase} (requires running, stopping, or stop-failed)`,
		);
	}
	return { ...b, phase: "stopped" };
}

/** The reconcile reducer for attributed live statuses. It does NOT widen
 *  `SpawnPhase` — the live state lands on the agent's lifecycle and
 *  `agentDotState` renders it (Board state model precedence). Only meaningful
 *  once `running` (the store invokes it only then), so the live state is
 *  discarded and the phase is left untouched — never derived from `_state`,
 *  and never forced onto a binding that has since left `running`. */
export function applySessionStatus(
	b: SessionBinding,
	_state: AgentSessionState,
): SessionBinding {
	return { ...b };
}

/** The pre-reconcile dot, total over every phase. The board reads it until an
 *  attributed live status arrives for the agent (the switch is
 *  attribution-gated). An empty-prompt `running` is "start idle", so a working
 *  dot would be knowably wrong at mint time. */
export function bindingDotState(b: SessionBinding): AgentState {
	switch (b.phase) {
		case "spawning":
		case "stopping":
			return "working";
		case "spawn-failed":
		case "stop-failed":
			return "error";
		case "stopped":
			return "stopped";
		case "running":
			return b.initialPrompt ? "working" : "idle";
		default: {
			const _exhaustive: never = b.phase;
			throw new Error(`bindingDotState: unhandled phase ${_exhaustive}`);
		}
	}
}
