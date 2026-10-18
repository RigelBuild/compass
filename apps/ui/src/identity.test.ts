import { describe, expect, test } from "bun:test";
import { createRoot, flush } from "solid-js";
import { agentDmChannel } from "./comms";
import {
	STUB_ACCOUNTS,
	STUB_CHANNELS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
} from "./comms-stub";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import { type AppStore, CALLER_ID, createAppStore } from "./store";
import type { Account, Agent } from "./stub-data";
import { STUB_AGENTS, STUB_ISSUES } from "./stub-data";
import { testQueryClient } from "./test-support";

// Pin fixture reconciliation and the composed agent view model.

// Store accessors require an owner for these tests.
function withStore(body: (store: AppStore) => void): void {
	createRoot((dispose) => {
		const store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		try {
			body(store);
		} finally {
			dispose();
		}
	});
}

// Include agentView in the test type until it is part of AppStore.
type WithAgentView = AppStore & {
	agentView(id: string): Agent | undefined;
};
const agentView = (store: AppStore, id: string): Agent | undefined =>
	(store as WithAgentView).agentView(id);

// These comms-only accounts are dropped; acc-cook overlaps a surviving board agent.
const DROPPED_ACCOUNTS: Record<string, true> = {
	"acc-mercator": true,
	"acc-compass": true,
	"acc-xenophon": true,
	"acc-franklin": true,
};

// The dropped harness axis was Agent.kind; account and channel kinds remain.
const HARNESS_KINDS: Record<string, true> = {
	omp: true,
	claude: true,
	codex: true,
	opencode: true,
	seal: true,
};

const accountsById = (): Map<string, Account> =>
	new Map(STUB_ACCOUNTS.map((account) => [account.id, account]));

// The surviving agent id-space — the single account-id space T1 collapses onto
// (design.md:284). Derived from STUB_AGENTS (the roster source of truth,
// design.md:294) so the suite survives a fixture-roster choice.
const survivingAgentIds = (): Set<string> =>
	new Set(STUB_AGENTS.map((agent) => agent.account.id));

// ── Deep fixture walkers (item: dead id-space / dropped fields) ───────────────
const FIXTURES: readonly [string, unknown][] = [
	["STUB_AGENTS", STUB_AGENTS],
	["STUB_ACCOUNTS", STUB_ACCOUNTS],
	["STUB_CHANNELS", STUB_CHANNELS],
	["STUB_MESSAGES", STUB_MESSAGES],
	["STUB_SESSION_EVENTS", STUB_SESSION_EVENTS],
];

function stringValues(value: unknown): string[] {
	const out: string[] = [];
	const walk = (node: unknown): void => {
		if (typeof node === "string") out.push(node);
		else if (Array.isArray(node)) node.forEach(walk);
		else if (node && typeof node === "object")
			Object.values(node).forEach(walk);
	};
	walk(value);
	return out;
}

function objectKeys(value: unknown): string[] {
	const out: string[] = [];
	const walk = (node: unknown): void => {
		if (Array.isArray(node)) node.forEach(walk);
		else if (node && typeof node === "object") {
			for (const [key, child] of Object.entries(node)) {
				out.push(key);
				walk(child);
			}
		}
	};
	walk(value);
	return out;
}

function kindValues(value: unknown): string[] {
	const out: string[] = [];
	const walk = (node: unknown): void => {
		if (Array.isArray(node)) node.forEach(walk);
		else if (node && typeof node === "object") {
			const record = node as Record<string, unknown>;
			if (typeof record.kind === "string") out.push(record.kind);
			Object.values(record).forEach(walk);
		}
	};
	walk(value);
	return out;
}

