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
 * Publish unless the newest published first-parent ancestor already carries
 * this head's lane closure. Diffing only the push's `before` misses a closure
 * change whose own publish is still queued, so the paired tag would find a
 * stale image.
 */
export async function decidePublish(input: {
	ancestors: readonly string[];
	probe: (sha12: string) => Promise<ProbeResult>;
	changedSince: (commit: string) => Promise<string[]>;
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
		await input.changedSince(resolved.commit),
		input.lane,
	);
	return hits.length > 0
		? {
				shouldPublish: true,
				reason: `newest published :git-${resolved.sha12} lacks ${hits.join(", ")}`,
			}
		: {
				shouldPublish: false,
				reason: `:git-${resolved.sha12} carries the ${input.lane} closure`,
			};
}
