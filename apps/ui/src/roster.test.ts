import { describe, expect, test } from "bun:test";
import type { AgentPresenceInfo } from "./live/adapt";
import { joinAgents } from "./roster";
import type { Account } from "./stub-data";

// roster.ts is the pure join at the store's seam: it composes the board's live
// `Agent` view-models from the durable `accounts` (identity) and the ephemeral
// presence map (lifecycle/activity; T2). These tests defend the two contracts a
// naive join would silently break: the miss rule (R2/DL-194 — an account absent
// from the presence map is at-rest, so `"stopped"`, NOT the components'
// `?? "idle"` false-live fallback, and NOT undefined) and the preserved account
// order (the `agentTree` stable-order contract, stub-data.ts:372-377).

// A minimal agent-kind account — joinAgents reads only `id` and `kind`; the rest
// satisfies the shape.
function agentAccount(id: string): Account {
	return { id, handle: id, displayName: id, kind: "agent" };
}

function userAccount(id: string): Account {
	return { id, handle: id, displayName: id, kind: "user" };
}

describe("joinAgents", () => {
	test("projects a present entry's lifecycle and activity onto the agent", () => {
		const accounts = [agentAccount("acc-cook")];
		const presence = new Map<string, AgentPresenceInfo>([
			["acc-cook", { lifecycle: "working", activity: "cooking" }],
		]);

		const agents = joinAgents(accounts, presence);

		expect(agents).toHaveLength(1);
		expect(agents[0]?.account.id).toBe("acc-cook");
		expect(agents[0]?.lifecycle).toBe("working");
		expect(agents[0]?.activity).toBe("cooking");
		// The join is UI-agnostic about terminals: a live agent has none.
		expect(agents[0]?.terminals).toEqual([]);
	});

	test("a presence-map MISS projects lifecycle 'stopped' (R2/DL-194)", () => {
		// An account present in `accounts` but absent from the presence seed — a
		// snapshot-boundary race or an un-re-seeded accountChanged arrival — is an
		// at-rest/unstarted agent: the stopped dot, never the false-live idle dot.
		const agents = joinAgents([agentAccount("acc-new")], new Map());

		expect(agents[0]?.lifecycle).toBe("stopped");
		// The two failure modes the rule exists to forbid.
		expect(agents[0]?.lifecycle).not.toBe("idle");
		expect(agents[0]?.lifecycle).not.toBeUndefined();
		// No presence entry → no activity note.
		expect(agents[0]?.activity).toBeUndefined();
	});

	test("a present-but-UNSPECIFIED entry keeps lifecycle undefined (the defensive idle arm)", () => {
		// A present entry whose lifecycle is undefined (wire AgentPresence
		// UNSPECIFIED) is NOT a miss — it uses its info as-is, so lifecycle stays
		// undefined and the component's `?? "idle"` fallback applies. Only a
		// genuine map miss becomes "stopped".
		const presence = new Map<string, AgentPresenceInfo>([
			["acc-quiet", { lifecycle: undefined, activity: undefined }],
		]);

		const agents = joinAgents([agentAccount("acc-quiet")], presence);

		expect(agents[0]?.lifecycle).toBeUndefined();
		expect(agents[0]?.lifecycle).not.toBe("stopped");
	});

	test("preserves account order and filters out non-agent accounts", () => {
		// Deliberately interleave user/agent/system and put agents out of any
		// sorted order, so a re-sort or a filter that drops the wrong kind reddens.
		const accounts: Account[] = [
			agentAccount("acc-zulu"),
			userAccount("acc-me"),
			agentAccount("acc-alpha"),
			{ id: "acc-sys", handle: "sys", displayName: "sys", kind: "system" },
			agentAccount("acc-mike"),
		];
		const presence = new Map<string, AgentPresenceInfo>([
			["acc-alpha", { lifecycle: "idle" }],
		]);

		const agents = joinAgents(accounts, presence);

		// Only the three agent accounts, in their original input order.
		expect(agents.map((a) => a.account.id)).toEqual([
			"acc-zulu",
			"acc-alpha",
			"acc-mike",
		]);
		// The present one keeps its lifecycle; the two misses are "stopped".
		expect(agents.map((a) => a.lifecycle)).toEqual([
			"stopped",
			"idle",
			"stopped",
		]);
	});
});
