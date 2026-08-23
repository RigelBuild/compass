import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentPresence,
	ChannelKind,
	ChannelSchema,
	create,
	RosterEntrySchema,
	UserAccountSchema,
} from "@compass/client";
import { render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import { createFakeComms, type FakeComms } from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { AgentView } from "./AgentView";

// The LIVE AgentView header (T4): a live agent has no fixture `model`/`cwd`
// (both optional on the joined `Agent`), so the `.av-model` / `.av-cwd` spans
// are each gated on a `<Show>` — an absent field renders NOTHING rather than an
// empty span. The offline fixture agents always carry both, so only a
// live-shaped store can exercise the absent arm.

const CALLER = "acc-me";
const AGENT = "acc-cook";
const HOME = "dm-cook";

const wireUserAccount = (id: string) =>
	create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: { case: "user", value: create(UserAccountSchema, {}) },
	});

/** An agent-kind account whose home DM is `HOME` — the workspace centers on it
 *  when opened. Joins to an `Agent` with no model/cwd. */
const wireAgentAccount = (id: string) =>
	create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, {
				ownerUserId: CALLER,
				homeChannelId: HOME,
			}),
		},
	});

/** The agent's home DM channel — so `openAgent` has a channel to center on. */
const wireDm = (id: string) =>
	create(ChannelSchema, {
		id,
		name: id,
		kind: ChannelKind.DM,
		memberAccountIds: [CALLER, AGENT],
		subscriberAccountIds: [CALLER, AGENT],
	});

const wireRosterEntry = (agentAccountId: string, presence: AgentPresence) =>
	create(RosterEntrySchema, { agentAccountId, presence, activity: "" });

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
				<AgentView />
			</StoreContext>
		);
	});
	for (let i = 0; i < 20; i++) await Promise.resolve();
	return { store, container };
}

describe("AgentView — live agent header (T4)", () => {
	test("an agent without model/cwd renders no .av-model / .av-cwd span", async () => {
		const fake = createFakeComms({
			accounts: [wireUserAccount(CALLER), wireAgentAccount(AGENT)],
			channels: [wireDm(HOME)],
			roster: [wireRosterEntry(AGENT, AgentPresence.WORKING)],
		});

		const { store, container } = await mountLive(fake);
		store.openAgent(AGENT);
		for (let i = 0; i < 20; i++) await Promise.resolve();

		// The live agent resolved and carries neither fixture field.
		const agent = store.selectedAgent();
		expect(agent?.account.id).toBe(AGENT);
		expect(agent?.model).toBeUndefined();
		expect(agent?.cwd).toBeUndefined();

		// The header rendered (the agent is selected) but the two optional spans
		// are absent — not present-and-empty.
		expect(container.querySelector(".av-header")).not.toBeNull();
		expect(container.querySelector(".av-model")).toBeNull();
		expect(container.querySelector(".av-cwd")).toBeNull();
	});
});
