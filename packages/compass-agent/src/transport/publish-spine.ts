// The per-session Publish spine, shared by the socket FrameSink (trace/session
// frames) and the socket ControlSource (control-plane ack frames). One ordered
// publisher per session is load-bearing: the Runner assigns a monotonic
// RunnerSeq to each frame in arrival order (agent_gateway.proto Publish), so a
// single ordered producer keeps hub gap-detection well-defined. Both producers
// feed THIS spine, reached only through `RunnerTransport.publishSpine()` (a
// memoized singleton) — the frozen C4 factory signatures take just `transport`,
// so the transport is the sole shared handle they have.
//
// Delivery mechanism — STREAM CYCLING. A long-lived client-stream held open for
// the whole session does NOT flush under bun's http2: the runtime buffers DATA
// frames until the request stream ENDS, so a session-lifetime `publish()` never
// delivers a frame while it stays open. The spine therefore sends in bounded
// BATCHES: it opens `publish()`, feeds the currently-queued frames into a
// generator that then RETURNS (ending the stream, which flushes), and opens a
// fresh `publish()` for the next batch. The Runner assigns the upstream seq and
// hub gap-detection tolerates a publisher that reconnects, so cycling the stream
// is wire-safe. A batch closes as soon as the queue drains, so a lone frame
// (notably the terminal STOPPED at teardown) flushes in its own immediate batch.
//
// Overload policy (transport-consolidation OQ-2(c), P1 #2): trace/session frames
// are loss-tolerable and ride a BOUNDED queue — on overflow the OLDEST queued
// trace frame is dropped and a counter incremented (surfaced as a session
// diagnostic). Control-plane ack frames and lifecycle/status frames (notably the
// terminal STOPPED) are NOT loss-tolerable: they ride a separate priority queue
// that is never the drop target and is always drained AHEAD of the trace
// backlog, both within a batch and by forcing the next batch, so teardown
// delivers STOPPED to the socket within the shutdown deadline even when the
// trace buffer is saturated. The never-drop guarantee extends to a FAILED cycled
// batch: a batch send that throws (socket blip mid-teardown) drops its trace
// frames (loss-tolerable, counted) but re-enqueues its priority frames at the
// front and retries on a bounded backoff, so a transient error on the final
// flush does not silently abandon STOPPED; only after the retry budget are those
// priority frames counted as definitively failed, in a SEPARATE counter from the
// trace drops (a never-drop loss is surfaced distinctly, never as a trace drop).

import type { PublishFrameRequest } from "../gen/compass/v1/agent_gateway_pb";

// The bounded trace/session send buffer. Chosen once here: large enough to
// absorb a normal burst of trace frames against transient backpressure, small
// enough that a wedged Runner cannot grow in-agent memory without bound
// (container OOM is the failure this cap prevents, OQ-2(c)/P1 #2).
export const TRACE_QUEUE_CAP = 1024;

// The most frames one cycled batch carries before it closes and a fresh stream
// opens. Bounds how much a single client-stream buffers before its flushing
// end(), and caps the work the drain generator does per cycle.
export const PUBLISH_BATCH_MAX = 256;

// Bounded-backoff retry for a failed cycled batch that carried PRIORITY frames
// (lifecycle/STOPPED, control acks — not loss-tolerable). Unlike trace frames,
// which are dropped on a failed batch, the priority frames are re-enqueued at
// the front and the batch retried on this schedule; after the last delay they
// are counted as definitively failed so drain() stays bounded on a dead socket.
const PRIORITY_BATCH_RETRY_MS: readonly number[] = [50, 200, 800];

export interface PublishSpine {
	// Enqueue a trace/session frame. Loss-tolerable: if the trace queue is at the
	// cap, the OLDEST queued trace frame is dropped and the drop counter
	// incremented. Never blocks the caller (emit() stays synchronous/void).
	enqueueTrace(frame: PublishFrameRequest): void;
	// Enqueue a priority frame (control-plane ack or lifecycle/status transition).
	// Never dropped; drained ahead of the trace backlog.
	enqueuePriority(frame: PublishFrameRequest): void;
	// How many trace frames have been LOST — overflow drops plus the trace frames
	// in a failed cycled batch. Loss-tolerable by contract; surfaced as a session
	// diagnostic.
	droppedTraceCount(): number;
	// How many PRIORITY frames (lifecycle/STOPPED, control acks) were lost after
	// their failed batch exhausted the bounded priority retry. NOT loss-tolerable
	// — a non-zero count is a contract breach surfaced distinctly, never folded
	// into droppedTraceCount.
	failedPriorityCount(): number;
	// Flush every queued frame (priority ahead of trace) and resolve once the
	// last batch's stream has closed. Idempotent; after it resolves the spine
	// rejects further enqueues (teardown is terminal).
	drain(): Promise<void>;
}

