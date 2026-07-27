import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	ChannelGroupSchema,
	ChannelGroupVisibility,
	ChannelKind,
	ChannelSchema,
	create,
	UserAccountSchema,
} from "@compass/client";
import type { Account } from "../stub-data";
import {
	adaptAccount,
	adaptChannel,
	adaptChannelGroup,
	agentHomeChannelIds,
	deriveMembership,
} from "./adapt";

// adapt.ts is the wire→domain adapter: it flattens protobuf-es oneofs/enums into
// the UI's flat domain shapes and derives the caller-relative fields the wire
// does not carry (membership, always-subscribed). These tests defend the
// structural bridges a plausible refactor could silently break — oneof
// flattening, empty-string→undefined normalization, enum totality, and the
// join/subscribe derivation — by constructing real generated messages and
// asserting the mapped domain value, never restating the mapping. Fixtures are
// built per-case via `create(...)` so an assertion pins behavior, not defaults.

// ── Wire fixture builders — only the fields a case exercises carry meaning; the
//    generated defaults fill the rest so a shape change never perturbs an
//    assertion. ────────────────────────────────────────────────────────────────
function wireUser(id: string, handle: string, displayName: string) {
	return create(AccountSchema, {
		id,
		handle,
		displayName,
		kind: { case: "user", value: create(UserAccountSchema, {}) },
	});
}
function wireAgent(
	id: string,
	handle: string,
	displayName: string,
	ownerUserId: string,
	homeChannelId: string,
) {
	return create(AccountSchema, {
		id,
		handle,
		displayName,
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, { ownerUserId, homeChannelId }),
		},
	});
}

describe("adaptAccount", () => {
	test("user account maps to a flat domain user with no agent fields", () => {
		const r = adaptAccount(wireUser("acc-matt", "matt", "Matt W"));
		expect(r).toEqual({
			id: "acc-matt",
			handle: "matt",
			displayName: "Matt W",
			kind: "user",
		});
		// The agent-only fields must be absent, not present-and-undefined: a user
		// carries no ownership or home DM.
		expect(r).not.toHaveProperty("ownerUserId");
		expect(r).not.toHaveProperty("homeChannelId");
	});

	test("agent account lifts ownerUserId and homeChannelId from the agent arm", () => {
		const r = adaptAccount(
			wireAgent("acc-cook", "cook", "Cook", "acc-matt", "chan-home-cook"),
		);
		expect(r).toEqual({
			id: "acc-cook",
			handle: "cook",
			displayName: "Cook",
			kind: "agent",
			ownerUserId: "acc-matt",
			homeChannelId: "chan-home-cook",
		});
	});

	test("agent with empty-string homeChannelId normalizes to undefined", () => {
		const r = adaptAccount(
			wireAgent("acc-cook", "cook", "Cook", "acc-matt", ""),
		);
		expect(r.kind).toBe("agent");
		// Empty-string wire "unset" → domain absent, NOT the literal "".
		expect(r.homeChannelId).toBeUndefined();
		expect(r.homeChannelId).not.toBe("");
		// ownerUserId is unaffected by the home-channel normalization.
		expect(r.ownerUserId).toBe("acc-matt");
	});

	test("malformed account with unset kind oneof falls back to an inert user", () => {
		// No `kind` set → protobuf-es leaves the oneof as {case: undefined}.
		const w = create(AccountSchema, {
			id: "acc-bad",
			handle: "bad",
			displayName: "Bad Row",
		});
		expect(w.kind.case).toBeUndefined();
		let r: Account;
		expect(() => {
			r = adaptAccount(w);
		}).not.toThrow();
		// Inert: a user with no ownership/home DM rather than a thrown roster.
		// biome-ignore lint/style/noNonNullAssertion: assigned in the no-throw block above.
		expect(r!).toEqual({
			id: "acc-bad",
			handle: "bad",
			displayName: "Bad Row",
			kind: "user",
		});
	});

	test("id/handle/displayName pass through verbatim across kinds", () => {
		const u = adaptAccount(wireUser("id-u", "h-u", "Disp U"));
		expect([u.id, u.handle, u.displayName]).toEqual(["id-u", "h-u", "Disp U"]);
		const a = adaptAccount(
			wireAgent("id-a", "h-a", "Disp A", "acc-owner", "chan-a"),
		);
		expect([a.id, a.handle, a.displayName]).toEqual(["id-a", "h-a", "Disp A"]);
	});
});

