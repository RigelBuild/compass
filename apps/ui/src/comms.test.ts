import { describe, expect, test } from "bun:test";
import type { Thread } from "./comms";
import {
	agentDmAccountId,
	agentDmChannel,
	blockText,
	browsableChannels,
	channelSections,
	dmChannels,
	dmLabel,
	handleOf,
	isDm,
	parseMentions,
	railChannels,
	threadSummary,
	threadsOf,
} from "./comms";
import type {
	Account,
	Ask,
	Channel,
	ChannelGroup,
	ChannelKind,
	ConvBlock,
	Message,
} from "./comms-stub";
import { STUB_MESSAGES } from "./comms-stub";

// comms.ts is the pure core of the comms model: it partitions channels
// into the rail's sections/DMs/browse list, threads a channel's flat message
// stream into roots+replies, and parses composer mentions. These tests defend
// the observable contracts each surface reads — ordering, grouping, the
// orphan/cycle threading rules, and the mention grammar — against a refactor or
// a fixture edit silently changing the shape. Fixtures are built inline (small,
// per-edge) so an assertion pins the behavior, not today's stub values.

// ── Minimal fixture builders — only the fields a helper reads carry meaning; the
//    rest satisfy the interface and are held constant so a shape change never
//    perturbs an assertion. ────────────────────────────────────────────────────
function acc(over: Partial<Account> & Pick<Account, "id">): Account {
	return { handle: over.id, displayName: over.id, kind: "user", ...over };
}
function byIdOf(...accts: Account[]): Map<string, Account> {
	return new Map(accts.map((a) => [a.id, a]));
}
function ch(over: Partial<Channel> & Pick<Channel, "id">): Channel {
	return {
		name: over.id,
		kind: "channel",
		memberAccountIds: [],
		membership: "joined",
		...over,
	};
}
function grp(
	over: Partial<ChannelGroup> & Pick<ChannelGroup, "id">,
): ChannelGroup {
	return { name: over.id, visibility: "owner", ...over };
}
function msg(
	over: Partial<Message> & Pick<Message, "id" | "channelId" | "atUnixMs">,
): Message {
	return { authorAccountId: "acc-author", blocks: [], ...over };
}
const textBlock = (text: string): ConvBlock => ({ kind: "text", text });
const askBlock = (): ConvBlock => ({
	kind: "ask",
	ask: {
		askId: "ask-x",
		questions: [
			{
				questionId: "q1",
				question: "q?",
				options: [],
				allowMultiple: false,
				chosenOptionIds: [],
			},
		],
	},
});
const CH = "ch-x";

