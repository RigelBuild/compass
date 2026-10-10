/** Destination providers for palette and top-bar search (RIG-2483, A4/D9).
 * Agents, channels, topics, and views are local. Issues, PRs, and messages use
 * live search clients; without clients, issues and PRs fall back to the held board.
 *
 * Providers are async; `queryDestinations` isolates failures and drops stale
 * results. Local rows use fuzzy scores; remote rows retain server rank.
 */

import type { Message } from "@compass/client";
import { prRows } from "../board";
import { adaptIssue } from "../live/adapt";
import type { LiveClients } from "../live/client";
import type { AppStore } from "../store";
import type { Issue } from "../stub-data";
import type {
	Destination,
	DestinationKind,
	DestinationProvider,
} from "./commands";
import { fuzzyScore } from "./fuzzy";

/** Filter+score a set of `{ id, title }` candidates against `input`, mapping the
 *  survivors to `Destination`s of `kind` with the given `navigate` factory. */
function scored<T extends { id: string; title: string }>(
	items: readonly T[],
	kind: DestinationKind,
	input: string,
	navigate: (item: T) => () => void,
): Destination[] {
	const out: Destination[] = [];
	for (const item of items) {
		const score = fuzzyScore(input, item.title);
		if (score === null) continue;
		out.push({
			id: item.id,
			title: item.title,
			kind,
			navigate: navigate(item),
			score,
		});
	}
	return out;
}

/**
 * The five static view destinations (Agents/Bridge/Backlog/Done/Settings) — the
 * same `show*` paths the D6 seed commands fire, surfaced as navigation results.
 */
const VIEW_TARGETS = [
	{ id: "agents", title: "Agents" },
	{ id: "bridge", title: "Bridge" },
	{ id: "backlog", title: "Backlog" },
	{ id: "done", title: "Done" },
	{ id: "settings", title: "Settings" },
] as const satisfies readonly { id: string; title: string }[];

// Keyed on the target ids, so a new view target cannot ship without its route.
const VIEW_SHOW = {
	agents: "showAgents",
	bridge: "showBridge",
	backlog: "showBacklog",
	done: "showDone",
	settings: "showSettings",
} as const satisfies Record<
	(typeof VIEW_TARGETS)[number]["id"],
	keyof AppStore
>;

function mapMessageHit(
	message: Message,
	store: AppStore,
	rank: number,
): Destination[] {
	// SearchMessages hits carry their channel; fall back to the topic set if empty.
	const channelId =
		message.channelId ||
		store.topics().find((candidate) => candidate.id === message.topicId)
			?.channelId;
	if (!channelId) return [];
	const textTitle = message.blocks
		.filter((block) => block.block.case === "text")
		.flatMap((block) =>
			block.block.case === "text" ? block.block.value.split(/\r?\n/) : [],
		)
		.map((line) => line.trim())
		.find((line) => line.length > 0);
	const askTitle = message.blocks
		.filter((block) => block.block.case === "ask")
		.flatMap((block) =>
			block.block.case === "ask"
				? block.block.value.questions.map((question) =>
						question.question.trim(),
					)
				: [],
		)
		.find((question) => question.length > 0);
	const firstLine = textTitle ?? askTitle;
	if (!firstLine) return [];
	const title =
		firstLine.length > 120 ? `${firstLine.slice(0, 117)}…` : firstLine;
	// The server returns best-match-first; score by rank so the group sort keeps it.
	const score = -rank;
	return [
		{
			id: message.id,
			title,
			kind: "message",
			score,
			navigate: () => store.openTopic(message.topicId, channelId),
		},
	];
}

/**
 * Build every store-backed destination provider. Live search clients are absent
 * in fixture mode; local providers still resolve from the store's accessors.
 */