describe("adaptChannelGroup", () => {
	test("maps id/name verbatim", () => {
		const r = adaptChannelGroup(
			create(ChannelGroupSchema, { id: "grp-matt", name: "matt" }),
		);
		expect(r.id).toBe("grp-matt");
		expect(r.name).toBe("matt");
	});

	for (const [wire, expected] of [
		[ChannelGroupVisibility.OWNER, "owner"],
		[ChannelGroupVisibility.SHARED, "shared"],
	] as const) {
		test(`visibility ${ChannelGroupVisibility[wire]} → "${expected}"`, () => {
			const r = adaptChannelGroup(
				create(ChannelGroupSchema, { id: "g", name: "g", visibility: wire }),
			);
			expect(r.visibility).toBe(expected);
		});
	}

	test("empty parentGroupId/ownerUserId normalize to undefined", () => {
		const r = adaptChannelGroup(
			create(ChannelGroupSchema, {
				id: "g",
				name: "g",
				parentGroupId: "",
				ownerUserId: "",
			}),
		);
		expect(r.parentGroupId).toBeUndefined();
		expect(r.ownerUserId).toBeUndefined();
	});

	test("non-empty parentGroupId/ownerUserId pass through", () => {
		const r = adaptChannelGroup(
			create(ChannelGroupSchema, {
				id: "g",
				name: "g",
				parentGroupId: "grp-parent",
				ownerUserId: "acc-matt",
			}),
		);
		expect(r.parentGroupId).toBe("grp-parent");
		expect(r.ownerUserId).toBe("acc-matt");
	});
});

describe("deriveMembership", () => {
	const caller = "acc-me";
	const chan = (over: { members?: string[]; subscribers?: string[] }) =>
		create(ChannelSchema, {
			id: "c",
			name: "c",
			memberAccountIds: over.members ?? [],
			subscriberAccountIds: over.subscribers ?? [],
		});

	test("caller in subscribers → subscribed", () => {
		expect(
			deriveMembership(
				chan({ subscribers: [caller], members: [caller] }),
				caller,
			),
		).toBe("subscribed");
	});

	test("caller in members but not subscribers → joined", () => {
		expect(
			deriveMembership(
				chan({ members: [caller], subscribers: ["acc-other"] }),
				caller,
			),
		).toBe("joined");
	});

	test("caller in neither → none", () => {
		expect(
			deriveMembership(
				chan({ members: ["acc-a"], subscribers: ["acc-b"] }),
				caller,
			),
		).toBe("none");
	});

	test("subscriber tier wins even when caller is also a member", () => {
		// Both lists contain the caller — the top (subscribed) tier must win.
		expect(
			deriveMembership(
				chan({ members: [caller], subscribers: [caller] }),
				caller,
			),
		).toBe("subscribed");
	});
});