describe("threadsOf", () => {
	// The center-column contract: roots chronological, each carrying its replies
	// chronological, regardless of input order. Input is deliberately shuffled and
	// reply times interleaved so a sort or grouping regression shows.
	test("returns roots chronologically, each with its replies chronologically", () => {
		const r1 = msg({ id: "r1", channelId: CH, atUnixMs: 100 });
		const r2 = msg({ id: "r2", channelId: CH, atUnixMs: 300 });
		const a = msg({
			id: "a",
			channelId: CH,
			atUnixMs: 500,
			parentMessageId: "r1",
		});
		const b = msg({
			id: "b",
			channelId: CH,
			atUnixMs: 200,
			parentMessageId: "r1",
		});
		const c = msg({
			id: "c",
			channelId: CH,
			atUnixMs: 400,
			parentMessageId: "r2",
		});

		const threads = threadsOf([a, r2, b, r1, c], CH);

		expect(threads.map((t) => t.root.id)).toEqual(["r1", "r2"]);
		// r1's replies by time: b(200) then a(500) — NOT input order (a before b).
		expect(threads[0].replies.map((m) => m.id)).toEqual(["b", "a"]);
		expect(threads[1].replies.map((m) => m.id)).toEqual(["c"]);
	});

	// Equal timestamps fall back to an id tiebreak, so the order is stable rather
	// than input-order-dependent. Input is id-descending to prove the tiebreak
	// reorders it.
	test("breaks equal-timestamp ties by id (stable order)", () => {
		const mb = msg({ id: "m-b", channelId: CH, atUnixMs: 100 });
		const ma = msg({ id: "m-a", channelId: CH, atUnixMs: 100 });

		const threads = threadsOf([mb, ma], CH);

		expect(threads.map((t) => t.root.id)).toEqual(["m-a", "m-b"]);
	});

	// A reply whose parent isn't in the channel must surface as its own root —
	// nothing is dropped just because the parent is missing.
	test("a reply with an unresolved parent becomes its own root (nothing dropped)", () => {
		const root = msg({ id: "root", channelId: CH, atUnixMs: 100 });
		const orphan = msg({
			id: "orphan",
			channelId: CH,
			atUnixMs: 200,
			parentMessageId: "ghost",
		});

		const threads = threadsOf([root, orphan], CH);

		expect(threads.map((t) => t.root.id)).toEqual(["root", "orphan"]);
		expect(threads.find((t) => t.root.id === "orphan")?.replies).toEqual([]);
		expect(
			new Set(
				threads.flatMap((t) => [t.root.id, ...t.replies.map((r) => r.id)]),
			),
		).toEqual(new Set(["root", "orphan"]));
	});

	// A parent that lives in a DIFFERENT channel is not visible to this channel's
	// threading, so the reply roots in its own channel rather than vanishing. This
	// bites if byId were built from all messages instead of the channel's own.
	test("a message whose parent is in another channel roots in its own channel", () => {
		const A = "ch-a";
		const B = "ch-b";
		const inA = msg({ id: "in-a", channelId: A, atUnixMs: 100 });
		const inB = msg({
			id: "in-b",
			channelId: B,
			atUnixMs: 200,
			parentMessageId: "in-a",
		});

		const threads = threadsOf([inA, inB], B);

		expect(threads.map((t) => t.root.id)).toEqual(["in-b"]);
		expect(threads[0].replies).toEqual([]);
	});

	// Cycle guard: a mutual parent cycle must not loop forever and must not
	// corrupt or duplicate the well-formed threads around it. (The resolver drops
	// a pure even cycle rather than rooting it — that drop is an implementation
	// detail we deliberately do NOT pin; the load-bearing contract is: it
	// terminates, the normal thread is intact, and nothing is emitted twice.)
	test("a mutual parent cycle terminates without corrupting or duplicating other threads", () => {
		const R = msg({ id: "R", channelId: CH, atUnixMs: 100 });
		const rep = msg({
			id: "rep",
			channelId: CH,
			atUnixMs: 150,
			parentMessageId: "R",
		});
		const x = msg({
			id: "x",
			channelId: CH,
			atUnixMs: 200,
			parentMessageId: "y",
		});
		const y = msg({
			id: "y",
			channelId: CH,
			atUnixMs: 300,
			parentMessageId: "x",
		});

		const threads = threadsOf([R, rep, x, y], CH); // must return, not hang

		expect(threads.map((t) => t.root.id)).toEqual(["R"]);
		expect(threads[0].replies.map((m) => m.id)).toEqual(["rep"]);
		const emitted = threads.flatMap((t) => [
			t.root.id,
			...t.replies.map((r) => r.id),
		]);
		expect(emitted.length).toBe(new Set(emitted).size); // no duplication
	});

	// A self-referential parent must terminate and surface the message as a root
	// (an unresolved/cyclic parent still threads somewhere). Without the bound in
	// rootIdOf this hangs.
	test("a self-referential parent terminates and surfaces as a root", () => {
		const s = msg({
			id: "s",
			channelId: CH,
			atUnixMs: 100,
			parentMessageId: "s",
		});

		const threads = threadsOf([s], CH);

		expect(threads.map((t) => t.root.id)).toEqual(["s"]);
		expect(threads[0].replies).toEqual([]);
	});
});

