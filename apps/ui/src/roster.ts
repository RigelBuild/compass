// The roster join — the pure seam that composes the board's live `Agent`
// view-models from the two independent sources the store already holds: the
// durable `accounts` (identity, from the comms snapshot / accountChanged
// stream) and the ephemeral `presence` map (lifecycle + activity, seeded by
// GetRoster and tailed by AgentPresenceChanged; T2). Sibling of `board.ts`,
// same shape: pure over injected inputs (no fixture import, no store), so the
// join contract is unit-testable and the store just wires it into a memo.
//
// The miss rule (R2 / DL-194): an account present in `accounts` but ABSENT from
// the presence map — a snapshot-boundary race, or a post-snapshot
// `accountChanged` arrival never re-seeded — is an at-rest/unstarted agent, so
// it maps to `lifecycle: "stopped"`, mirroring the server's absent→OFFLINE→
// stopped default at this client seam. It must NOT fall through to the
// components' `lifecycle ?? "idle"` fallback, which would render a false-live
// grey idle dot; the first real AgentPresenceChanged upserts the map and flips
// it. A PRESENT-but-UNSPECIFIED entry (its `lifecycle` is undefined) keeps
// undefined → the defensive idle arm; only a genuine miss becomes "stopped".

import type { AgentPresenceInfo } from "./live/adapt";
import type { Account, Agent, RuntimeMarker } from "./stub-data";

/** Join the durable agent accounts with their ephemeral presence into the
 *  board's `Agent` view-models. Filters to `kind === "agent"`, PRESERVES
 *  account order (the `agentTree` stable-order contract, stub-data.ts:372-377),
 *  and composes `{ account, lifecycle, activity, terminals: [] }` per agent. A
 *  presence-map MISS → `lifecycle: "stopped"` (R2 / DL-194); a present entry
 *  uses its `info.lifecycle` as-is (undefined stays undefined).
 *
 *  `runtime` is a third independent source, keyed the same way: it arrives on
 *  the session-status stream rather than the presence one, so an agent with no
 *  session status yet carries no marker. A miss stays undefined — unlike
 *  lifecycle there is no safe default, because guessing a posture would render
 *  an uncontained agent as contained. It is required rather than defaulted so
 *  rendering no markers is always a decision at the callsite. */
export function joinAgents(
	accounts: readonly Account[],
	presence: ReadonlyMap<string, AgentPresenceInfo>,
	runtime: ReadonlyMap<string, RuntimeMarker>,
): Agent[] {
	const agents: Agent[] = [];
	for (const account of accounts) {
		if (account.kind !== "agent") continue;
		const info = presence.get(account.id);
		agents.push({
			account,
			lifecycle: info ? info.lifecycle : "stopped",
			activity: info?.activity,
			runtime: runtime.get(account.id),
			terminals: [],
		});
	}
	return agents;
}
