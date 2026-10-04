import { describe, expect, test } from "bun:test";
import {
	AskQuestionSchema,
	AskSchema,
	CommsService,
	CompassService,
	create,
	createCommsClient,
	createCompassClient,
	createRouterTransport,
	type Issue,
	IssueSchema,
	type Message,
	MessageBlockSchema,
	MessageSchema,
	PullRequestSchema,
	type Transport,
} from "@compass/client";
import { createRoot } from "solid-js";
import { STUB_COMMS_STATE } from "../comms-stub";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import type { Destination, DestinationProvider } from "./commands";
import {
	createStoreDestinationProviders,
	queryDestinations,
} from "./destinations";

// Destination providers (RIG-2483, A4/T3): local kinds read store accessors; issue,
// PR, and message kinds call search RPCs through fake transports, and issue/PR fall
// back to the held board without clients. These mount a fixture-backed store inside
// createRoot and drive navigation through the store's in-memory route path.

async function withStoreAsync(
	body: (store: AppStore) => Promise<void>,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
	});
	try {
		await body(store);
	} finally {
		dispose();
	}
}

/** Drain the microtask queue for Solid route updates after navigation. */
async function flush(): Promise<void> {
	for (let i = 0; i < 20; i++) await Promise.resolve();
}
function searchTransport(
	hits: Message[],
	onCall: () => void = () => {},
	shouldReject = false,
): Transport {
	return issueSearchTransport({
		messageHits: hits,
		onMessageCall: onCall,
		rejectMessages: shouldReject,
	});
}

function issueSearchTransport(opts: {
	issueHits?: Issue[];
	onIssueCall?: () => void;
	rejectIssues?: boolean;
	messageHits?: Message[];
	onMessageCall?: () => void;
	rejectMessages?: boolean;
}): Transport {
	return createRouterTransport(({ service }) => {
		service(CommsService, {
			searchMessages: () => {
				opts.onMessageCall?.();
				if (opts.rejectMessages) throw new Error("search unavailable");
				return { messages: opts.messageHits ?? [] };
			},
		});
		service(CompassService, {
			searchIssues: () => {
				opts.onIssueCall?.();
				if (opts.rejectIssues) throw new Error("issue search unavailable");
				return { issues: opts.issueHits ?? [] };
			},
		});
	});
}

function makeSearchIssue(id: string, title: string, prNumber: number) {
	return create(IssueSchema, {
		id,
		title,
		repo: "RigelBuild/compass",
		number: prNumber,
		prs: [
			create(PullRequestSchema, {
				repo: "RigelBuild/compass",
				number: prNumber,
				title: `PR ${prNumber}`,
				forgeState: "open",
			}),
		],
	});
}

function makeSearchHit(
	id: string,
	topicId: string,
	text: string,
	channelId = "",
) {
	return create(MessageSchema, {
		id,
		topicId,
		channelId,
		blocks: [
			create(MessageBlockSchema, {
				block: { case: "text", value: text },
			}),
		],
	});
}
function makeAskSearchHit(id: string, topicId: string, question: string) {
	return create(MessageSchema, {
		id,
		topicId,
		blocks: [
			create(MessageBlockSchema, {
				block: {
					case: "ask",
					value: create(AskSchema, {
						questions: [
							create(AskQuestionSchema, { questionId: "q1", question }),
						],
					}),
				},
			}),
		],
	});
}

function makeSearchHitWithLeadingNewline(id: string, topicId: string) {
	return create(MessageSchema, {
		id,
		topicId,
		blocks: [
			create(MessageBlockSchema, {
				block: { case: "text", value: "\nA title after a blank line" },
			}),
		],
	});
}

function liveSearchClients(transport: Transport) {
	return {
		comms: createCommsClient(transport),
		compass: createCompassClient(transport),
	};
}

const CURRENT_GEN = () => 1;

