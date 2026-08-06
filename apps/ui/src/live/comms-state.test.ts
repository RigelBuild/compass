import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	ChannelGroupSchema,
	ChannelKind,
	ChannelSchema,
	create,
	UserAccountSchema,
} from "@compass/client";
import type { Account, Channel, ChannelGroup, Message } from "../comms-stub";
import {
	applyEvent,
	type CommsEvent,
	type CommsSnapshot,
	type CommsState,
	EMPTY_COMMS_STATE,
	reduceSnapshot,
} from "./comms-state";

// comms-state.ts is the pure live-comms reducer: reduceSnapshot turns a raw
// snapshot into a fresh CommsState (wire→domain adaptation + (atUnixMs,id)
// message sort), and applyEvent folds one already-decoded stream event into the
// next state. The reducer is now CURSOR-FREE (SEA-1333 correction): the stream
// `seq` and instance epoch are driver-local transport bookkeeping in stream.ts,
// deliberately kept OUT of the reduced domain state, so there is no cursor to
// carry or assert here. These tests defend the contracts a plausible refactor
// could silently break — dedup-by-id (the at-least-once redelivery guard),
// (atUnixMs,id) insert ordering, per-collection upsert, and value immutability —
// by asserting the observed next state, never restating the reducer body. The
// wire→domain derivation itself is covered exhaustively in adapt.test.ts; here
// one integration case proves reduceSnapshot wires it.

// ── Domain fixtures (applyEvent consumes pre-adapted domain objects) ──────────
const CALLER = "acc-me";

function msg(id: string, atUnixMs: number, text = id): Message {
	return {
		id,
		topicId: "top-1",
		authorAccountId: CALLER,
		atUnixMs,
		blocks: [{ kind: "text", text }],
	};
}
function chan(id: string, over: Partial<Channel> = {}): Channel {
	return {
		id,
		name: id,
		kind: "channel",
		memberAccountIds: [],
		membership: "none",
		postPolicy: "open",
		...over,
	};
}
function group(id: string, over: Partial<ChannelGroup> = {}): ChannelGroup {
	return { id, name: id, visibility: "owner", ...over };
}
function account(id: string, over: Partial<Account> = {}): Account {
	return { id, handle: id, displayName: id, kind: "user", ...over };
}

// A non-empty starting state for the per-event transitions — one row in each
// collection so an "other collections untouched" assertion has something to hold.
function baseState(): CommsState {
	return {
		accounts: [account("acc-a")],
		channelGroups: [group("grp-a")],
		channels: [chan("chan-a")],
		topics: [],
		messages: [msg("m-mid", 100)],
	};
}

describe("reduceSnapshot", () => {
	test("adapts wire collections and sorts messages by (atUnixMs,id)", () => {
		// Caller + one agent whose home DM the caller is subscribed to → that
		// channel must derive alwaysSubscribed=true (the caller-aware adaptation
		// reduceSnapshot performs; the full derivation lives in adapt.test.ts).
		const homeId = "chan-home-cook";
		const snap: CommsSnapshot = {
			accounts: [
				create(AccountSchema, {
					id: CALLER,
					handle: "me",
					displayName: "Me",
					kind: { case: "user", value: create(UserAccountSchema, {}) },
				}),
				create(AccountSchema, {
					id: "acc-cook",
					handle: "cook",
					displayName: "Cook",
					kind: {
						case: "agent",
						value: create(AgentAccountSchema, {
							ownerUserId: CALLER,
							homeChannelId: homeId,
						}),
					},
				}),
			],
			channelGroups: [create(ChannelGroupSchema, { id: "grp-me", name: "me" })],
			channels: [
				create(ChannelSchema, {
					id: homeId,
					name: "cook",
					kind: ChannelKind.DM,
					memberAccountIds: [CALLER, "acc-cook"],
					subscriberAccountIds: [CALLER, "acc-cook"],
				}),
			],
			topics: [],
			// Deliberately out of (atUnixMs,id) order so the sort is observable.
			messages: [msg("m2", 200), msg("m1", 100), msg("m3", 100)],
		};

		const state = reduceSnapshot(CALLER, snap);

		// Messages sorted by atUnixMs, then id (the (100,m1) < (100,m3) tiebreak).
		expect(state.messages.map((m) => m.id)).toEqual(["m1", "m3", "m2"]);
		// Wire adaptation ran: two flat domain accounts + the group name.
		expect(state.accounts.map((a) => a.id)).toEqual([CALLER, "acc-cook"]);
		expect(state.channelGroups[0]?.name).toBe("me");
		// Caller-aware channel adaptation: the subscribed home DM is implicitly,
		// non-togglably subscribed — proves callerId + accounts were threaded in.
		expect(state.channels[0]?.membership).toBe("subscribed");
		expect(state.channels[0]?.alwaysSubscribed).toBe(true);
	});
});