describe("threadSummary", () => {
	// threadSummary is the pure stream-facing derivation the Slack-model
	// ThreadView reads: replies.length, the distinct reply authors in
	// first-reply order, and the latest reply time. These pin the three
	// observable numbers a summary renders — nothing here restates threadsOf.

	// Zero replies is the render-nothing case (callers only draw a summary when
	// replyCount > 0): the whole record must be the neutral shape, exactly.
	test("zero replies → neutral summary (count 0, no participants, time 0)", () => {
		const root = msg({ id: "r0", channelId: CH, atUnixMs: 100 });
		const thread: Thread = { root, replies: [] };

		expect(threadSummary(thread)).toEqual({
			replyCount: 0,
			participantIds: [],
			lastReplyAtUnixMs: 0,
		});
	});

	// The fixture thread (msg-c1): two replies, msg-c2 by acc-livingstone at
	// min(24) then msg-c3 by acc-cook at min(27). replyCount counts replies;
	// participantIds are the DISTINCT reply authors in first-reply order
	// (livingstone replied first, then cook); lastReplyAtUnixMs is the max reply
	// time. Derived from threadsOf over the stub so a fixture reshuffle can't
	// stale it.
	test("fixture thread: count, distinct participants in first-reply order, max reply time", () => {
		const thread = threadsOf(STUB_MESSAGES, "ch-svc-compass").find(
			(t) => t.root.id === "msg-c1",
		);
		if (!thread) throw new Error("fixture thread msg-c1 not found");
		const c3 = thread.replies.find((r) => r.id === "msg-c3");
		if (!c3) throw new Error("fixture reply msg-c3 not found");

		const summary = threadSummary(thread);

		expect(summary.replyCount).toBe(2);
		expect(summary.participantIds).toEqual(["acc-livingstone", "acc-cook"]);
		expect(summary.lastReplyAtUnixMs).toBe(c3.atUnixMs);
	});

	// A duplicate reply author collapses to one participant entry, kept at its
	// FIRST appearance. Two replies by the same author then one by another →
	// [first-author, second-author], not three entries.
	test("a repeated reply author appears once, at its first-reply position", () => {
		const root = msg({ id: "rd", channelId: CH, atUnixMs: 100 });
		const thread: Thread = {
			root,
			replies: [
				msg({
					id: "d1",
					channelId: CH,
					atUnixMs: 200,
					authorAccountId: "acc-a",
				}),
				msg({
					id: "d2",
					channelId: CH,
					atUnixMs: 300,
					authorAccountId: "acc-b",
				}),
				msg({
					id: "d3",
					channelId: CH,
					atUnixMs: 400,
					authorAccountId: "acc-a",
				}),
			],
		};

		expect(threadSummary(thread).participantIds).toEqual(["acc-a", "acc-b"]);
		expect(threadSummary(thread).replyCount).toBe(3);
	});

	// lastReplyAtUnixMs is the MAX reply time, not the last array element. Replies
	// deliberately out of time order [300, 100, 200] → 300, so a "return the last
	// reply's time" regression reddens.
	test("lastReplyAtUnixMs is the max reply time, not the last-in-array", () => {
		const root = msg({ id: "ro", channelId: CH, atUnixMs: 50 });
		const thread: Thread = {
			root,
			replies: [
				msg({
					id: "o1",
					channelId: CH,
					atUnixMs: 300,
					authorAccountId: "acc-a",
				}),
				msg({
					id: "o2",
					channelId: CH,
					atUnixMs: 100,
					authorAccountId: "acc-b",
				}),
				msg({
					id: "o3",
					channelId: CH,
					atUnixMs: 200,
					authorAccountId: "acc-c",
				}),
			],
		};

		expect(threadSummary(thread).lastReplyAtUnixMs).toBe(300);
	});

	// participantIds derives from REPLIES only: a root author who never replies is
	// absent. Root by acc-root, sole reply by acc-other → [acc-other], and
	// acc-root is not present.
	test("a root author who never replies is absent from participantIds", () => {
		const root = msg({
			id: "rr",
			channelId: CH,
			atUnixMs: 100,
			authorAccountId: "acc-root",
		});
		const thread: Thread = {
			root,
			replies: [
				msg({
					id: "rp",
					channelId: CH,
					atUnixMs: 200,
					authorAccountId: "acc-other",
				}),
			],
		};

		const ids = threadSummary(thread).participantIds;
		expect(ids).toEqual(["acc-other"]);
		expect(ids).not.toContain("acc-root");
	});

	// The inverse: a root author who ALSO replies IS a participant — via the
	// reply, not the root. The fixture root (acc-cook) authored msg-c3, so cook
	// appears; the entry is earned by the reply.
	test("a root author who also replies is a participant (via the reply)", () => {
		const thread = threadsOf(STUB_MESSAGES, "ch-svc-compass").find(
			(t) => t.root.id === "msg-c1",
		);
		if (!thread) throw new Error("fixture thread msg-c1 not found");

		expect(thread.root.authorAccountId).toBe("acc-cook");
		expect(threadSummary(thread).participantIds).toContain("acc-cook");
	});

	// The overflow model: with ≥6 distinct reply authors, threadSummary returns
	// the FULL distinct list (the 5-badge view cap is a ThreadView concern, not a
	// model one). Six distinct authors → six participantIds, in first-reply order.
	test("keeps the full distinct participant list past the view's 5-badge cap", () => {
		const root = msg({ id: "rc", channelId: CH, atUnixMs: 100 });
		const authors = ["acc-1", "acc-2", "acc-3", "acc-4", "acc-5", "acc-6"];
		const thread: Thread = {
			root,
			replies: authors.map((a, i) =>
				msg({
					id: `c-${i}`,
					channelId: CH,
					atUnixMs: 200 + i * 10,
					authorAccountId: a,
				}),
			),
		};

		expect(threadSummary(thread).participantIds).toEqual(authors);
		expect(threadSummary(thread).replyCount).toBe(6);
	});
});

