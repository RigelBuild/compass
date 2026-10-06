// The tour's product events over the analytics embed. Type-only import: tour code
// never loads posthog-js, so with the flag off every capture is the embed's no-op.

import type { Analytics } from "../analytics/analytics";

export type TourEvent =
	| { name: "tour_started"; trigger: "first-run" | "replay" | "resume" }
	| { name: "tour_step_viewed"; step_id: string; index: number }
	| { name: "tour_dismissed"; step_id: string }
	| { name: "tour_completed" };

/** Sends one tour event through the embed the caller holds at call time. An
 *  absent embed (offline store, tests) is a silent no-op. */
export function captureTourEvent(
	analytics: Analytics | undefined,
	event: TourEvent,
): void {
	const { name, ...props } = event;
	analytics?.capture(name, props);
}
