// A hand-written CompassClient double for driving the store's agent-lifecycle
// and boot-probe paths without a server — today StopAgentSession (recorded
// verbatim so a test asserts the exact wire request the UI issued) and
// GetServerInfo (the boot liveness/version probe the daemon banner reads).
//
// It models the ONE server behaviour a permissive double would hide: the RPC is
// Runner-backed, so a server built with no RunnerHub attached answers
// `Unavailable` (go/server/service.go:152-154) rather than succeeding. That is a
// real, expected condition on the socket-only path, and the UI must surface it —
// `failNextStop` is how a test drives it; `failNextProbe` drives the boot
// probe's rejection (server down / RPC error) the same one-shot way.
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
	/** The server info GetServerInfo returns — the boot probe reads it into the
	 *  daemon banner. Set before constructing the store to drive the live path. */
	serverInfo: { version: string; apiVersion: string };
	/** Reject the next GetServerInfo with `error` (one-shot) — the server-down /
	 *  RPC-error path the boot probe must leave the banner offline for. */
	failNextProbe: (error: Error) => void;
	/** The account id WhoAmI returns — the caller the UI learns from the server
	 *  at boot (compass.proto WhoAmI), the boot source that replaced the env var.
	 *  Defaults to the fixture caller so boot/store tests resolve a caller with no
	 *  setup. Set before boot to drive an alternate identity. */
	whoAmIAccountId: { accountId: string };
	/** Reject the next WhoAmI with `error` (one-shot) — the server-answered-but-
	 *  identity-unlearnable path a boot guard must surface. */
	failNextWhoAmI: (error: Error) => void;
}

/** Build the fake. Pure and synchronous apart from the RPC's promise. */
export function createFakeCompass(): FakeCompass {
	const stops: RecordedStop[] = [];
	let stopFailure: Error | undefined;
	let probeFailure: Error | undefined;
	let whoAmIFailure: Error | undefined;
	const serverInfo = { version: "9.9.9-test", apiVersion: "compass.v1" };
	// Defaults to the fixture caller (store.ts CALLER_ID) so boot/store tests
	// resolve a caller with no per-test setup.
	const whoAmIAccountId = { accountId: "acc-matt" };

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
		getServerInfo: async (_req: Record<string, never>) => {
			if (probeFailure) {
				const err = probeFailure;
				probeFailure = undefined;
				throw err;
			}
			return {
				version: serverInfo.version,
				apiVersion: serverInfo.apiVersion,
			};
		},
		whoAmI: async (_req: Record<string, never>) => {
			if (whoAmIFailure) {
				const err = whoAmIFailure;
				whoAmIFailure = undefined;
				throw err;
			}
			return { accountId: whoAmIAccountId.accountId };
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
		serverInfo,
		failNextProbe: (error) => {
			probeFailure = error;
		},
		whoAmIAccountId,
		failNextWhoAmI: (error) => {
			whoAmIFailure = error;
		},
	};
}
