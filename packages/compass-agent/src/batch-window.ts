// The idle batching window: an idle agent waits for a quiet period (capped) before
// it starts a turn, so a burst of inbound items lands in one turn.

export interface BatchTimer {
	now(): number;
	/** Schedule fire after ms; returns a cancel thunk. */
	set(ms: number, fire: () => void): () => void;
}

export interface BatchWindow {
	readonly quietMs: number;
	readonly maxMs: number;
	readonly timer?: BatchTimer; // default realBatchTimer
}

export const realBatchTimer: BatchTimer = {
	now: () => Date.now(),
	set(ms, fire) {
		// biome-ignore lint/style/noRestrictedGlobals: production batching-window timer; tests inject a hand-driven BatchTimer instead
		const handle = setTimeout(fire, ms);
		return () => clearTimeout(handle);
	},
};

export const DEFAULT_BATCH_WINDOW: BatchWindow = {
	quietMs: 10_000,
	maxMs: 60_000,
};