describe("channelSections", () => {
	// One section per group in the caller's group order; channel order within a
	// group is fixture order (not sorted); DMs are excluded even when they carry a
	// groupId. Input group order (g2,g1) differs from list order to prove the
	// output follows `groups`, and within-g1 order is id-descending to prove it
	// preserves fixture order.
	test("sections follow group order; channel order within a group is fixture order; DMs excluded", () => {
		const g1 = grp({ id: "g1" });
		const g2 = grp({ id: "g2" });
		const c2 = ch({ id: "c2", groupId: "g2" });
		const c3 = ch({ id: "c3", groupId: "g1" });
		const c1 = ch({ id: "c1", groupId: "g1" });
		const dm = ch({ id: "dm1", groupId: "g1", kind: "dm" });

		const sections = channelSections([c2, c3, dm, c1], [g1, g2]);

		expect(sections.map((s) => s.group?.id)).toEqual(["g1", "g2"]);
		expect(sections[0].channels.map((c) => c.id)).toEqual(["c3", "c1"]);
		expect(sections[1].channels.map((c) => c.id)).toEqual(["c2"]);
		expect(sections.flatMap((s) => s.channels.map((c) => c.id))).not.toContain(
			"dm1",
		);
	});

	// An empty group is omitted (no empty rail header); ungrouped channels — both
	// no-group and unknown-group — fall into a single trailing `group: undefined`
	// section, in fixture order.
	test("omits empty groups and routes no-group + unknown-group channels to a trailing ungrouped section", () => {
		const g1 = grp({ id: "g1" });
		const g2 = grp({ id: "g2" }); // no channels → omitted
		const g3 = grp({ id: "g3" });
		const inG1 = ch({ id: "in-g1", groupId: "g1" });
		const inG3 = ch({ id: "in-g3", groupId: "g3" });
		const noGroup = ch({ id: "no-group" });
		const ghost = ch({ id: "ghost", groupId: "g-unknown" });

		const sections = channelSections(
			[inG1, inG3, noGroup, ghost],
			[g1, g2, g3],
		);

		expect(sections.map((s) => s.group?.id)).toEqual(["g1", "g3", undefined]);
		expect(sections[0].channels.map((c) => c.id)).toEqual(["in-g1"]);
		expect(sections[1].channels.map((c) => c.id)).toEqual(["in-g3"]);
		const trailing = sections[sections.length - 1];
		expect(trailing.group).toBeUndefined();
		expect(trailing.channels.map((c) => c.id)).toEqual(["no-group", "ghost"]);
	});

	// With no ungrouped channels there is NO trailing section — the `if
	// (ungrouped.length > 0)` guard. Bites if an empty ungrouped section were
	// always appended.
	test("no ungrouped channels → no trailing undefined section", () => {
		const g1 = grp({ id: "g1" });
		const c1 = ch({ id: "c1", groupId: "g1" });

		const sections = channelSections([c1], [g1]);

		expect(sections.map((s) => s.group?.id)).toEqual(["g1"]);
	});
});