export function createStoreDestinationProviders(
	store: AppStore,
	clients?: Pick<LiveClients, "comms" | "compass">,
): DestinationProvider[] {
	// Issue and PR rows share one in-flight SearchIssues; dropped once it settles
	// so a repeated query refetches rather than replaying stale or failed results.
	let inFlight: { query: string; promise: Promise<Issue[]> } | undefined;
	const searchIssues = (query: string): Promise<Issue[]> => {
		// Fixture mode has no server: fuzzy-match the held board so PR rows stay testable (§A4).
		if (!clients) {
			const hits = store.issues().flatMap((issue) => {
				const score = fuzzyScore(query, issue.title);
				return score === null ? [] : [{ issue, score }];
			});
			hits.sort((a, b) => b.score - a.score);
			return Promise.resolve(hits.map((hit) => hit.issue));
		}
		if (inFlight?.query === query) return inFlight.promise;
		const entry = {
			query,
			promise: clients.compass
				.searchIssues({ query, limit: 50 })
				.then((response) => response.issues.map(adaptIssue))
				.finally(() => {
					if (inFlight === entry) inFlight = undefined;
				}),
		};
		inFlight = entry;
		return entry.promise;
	};
	return [
		{
			id: "agents",
			query: (input) =>
				Promise.resolve(
					scored(
						store.agents().map((a) => ({
							id: a.account.id,
							title: a.account.displayName,
						})),
						"agent",
						input,
						(item) => () => store.openAgent(item.id),
					),
				),
		},
		{
			id: "channels",
			query: (input) =>
				Promise.resolve(
					scored(
						store.channels().map((c) => ({ id: c.id, title: c.name })),
						"channel",
						input,
						(item) => () => store.openChannel(item.id),
					),
				),
		},
		{
			id: "topics",
			query: (input) =>
				Promise.resolve(
					scored(
						store
							.topics()
							.filter((t) => !t.archived)
							.map((t) => ({ id: t.id, title: t.name })),
						"topic",
						input,
						(item) => () => store.openTopic(item.id),
					),
				),
		},
		{
			id: "views",
			query: (input) =>
				Promise.resolve(
					scored(VIEW_TARGETS, "view", input, (item) => () => {
						store[VIEW_SHOW[item.id]]();
					}),
				),
		},
		{
			id: "issues",
			query: async (input) => {
				const query = input.trim();
				if (!query) return [];
				const issues = await searchIssues(query);
				return issues.map((issue, rank) => ({
					kind: "issue",
					id: issue.id,
					title: issue.title,
					score: -rank,
					navigate: () => store.selectIssue(issue.id),
				}));
			},
		},
		{
			id: "prs",
			query: async (input) => {
				const query = input.trim();
				if (!query) return [];
				const issues = await searchIssues(query);
				return prRows(issues).map(({ issue, pr }, index) => ({
					kind: "pr",
					id: `${pr.repo}#${pr.number}`,
					title: pr.title,
					score: -index,
					navigate: () => {
						store.selectIssue(issue.id);
						store.setActiveRightTab("pr");
					},
				}));
			},
		},
		{
			id: "messages",
			query: async (input) => {
				const query = input.trim();
				if (!query || !clients) return [];
				const response = await clients.comms.searchMessages({
					query,
					limit: 50,
				});
				return response.messages.flatMap((message, rank) =>
					mapMessageHit(message, store, rank),
				);
			},
		},
	];
}

/**
 * Query every provider for `input` and group the survivors by kind, with two
 * guarantees:
 *   - **Per-provider isolation:** one rejected provider drops only its group;
 *     the surface still renders every provider that resolved (`allSettled`).
 *   - **Latest-wins:** `generation` is captured at issue and re-checked against
 *     `currentGeneration()` at resolve — a stale resolution (a slow keystroke-N
 *     provider landing after keystroke-N+1 fired) returns `null` and applies
 *     nothing, so it can never clobber newer results.
 */
export async function queryDestinations(
	providers: readonly DestinationProvider[],
	input: string,
	generation: number,
	currentGeneration: () => number,
): Promise<Map<DestinationKind, Destination[]> | null> {
	const settled = await Promise.allSettled(
		providers.map((p) => p.query(input)),
	);
	if (generation !== currentGeneration()) return null; // stale — drop wholesale

	const byKind = new Map<DestinationKind, Destination[]>();
	for (const result of settled) {
		if (result.status !== "fulfilled") continue; // rejected provider → skip group
		for (const dest of result.value) {
			const group = byKind.get(dest.kind);
			if (group) group.push(dest);
			else byKind.set(dest.kind, [dest]);
		}
	}
	for (const group of byKind.values()) {
		group.sort((a, b) => (b.score ?? 0) - (a.score ?? 0));
	}
	return byKind;
}
