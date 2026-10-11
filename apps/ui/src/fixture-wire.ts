import {
	AccountSchema,
	AgentAccountSchema,
	AgentPresence,
	AgentSessionState,
	AgentSessionStatusSchema,
	create,
	EgressPosture,
	IssueSchema,
	IssueState,
	RosterEntrySchema,
	RuntimeTier,
	SubscribeEventsResponseSchema,
	SystemAccountSchema,
	UserAccountSchema,
} from "@compass/client";
import { STUB_ACCOUNTS } from "./comms-stub";
import type { Account as DomainAccount } from "./stub-data";
import { STUB_AGENTS } from "./stub-data";

export const REPLAY_AGENT_ID = "acc-compass-ui";

// The session stream reports WORKING; the comms stream carries the waiting refinement.
export const REPLAY_EVENTS = [
	create(SubscribeEventsResponseSchema, {
		seq: 1n,
		atUnixMs: 0n,
		instanceEpoch: 1n,
		payload: {
			case: "agentSessionStatus",
			value: create(AgentSessionStatusSchema, {
				sessionId: "session-fixture-replay",
				agentAccountId: REPLAY_AGENT_ID,
				state: AgentSessionState.WORKING,
				runtimeTier: RuntimeTier.PODMAN,
				egressPosture: EgressPosture.ARMED,
			}),
		},
	}),
];

export const REPLAY_BOARD = [
	create(IssueSchema, {
		id: "fixture-replay-issue",
		repo: "RigelBuild/compass",
		number: 2153,
		title: "Replay state change",
		state: IssueState.IN_REVIEW,
		assignee: REPLAY_AGENT_ID,
		tracker: {
			kind: "linear",
			id: "RIG-2153",
			status: "In Review",
		},
	}),
];

function wireAccount(account: DomainAccount) {
	const kind =
		account.kind === "agent"
			? {
					case: "agent" as const,
					value: create(AgentAccountSchema, {
						ownerUserId: account.ownerUserId ?? "",
						homeChannelId: account.homeChannelId ?? "",
						parentAgentId: account.parentAgentId ?? "",
					}),
				}
			: account.kind === "system"
				? { case: "system" as const, value: create(SystemAccountSchema, {}) }
				: { case: "user" as const, value: create(UserAccountSchema, {}) };
	return create(AccountSchema, {
		id: account.id,
		handle: account.handle,
		displayName: account.displayName,
		kind,
	});
}

export const REPLAY_COMMS_SNAPSHOT = {
	accounts: STUB_ACCOUNTS.map(wireAccount),
	roster: STUB_AGENTS.map((agent) =>
		create(RosterEntrySchema, {
			agentAccountId: agent.account.id,
			presence: AgentPresence.WORKING,
		}),
	),
};
