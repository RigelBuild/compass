// Run work once the browser is idle, so it never competes with first paint.

/** Upper bound before idle work runs anyway on a busy main thread. */
export const IDLE_TIMEOUT_MS = 2000;

/** Delay used where `requestIdleCallback` is missing (WebKit has none). */
export const IDLE_FALLBACK_MS = 50;

/** Schedule `run` for idle time; returns a cancel function. */
export function whenIdle(run: () => void): () => void {
	if (typeof window.requestIdleCallback === "function") {
		const handle = window.requestIdleCallback(() => run(), {
			timeout: IDLE_TIMEOUT_MS,
		});
		return () => window.cancelIdleCallback(handle);
	}
	const handle = window.setTimeout(run, IDLE_FALLBACK_MS);
	return () => window.clearTimeout(handle);
}
