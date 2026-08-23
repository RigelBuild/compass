/**
 * Store-backed destination providers for the command palette's navigation mode
 * (RIG-2483, A4/D9). One provider per `DestinationKind` — agents, channels,
 * topics, views, issues, prs — each reading a reactive store accessor and
 * mapping its rows to `Destination`s whose `navigate()` routes through the
 * store's own action seam (so a palette jump and a click share one home).
 *
 * Providers are `Promise`-returning by contract (`DestinationProvider.query`);
 * the store-backed ones resolve synchronously-wrapped today, but the issue
 * provider rides the async tracker seam and later kinds may be genuinely async,
 * so `queryDestinations` is race-safe now via the latest-wins generation guard.
 *
 * Filtering + ranking is the in-house `fuzzyScore` (D2) over the destination
 * title; an empty query passes everything (score 0). `score` is stamped on each
 * `Destination` so the surface can order within a kind.
 */

import type { AppStore } from "../store";
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
 * The four static view destinations (Bridge/Backlog/Done/Settings) — the same
 * `show*` paths the D6 seed commands fire, surfaced as navigation results.
 */
const VIEW_TARGETS: readonly { id: string; title: string }[] = [
	{ id: "bridge", title: "Bridge" },
	{ id: "backlog", title: "Backlog" },
	{ id: "done", title: "Done" },
	{ id: "settings", title: "Settings" },
];

/**
 * Build every store-backed destination provider (all six kinds ship, D9). Each
 * provider's `query` resolves against the store's live accessors at call time,
 * so a streamed roster/issue update is reflected on the next keystroke.
 */
export function createStoreDestinationProviders(
	store: AppStore,
): DestinationProvider[] {
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
						store.topics().map((t) => ({ id: t.id, title: t.name })),
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
						if (item.id === "backlog") store.showBacklog();
						else if (item.id === "done") store.showDone();
						else if (item.id === "settings") store.showSettings();
						else store.showBridge();
					}),
				),
		},
		{
			id: "issues",
			query: (input) =>
				Promise.resolve(
					scored(
						store.assignedIssues().map((w) => ({ id: w.id, title: w.title })),
						"issue",
						input,
						(item) => () => store.selectIssue(item.id),
					),
				),
		},
		{
			id: "prs",
			query: (input) =>
				Promise.resolve(
					scored(
						store.prs().map((row) => ({
							id: `${row.pr.repo}#${row.pr.number}`,
							title: row.pr.title,
							issueId: row.issue.id,
						})),
						"pr",
						input,
						(item) => () => {
							store.selectIssue(item.issueId);
							store.setActiveRightTab("pr");
						},
					),
				),
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
