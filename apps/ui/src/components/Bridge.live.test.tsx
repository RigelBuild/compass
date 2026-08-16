import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentPresence,
	create,
	RosterEntrySchema,
	UserAccountSchema,
} from "@compass/client";
import { render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import { createFakeComms, type FakeComms } from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { Bridge } from "./Bridge";

// The LIVE render path for the Bridge board (T4): the board no longer reads
// STUB_AGENTS — `boardAgentsOf`/`prRowGroups` read `store.agents()`, the
// reactive join of the comms accounts (identity) and the roster presence map
// (lifecycle + activity). The offline Bridge.test.tsx mounts a store with no
// comms, so `store.agents()` there is the STUB_AGENTS fixture and the live read
// is never exercised. This test mounts Bridge over a live store
// (createFakeComms, no server) and proves the live roster reaches a swimlane
// gutter row: a joined agent holding an active issue partitions into a Bridge
// swimlane, so `store.agents()` (not STUB_AGENTS) is what feeds the board.

const CALLER = "acc-me";

/** A user account on the wire (the caller). */
const wireUserAccount = (id: string) =>
	create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: { case: "user", value: create(UserAccountSchema, {}) },
	});

/** An agent-kind account on the wire — joins into an `Agent` view-model with no
 *  fixture role/model/cwd (the live roster's honest shape). */
const wireAgentAccount = (id: string) =>
	create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, { ownerUserId: CALLER }),
		},
	});

/** A roster presence row — the presence-map seed GetRoster serves. */
const wireRosterEntry = (
	agentAccountId: string,
	presence: AgentPresence,
	activity = "",
) => create(RosterEntrySchema, { agentAccountId, presence, activity });

/** Mount Bridge over a live store and drain the driver's snapshot round-trip so
 *  the first snapshot has been adopted before the body runs. */
async function mountLive(fake: FakeComms): Promise<{
	store: AppStore;
	container: HTMLElement;
}> {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext.Provider value={store}>
				<Bridge />
			</StoreContext.Provider>
		);
	});
	for (let i = 0; i < 20; i++) await Promise.resolve();
	return { store, container };
}

describe("Bridge — live roster (T4/T5)", () => {
	// Bridge defaults to the Issues tab + swimlane mode, so the gutter renders
	// immediately. A live agent whose account id matches an active STUB_ISSUES
	// assignee (acc-cook) partitions into a Bridge swimlane; its gutter row
	// carries the joined account's handle. This can only be green if the board
	// reads the live `store.agents()` — STUB_AGENTS has no acc-cook live join.
	test("a Bridge swimlane renders a gutter row for a live agent", async () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER), wireAgentAccount("acc-cook")],
			roster: [wireRosterEntry("acc-cook", AgentPresence.WORKING, "cooking")],
		});

		const { store, container } = await mountLive(fake);

		// The live join produced the acc-cook agent (not STUB_AGENTS).
		expect(store.agents().map((a) => a.account.id)).toContain("acc-cook");

		// It partitions into a Bridge swimlane gutter row carrying its handle.
		const gutter = container.querySelector<HTMLButtonElement>(".swim-gutter");
		expect(gutter).not.toBeNull();
		expect(gutter?.querySelector(".g-name")?.textContent).toBe("acc-cook");
	});
});