describe("isDm", () => {
	for (const [kind, expected] of [
		["channel", false],
		["dm", true],
		["group_dm", true],
	] as [ChannelKind, boolean][]) {
		test(`${kind} → ${expected}`, () => {
			expect(isDm(ch({ id: "x", kind }))).toBe(expected);
		});
	}
});

describe("dmChannels", () => {
	// Only dm + group_dm, in fixture order; a plain channel is excluded.
	test("keeps dm + group_dm in order, drops plain channels", () => {
		const plain = ch({ id: "c", kind: "channel" });
		const d = ch({ id: "d", kind: "dm" });
		const gd = ch({ id: "gd", kind: "group_dm" });

		expect(
			dmChannels([plain, d, gd, ch({ id: "c2" })]).map((c) => c.id),
		).toEqual(["d", "gd"]);
	});
});

describe("dmLabel", () => {
	const caller = "acc-me";

	// Other participants' handles, caller excluded, joined in member order.
	test("joins other participants' handles, excluding the caller", () => {
		const byId = byIdOf(
			acc({ id: caller, handle: "me" }),
			acc({ id: "acc-a", handle: "alice" }),
			acc({ id: "acc-b", handle: "bob" }),
		);
		const dm = ch({
			id: "gd",
			kind: "group_dm",
			memberAccountIds: [caller, "acc-a", "acc-b"],
		});

		expect(dmLabel(dm, caller, byId)).toBe("alice, bob");
	});

	// A single UNRESOLVED other member resolves to its id (via handleOf) — it does
	// NOT trigger the channel.name fallback, which only fires when there is no
	// other member at all.
	test("an unresolved other member shows its id, not the channel name", () => {
		const byId = byIdOf(acc({ id: caller, handle: "me" }));
		const dm = ch({
			id: "dm",
			name: "the-dm",
			kind: "dm",
			memberAccountIds: [caller, "acc-ghost"],
		});

		expect(dmLabel(dm, caller, byId)).toBe("acc-ghost");
	});

	// No other members (only the caller) → fall back to the channel name so a
	// malformed DM never renders blank.
	test("falls back to the channel name when there are no other members", () => {
		const byId = byIdOf(acc({ id: caller, handle: "me" }));
		const dm = ch({
			id: "dm",
			name: "lonely",
			kind: "dm",
			memberAccountIds: [caller],
		});

		expect(dmLabel(dm, caller, byId)).toBe("lonely");
	});
});

