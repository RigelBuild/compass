/** Shared destination grouping and debounce controls for search surfaces. */
import type { Destination, DestinationKind } from "./commands";

const DEFAULT_SEARCH_DEBOUNCE_MS = 150;
export let SEARCH_DEBOUNCE_MS = DEFAULT_SEARCH_DEBOUNCE_MS;

export function setSearchDebounceMsForTest(ms: number): void {
	SEARCH_DEBOUNCE_MS = ms;
}

export function resetSearchDebounceForTest(): void {
	SEARCH_DEBOUNCE_MS = DEFAULT_SEARCH_DEBOUNCE_MS;
}
export const KIND_LABELS: Record<DestinationKind, string> = {
	view: "Views",
	agent: "Agents",
	channel: "Channels",
	topic: "Topics",
	issue: "Issues",
	pr: "Pull requests",
};

export const KIND_ORDER: readonly DestinationKind[] = [
	"view",
	"agent",
	"channel",
	"topic",
	"issue",
	"pr",
];

export interface DestinationSurfaceRow {
	readonly key: string;
	readonly title: string;
	readonly groupLabel: string;
	readonly groupStart: boolean;
	navigate(): void;
}

export function destinationSurfaceRows(
	byKind: ReadonlyMap<DestinationKind, readonly Destination[]>,
): DestinationSurfaceRow[] {
	const rows: DestinationSurfaceRow[] = [];
	for (const kind of KIND_ORDER) {
		const destinations = byKind.get(kind);
		if (!destinations?.length) continue;
		const groupLabel = KIND_LABELS[kind];
		destinations.forEach((destination, index) => {
			rows.push({
				key: `dest:${kind}:${destination.id}`,
				title: destination.title,
				groupLabel,
				groupStart: index === 0,
				navigate: destination.navigate,
			});
		});
	}
	return rows;
}
