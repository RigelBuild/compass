// The reconnect backoff shared by the compass stream drivers: full-jitter
// exponential waits that resolve early on abort.

/** The first retry waits up to RECONNECT_BASE_MS; each later one doubles the
 *  ceiling up to RECONNECT_CAP_MS. Full jitter spreads a fleet's reconnects so a
 *  server restart doesn't trigger a synchronized thundering herd. */
const RECONNECT_BASE_MS = 500;
const RECONNECT_CAP_MS = 30_000;

/** Await `ms`, resolving early if `signal` aborts — a teardown during backoff
 *  returns promptly instead of blocking out the full delay. */
function abortableDelay(ms: number, signal?: AbortSignal): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	if (signal?.aborted) {
		resolve();
		return promise;
	}
	// biome-ignore lint/style/noRestrictedGlobals: production abort-aware backoff delay (resolves early on signal), cleared on abort; not a test wait
	const timer = setTimeout(() => {
		signal?.removeEventListener("abort", onAbort);
		resolve();
	}, ms);
	const onAbort = () => {
		clearTimeout(timer);
		resolve();
	};
	signal?.addEventListener("abort", onAbort, { once: true });
	return promise;
}

/** A reconnect backoff: `wait()` sleeps a jittered, escalating delay; `reset()`
 *  drops the ceiling back to the base once the connection makes progress. */
export interface ReconnectBackoff {
	wait: () => Promise<void>;
	reset: () => void;
}

export function createReconnectBackoff(signal?: AbortSignal): ReconnectBackoff {
	let failures = 0;
	return {
		wait: () => {
			const ceiling = Math.min(
				RECONNECT_CAP_MS,
				RECONNECT_BASE_MS * 2 ** failures,
			);
			failures++;
			return abortableDelay(Math.random() * ceiling, signal);
		},
		reset: () => {
			failures = 0;
		},
	};
}