describe("parseMentions", () => {
	// Assert the parsed {handle, reserved} sequence AND the offset invariant: for
	// every match, text.slice(start, end) is exactly "@" + handle. An off-by-one
	// in start/end reddens the slice check.
	const parsed = (text: string): { handle: string; reserved: boolean }[] => {
		const got = parseMentions(text);
		for (const m of got)
			expect(text.slice(m.start, m.end)).toBe(`@${m.handle}`);
		return got.map((m) => ({ handle: m.handle, reserved: m.reserved }));
	};

	test("single mention with correct offsets", () => {
		const got = parseMentions("hi @matt!");
		expect(got).toHaveLength(1);
		expect(got[0]).toMatchObject({ handle: "matt", reserved: false });
		expect(got[0].start).toBe(3);
		expect(got[0].end).toBe(8);
		expect("hi @matt!".slice(3, 8)).toBe("@matt");
	});

	test("multiple mentions in order of appearance", () => {
		expect(parsed("@alpha then @beta")).toEqual([
			{ handle: "alpha", reserved: false },
			{ handle: "beta", reserved: false },
		]);
	});

	test("reserved tokens are flagged case-insensitively", () => {
		expect(parsed("@Everyone @AGENTS @users @matt")).toEqual([
			{ handle: "Everyone", reserved: true },
			{ handle: "AGENTS", reserved: true },
			{ handle: "users", reserved: true },
			{ handle: "matt", reserved: false },
		]);
	});

	test("dotted and hyphenated handles match fully", () => {
		expect(parsed("ping @svc.compass and @ci-build")).toEqual([
			{ handle: "svc.compass", reserved: false },
			{ handle: "ci-build", reserved: false },
		]);
	});

	test("a bare @ or @. does not match", () => {
		expect(parseMentions("@ nothing @. here @")).toEqual([]);
	});
});

describe("blockText", () => {
	test("text block → its text", () => {
		expect(blockText(textBlock("hello"))).toBe("hello");
	});
	test("ask block → empty string", () => {
		expect(blockText(askBlock())).toBe("");
	});
});

describe("handleOf", () => {
	test("known id → its handle", () => {
		expect(
			handleOf(byIdOf(acc({ id: "acc-a", handle: "alice" })), "acc-a"),
		).toBe("alice");
	});
	test("unknown id → the id itself", () => {
		expect(handleOf(new Map(), "acc-ghost")).toBe("acc-ghost");
	});
});