describe("createStoreDestinationProviders", () => {
	test("an empty query lists only the local destination kinds", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const byKind = await queryDestinations(providers, "", 1, CURRENT_GEN);
			expect(byKind).not.toBeNull();
			const kinds = byKind as Map<string, Destination[]>;
			for (const kind of ["agent", "channel", "topic", "view"]) {
				expect((kinds.get(kind) ?? []).length).toBeGreaterThan(0);
			}
			expect(kinds.has("issue")).toBe(false);
			expect(kinds.has("pr")).toBe(false);
		});
	});

	test("the views provider yields exactly Bridge/Backlog/Done/Settings", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const views = await providers.find((p) => p.id === "views")?.query("");
			expect((views ?? []).map((d) => d.title).sort()).toEqual([
				"Backlog",
				"Bridge",
				"Done",
				"Settings",
			]);
		});
	});

	test("one remote SearchIssues response feeds issue and PR rows in hit order", async () => {
		await withStoreAsync(async (store) => {
			let calls = 0;
			// Hit order (444's issue first) conflicts with PR number and title order.
			const issues = [
				makeSearchIssue("ws-864", "Tauri desktop shell", 444),
				makeSearchIssue("ws-1023", "Agent process management", 443),
			];
			const providers = createStoreDestinationProviders(
				store,
				liveSearchClients(
					issueSearchTransport({
						issueHits: issues,
						onIssueCall: () => calls++,
					}),
				),
			);
			const byKind = await queryDestinations(
				providers,
				"shell",
				1,
				CURRENT_GEN,
			);
			expect(calls).toBe(1);
			const order = (rows: Destination[] = []) =>
				[...rows]
					.sort((a, b) => (b.score ?? 0) - (a.score ?? 0))
					.map((row) => row.id);
			expect(order(byKind?.get("issue"))).toEqual(["ws-864", "ws-1023"]);
			expect(order(byKind?.get("pr"))).toEqual([
				"RigelBuild/compass#444",
				"RigelBuild/compass#443",
			]);
			const row = (kind: "issue" | "pr", id: string) =>
				byKind?.get(kind)?.find((dest) => dest.id === id);
			row("issue", "ws-864")?.navigate();
			await flush();
			expect(store.selectedIssue()?.id).toBe("ws-864");
			row("pr", "RigelBuild/compass#443")?.navigate();
			await flush();
			expect(store.selectedIssue()?.id).toBe("ws-1023");
			expect(store.activeRightTab()).toBe("pr");
		});
	});

	test("empty and whitespace issue queries do not make RPC calls", async () => {
		await withStoreAsync(async (store) => {
			let calls = 0;
			const providers = createStoreDestinationProviders(
				store,
				liveSearchClients(issueSearchTransport({ onIssueCall: () => calls++ })),
			);
			for (const id of ["issues", "prs"]) {
				const provider = providers.find((item) => item.id === id);
				expect(await provider?.query("")).toEqual([]);
				expect(await provider?.query(" \t ")).toEqual([]);
			}
			expect(calls).toBe(0);
		});
	});

	test("without clients, issue and PR rows come from the held board", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const byKind = await queryDestinations(
				providers,
				"tauri",
				1,
				CURRENT_GEN,
			);
			expect(byKind?.get("issue")?.map((row) => row.id)).toEqual(["ws-864"]);
			expect(byKind?.get("pr")?.map((row) => row.id)).toEqual([
				"RigelBuild/compass#444",
			]);
		});
	});

	test("a repeated issue query after the search settles fetches again", async () => {
		await withStoreAsync(async (store) => {
			let calls = 0;
			const providers = createStoreDestinationProviders(
				store,
				liveSearchClients(issueSearchTransport({ onIssueCall: () => calls++ })),
			);
			await queryDestinations(providers, "match", 1, CURRENT_GEN);
			await queryDestinations(providers, "match", 2, () => 2);
			expect(calls).toBe(2);
		});
	});

	test("a rejected issue search preserves other destination groups", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(
				store,
				liveSearchClients(issueSearchTransport({ rejectIssues: true })),
			);
			const byKind = await queryDestinations(
				providers,
				"settings",
				1,
				CURRENT_GEN,
			);
			expect(byKind?.get("issue")).toBeUndefined();
			expect(byKind?.get("pr")).toBeUndefined();
			expect(byKind?.get("view")?.map((view) => view.title)).toEqual([
				"Settings",
			]);
		});
	});

	test("the agents provider navigates via the store's in-memory route path", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const agents =
				(await providers.find((p) => p.id === "agents")?.query("")) ?? [];
			expect(agents.length).toBeGreaterThan(0);
			agents[0].navigate();
			await flush();
			expect(store.view()).toBe("agent");
		});
	});

	test("a query filters each provider's rows by fuzzy match", async () => {
		await withStoreAsync(async (store) => {
			const providers = createStoreDestinationProviders(store);
			const views =
				(await providers.find((p) => p.id === "views")?.query("sett")) ?? [];
			expect(views.map((d) => d.title)).toEqual(["Settings"]);
		});
	});
	test("message hits map to rows and navigate through their topic", async () => {
		await withStoreAsync(async (store) => {
			let calls = 0;
			const clients = liveSearchClients(
				searchTransport(
					[
						makeSearchHit(
							"msg-search",
							"top-ann-posture",
							"First line\nSecond line",
						),
					],
					() => calls++,
				),
			);
			const providers = createStoreDestinationProviders(store, clients);
			const messages = await providers
				.find((p) => p.id === "messages")
				?.query("posture");
			expect(calls).toBe(1);
			expect(
				messages?.map((message) => [message.id, message.title, message.kind]),
			).toEqual([["msg-search", "First line", "message"]]);
			messages?.[0]?.navigate();
			await flush();
			expect(store.view()).toBe("topic");
			expect(store.selectedTopicId()).toBe("top-ann-posture");
		});
	});
	test("message titles truncate at 120 characters", async () => {
		await withStoreAsync(async (store) => {
			const longText = "x".repeat(121);
			const clients = liveSearchClients(
				searchTransport([
					makeSearchHit("msg-long", "top-ann-posture", longText),
				]),
			);
			const messages = await createStoreDestinationProviders(store, clients)
				.find((provider) => provider.id === "messages")
				?.query("x");
			expect(messages?.[0]?.title).toBe(`${"x".repeat(117)}…`);
		});
	});

	test("ask-only hits use the question as their title", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(
				searchTransport([
					makeAskSearchHit("msg-ask", "top-ann-posture", "Which approach?"),
				]),
			);
			const messages = await createStoreDestinationProviders(store, clients)
				.find((provider) => provider.id === "messages")
				?.query("approach");
			expect(messages?.map((message) => message.title)).toEqual([
				"Which approach?",
			]);
		});
	});

	test("message titles skip leading blank lines", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(
				searchTransport([
					makeSearchHitWithLeadingNewline("msg-newline", "top-ann-posture"),
				]),
			);
			const messages = await createStoreDestinationProviders(store, clients)
				.find((provider) => provider.id === "messages")
				?.query("title");
			expect(messages?.map((message) => message.title)).toEqual([
				"A title after a blank line",
			]);
		});
	});

	test("message hits with no text or ask question are dropped", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(
				searchTransport([makeSearchHit("msg-blank", "top-ann-posture", "\n ")]),
			);
			const messages = await createStoreDestinationProviders(store, clients)
				.find((provider) => provider.id === "messages")
				?.query("blank");
			expect(messages).toEqual([]);
		});
	});

	test("message provider returns no rows without clients", async () => {
		await withStoreAsync(async (store) => {
			const messages = await createStoreDestinationProviders(store)
				.find((provider) => provider.id === "messages")
				?.query("search");
			expect(messages).toEqual([]);
		});
	});

	test("a hit without a channel id outside the topic set is dropped", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(
				searchTransport([
					makeSearchHit("msg-off-set", "top-archived", "Archived hit"),
				]),
			);
			const providers = createStoreDestinationProviders(store, clients);
			const messages = await providers
				.find((p) => p.id === "messages")
				?.query("archived");
			expect(messages).toEqual([]);
		});
	});

	test("a hit outside the topic set routes by its wire channel id", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(
				searchTransport([
					makeSearchHit(
						"msg-off-set",
						"top-archived",
						"Archived hit",
						"ch-announcements",
					),
				]),
			);
			const providers = createStoreDestinationProviders(store, clients);
			const messages = await providers
				.find((p) => p.id === "messages")
				?.query("archived");
			expect(messages?.map((message) => message.id)).toEqual(["msg-off-set"]);
			messages?.[0]?.navigate();
			await flush();
			// openTopic(topicId) alone no-ops here: the topic is not in the client set.
			expect(store.selectedChannelId()).toBe("ch-announcements");
			expect(store.selectedTopicId()).toBe("top-archived");
		});
	});

	test("message rows keep the server's best-match-first order", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(
				searchTransport([
					makeSearchHit("msg-best", "top-ann-posture", "body mentions it"),
					makeSearchHit("msg-next", "top-ann-posture", "posture"),
				]),
			);
			const providers = createStoreDestinationProviders(store, clients);
			const byKind = await queryDestinations(
				providers,
				"posture",
				1,
				CURRENT_GEN,
			);
			expect((byKind?.get("message") ?? []).map((d) => d.id)).toEqual([
				"msg-best",
				"msg-next",
			]);
		});
	});

	test("empty and whitespace message queries do not make RPC calls", async () => {
		await withStoreAsync(async (store) => {
			let calls = 0;
			const clients = liveSearchClients(searchTransport([], () => calls++));
			const providers = createStoreDestinationProviders(store, clients);
			const messages = providers.find((p) => p.id === "messages");
			expect(await messages?.query("")).toEqual([]);
			expect(await messages?.query("  \t ")).toEqual([]);
			expect(calls).toBe(0);
		});
	});

	test("a rejected message search preserves other destination groups", async () => {
		await withStoreAsync(async (store) => {
			const clients = liveSearchClients(searchTransport([], undefined, true));
			const providers = createStoreDestinationProviders(store, clients);
			const byKind = await queryDestinations(
				providers,
				"settings",
				1,
				CURRENT_GEN,
			);
			expect(byKind?.get("message")).toBeUndefined();
			expect(byKind?.get("view")?.map((view) => view.title)).toEqual([
				"Settings",
			]);
		});
	});
});

