import {
	closureChanges,
	EXIT,
	type ImageLane,
	type ProbeResult,
	type Resolved,
	RetagError,
	resolveAncestor,
} from "./retag-core.ts";

export type PublishDecision = {
	shouldPublish: boolean;
	reason: string;
};

/**
 * Publish unless no first-parent commit since the newest published ancestor
 * touched this lane's closure. Diffing only the push's `before` misses a
 * closure change whose publish is still queued. A net tree diff misses a head
 * that reverts one, which tag-main then rejects once the reverted image lands.
 */
export async function decidePublish(input: {
	ancestors: readonly string[];
	probe: (sha12: string) => Promise<ProbeResult>;
	/** Every path any first-parent commit after `commit` touched, up to the head. */
	touchedSince: (commit: string) => Promise<string[]>;
	closurePaths: string;
	lane: ImageLane;
}): Promise<PublishDecision> {
	let resolved: Resolved;
	try {
		resolved = await resolveAncestor(input.ancestors, input.probe, input.lane);
	} catch (err) {
		if (err instanceof RetagError && err.code === EXIT.noAncestor) {
			return {
				shouldPublish: true,
				reason: "no published ancestor in the walk",
			};
		}
		throw err;
	}
	if (resolved.position === 1) {
		return {
			shouldPublish: false,
			reason: `:git-${resolved.sha12} is already published`,
		};
	}
	const hits = closureChanges(
		input.closurePaths,
		await input.touchedSince(resolved.commit),
		input.lane,
	);
	return hits.length > 0
		? {
				shouldPublish: true,
				reason: `commits since published :git-${resolved.sha12} touch ${hits.join(", ")}`,
			}
		: {
				shouldPublish: false,
				reason: `no commit since :git-${resolved.sha12} touches the ${input.lane} closure`,
			};
}
