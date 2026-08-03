import { describe, expect, test } from "bun:test";
import type { TopicGroup } from "./comms";
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
	topicSummary,
	topicsOf,
} from "./comms";
import type {
	Account,
	Ask,
	Channel,
	ChannelGroup,
	ChannelKind,
	ConvBlock,
	Message,
	Topic,
} from "./comms-stub";
import { STUB_MESSAGES, STUB_TOPICS } from "./comms-stub";

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
	over: Partial<Message> & Pick<Message, "id" | "topicId" | "atUnixMs">,
): Message {
	return { authorAccountId: "acc-author", blocks: [], ...over };
}
function top(over: Partial<Topic> & Pick<Topic, "id" | "channelId">): Topic {
	return {
		name: over.id,
		createdAtUnixMs: 0,
		createdByAccountId: "acc-author",
		archived: false,
		...over,
	};
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
		answered: false,
	},
});
const CH = "ch-x";

describe("topicsOf", () => {
	// The channel index contract: every topic in the channel becomes a group,
	// each carrying its own messages chronological, groups ordered by last
	// activity DESCENDING (most-recently-active topic first). Input is shuffled so
	// a sort or grouping regression shows.
	test("groups a channel's topics, messages chronological, ordered by last activity desc", () => {
		const tA = top({ id: "top-a", channelId: CH });
		const tB = top({ id: "top-b", channelId: CH });
		// top-a's newest message is at 500; top-b's newest is at 300 → a before b.
		const a1 = msg({ id: "a1", topicId: "top-a", atUnixMs: 100 });
		const a2 = msg({ id: "a2", topicId: "top-a", atUnixMs: 500 });
		const b1 = msg({ id: "b1", topicId: "top-b", atUnixMs: 300 });
		const b2 = msg({ id: "b2", topicId: "top-b", atUnixMs: 200 });

		const groups = topicsOf([tB, tA], [a2, b1, a1, b2], CH);

		expect(groups.map((g) => g.topic.id)).toEqual(["top-a", "top-b"]);
		// Each group's messages are chronological, not input order.
		expect(groups[0].messages.map((m) => m.id)).toEqual(["a1", "a2"]);
		expect(groups[1].messages.map((m) => m.id)).toEqual(["b2", "b1"]);
	});

	// A topic in a DIFFERENT channel is excluded; only the requested channel's
	// topics group.
	test("excludes topics from other channels", () => {
		const here = top({ id: "here", channelId: CH });
		const elsewhere = top({ id: "elsewhere", channelId: "ch-other" });
		const m1 = msg({ id: "m1", topicId: "here", atUnixMs: 100 });
		const m2 = msg({ id: "m2", topicId: "elsewhere", atUnixMs: 200 });

		const groups = topicsOf([here, elsewhere], [m1, m2], CH);

		expect(groups.map((g) => g.topic.id)).toEqual(["here"]);
	});

	// An empty topic (no messages) still appears — sorted by its creation time,
	// not sunk to the epoch. A fresh topic created after an older active one sorts
	// FIRST (its createdAtUnixMs beats the older topic's last message time).
	test("an empty topic sorts by its creation time and is never dropped", () => {
		const older = top({ id: "older", channelId: CH, createdAtUnixMs: 10 });
		const fresh = top({ id: "fresh", channelId: CH, createdAtUnixMs: 400 });
		const m1 = msg({ id: "m1", topicId: "older", atUnixMs: 100 });

		const groups = topicsOf([older, fresh], [m1], CH);

		expect(groups.map((g) => g.topic.id)).toEqual(["fresh", "older"]);
		expect(groups.find((g) => g.topic.id === "fresh")?.messages).toEqual([]);
	});

	// Never drops a message: every message whose topic is in the channel lands in
	// exactly one group, and the union of grouped messages equals the input.
	test("every channel message lands in exactly one group (nothing dropped)", () => {
		const tA = top({ id: "top-a", channelId: CH });
		const tB = top({ id: "top-b", channelId: CH });
		const a1 = msg({ id: "a1", topicId: "top-a", atUnixMs: 100 });
		const a2 = msg({ id: "a2", topicId: "top-a", atUnixMs: 200 });
		const b1 = msg({ id: "b1", topicId: "top-b", atUnixMs: 150 });

		const groups = topicsOf([tA, tB], [a1, a2, b1], CH);

		const grouped = groups.flatMap((g) => g.messages.map((m) => m.id));
		expect(new Set(grouped)).toEqual(new Set(["a1", "a2", "b1"]));
		expect(grouped.length).toBe(3); // no duplication
	});

	// Fixture ground truth: ch-svc-compass carries two topics (top-compass-t3a,
	// top-compass-integration). Derived from the stub so a reshuffle can't stale
	// the assertion.
	test("fixture: ch-svc-compass groups its two topics", () => {
		const groups = topicsOf(STUB_TOPICS, STUB_MESSAGES, "ch-svc-compass");
		expect(new Set(groups.map((g) => g.topic.id))).toEqual(
			new Set(["top-compass-t3a", "top-compass-integration"]),
		);
	});

	// An ARCHIVED topic is hidden from the channel index (matching the snapshot
	// loader's listTopics{includeArchived:false}), even though its messages are
	// retained in the flat message set — a live `topicUpserted` archive event
	// must not leave the archived topic showing as an index row. Its sibling
	// active topic still groups, and the archived topic's messages are simply not
	// surfaced through the index (TopicView reads them directly by id).
	test("excludes an archived topic from the index but keeps the active sibling", () => {
		const active = top({ id: "active", channelId: CH });
		const gone = top({ id: "gone", channelId: CH, archived: true });
		const a1 = msg({ id: "a1", topicId: "active", atUnixMs: 100 });
		const g1 = msg({ id: "g1", topicId: "gone", atUnixMs: 200 });

		const groups = topicsOf([active, gone], [a1, g1], CH);

		expect(groups.map((g) => g.topic.id)).toEqual(["active"]);
		expect(groups[0].messages.map((m) => m.id)).toEqual(["a1"]);
	});
});

