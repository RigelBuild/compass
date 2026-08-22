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
import { LeftSidebar } from "./LeftSidebar";

// The LIVE render path for the Agent workspaces tree (T4/T5): the board no
// longer reads STUB_AGENTS — it reads `store.agents()`, the reactive join of
// the comms accounts (identity) and the roster presence map (lifecycle +
// activity). These tests mount LeftSidebar over a live store (createFakeComms,
// no server) and defend what a live roster looks like on the surface:
//
//   - a joined agent renders as a `.tree-agent` leaf carrying its handle and,
//     when present, its activity note;
//   - a live agent has NO fixture `role` (role === undefined), so it renders
//     NO `.role-pip` — the pip is fixture-only chrome;
//   - the `.tree-empty` seam renders ONLY on a genuinely empty roster PAST the
//     first snapshot, never during the connect window (T5 / R4).

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

/** Mount LeftSidebar over a live store and drain the driver's snapshot
 *  round-trip so the first snapshot has been adopted before the body runs. */
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
			<StoreContext value={store}>
				<LeftSidebar />
			</StoreContext>
		);
	});
	for (let i = 0; i < 20; i++) await Promise.resolve();
	return { store, container };
}

const treeAgentNames = (c: HTMLElement): (string | null)[] =>
	[...c.querySelectorAll<HTMLElement>(".tree-agent .name")].map(
		(n) => n.textContent,
	);

describe("LeftSidebar — live roster (T4/T5)", () => {
	// A live agent joined from accounts + presence renders as a tree leaf with
	// its handle and activity note — and, being roleless, NO role-pip. The old
	// STUB_AGENTS render could never show this: every fixture agent has a role.
	test("renders the joined live agent tree with activity and no role-pip", async () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER), wireAgentAccount("acc-cook")],
			roster: [wireRosterEntry("acc-cook", AgentPresence.WORKING, "cooking")],
		});

		const { store, container } = await mountLive(fake);

		// The join produced exactly the one agent account (the user is filtered).
		expect(store.agents().map((a) => a.account.id)).toEqual(["acc-cook"]);
		expect(treeAgentNames(container)).toEqual(["acc-cook"]);
		// Its activity note rendered.
		expect(container.querySelector(".agent-activity")?.textContent).toBe(
			"cooking",
		);
		// A live agent has role === undefined → NO pip.
		expect(store.agents()[0]?.role).toBeUndefined();
		expect(container.querySelector(".tree-agent .role-pip")).toBeNull();
	});

	// T5: a genuinely empty roster PAST the first snapshot renders the
	// `.tree-empty` row instead of a bare (empty) tree.
	test("an empty live roster past the first snapshot renders .tree-empty", async () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER)],
			roster: [],
		});

		const { store, container } = await mountLive(fake);

		expect(store.firstSnapshotArrived()).toBe(true);
		expect(store.agents()).toEqual([]);
		expect(container.querySelector(".tree-empty")).not.toBeNull();
		expect(container.querySelectorAll(".tree-agent").length).toBe(0);
	});

	// T5: BEFORE the first snapshot the join is transiently empty (the live boot
	// starts from EMPTY_COMMS_STATE), so a bare length===0 would flash the empty
	// state during every connect window. Gated on firstSnapshotArrived, it does
	// not: no .tree-empty before the snapshot lands.
	test("a pre-first-snapshot live store renders no .tree-empty", () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER)],
			roster: [],
		});

		// Mount but do NOT drain the microtask queue — the boundary has not been
		// delivered, so the snapshot has not been adopted yet.
		let store!: AppStore;
		const { container } = render(() => {
			store = createAppStore({
				comms: fake.client,
				callerId: CALLER,
				queryClient: testQueryClient(),
			});
			return (
				<StoreContext value={store}>
					<LeftSidebar />
				</StoreContext>
			);
		});

		expect(store.firstSnapshotArrived()).toBe(false);
		expect(container.querySelector(".tree-empty")).toBeNull();
	});

	// T5: a non-empty roster renders the tree, never the empty seam.
	test("a non-empty live roster renders no .tree-empty", async () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER), wireAgentAccount("acc-cook")],
			roster: [wireRosterEntry("acc-cook", AgentPresence.WORKING, "cooking")],
		});

		const { container } = await mountLive(fake);

		expect(container.querySelector(".tree-empty")).toBeNull();
		expect(container.querySelectorAll(".tree-agent").length).toBeGreaterThan(0);
	});
});
