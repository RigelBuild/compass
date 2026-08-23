import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
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

// T1 — Agent identity: separate co-addressed types + fixture reconciliation
// (design record `docs/designs/product/compass-architecture-lineage/design.md`,
// T1 §282-400). RED-FIRST: this suite is authored against T1's specified surface
// — the account-kind `Agent`/`Account` in stub-data.ts (design.md:325-362), the
// cached `account.homeChannelId` (:339,367-369), the derived `STUB_ACCOUNTS`
// (:365-367), `store.agentView(id)` (:370-372), and the re-keyed comms/session/
// issue fixtures (:294-317) — none of which exist yet. Every test here MUST
// fail now and pass once T1 lands. It asserts fixture invariants + the composed
// view-model contract, never plumbing; no test restates an implementation detail.
//
// NOTE on item "no kind field": the frozen record's new `Account` KEEPS
// `kind: "user" | "agent"` (design.md:334,337) — it is the OLD harness-flavoured
// axis that is dropped: the `harness` key (design.md:340 "no `harness` — dropped")
// and the old `Agent.kind: AgentKind` ("omp"|"claude"|… ). So the scans below
// target the harness axis (a `harness` key anywhere; a `kind` VALUED as a harness
// enum; a top-level `kind` on the new `Agent`) — NOT the surviving Account.kind.

// A pane-free reactive root for the store accessors (mirrors store.test.ts): the
// composed `agentView`/`agentSession` memos only compute inside an owner.
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

// The store method T1 adds (design.md:370) — declared as an augmentation so the
// call site type-checks both before T1 (cast) and after (no-op superset). At
// runtime before T1 the method is absent, so the call reddens.
type WithAgentView = AppStore & {
	agentView(id: string): Agent | undefined;
};
const agentView = (store: AppStore, id: string): Agent | undefined =>
	(store as WithAgentView).agentView(id);

// The four comms-only agent accounts that do NOT survive as separate identities
// (design.md:288-290, :304-306). `acc-cook` overlaps a board agent and SURVIVES.
const DROPPED_ACCOUNTS: Record<string, true> = {
	"acc-mercator": true,
	"acc-compass": true,
	"acc-xenophon": true,
	"acc-franklin": true,
};

// The old harness axis dropped by T1 (fork 2): the `kind: AgentKind` values on
// the pre-T1 `Agent`. Account.kind ("user"|"agent") and Channel.kind survive.
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
	// Every board agent carries a cached `account.homeChannelId` that (a) resolves
	// to a real DM channel and (b) equals what `agentDmChannel` computes live — so
	// the chat pane reads the home DM O(1) instead of a per-render search, and the
	// cached id can't drift stale from the channel it names. Pre-T1: 0/10 resolve
	// (no `account`), so this reddens; post-T1: 10/10.
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
	// The store's `agentView(id)` is the pure seam that composes the durable
	// `account` with the optional ephemeral `lifecycle` by shared account id. It
	// must return a view for every board agent (account carried through, lifecycle
	// preserved) and undefined for an id no agent owns.
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
	// `lifecycle` (the agent-object field SubscribeEvents feeds) and the opaque OMP
	// `AgentSession` (session-events-stub, read by account id) are independent: an agent
	// created with a lifecycle but never run has `agentView().lifecycle` present
	// while its session trace is empty. Reddens if the two are conflated (e.g. a
	// missing session nulling the lifecycle, or a synthetic session appearing).
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
			// Open the agent's workspace → observation reads its session by selected id.
			store.openAgent(candidate.account.id);
			expect(store.selectedAgentId()).toBe(candidate.account.id);
			expect(store.agentSession()).toBeUndefined();
		});
	});
});

// ── 4. STUB_ACCOUNTS is derived (design.md:365-367, :396) ─────────────────────
describe("STUB_ACCOUNTS derivation", () => {
	// STUB_ACCOUNTS is NOT hand-listed for the roster — it is `[MATT_ACCOUNT,
	// ...STUB_AGENTS.map(a => a.account), COMPASS_ACCOUNT]`. So it holds exactly
	// one caller (user), one account per agent in order (each the SAME object the
	// roster owns — referential identity proves derivation, not a parallel copy
	// that can drift), plus the one reserved `@compass` system sender appended
	// last (a comms account, not a roster agent — it never enters the agent tree).
	test("is the caller plus each agent's own account object, then the system sender", () => {
		const agentAccounts = STUB_AGENTS.map((agent) => agent.account);
		expect(STUB_ACCOUNTS.length).toBe(agentAccounts.length + 2);

		const caller = STUB_ACCOUNTS[0];
		expect(caller.id).toBe(CALLER_ID);
		expect(caller.kind).toBe("user");

		agentAccounts.forEach((account, i) => {
			expect(STUB_ACCOUNTS[i + 1]).toBe(account); // derived, not duplicated
		});

		// The system sender is last: exactly one, kind "system", handle "compass".
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
	// The old `agent-<handle>` id space is fully collapsed onto `acc-<handle>`: no
	// `agent-*` id may remain in any reconciled fixture (values OR object keys).
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

	// The old harness-flavoured `Agent.kind` ("omp"|"claude"|…) is gone. The
	// surviving `Account.kind` ("user"|"agent") and `Channel.kind` are NOT harness
	// values, so this bites only the dropped axis. The new `Agent` view-model
	// carries no top-level `kind` at all.
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
	// Every session key must name a surviving board agent, else the T4 log panel
	// has nothing to show (today keyed on non-surviving acc-franklin/acc-compass).
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