// Build the spine over a driver that consumes an AsyncIterable of frames (the
// transport's `client.publish`, whose promise resolves at stream end). The
// driver is invoked once per batch, lazily — an idle agent that never emits
// opens no stream.
export function createPublishSpine(
	publish: (stream: AsyncIterable<PublishFrameRequest>) => Promise<unknown>,
): PublishSpine {
	const priority: PublishFrameRequest[] = [];
	const trace: PublishFrameRequest[] = [];
	let dropped = 0;
	// Trace frames lost when a cycled batch's send failed (transient transport
	// error). Trace is loss-tolerable, so these are counted into the same
	// diagnostic as overflow drops — NOT the priority counter below.
	let failedBatchFrames = 0;
	// Priority frames (lifecycle/STOPPED, control acks) lost after a failed batch
	// exhausted the bounded priority retry. Kept separate from trace drops: a
	// never-drop loss must be surfaced as its own contract breach.
	let failedPriorityFrames = 0;
	let ended = false;
	// The in-flight batch pump, if one is running. A single pump drains both
	// queues batch by batch until they are empty, then clears itself; a later
	// enqueue starts a fresh pump. Serializing on one pump keeps global frame
	// order (priority-first, FIFO within class) and one stream open at a time.
	let pump: Promise<void> | undefined;

	// Pull up to PUBLISH_BATCH_MAX frames, priority first, for one batch, and
	// report how many leading frames came from the priority lane — the caller
	// needs the split to treat a failed batch's priority frames (never-drop)
	// differently from its trace frames (loss-tolerable).
	function takeBatch(): {
		batch: PublishFrameRequest[];
		priorityCount: number;
	} {
		const batch: PublishFrameRequest[] = [];
		while (batch.length < PUBLISH_BATCH_MAX && priority.length > 0) {
			// biome-ignore lint/style/noNonNullAssertion: length checked
			batch.push(priority.shift()!);
		}
		const priorityCount = batch.length;
		while (batch.length < PUBLISH_BATCH_MAX && trace.length > 0) {
			// biome-ignore lint/style/noNonNullAssertion: length checked
			batch.push(trace.shift()!);
		}
		return { batch, priorityCount };
	}

	// Drain the queues one cycled stream at a time. Each iteration opens a fresh
	// `publish()` over a generator that yields the batch then RETURNS, so the
	// stream ends and bun flushes it. Loops until both queues are empty.
	async function runPump(): Promise<void> {
		// Consecutive failed batches that carried priority frames, reset on any
		// successful send. Bounds the priority retry so a persistently-dead socket
		// cannot wedge drain().
		let priorityRetries = 0;
		while (priority.length > 0 || trace.length > 0) {
			const { batch, priorityCount } = takeBatch();
			async function* oneBatch(): AsyncGenerator<PublishFrameRequest> {
				for (const frame of batch) yield frame;
			}
			try {
				await publish(oneBatch());
				priorityRetries = 0;
			} catch {
				// A batch send failed (socket dropped, Runner mid-restart). Trace
				// frames are the loss-tolerable class: a failed trace batch is NOT
				// retried (the durable conversation path is a separate awaited unary)
				// and the dropped frames are counted with the overflow drops.
				failedBatchFrames += batch.length - priorityCount;
				if (priorityCount === 0) continue;
				// Priority frames (lifecycle/STOPPED — notably the terminal flush)
				// are NOT loss-tolerable: re-enqueue them at the FRONT and retry the
				// batch on a bounded backoff rather than dropping them, so a transient
				// blip on the final flush does not silently abandon STOPPED. After the
				// retry budget they are counted as definitively failed — kept SEPARATE
				// from trace drops (a never-drop loss is not a loss-tolerable drop) —
				// and retrying stops so drain() stays bounded.
				if (priorityRetries >= PRIORITY_BATCH_RETRY_MS.length) {
					failedPriorityFrames += priorityCount;
					continue;
				}
				priority.unshift(...batch.slice(0, priorityCount));
				const delay = PRIORITY_BATCH_RETRY_MS[priorityRetries];
				priorityRetries++;
				await new Promise((r) => setTimeout(r, delay));
			}
		}
	}

	// Start the pump if idle; when it finishes, clear the handle unless more
	// frames arrived meanwhile (in which case keep pumping).
	function kick(): void {
		if (pump !== undefined) return;
		pump = (async () => {
			try {
				// Defer the first batch one microtask so a synchronous burst of
				// emit()s — e.g. a saturated trace backlog followed by the terminal
				// STOPPED, all enqueued in one tick — is fully queued before the first
				// takeBatch() runs. takeBatch drains priority-first, so STOPPED then
				// leads the batch AHEAD of the trace backlog (the record's terminal-
				// flush guarantee), rather than a trace frame racing out in a batch
				// opened before STOPPED was enqueued.
				await Promise.resolve();
				do {
					await runPump();
					// A frame enqueued during the final publish() is caught by the
					// while-guard here before we clear the pump.
				} while (priority.length > 0 || trace.length > 0);
			} finally {
				pump = undefined;
			}
		})();
	}

	return {
		enqueueTrace(frame) {
			if (ended) return;
			if (trace.length >= TRACE_QUEUE_CAP) {
				trace.shift();
				dropped++;
			}
			trace.push(frame);
			kick();
		},
		enqueuePriority(frame) {
			if (ended) return;
			priority.push(frame);
			kick();
		},
		droppedTraceCount() {
			return dropped + failedBatchFrames;
		},
		failedPriorityCount() {
			return failedPriorityFrames;
		},
		async drain() {
			ended = true;
			// Flush whatever is already queued (e.g. the terminal STOPPED enqueued
			// just before teardown). enqueue is now closed, but kick() re-opens the
			// pump for the queued frames, and its do/while keeps cycling batches
			// until both queues empty — so drain resolves only once all are sent.
			kick();
			await pump;
		},
	};
}