describe("agentDmAccountId", () => {
	const caller = "acc-me";
	const byId = byIdOf(
		acc({ id: caller, handle: "me" }),
		acc({ id: "acc-agent", handle: "agent", kind: "agent" }),
		acc({ id: "acc-agent2", handle: "agent2", kind: "agent" }),
		acc({ id: "acc-human", handle: "human", kind: "user" }),
	);

	test("1:1 DM whose other party is an agent → that agent's id", () => {
		const dm = ch({
			id: "dm",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmAccountId(dm, caller, byId)).toBe("acc-agent");
	});
	test("human↔human DM → undefined", () => {
		const dm = ch({
			id: "dm",
			kind: "dm",
			memberAccountIds: [caller, "acc-human"],
		});
		expect(agentDmAccountId(dm, caller, byId)).toBeUndefined();
	});
	test("DM with an unresolved other party → undefined", () => {
		const dm = ch({
			id: "dm",
			kind: "dm",
			memberAccountIds: [caller, "acc-ghost"],
		});
		expect(agentDmAccountId(dm, caller, byId)).toBeUndefined();
	});
	test("DM with more than one other party → undefined", () => {
		const dm = ch({
			id: "dm",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent", "acc-agent2"],
		});
		expect(agentDmAccountId(dm, caller, byId)).toBeUndefined();
	});
	test("group_dm (even with a single agent) → undefined", () => {
		const gd = ch({
			id: "gd",
			kind: "group_dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmAccountId(gd, caller, byId)).toBeUndefined();
	});
	test("plain channel → undefined", () => {
		const plain = ch({
			id: "c",
			kind: "channel",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmAccountId(plain, caller, byId)).toBeUndefined();
	});
});

describe("railChannels / browsableChannels", () => {
	const none1 = ch({ id: "none1", membership: "none" });
	const joined1 = ch({ id: "joined1", membership: "joined" });
	const sub1 = ch({ id: "sub1", membership: "subscribed" });
	const none2 = ch({ id: "none2", membership: "none" });
	const list = [none1, joined1, sub1, none2];

	test("railChannels keeps joined + subscribed, drops none", () => {
		expect(railChannels(list).map((c) => c.id)).toEqual(["joined1", "sub1"]);
	});
	test("browsableChannels keeps only none", () => {
		expect(browsableChannels(list).map((c) => c.id)).toEqual([
			"none1",
			"none2",
		]);
	});
	// The two lists partition the channels: disjoint and exhaustive.
	test("together they partition the channels (disjoint + exhaustive)", () => {
		const union = [
			...railChannels(list).map((c) => c.id),
			...browsableChannels(list).map((c) => c.id),
		];
		expect(new Set(union).size).toBe(union.length);
		expect(new Set(union)).toEqual(new Set(list.map((c) => c.id)));
	});
});

describe("agentDmChannel", () => {
	// The inverse of agentDmAccountId: given an agent id, find the DM channel to
	// center that agent's workspace on. Same account cast as the agentDmAccountId
	// tests (one caller, two agents, one human), but the cases are its mirror —
	// we query BY agent id and assert which channel (or none) comes back.
	const caller = "acc-me";
	const byId = byIdOf(
		acc({ id: caller, handle: "me" }),
		acc({ id: "acc-agent", handle: "agent", kind: "agent" }),
		acc({ id: "acc-agent2", handle: "agent2", kind: "agent" }),
		acc({ id: "acc-human", handle: "human", kind: "user" }),
	);

	// The agent's 1:1 DM is returned — and a plain channel that ALSO includes the
	// agent is skipped, proving resolution runs through agentDmAccountId's DM
	// classification, not a naive "is the agent a member" scan.
	test("agent DM present (beside a plain channel with the agent) → that DM", () => {
		const plain = ch({
			id: "c-shared",
			kind: "channel",
			memberAccountIds: [caller, "acc-agent"],
		});
		const dm = ch({
			id: "dm-agent",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmChannel([plain, dm], "acc-agent", caller, byId)?.id).toBe(
			"dm-agent",
		);
	});

	// A group DM whose only other party is the agent does NOT resolve — group DMs
	// have no single observed agent (mirrors agentDmAccountId's group_dm case).
	test("group DM with the agent → undefined", () => {
		const gd = ch({
			id: "gd",
			kind: "group_dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmChannel([gd], "acc-agent", caller, byId)).toBeUndefined();
	});

	// A plain channel containing the agent is not its DM — no workspace channel.
	test("only a plain channel with the agent → undefined", () => {
		const plain = ch({
			id: "c",
			kind: "channel",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmChannel([plain], "acc-agent", caller, byId)).toBeUndefined();
	});

	// Querying a HUMAN id finds nothing: a human↔human DM resolves to undefined
	// (its other party isn't an agent), so it never matches the queried id.
	test("querying a human id (human↔human DM present) → undefined", () => {
		const humanDm = ch({
			id: "dm-human",
			kind: "dm",
			memberAccountIds: [caller, "acc-human"],
		});
		expect(
			agentDmChannel([humanDm], "acc-human", caller, byId),
		).toBeUndefined();
	});

	// An agent with no DM in the list resolves to nothing.
	test("unknown / DM-less agent id → undefined", () => {
		const dm = ch({
			id: "dm-agent",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmChannel([dm], "acc-ghost", caller, byId)).toBeUndefined();
	});

	// `find` returns the FIRST matching DM when two DMs both resolve to the same
	// agent (documented "first such DM"). A findLast/filter-last regression would
	// return "dm-2" here.
	test("returns the first matching DM when two resolve to the same agent", () => {
		const first = ch({
			id: "dm-1",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		const second = ch({
			id: "dm-2",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		expect(agentDmChannel([first, second], "acc-agent", caller, byId)?.id).toBe(
			"dm-1",
		);
	});

	// The inverse-identity invariant tying the two functions together: over a
	// mixed list, every channel that agentDmAccountId classifies as an agent DM
	// round-trips back to ITSELF through agentDmChannel. The final assertion keeps
	// the loop non-vacuous (there is at least one agent DM to round-trip).
	test("round-trips every agent DM back to itself (inverse of agentDmAccountId)", () => {
		const plain = ch({
			id: "c",
			kind: "channel",
			memberAccountIds: [caller, "acc-agent"],
		});
		const dmA = ch({
			id: "dm-a",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		const dmB = ch({
			id: "dm-b",
			kind: "dm",
			memberAccountIds: [caller, "acc-agent2"],
		});
		const gd = ch({
			id: "gd",
			kind: "group_dm",
			memberAccountIds: [caller, "acc-agent"],
		});
		const humanDm = ch({
			id: "dm-human",
			kind: "dm",
			memberAccountIds: [caller, "acc-human"],
		});
		const list = [plain, dmA, gd, dmB, humanDm];

		for (const c of list) {
			const agentId = agentDmAccountId(c, caller, byId);
			if (agentId === undefined) continue;
			expect(agentDmChannel(list, agentId, caller, byId)).toBe(c);
		}
		expect(
			list.some((c) => agentDmAccountId(c, caller, byId) !== undefined),
		).toBe(true);
	});
});

// ── Comms ask fixture integrity (refolded from the deleted ask-contract.test.ts,
//    now over the CHANNEL asks in STUB_MESSAGES) ───────────────────────────────
// The channel renders `ask` blocks as structured, multi-question question sets
// (compass.v1 Ask: {askId, questions: [{questionId, question, options:
// AskOption[], allowMultiple, chosenOptionIds}]}). These pin the load-bearing
// invariants the AskBlock renderer (ChannelView) trusts without re-checking —
// the ones a fixture edit or a widened type could silently break. They walk
// EVERY question of EVERY ask in STUB_MESSAGES so a second production ask (there
// is one today: msg-c4 `ask-s4-integration`) is held to the same contract. No
// question/label strings are asserted (copy, not contract). This is
// fixture-precondition coverage; the answerAsk MUTATION path lives in
// store.test.ts and is not restated here.
const COMMS_ASKS: readonly Ask[] = STUB_MESSAGES.flatMap((message) =>
	message.blocks
		.filter(
			(block): block is Extract<ConvBlock, { kind: "ask" }> =>
				block.kind === "ask",
		)
		.map((block) => block.ask),
);

describe("comms ask fixture is non-vacuous", () => {
	// Guard against a silently-empty walk: if a refactor drops every ask block
	// from STUB_MESSAGES, the per-ask invariants below iterate nothing and pass
	// trivially, green-washing zero coverage. This reddens the moment the ask
	// surface disappears from the channel fixture.
	test("STUB_MESSAGES contains at least one ask block", () => {
		expect(COMMS_ASKS.length).toBeGreaterThan(0);
	});
});

describe("comms ask chosen ids reference real options (referential integrity)", () => {
	// The renderer marks an option selected via chosenOptionIds.includes(o.id). A
	// chosen id that matches no option silently highlights nothing — an answered
	// ask reads as unanswered, with no error anywhere. Reddens on a typo'd, stale,
	// or renamed chosen id. Walked per question.
	for (const ask of COMMS_ASKS) {
		test(`ask ${ask.askId}`, () => {
			for (const question of ask.questions) {
				const optionIds = new Set(question.options.map((option) => option.id));
				for (const chosen of question.chosenOptionIds) {
					expect(optionIds.has(chosen)).toBe(true);
				}
			}
		});
	}
});

describe("comms single-select asks hold at most one chosen id", () => {
	// allowMultiple===false is a radio group. More than one chosen id then means
	// two options render selected at once — an impossible state the UI never
	// intends. Reddens if a single-select question accumulates >1 chosen id.
	for (const ask of COMMS_ASKS) {
		test(`ask ${ask.askId}`, () => {
			for (const question of ask.questions) {
				if (question.allowMultiple) continue;
				expect(question.chosenOptionIds.length).toBeLessThanOrEqual(1);
			}
		});
	}
});

describe("comms ask option ids are unique within an ask", () => {
	// Duplicate ids make chosenOptionIds.includes() light BOTH options, and a
	// click on either resolves ambiguously. Reddens if two options in one
	// question share an id (Set collapses the dup, so size < length).
	for (const ask of COMMS_ASKS) {
		test(`ask ${ask.askId}`, () => {
			for (const question of ask.questions) {
				const ids = question.options.map((option) => option.id);
				expect(new Set(ids).size).toBe(ids.length);
			}
		});
	}
});
