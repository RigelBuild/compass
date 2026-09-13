// The roster join — the pure seam composing the board's live `Agent` view-models from
// durable `accounts` (identity) and the ephemeral `presence` map (lifecycle + activity, T2).
// The miss rule (R2 / DL-194): an account ABSENT from presence is an at-rest agent →
// `lifecycle: "stopped"`, NOT the components' `?? "idle"` fallback (which is a false-live dot).

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