describe("applyEvent — message dedup + ordering", () => {
	test("messagePosted for a known id REPLACES the row (no duplicate)", () => {
		// At-least-once redelivery: the same id arriving twice must not duplicate.
		const posted: CommsEvent = {
			kind: "messagePosted",
			message: msg("m1", 100, "first"),
		};
		const s1 = applyEvent(EMPTY_COMMS_STATE, posted);
		const redelivered: CommsEvent = {
			kind: "messagePosted",
			message: msg("m1", 100, "second"),
		};
		const s2 = applyEvent(s1, redelivered);

		expect(s2.messages).toHaveLength(1);
		expect(s2.messages[0]?.blocks).toEqual([{ kind: "text", text: "second" }]);
	});

	test("messageUpdated replaces the row's content in place", () => {
		const s1 = applyEvent(EMPTY_COMMS_STATE, {
			kind: "messagePosted",
			message: msg("m1", 100, "orig"),
		});
		const s2 = applyEvent(s1, {
			kind: "messageUpdated",
			message: msg("m1", 100, "edited"),
		});
		expect(s2.messages).toHaveLength(1);
		expect(s2.messages[0]?.blocks).toEqual([{ kind: "text", text: "edited" }]);
	});

	test("messagePosted inserts into (atUnixMs,id) sorted position, not append", () => {
		// Post a newer message, then an OLDER one — the older must land before it.
		const s1 = applyEvent(EMPTY_COMMS_STATE, {
			kind: "messagePosted",
			message: msg("m-late", 200),
		});
		const s2 = applyEvent(s1, {
			kind: "messagePosted",
			message: msg("m-early", 100),
		});
		expect(s2.messages.map((m) => m.id)).toEqual(["m-early", "m-late"]);
	});
});

describe("applyEvent — per-collection upsert", () => {
	test("channelChanged appends a new id and leaves other collections untouched", () => {
		const base = baseState();
		const next = applyEvent(base, {
			kind: "channelChanged",
			channel: chan("chan-b"),
		});
		expect(next.channels.map((c) => c.id)).toEqual(["chan-a", "chan-b"]);
		// Other collections keep their identity (structural sharing).
		expect(next.accounts).toBe(base.accounts);
		expect(next.channelGroups).toBe(base.channelGroups);
		expect(next.messages).toBe(base.messages);
	});

	test("channelChanged for an existing id replaces it in place", () => {
		const base = baseState();
		const next = applyEvent(base, {
			kind: "channelChanged",
			channel: chan("chan-a", { name: "renamed" }),
		});
		expect(next.channels).toHaveLength(1);
		expect(next.channels[0]?.name).toBe("renamed");
	});

	test("channelGroupChanged upserts into channelGroups only", () => {
		const base = baseState();
		const next = applyEvent(base, {
			kind: "channelGroupChanged",
			group: group("grp-b"),
		});
		expect(next.channelGroups.map((g) => g.id)).toEqual(["grp-a", "grp-b"]);
		expect(next.accounts).toBe(base.accounts);
		expect(next.channels).toBe(base.channels);
	});

	test("accountChanged upserts into accounts only", () => {
		const base = baseState();
		const next = applyEvent(base, {
			kind: "accountChanged",
			account: account("acc-b"),
		});
		expect(next.accounts.map((a) => a.id)).toEqual(["acc-a", "acc-b"]);
		expect(next.channels).toBe(base.channels);
		expect(next.channelGroups).toBe(base.channelGroups);
	});
});

describe("applyEvent — channel removal", () => {
	test("channelRemoved drops the channel row and leaves other collections untouched", () => {
		// The ghost-channel signal: the caller's own removal arrives as a
		// channelChanged the driver decodes to channelRemoved; applying it must
		// drop the row rather than leave a lingering membership='none' channel.
		const base = baseState();
		const next = applyEvent(base, {
			kind: "channelRemoved",
			channelId: "chan-a",
		});
		expect(next.channels).toEqual([]);
		// Only channels changed — the other collections keep their identity.
		expect(next.accounts).toBe(base.accounts);
		expect(next.channelGroups).toBe(base.channelGroups);
		expect(next.messages).toBe(base.messages);
	});

	test("channelRemoved for an absent id is a same-ref no-op on channels", () => {
		// Removing an id that isn't present must not churn the array — the reducer
		// returns the same channels reference so downstream signals don't refire.
		const base = baseState();
		const next = applyEvent(base, {
			kind: "channelRemoved",
			channelId: "chan-missing",
		});
		expect(next.channels).toBe(base.channels);
	});
});

describe("applyEvent — immutability", () => {
	test("returns a fresh state and never mutates the input's arrays", () => {
		const base = baseState();
		const priorMessages = base.messages;
		const priorLen = base.messages.length;

		const next = applyEvent(base, {
			kind: "messagePosted",
			message: msg("m-new", 50),
		});

		// A new state object with a new messages array…
		expect(next).not.toBe(base);
		expect(next.messages).not.toBe(base.messages);
		// …and the input's array is untouched (same reference, same length).
		expect(base.messages).toBe(priorMessages);
		expect(base.messages).toHaveLength(priorLen);
	});
});
