import { describe, expect, test } from "bun:test";
import type { Ask as WireAsk } from "@compass/client";
import {
	AccountSchema,
	AgentAccountSchema,
	AskOptionSchema,
	AskQuestionSchema,
	AskSchema,
	ChannelGroupSchema,
	ChannelGroupVisibility,
	ChannelKind,
	ChannelSchema,
	create,
	MessageBlockSchema,
	MessageSchema,
	UserAccountSchema,
} from "@compass/client";
import type { Account } from "../stub-data";
import {
	adaptAccount,
	adaptAsk,
	adaptChannel,
	adaptChannelGroup,
	adaptMessage,
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

// ── Message / Ask fixture builders ───────────────────────────────────────────
function textBlock(text: string) {
	return create(MessageBlockSchema, { block: { case: "text", value: text } });
}
function askBlock(ask: WireAsk) {
	return create(MessageBlockSchema, { block: { case: "ask", value: ask } });
}
/** A block whose oneof is unset — the stand-in for any non-durable/unknown
 *  wire case the domain does not model. */
function unsetBlock() {
	return create(MessageBlockSchema, {});
}

describe("adaptAsk", () => {
	test("maps askId and every question, preserving question and option order", () => {
		const r = adaptAsk(
			create(AskSchema, {
				askId: "ask-1",
				questions: [
					create(AskQuestionSchema, {
						questionId: "q-1",
						question: "Which lane?",
						options: [
							create(AskOptionSchema, { id: "o-a", label: "A" }),
							create(AskOptionSchema, { id: "o-b", label: "B" }),
							create(AskOptionSchema, { id: "o-c", label: "C" }),
						],
						allowMultiple: true,
						chosenOptionIds: ["o-c", "o-a"],
					}),
					create(AskQuestionSchema, {
						questionId: "q-2",
						question: "Ship it?",
						options: [create(AskOptionSchema, { id: "o-y", label: "Yes" })],
					}),
				],
			}),
		);
		expect(r.askId).toBe("ask-1");
		// Question order is the ask order, not a set.
		expect(r.questions.map((q) => q.questionId)).toEqual(["q-1", "q-2"]);
		const [q1, q2] = r.questions;
		expect(q1?.question).toBe("Which lane?");
		// Option order is preserved verbatim.
		expect(q1?.options).toEqual([
			{ id: "o-a", label: "A", description: undefined },
			{ id: "o-b", label: "B", description: undefined },
			{ id: "o-c", label: "C", description: undefined },
		]);
		expect(q1?.allowMultiple).toBe(true);
		// Chosen ids carry through in the answer's own order, not re-sorted.
		expect(q1?.chosenOptionIds).toEqual(["o-c", "o-a"]);
		// A pending single-select question: unanswered and not multi.
		expect(q2?.allowMultiple).toBe(false);
		expect(q2?.chosenOptionIds).toEqual([]);
	});

	test("option description normalizes empty-string wire unset to undefined", () => {
		const r = adaptAsk(
			create(AskSchema, {
				askId: "ask-2",
				questions: [
					create(AskQuestionSchema, {
						questionId: "q-1",
						question: "Pick",
						options: [
							create(AskOptionSchema, { id: "o-a", label: "A" }),
							create(AskOptionSchema, {
								id: "o-b",
								label: "B",
								description: "the safer one",
							}),
						],
					}),
				],
			}),
		);
		const opts = r.questions[0]?.options ?? [];
		expect(opts[0]?.description).toBeUndefined();
		expect(opts[0]?.description).not.toBe("");
		expect(opts[1]?.description).toBe("the safer one");
	});

	test("an ask with no options on a question maps to an empty option list", () => {
		const r = adaptAsk(
			create(AskSchema, {
				askId: "ask-3",
				questions: [
					create(AskQuestionSchema, { questionId: "q-1", question: "Free?" }),
				],
			}),
		);
		expect(r.questions[0]?.options).toEqual([]);
	});
});

describe("adaptMessage", () => {
	test("maps the scalar fields and flattens the container oneof to channelId", () => {
		const r = adaptMessage(
			create(MessageSchema, {
				id: "msg-1",
				container: { case: "channelId", value: "chan-1" },
				authorAccountId: "acc-matt",
				atUnixMs: 1_700_000_000_000n,
				blocks: [textBlock("hello")],
			}),
		);
		expect(r.id).toBe("msg-1");
		expect(r.channelId).toBe("chan-1");
		expect(r.authorAccountId).toBe("acc-matt");
	});

	test("atUnixMs converts the wire bigint to a real number", () => {
		const r = adaptMessage(
			create(MessageSchema, { id: "msg-1", atUnixMs: 1_700_000_000_000n }),
		);
		// A number, not a bigint: the domain sorts/compares these with `<`.
		expect(typeof r.atUnixMs).toBe("number");
		expect(r.atUnixMs).toBe(1_700_000_000_000);
	});

	test("a text block maps to a domain text block", () => {
		const r = adaptMessage(
			create(MessageSchema, { id: "msg-1", blocks: [textBlock("hi there")] }),
		);
		expect(r.blocks).toEqual([{ kind: "text", text: "hi there" }]);
	});

	test("an ask block maps through adaptAsk", () => {
		const ask = create(AskSchema, {
			askId: "ask-1",
			questions: [
				create(AskQuestionSchema, {
					questionId: "q-1",
					question: "Which?",
					options: [create(AskOptionSchema, { id: "o-a", label: "A" })],
					allowMultiple: true,
					chosenOptionIds: ["o-a"],
				}),
			],
		});
		const r = adaptMessage(
			create(MessageSchema, { id: "msg-1", blocks: [askBlock(ask)] }),
		);
		expect(r.blocks).toEqual([{ kind: "ask", ask: adaptAsk(ask) }]);
		const block = r.blocks[0];
		expect(block?.kind).toBe("ask");
		if (block?.kind !== "ask") throw new Error("expected an ask block");
		expect(block.ask.questions[0]?.allowMultiple).toBe(true);
		expect(block.ask.questions[0]?.chosenOptionIds).toEqual(["o-a"]);
	});

	test("a non-durable/unset block case is DROPPED, keeping the durable ones in order", () => {
		const r = adaptMessage(
			create(MessageSchema, {
				id: "msg-1",
				blocks: [textBlock("first"), unsetBlock(), textBlock("second")],
			}),
		);
		// Dropped, never a placeholder block and never a throw.
		expect(r.blocks).toEqual([
			{ kind: "text", text: "first" },
			{ kind: "text", text: "second" },
		]);
	});

	test("a message whose blocks are all non-durable yields an empty blocks array", () => {
		const r = adaptMessage(
			create(MessageSchema, {
				id: "msg-1",
				blocks: [unsetBlock(), unsetBlock()],
			}),
		);
		expect(r.blocks).toEqual([]);
	});

	test("parentMessageId empty-string wire unset maps to undefined", () => {
		const r = adaptMessage(
			create(MessageSchema, { id: "msg-1", parentMessageId: "" }),
		);
		// The threading seam: comms.ts treats a falsy parent as a root, and the
		// wire round-trip pins "" — the conversion must land on undefined, not "".
		expect(r.parentMessageId).toBeUndefined();
		expect(r.parentMessageId).not.toBe("");
	});

	test("a real parentMessageId round-trips verbatim", () => {
		const r = adaptMessage(
			create(MessageSchema, { id: "msg-2", parentMessageId: "msg-1" }),
		);
		expect(r.parentMessageId).toBe("msg-1");
	});

	test("an unset container oneof maps to an empty channelId rather than throwing", () => {
		const w = create(MessageSchema, { id: "msg-1" });
		expect(w.container.case).toBeUndefined();
		expect(() => adaptMessage(w)).not.toThrow();
		expect(adaptMessage(w).channelId).toBe("");
	});
});
