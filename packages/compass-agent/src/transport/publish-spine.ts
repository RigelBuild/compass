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

import {
	Duration,
	Effect,
	Either,
	Fiber,
	Logger,
	ManagedRuntime,
	Option,
	Queue,
} from "effect";
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
	// The spine's transitional ManagedRuntime (design record
	// docs/designs/platform/compass-agent-effect-adoption/design.md, T3/OQ-3).
	// Effect is confined module-private here: it backs the sliding trace queue,
	// the wake latch, and the forked pump fiber, and is disposed at the end of
	// drain(). T5 consolidates runtime ownership into the transport. The default
	// logger is removed so a handled pump-send failure does not double-report to
	// the console (the loss disposition is already folded into the drop counters),
	// mirroring the frame-sink's own runtime.
	const runtime = ManagedRuntime.make(Logger.remove(Logger.defaultLogger));
	// Trace/session lane: a bounded drop-OLDEST sliding queue. The sync emit()
	// path reads unsafeSize() BEFORE offering (size == cap ⇒ the imminent offer
	// evicts the oldest) and then runs the effectful offer synchronously — sliding
	// offer never suspends, so runSync cannot throw on a live queue, and the
	// eviction stays synchronously countable. unsafeOffer is NOT used: in effect
	// 3.22.1 it bypasses the sliding strategy and rejects the NEWEST element on a
	// full queue (drop-newest), the opposite of this lane's contract (see the
	// design.md T3 amendment and effect-smoke.test.ts).
	const traceQ = runtime.runSync(
		Queue.sliding<PublishFrameRequest>(TRACE_QUEUE_CAP),
	);
	// Priority lane: a plain array (ruled — design record OQ-7; effect 3.22.1
	// ships no primitive that is FIFO, front-reinsertable on a failed batch, and
	// synchronously drainable at once). Drained AHEAD of the trace backlog.
	const priority: PublishFrameRequest[] = [];
	// Wake latch: a capacity-1 sliding<void>. enqueueTrace/enqueuePriority offer a
	// unit (unsafeOffer is correct HERE — a 1-slot signal, not the trace lane —
	// and coalesces a synchronous burst into one wake); the pump blocks on take
	// while both lanes are empty, so an agent that never emits opens no stream.
	const wake = runtime.runSync(Queue.sliding<void>(1));
	let dropped = 0;
	// Trace frames lost when a cycled batch's send failed (transient transport
	// error). Trace is loss-tolerable, so these are counted into the same
	// diagnostic as overflow drops — NOT the priority counter below.
	let failedBatchFrames = 0;
	// Priority frames (lifecycle/STOPPED, control acks) lost after a failed batch
	// exhausted the bounded priority retry. Kept separate from trace drops: a
	// never-drop loss must be surfaced as its own contract breach.
	let failedPriorityFrames = 0;
	// Terminal flag (drain has begun): late enqueues become silent no-ops. NOT
	// Queue.shutdown, which throws on a late offer.
	let ended = false;
	// One-shot idempotency latch for drain(): a second call is a no-op (the first
	// already disposed the runtime).
	let drained = false;
	// The forked pump fiber, started lazily on the first enqueue. A single fiber
	// drains both lanes batch by batch, blocking on the wake latch while idle;
	// drain() joins it (never interrupts — interrupt could abandon queued
	// never-drop priority frames).
	let pumpFiber: Fiber.RuntimeFiber<void> | undefined;

	function traceSize(): number {
		return Option.getOrElse(traceQ.unsafeSize(), () => 0);
	}

	// Pull up to PUBLISH_BATCH_MAX frames, priority first, for one batch, and
	// report how many leading frames came from the priority lane — the caller
	// needs the split to treat a failed batch's priority frames (never-drop)
	// differently from its trace frames (loss-tolerable).
	const takeBatch = Effect.gen(function* () {
		const batch: PublishFrameRequest[] = [];
		while (batch.length < PUBLISH_BATCH_MAX && priority.length > 0) {
			// biome-ignore lint/style/noNonNullAssertion: length checked
			batch.push(priority.shift()!);
		}
		const priorityCount = batch.length;
		const room = PUBLISH_BATCH_MAX - batch.length;
		if (room > 0) {
			const traceFrames = yield* Queue.takeUpTo(traceQ, room);
			for (const frame of traceFrames) batch.push(frame);
		}
		return { batch, priorityCount };
	});

	// The pump: drain the lanes one cycled stream at a time. Each iteration opens
	// a fresh `publish()` over a generator that yields the batch then RETURNS, so
	// the stream ends and bun flushes it. Priority-first, cap PUBLISH_BATCH_MAX.
	// Terminal exit = ended && both lanes empty; the fiber returning is what
	// resolves drain()'s join.
	const pumpLoop = Effect.gen(function* () {
		// Consecutive failed batches that carried priority frames, reset on any
		// successful send. This pump-run-scoped budget bounds the priority retry so
		// a persistently-dead socket cannot wedge drain(): it caps the total retry
		// delay across ALL queued priority batches at O(1) (a per-batch
		// Schedule.fromDelays would make drain() O(N) on a dead socket).
		let priorityRetries = 0;
		// Defer the first batch one scheduler yield so a synchronous burst of
		// emit()s — e.g. a saturated trace backlog followed by the terminal
		// STOPPED, all enqueued in one tick — is fully queued before the first
		// takeBatch runs. takeBatch drains priority-first, so STOPPED then leads the
		// batch AHEAD of the trace backlog (the record's terminal-flush guarantee),
		// rather than a trace frame racing out in a batch opened before STOPPED was
		// enqueued.
		yield* Effect.yieldNow();
		while (true) {
			// Block while idle: an agent that never emits opens no stream. A stale
			// coalesced wake causes at most one immediate take before re-blocking.
			while (priority.length === 0 && traceSize() === 0 && !ended) {
				yield* Queue.take(wake);
			}
			if (ended && priority.length === 0 && traceSize() === 0) return;
			const { batch, priorityCount } = yield* takeBatch;
			async function* oneBatch(): AsyncGenerator<PublishFrameRequest> {
				for (const frame of batch) yield frame;
			}
			const result = yield* Effect.either(
				Effect.tryPromise(() => publish(oneBatch())),
			);
			if (Either.isRight(result)) {
				priorityRetries = 0;
				continue;
			}
			// A batch send failed (socket dropped, Runner mid-restart). Trace frames
			// are the loss-tolerable class: a failed trace batch is NOT retried (the
			// durable conversation path is a separate awaited unary) and the dropped
			// frames are counted with the overflow drops.
			failedBatchFrames += batch.length - priorityCount;
			if (priorityCount === 0) continue;
			// Priority frames (lifecycle/STOPPED — notably the terminal flush) are
			// NOT loss-tolerable: re-enqueue them at the FRONT and retry the batch on
			// a bounded backoff rather than dropping them, so a transient blip on the
			// final flush does not silently abandon STOPPED. After the retry budget
			// they are counted as definitively failed — kept SEPARATE from trace
			// drops (a never-drop loss is not a loss-tolerable drop) — and retrying
			// stops so drain() stays bounded.
			if (priorityRetries >= PRIORITY_BATCH_RETRY_MS.length) {
				failedPriorityFrames += priorityCount;
				continue;
			}
			priority.unshift(...batch.slice(0, priorityCount));
			// The `>= length` guard above is the sole bound, so priorityRetries is
			// in [0, length-1] here — index directly, no clamp needed.
			const delay = PRIORITY_BATCH_RETRY_MS[priorityRetries];
			priorityRetries++;
			yield* Effect.sleep(Duration.millis(delay));
		}
	});

	// Start the pump if it has not been forked yet. A forked fiber that finds both
	// lanes empty blocks on the wake latch, so forking on the first enqueue opens
	// no stream by itself.
	function kick(): void {
		if (pumpFiber !== undefined) return;
		pumpFiber = runtime.runFork(pumpLoop);
	}

	return {
		enqueueTrace(frame) {
			if (ended) return;
			// Pre-offer size read is the observable-drop contract: on a full sliding
			// queue the effectful offer evicts the OLDEST, which the offer itself
			// does not signal. Sync producer, so the read and the offer share one
			// tick and no take interleaves.
			if (traceSize() >= TRACE_QUEUE_CAP) dropped++;
			runtime.runSync(Queue.offer(traceQ, frame));
			Queue.unsafeOffer(wake, undefined);
			kick();
		},
		enqueuePriority(frame) {
			if (ended) return;
			priority.push(frame);
			Queue.unsafeOffer(wake, undefined);
			kick();
		},
		droppedTraceCount() {
			return dropped + failedBatchFrames;
		},
		failedPriorityCount() {
			return failedPriorityFrames;
		},
		async drain() {
			// Idempotent: a second drain is a no-op (the first disposed the runtime).
			if (drained) return;
			drained = true;
			ended = true;
			try {
				// Wake the (possibly idle-blocked) pump so it observes `ended`, flushes
				// the remaining queued frames priority-first (a same-tick STOPPED still
				// leads), and RETURNS. Joining the fiber — never interrupting it — is
				// what keeps a queued never-drop priority frame from being abandoned.
				Queue.unsafeOffer(wake, undefined);
				if (pumpFiber !== undefined) {
					await runtime.runPromise(Fiber.join(pumpFiber));
				}
			} finally {
				// Dispose the transitional runtime at the END of drain, in a `finally`
				// so a throwing join cannot strand it. T5 consolidates runtime
				// ownership later.
				await runtime.dispose();
			}
		},
	};
}