// ── 1. Home channel resolves for every agent (design.md:391-393) ──────────────
describe("agent home channels (homeChannelId)", () => {
	// Every cached home channel must resolve to the agent's live DM channel.
	test("every agent's homeChannelId resolves and matches agentDmChannel", () => {
		expect(STUB_AGENTS.length).toBeGreaterThan(0); // non-vacuous walk
		const byId = accountsById();
		const offenders = STUB_AGENTS.filter((agent) => {
			const home = agent.account.homeChannelId;
			if (!home) return true;
			const channel = STUB_CHANNELS.find((c) => c.id === home);
			if (channel?.kind !== "dm") return true;
			const live = agentDmChannel(
				STUB_CHANNELS,
				agent.account.id,
				CALLER_ID,
				byId,
			);
			return live?.id !== home; // cached id must equal the live resolution
		});
		expect(offenders.map((a) => a.account?.handle ?? a.account?.id)).toEqual(
			[],
		);
	});
});

// ── 2. agentView composition (design.md:370-372, :393-394) ────────────────────
describe("agentView composition", () => {
	// agentView joins a durable account with optional lifecycle by account id.
	test("composes account + optional lifecycle for every board agent", () => {
		expect(STUB_AGENTS.length).toBeGreaterThan(0);
		withStore((store) => {
			for (const agent of STUB_AGENTS) {
				const view = agentView(store, agent.account.id);
				expect(view).toBeDefined();
				expect(view?.account.id).toBe(agent.account.id);
				expect(view?.lifecycle).toBe(agent.lifecycle);
			}
		});
	});

	test("returns undefined for an unknown id", () => {
		withStore((store) => {
			expect(agentView(store, "acc-not-a-real-agent")).toBeUndefined();
		});
	});
});

// ── 3. Honest optionality (design.md:342-344, :394-395) ───────────────────────
describe("honest lifecycle vs session optionality", () => {
	// Lifecycle belongs to the agent; session events remain independently optional.
	test("an agent with lifecycle but no session yields lifecycle present, empty session", () => {
		const candidate = STUB_AGENTS.find(
			(agent) =>
				agent.lifecycle !== undefined &&
				STUB_SESSION_EVENTS[agent.account.id] === undefined,
		);
		expect(candidate).toBeDefined();
		if (!candidate) return;
		withStore((store) => {
			expect(agentView(store, candidate.account.id)?.lifecycle).toBeDefined();
			// Opening the workspace reads its session by selected agent id.
			store.openAgent(candidate.account.id);
			flush();
			expect(store.selectedAgentId()).toBe(candidate.account.id);
			expect(store.focusedView().agentSession()).toBeUndefined();
		});
	});
});

// ── 4. STUB_ACCOUNTS is derived (design.md:365-367, :396) ─────────────────────
describe("STUB_ACCOUNTS derivation", () => {
	// Accounts are caller, agent roster, then system sender.
	test("is the caller plus each agent's own account object, then the system sender", () => {
		const agentAccounts = STUB_AGENTS.map((agent) => agent.account);
		expect(STUB_ACCOUNTS.length).toBe(agentAccounts.length + 2);

		const caller = STUB_ACCOUNTS[0];
		expect(caller.id).toBe(CALLER_ID);
		expect(caller.kind).toBe("user");

		agentAccounts.forEach((account, i) => {
			expect(STUB_ACCOUNTS[i + 1]).toBe(account); // derived, not duplicated
		});

		// The final entry is the sole system sender.
		const system = STUB_ACCOUNTS[STUB_ACCOUNTS.length - 1];
		expect(system.kind).toBe("system");
		expect(system.handle).toBe("compass");

		const ids = STUB_ACCOUNTS.map((account) => account.id);
		expect(new Set(ids).size).toBe(ids.length); // no dup accounts
		const users = STUB_ACCOUNTS.filter((account) => account.kind === "user");
		expect(users.map((u) => u.id)).toEqual([CALLER_ID]); // exactly one caller
		const systems = STUB_ACCOUNTS.filter((a) => a.kind === "system");
		expect(systems).toHaveLength(1); // exactly one system sender
	});
});

