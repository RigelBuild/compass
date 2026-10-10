// The roster join: composes the board's live `Agent` view-models from durable accounts
// and the ephemeral presence and session-status maps.

import { agentDotState } from "./agent-state";
import type { AgentPresenceInfo } from "./live/adapt";
import type { AccountSession } from "./live/events";
import type { Account, Agent, RuntimeMarker } from "./stub-data";

/** Join agent accounts into view-models in account order. Lifecycle: a known
 *  session via `agentDotState`, else presence, else `"stopped"` (at rest).
 *  `runtime` has no default: a guessed posture could show an uncontained agent
 *  as contained. */
export function joinAgents(
	accounts: readonly Account[],
	presence: ReadonlyMap<string, AgentPresenceInfo>,
	runtime: ReadonlyMap<string, RuntimeMarker>,
	sessions: ReadonlyMap<string, AccountSession>,
	lastOpened: ReadonlyMap<string, number>,
): Agent[] {
	const agents: Agent[] = [];
	for (const account of accounts) {
		if (account.kind !== "agent") continue;
		const info = presence.get(account.id);
		const session = sessions.get(account.id);
		const lifecycle = session
			? agentDotState(session.state, {
					awaitingInput: info?.lifecycle === "waiting",
					turnDoneUnopened:
						session.turnEndedAtUnixMs !== undefined &&
						session.turnEndedAtUnixMs > (lastOpened.get(account.id) ?? 0),
				})
			: info
				? info.lifecycle
				: "stopped";
		agents.push({
			account,
			lifecycle,
			activity: info?.activity,
			runtime: runtime.get(account.id),
			terminals: [],
		});
	}
	return agents;
}
