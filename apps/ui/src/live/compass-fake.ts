// A hand-written CompassClient double for driving the store's agent-lifecycle and
// boot-probe paths without a server — StopAgentSession (recorded verbatim) and
// GetServerInfo (the boot probe). Models the ONE behaviour a permissive double hides:
// a Runner-backed RPC answers `Unavailable` with no RunnerHub. Also scripts the
// session-status and SubscribeAgentSession streams. Sibling of comms-fake.ts. Dev/test-only.

import {
	type AgentSessionFrame,
	AgentSessionFrameSchema,
	AgentSessionState,
	AgentSessionStatusSchema,
	type CompassClient,
	create,
	type SubscribeEventsResponse,
	SubscribeEventsResponseSchema,
	type SessionEvent as WireSessionEvent,
} from "@compass/client";

/** A push queue one stream at a time drains: items pushed with no reader wait
 *  for the next one. */
interface FrameQueue<T> {
	push: (item: T) => void;
	/** Yield queued and future items until `signal` aborts. */
	drain: (signal?: AbortSignal) => AsyncGenerator<T>;
}

function createFrameQueue<T>(): FrameQueue<T> {
	const items: T[] = [];
	let wake: (() => void) | undefined;
	return {
		push: (item) => {
			items.push(item);
			wake?.();
		},
		drain: async function* (signal) {
			while (!signal?.aborted) {
				const next = items.shift();
				if (next !== undefined) {
					yield next;
					continue;
				}
				const { promise, resolve } = Promise.withResolvers<void>();
				wake = resolve;
				signal?.addEventListener("abort", () => resolve(), { once: true });
				await promise;
				wake = undefined;
			}
		},
	};
}

/** One SubscribeAgentSession the UI opened; `aborted` flips when it cancels. */
export interface RecordedSessionSubscribe {
	readonly sessionId: string;
	aborted: boolean;
}

/** One recorded StopAgentSession request. */
export interface RecordedStop {
	readonly sessionId: string;
}

/** One recorded SkipBatchWindow request. */
export interface RecordedSkip {
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
	/** Every SkipBatchWindow the UI issued, in order. */
	readonly skips: RecordedSkip[];
	/** Reject the next SkipBatchWindow after recording it. */
	failNextSkip: (error: Error) => void;
	/** The server info GetServerInfo returns — the boot probe reads it into the
	 *  daemon banner. Set before constructing the store to drive the live path. */
	serverInfo: { version: string; apiVersion: string; rev: string };
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
	/** Emit an agentSessionStatus on the SubscribeEvents stream — the source of
	 *  the session ids the store tails. */
	pushSessionStatus: (
		accountId: string,
		sessionId: string,
		state: AgentSessionState,
		atUnixMs?: number,
	) => void;
	/** Emit a resyncRequired on the SubscribeEvents stream: the store clears its
	 *  session map and the events driver cold-starts. */
	pushResync: () => void;
	/** Queue one SubscribeAgentSession frame for `sessionId`. Delivered to the
	 *  open subscription, else held for the next one. */
	pushSessionFrame: (
		sessionId: string,
		frame: { event?: WireSessionEvent; state?: AgentSessionState },
	) => void;
	/** Every SubscribeAgentSession the UI opened, in order. */
	readonly sessionSubscribes: RecordedSessionSubscribe[];
	/** The session ids with a currently open SubscribeAgentSession. */
	openSessionTails: () => string[];
	/** Reject the next SubscribeAgentSession with `error` (one-shot), after
	 *  recording it; with `hold`, the rejection waits until `hold` settles. */
	failNextSessionSubscribe: (error: Error, hold?: Promise<void>) => void;
}

