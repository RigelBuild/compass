// A hand-written CompassClient double for driving the store's agent-lifecycle
// path without a server — today just StopAgentSession, recorded verbatim so a
// test asserts the exact wire request the UI issued.
//
// It models the ONE server behaviour a permissive double would hide: the RPC is
// Runner-backed, so a server built with no RunnerHub attached answers
// `Unavailable` (go/server/service.go:152-154) rather than succeeding. That is a
// real, expected condition on the socket-only path, and the UI must surface it —
// `failNextStop` is how a test drives it.
//
// Sibling of comms-fake.ts (the CommsClient double) and shaped the same way, so
// every live-path suite describes ONE server. Dev/test-only — nothing in the
// shipped app imports it.

import type { CompassClient } from "@compass/client";

/** One recorded StopAgentSession — the whole request is a session id, the
 *  cursor StartAgentSession minted (compass_pb.ts:831-836). */
export interface RecordedStop {
	readonly sessionId: string;
}

export interface FakeCompass {
	readonly client: CompassClient;
	/** Every StopAgentSession the UI issued, in order. */
	readonly stops: RecordedStop[];
	/** Reject the next StopAgentSession with `error` (one-shot) — the
	 *  `Unavailable` (no RunnerHub) path the socket-only server really answers
	 *  with. Thrown BEFORE the request is recorded is wrong: the UI DID issue it,
	 *  so it is recorded first and then refused. */
	failNextStop: (error: Error) => void;
}

/** Build the fake. Pure and synchronous apart from the RPC's promise. */
export function createFakeCompass(): FakeCompass {
	const stops: RecordedStop[] = [];
	let stopFailure: Error | undefined;

	const client = {
		stopAgentSession: async (req: { sessionId: string }) => {
			stops.push({ sessionId: req.sessionId });
			if (stopFailure) {
				const err = stopFailure;
				stopFailure = undefined;
				throw err;
			}
			return {};
		},
	};

	return {
		// The double implements only the driven subset, so a structural check
		// would (rightly) reject it; the unknown-cast is the one sanctioned seam,
		// mirroring comms-fake.ts's.
		client: client as unknown as CompassClient,
		stops,
		failNextStop: (error) => {
			stopFailure = error;
		},
	};
}