describe("queryDestinations", () => {
	test("per-provider isolation: a rejected provider drops only its group", async () => {
		const good: DestinationProvider = {
			id: "good",
			query: () =>
				Promise.resolve([
					{ id: "a", title: "Alpha", kind: "agent", navigate: () => {} },
				]),
		};
		const bad: DestinationProvider = {
			id: "bad",
			query: () => Promise.reject(new Error("provider blew up")),
		};
		const byKind = await queryDestinations([good, bad], "", 1, CURRENT_GEN);
		expect(byKind).not.toBeNull();
		const kinds = byKind as Map<string, Destination[]>;
		expect((kinds.get("agent") ?? []).map((d) => d.id)).toEqual(["a"]);
		// The rejected provider contributed no group; the surface still resolved.
		expect(kinds.size).toBe(1);
	});

	test("latest-wins: a stale-generation resolution applies nothing (returns null)", async () => {
		let release!: (v: Destination[]) => void;
		const pending: DestinationProvider = {
			id: "slow",
			query: () =>
				new Promise<Destination[]>((resolve) => {
					release = resolve;
				}),
		};
		// generation captured at issue is 1, but the counter has since moved to 2.
		const result = queryDestinations([pending], "", 1, () => 2);
		release([{ id: "z", title: "Zed", kind: "agent", navigate: () => {} }]);
		expect(await result).toBeNull();
	});

	test("a current-generation resolution applies its results", async () => {
		let release!: (v: Destination[]) => void;
		const pending: DestinationProvider = {
			id: "slow",
			query: () =>
				new Promise<Destination[]>((resolve) => {
					release = resolve;
				}),
		};
		const result = queryDestinations([pending], "", 5, () => 5);
		release([{ id: "z", title: "Zed", kind: "agent", navigate: () => {} }]);
		const byKind = await result;
		expect(byKind).not.toBeNull();
		expect((byKind as Map<string, Destination[]>).get("agent")?.[0].id).toBe(
			"z",
		);
	});
});