/** Build the fake. Pure and synchronous apart from the RPC's promise. */
export function createFakeCompass(): FakeCompass {
	const stops: RecordedStop[] = [];
	let stopFailure: Error | undefined;
	const skips: RecordedSkip[] = [];
	let skipFailure: Error | undefined;
	let probeFailure: Error | undefined;
	let whoAmIFailure: Error | undefined;
	const serverInfo = {
		version: "9.9.9-test",
		apiVersion: "compass.v1",
		rev: "",
	};
	// Defaults to the fixture caller (store.ts CALLER_ID) so boot/store tests
	// resolve a caller with no per-test setup.
	const whoAmIAccountId = { accountId: "acc-matt" };
	const events = createFrameQueue<SubscribeEventsResponse>();
	let eventSeq = 0n;
	const sessionQueues = new Map<string, FrameQueue<AgentSessionFrame>>();
	const sessionQueue = (sessionId: string): FrameQueue<AgentSessionFrame> => {
		let queue = sessionQueues.get(sessionId);
		if (!queue) {
			queue = createFrameQueue();
			sessionQueues.set(sessionId, queue);
		}
		return queue;
	};
	const sessionSubscribes: RecordedSessionSubscribe[] = [];
	let sessionSubscribeFailure:
		| { error: Error; hold?: Promise<void> }
		| undefined;

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
		skipBatchWindow: async (req: { sessionId: string }) => {
			skips.push({ sessionId: req.sessionId });
			if (skipFailure) {
				const err = skipFailure;
				skipFailure = undefined;
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
				rev: serverInfo.rev,
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
		// The board read stream. Yields only what a test pushes via pushSessionStatus
		// and holds open until abort; board events are scripted in events.test.ts.
		subscribeEvents: (
			_req: unknown,
			opts?: { signal?: AbortSignal },
		): AsyncGenerator<SubscribeEventsResponse> => events.drain(opts?.signal),
		// The cold-start re-snapshot the events driver reads at its first frame.
		listBoardIssues: async () => ({ issues: [] }),
		// Sends the server's registration ack first, then the session's queue.
		subscribeAgentSession: async function* (
			req: { sessionId: string },
			opts?: { signal?: AbortSignal },
		): AsyncGenerator<AgentSessionFrame> {
			const record = { sessionId: req.sessionId, aborted: false };
			sessionSubscribes.push(record);
			try {
				if (sessionSubscribeFailure) {
					const { error, hold } = sessionSubscribeFailure;
					sessionSubscribeFailure = undefined;
					await hold;
					throw error;
				}
				yield create(AgentSessionFrameSchema, { sessionId: req.sessionId });
				yield* sessionQueue(req.sessionId).drain(opts?.signal);
			} finally {
				record.aborted = true;
			}
		},
	};

	return {
		// The double implements only the driven subset, so a structural check
		// would (rightly) reject it; the unknown-cast is the one sanctioned seam,
		// mirroring comms-fake.ts's.
		client: client as unknown as CompassClient,
		stops,
		skips,
		failNextSkip: (error) => {
			skipFailure = error;
		},
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
		pushSessionStatus: (accountId, sessionId, state, atUnixMs = 0) => {
			eventSeq++;
			events.push(
				create(SubscribeEventsResponseSchema, {
					seq: eventSeq,
					atUnixMs: BigInt(atUnixMs),
					instanceEpoch: 1n,
					payload: {
						case: "agentSessionStatus",
						value: create(AgentSessionStatusSchema, {
							sessionId,
							agentAccountId: accountId,
							state,
						}),
					},
				}),
			);
		},
		pushResync: () => {
			events.push(
				create(SubscribeEventsResponseSchema, {
					seq: 0n,
					instanceEpoch: 1n,
					payload: { case: "resyncRequired", value: {} },
				}),
			);
		},
		pushSessionFrame: (sessionId, frame) => {
			sessionQueue(sessionId).push(
				create(AgentSessionFrameSchema, {
					sessionId,
					event: frame.event,
					state: frame.state ?? AgentSessionState.UNSPECIFIED,
				}),
			);
		},
		sessionSubscribes,
		openSessionTails: () =>
			sessionSubscribes.filter((s) => !s.aborted).map((s) => s.sessionId),
		failNextSessionSubscribe: (error, hold) => {
			sessionSubscribeFailure = { error, hold };
		},
	};
}