// ── 5. No dead id-space / dropped fields (design.md:284, :340, :396-397) ───────
describe("dead id-space and dropped fields", () => {
	// No fixture id or key uses the retired agent-handle namespace.
	test("no agent-* id survives in any fixture", () => {
		const offenders: string[] = [];
		for (const [name, fixture] of FIXTURES) {
			const strings = [...stringValues(fixture), ...objectKeys(fixture)];
			for (const s of strings) {
				if (/^agent-/.test(s)) offenders.push(`${name}: ${s}`);
			}
		}
		expect(offenders).toEqual([]);
	});

	// The `harness` interop label is dropped from Account (fork 2, design.md:340).
	test("no `harness` field survives in any fixture", () => {
		const offenders = FIXTURES.filter(([, fixture]) =>
			objectKeys(fixture).includes("harness"),
		).map(([name]) => name);
		expect(offenders).toEqual([]);
	});

	// Agent no longer has a harness kind; account and channel kinds remain.
	test("the dropped harness `kind` axis is gone (Account.kind is kept)", () => {
		const offenders: string[] = [];
		for (const [name, fixture] of FIXTURES) {
			for (const value of kindValues(fixture)) {
				if (HARNESS_KINDS[value]) offenders.push(`${name}: ${value}`);
			}
		}
		expect(offenders).toEqual([]);
		for (const agent of STUB_AGENTS) {
			expect(Object.hasOwn(agent, "kind")).toBe(false);
		}
	});
});

// ── 6. Sessions re-keyed onto survivors (design.md:300-303, :398) ─────────────
describe("STUB_SESSION_EVENTS re-keying", () => {
	// Session keys must resolve to a surviving roster agent.
	test("every session key resolves to a surviving agent id", () => {
		const surviving = survivingAgentIds();
		const keys = Object.keys(STUB_SESSION_EVENTS);
		expect(keys.length).toBeGreaterThan(0); // the panel has content
		const orphans = keys.filter((key) => !surviving.has(key));
		expect(orphans).toEqual([]);
	});
});

// ── 7. No dangling comms refs (design.md:304-306, :399-400) ───────────────────
describe("comms reference integrity", () => {
	const commsRefs = (): string[] => [
		...STUB_MESSAGES.map((message) => message.authorAccountId),
		...STUB_CHANNELS.flatMap((channel) => channel.memberAccountIds),
	];

	// No message author, channel member, or group-DM member may point at one of
	// the four dropped comms accounts (acc-cook is NOT dropped — it overlaps a
	// board agent). The content they authored is re-homed onto survivors.
	test("no message author or channel/group-DM member references a dropped account", () => {
		const dropped = commsRefs().filter((id) => DROPPED_ACCOUNTS[id]);
		expect(dropped).toEqual([]);
	});

	// And every referenced account id must resolve in STUB_ACCOUNTS — re-authoring
	// onto a typo'd or missing id would dangle silently.
	test("every referenced comms account id resolves in STUB_ACCOUNTS", () => {
		const known = new Set(STUB_ACCOUNTS.map((account) => account.id));
		const dangling = commsRefs().filter((id) => !known.has(id));
		expect(dangling).toEqual([]);
	});
});

// ── 8. Assignee migration (design.md:317, :400) ───────────────────────────────
describe("issue assignee migration", () => {
	// Every assigned issue must name a surviving agent id (old ids were
	// `agent-<handle>`); an unassigned issue (null) is left untouched.
	test("every issue assignee resolves to a surviving agent id", () => {
		const surviving = survivingAgentIds();
		const assignees = STUB_ISSUES.map((w) => w.assignee).filter(
			(a): a is string => a !== null,
		);
		expect(assignees.length).toBeGreaterThan(0); // non-vacuous
		const orphans = assignees.filter((id) => !surviving.has(id));
		expect(orphans).toEqual([]);
	});
});