describe("topicSummary", () => {
	// topicSummary is the pure index-row derivation: message count, the DISTINCT
	// author ids in first-post order, and the last-activity time. These pin the
	// three observable numbers an index row renders — nothing here restates
	// topicsOf.

	// An empty topic is the neutral shape, exactly (count 0, no participants,
	// time 0).
	test("empty topic → neutral summary (count 0, no participants, time 0)", () => {
		const group: TopicGroup = {
			topic: top({ id: "t0", channelId: CH }),
			messages: [],
		};
		expect(topicSummary(group)).toEqual({
			messageCount: 0,
			participantIds: [],
			lastActivityAtUnixMs: 0,
		});
	});

	// The fixture topic (top-compass-t3a): three messages, cook(10)/livingstone(24)/
	// cook(27). messageCount counts messages; participantIds are the DISTINCT
	// authors in first-post order (cook posted first, then livingstone);
	// lastActivityAtUnixMs is the max post time. Derived from topicsOf over the
	// stub so a fixture reshuffle can't stale it.
	test("fixture topic: count, distinct participants in first-post order, max time", () => {
		const group = topicsOf(STUB_TOPICS, STUB_MESSAGES, "ch-svc-compass").find(
			(g) => g.topic.id === "top-compass-t3a",
		);
		if (!group) throw new Error("fixture topic top-compass-t3a not found");
		const last = group.messages[group.messages.length - 1];

		const summary = topicSummary(group);

		expect(summary.messageCount).toBe(3);
		expect(summary.participantIds).toEqual(["acc-cook", "acc-livingstone"]);
		expect(summary.lastActivityAtUnixMs).toBe(last.atUnixMs);
	});

	// A repeated author collapses to one participant entry, kept at its FIRST
	// appearance.
	test("a repeated author appears once, at its first-post position", () => {
		const group: TopicGroup = {
			topic: top({ id: "td", channelId: CH }),
			messages: [
				msg({
					id: "d1",
					topicId: "td",
					atUnixMs: 200,
					authorAccountId: "acc-a",
				}),
				msg({
					id: "d2",
					topicId: "td",
					atUnixMs: 300,
					authorAccountId: "acc-b",
				}),
				msg({
					id: "d3",
					topicId: "td",
					atUnixMs: 400,
					authorAccountId: "acc-a",
				}),
			],
		};

		expect(topicSummary(group).participantIds).toEqual(["acc-a", "acc-b"]);
		expect(topicSummary(group).messageCount).toBe(3);
	});

	// lastActivityAtUnixMs is the MAX post time, not the last array element.
	test("lastActivityAtUnixMs is the max post time, not the last-in-array", () => {
		const group: TopicGroup = {
			topic: top({ id: "to", channelId: CH }),
			messages: [
				msg({
					id: "o1",
					topicId: "to",
					atUnixMs: 300,
					authorAccountId: "acc-a",
				}),
				msg({
					id: "o2",
					topicId: "to",
					atUnixMs: 100,
					authorAccountId: "acc-b",
				}),
				msg({
					id: "o3",
					topicId: "to",
					atUnixMs: 200,
					authorAccountId: "acc-c",
				}),
			],
		};

		expect(topicSummary(group).lastActivityAtUnixMs).toBe(300);
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
