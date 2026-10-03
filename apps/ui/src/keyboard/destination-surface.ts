import type { Destination, DestinationKind } from "./commands";

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
