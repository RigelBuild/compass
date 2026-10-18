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
import { ViewContext } from "../view-scope";
import { Bridge } from "./Bridge";

// Live roster join is not exercised by the offline fixture store.

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
			<StoreContext value={store}>
				<ViewContext value={store.focusedView()}>
					<Bridge />
				</ViewContext>
			</StoreContext>
		);
	});
	for (let i = 0; i < 20; i++) await Promise.resolve();
	return { store, container };
}

describe("Bridge — live roster (T4/T5)", () => {
	// A live agent matching an issue assignee must appear in the board roster.
	test("a Bridge swimlane renders a gutter row for a live agent", async () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER), wireAgentAccount("acc-compass-ui")],
			roster: [
				wireRosterEntry("acc-compass-ui", AgentPresence.WORKING, "cooking"),
			],
		});

		const { store, container } = await mountLive(fake);

		// The live join produced the acc-compass-ui agent (not STUB_AGENTS).
		expect(store.agents().map((a) => a.account.id)).toContain("acc-compass-ui");

		// It partitions into a Bridge swimlane gutter row carrying its handle.
		const gutter = container.querySelector<HTMLButtonElement>(".bridge-lane");
		expect(gutter).not.toBeNull();
		expect(gutter?.querySelector(".g-name")?.textContent).toBe(
			"acc-compass-ui",
		);
	});
});
