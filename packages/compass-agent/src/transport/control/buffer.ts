// The single-consumer async queue behind the socket ControlSource's iterable.
// Split out of `control-source.ts` so the queue's policy — and the reason it
// differs from the FrameSink's — is readable on its own.

import type { AgentControl } from "./../../control";

// One op waiting for the consumer to pull, tagged with its Runner-assigned seq so
// the source can ack it (apply-then-ack) and dedup a redelivery of it.
export interface Queued {
	readonly op: AgentControl;
	readonly seq: bigint;
}

// A single-consumer async queue: the pump `push`es decoded representable ops, the
// iterable `pull`s them. `close()` ends it cleanly (→ STOPPED); `fail()` ends it
// with the drop error once the reconnect budget is spent.
export class AsyncBuffer {
	// Intentionally UNCAPPED, unlike the FrameSink's trace queue (TRACE_QUEUE_CAP,
	// drop-oldest under a wedged consumer, OQ-2(c)). Control ops are NOT
	// loss-tolerable — silently dropping the oldest would lose a prompt/askAnswer
	// the agent must apply — so the trace path's drop-oldest is the wrong policy
	// here. Unbounded growth is bounded in practice by the Runner's own retention:
	// it only redelivers ops past the acked cursor, and control volume is low, so
	// the backlog a parked consumer + reconnect can accumulate is small. The
	// asymmetry with the trace cap is a deliberate decision, not an oversight.
	#items: Queued[] = [];
	#closed = false;
	#error: unknown;
	#waiting:
		| {
				resolve: (r: IteratorResult<Queued>) => void;
				reject: (e: unknown) => void;
		  }
		| undefined;

	push(item: Queued): void {
		if (this.#closed) return;
		const w = this.#waiting;
		if (w !== undefined) {
			this.#waiting = undefined;
			w.resolve({ value: item, done: false });
		} else {
			this.#items.push(item);
		}
	}

	close(): void {
		if (this.#closed) return;
		this.#closed = true;
		const w = this.#waiting;
		if (w !== undefined) {
			this.#waiting = undefined;
			w.resolve({ value: undefined, done: true });
		}
	}

	fail(err: unknown): void {
		if (this.#closed) return;
		this.#closed = true;
		this.#error = err;
		const w = this.#waiting;
		if (w !== undefined) {
			this.#waiting = undefined;
			w.reject(err);
		}
	}

	pull(): Promise<IteratorResult<Queued>> {
		const next = this.#items.shift();
		if (next !== undefined)
			return Promise.resolve({ value: next, done: false });
		if (this.#error !== undefined) return Promise.reject(this.#error);
		if (this.#closed) return Promise.resolve({ value: undefined, done: true });
		return new Promise((resolve, reject) => {
			// Single-consumer, enforced rather than documented: a second concurrent
			// pull() would overwrite #waiting and abandon the first promise forever
			// — a silent hang. The class is exported now, so the invariant is no
			// longer protected by being file-local. Name the violation instead.
			if (this.#waiting !== undefined) {
				reject(new Error("AsyncBuffer is single-consumer: concurrent pull()"));
				return;
			}
			this.#waiting = { resolve, reject };
		});
	}
}