describe("adaptChannel", () => {
	const caller = "acc-me";
	const wireChannel = (over: {
		id?: string;
		name?: string;
		groupId?: string;
		kind?: ChannelKind;
		members?: string[];
		subscribers?: string[];
	}) =>
		create(ChannelSchema, {
			id: over.id ?? "c",
			name: over.name ?? "c",
			groupId: over.groupId ?? "",
			kind: over.kind ?? ChannelKind.CHANNEL,
			memberAccountIds: over.members ?? [],
			subscriberAccountIds: over.subscribers ?? [],
		});
	const empty: ReadonlySet<string> = new Set();

	for (const [wire, expected] of [
		[ChannelKind.CHANNEL, "channel"],
		[ChannelKind.DM, "dm"],
		[ChannelKind.GROUP_DM, "group_dm"],
	] as const) {
		test(`kind ${ChannelKind[wire]} → "${expected}"`, () => {
			expect(
				adaptChannel(wireChannel({ kind: wire }), caller, empty).kind,
			).toBe(expected);
		});
	}

	test("empty groupId → undefined; non-empty passes through", () => {
		expect(
			adaptChannel(wireChannel({ groupId: "" }), caller, empty).groupId,
		).toBeUndefined();
		expect(
			adaptChannel(wireChannel({ groupId: "grp-matt" }), caller, empty).groupId,
		).toBe("grp-matt");
	});

	test("memberAccountIds pass through", () => {
		const members = ["acc-a", "acc-b", caller];
		expect(
			adaptChannel(wireChannel({ members }), caller, empty).memberAccountIds,
		).toEqual(members);
	});

	test("membership is the deriveMembership result", () => {
		const w = wireChannel({ members: [caller], subscribers: ["acc-other"] });
		expect(adaptChannel(w, caller, empty).membership).toBe(
			deriveMembership(w, caller),
		);
	});

	test("alwaysSubscribed is true only for a subscribed home channel", () => {
		const homeId = "chan-home";
		const homeSet: ReadonlySet<string> = new Set([homeId]);

		// Subscribed AND in the home set → the implicit, non-togglable flag.
		const subscribedHome = adaptChannel(
			wireChannel({ id: homeId, subscribers: [caller], members: [caller] }),
			caller,
			homeSet,
		);
		expect(subscribedHome.membership).toBe("subscribed");
		expect(subscribedHome.alwaysSubscribed).toBe(true);

		// Subscribed but NOT a home channel → absent (a normal togglable sub).
		const subscribedNonHome = adaptChannel(
			wireChannel({
				id: "chan-other",
				subscribers: [caller],
				members: [caller],
			}),
			caller,
			homeSet,
		);
		expect(subscribedNonHome.membership).toBe("subscribed");
		expect(subscribedNonHome.alwaysSubscribed).toBeUndefined();

		// The channel id IS in the home set but the caller is only joined (not
		// subscribed) → absent: always-subscribed requires the subscribed tier.
		const joinedHome = adaptChannel(
			wireChannel({
				id: homeId,
				members: [caller],
				subscribers: ["acc-other"],
			}),
			caller,
			homeSet,
		);
		expect(joinedHome.membership).toBe("joined");
		expect(joinedHome.alwaysSubscribed).toBeUndefined();
	});
});

describe("agentHomeChannelIds", () => {
	test("collects home ids of agents with a truthy homeChannelId only", () => {
		const accounts: Account[] = [
			{ id: "acc-matt", handle: "matt", displayName: "Matt", kind: "user" },
			{
				id: "acc-cook",
				handle: "cook",
				displayName: "Cook",
				kind: "agent",
				ownerUserId: "acc-matt",
				homeChannelId: "chan-home-cook",
			},
			{
				id: "acc-fable",
				handle: "fable",
				displayName: "Fable",
				kind: "agent",
				ownerUserId: "acc-matt",
				homeChannelId: "chan-home-fable",
			},
			// Agent with no home DM → excluded (undefined homeChannelId).
			{
				id: "acc-ghost",
				handle: "ghost",
				displayName: "Ghost",
				kind: "agent",
				ownerUserId: "acc-matt",
			},
		];
		const ids = agentHomeChannelIds(accounts);
		expect([...ids].sort()).toEqual(["chan-home-cook", "chan-home-fable"]);
		// A user's id-space never leaks in; a home-less agent is excluded.
		expect(ids.has("acc-matt")).toBe(false);
		expect(ids.size).toBe(2);
	});

	test("returns an empty set when no account is a home-bearing agent", () => {
		const ids = agentHomeChannelIds([
			{ id: "acc-a", handle: "a", displayName: "A", kind: "user" },
			{
				id: "acc-b",
				handle: "b",
				displayName: "B",
				kind: "agent",
				ownerUserId: "acc-a",
			},
		]);
		expect(ids.size).toBe(0);
	});
});
