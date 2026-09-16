// The per-session Publish spine, shared by the socket FrameSink (trace/session frames)
// and the socket ControlSource (control-plane ack frames). One ordered publisher is
// load-bearing: the Runner assigns a monotonic RunnerSeq in arrival order, so a single
// ordered producer keeps hub gap-detection well-defined. Both reach it via publishSpine().

// Delivery — STREAM CYCLING. A long-lived client-stream does NOT flush under bun's http2
// (DATA frames buffer until the request stream ENDS), so the spine sends in bounded
// BATCHES: open publish(), feed the queued frames into a generator that RETURNS (flushing),
// open a fresh publish() next. A lone terminal STOPPED flushes immediately in its own batch.

// Overload policy (OQ-2(c), P1 #2): trace/session frames are loss-tolerable on a BOUNDED
// queue — overflow drops the OLDEST trace frame, counted. Acks and lifecycle/status frames
// (notably terminal STOPPED) are NOT loss-tolerable: they ride a priority queue never the
// drop target, always drained AHEAD of the trace backlog, so STOPPED makes the deadline.

// The never-drop guarantee extends to a FAILED cycled batch: a send that throws drops its
// trace frames (counted) but re-enqueues its priority frames at the front and retries on a
// bounded backoff. Only after the retry budget are those priority frames counted failed,
// in a SEPARATE counter.

import {
	Duration,
	Effect,
	Either,
	Fiber,
	Logger,
	ManagedRuntime,
	Metric,
	Option,
	Queue,
} from "effect";
import type { PublishFrameRequest } from "../gen/compass/v1/agent_gateway_pb";
import {
	batchesFlushedBy,
	batchSizeBy,
	priorityBatchRetries,
	priorityFramesLost,
	priorityRetryDepthGauge,
	traceFramesLostFailedBatch,
	traceFramesLostOverflow,
	traceQueueDepthGauge,
} from "./otel-metrics";
import type { TransportRuntime } from "./runtime-channel";

// The bounded trace/session send buffer. Chosen once here: large enough to
// absorb a normal burst of trace frames against transient backpressure, small
// enough that a wedged Runner cannot grow in-agent memory without bound
// (container OOM is the failure this cap prevents, OQ-2(c)/P1 #2).
export const TRACE_QUEUE_CAP = 1024;

// The most frames one cycled batch carries before it closes and a fresh stream
// opens. Bounds how much a single client-stream buffers before its flushing
// end(), and caps the work the drain generator does per cycle.
export const PUBLISH_BATCH_MAX = 256;

// Bounded-backoff retry for a failed cycled batch carrying PRIORITY frames (not loss-
// tolerable). Unlike trace frames (dropped on a failed batch), priority frames are re-
// enqueued at the front and retried on this schedule; after the last delay they are
// counted definitively failed so drain() stays bounded on a dead socket.
export const PRIORITY_BATCH_RETRY_MS: readonly number[] = [50, 200, 800];

export interface PublishSpine {
	// Enqueue a trace/session frame. Loss-tolerable: if the trace queue is at the
	// cap, the OLDEST queued trace frame is dropped and the drop counter
	// incremented. Never blocks the caller (emit() stays synchronous/void).
	enqueueTrace(frame: PublishFrameRequest): void;
	// Enqueue a priority frame (control-plane ack or lifecycle/status transition).
	// Never dropped; drained ahead of the trace backlog.
	enqueuePriority(frame: PublishFrameRequest): void;
	// How many trace frames have been LOST — overflow drops plus the trace frames in a
	// failed cycled batch. Loss-tolerable by contract; surfaced as a session diagnostic.
	droppedTraceCount(): number;
	// How many PRIORITY frames were lost after their failed batch exhausted the bounded
	// retry. NOT loss-tolerable — a non-zero count is a contract breach surfaced distinctly.
	failedPriorityCount(): number;
	// Flush every queued frame (priority ahead of trace) and resolve once the
	// last batch's stream has closed. Idempotent; after it resolves the spine
	// rejects further enqueues (teardown is terminal).
	drain(): Promise<void>;
}

// Build the spine over a driver that consumes an AsyncIterable of frames (the transport's
// client.publish, resolving at stream end). Invoked once per batch, lazily — an idle agent
// opens no stream.

// `borrowedRuntime` is the single transport-owned ManagedRuntime threaded in by
// createUnixSocketTransport (§T5). The spine takes `publish`, not `transport`, so the
// runtime is passed by argument. When absent (test paths) it falls back to its OWN runtime
// and disposes it at drain() end; a borrowed one is NEVER disposed here.

// A `metricNamespace` prefixes the two LEVEL gauges (trace_queue_depth, priority_retry_
// depth) and the flush-shape rows. Defaults to "" — production yields the frozen names. A
// test passes a unique prefix so its reads hit a private registry entry, immune to the
// cross-file race (the global registry keys on the metric name): a gauge is last-writer-
// wins, and an exclusivity assertion needs the counters it did NOT expect to read zero.
export function createPublishSpine(
	publish: (stream: AsyncIterable<PublishFrameRequest>) => Promise<unknown>,
	borrowedRuntime?: TransportRuntime,
	metricNamespace = "",
): PublishSpine {
	const traceQueueDepth = traceQueueDepthGauge(metricNamespace);
	const priorityRetryDepth = priorityRetryDepthGauge(metricNamespace);
	const batchesFlushed = batchesFlushedBy(metricNamespace);
	const batchSize = batchSizeBy(metricNamespace);
	// Effect is confined module-private behind the spine: it backs the sliding trace queue,
	// the wake latch, and the forked pump fiber. The default logger is removed on the
	// fallback runtime so a handled pump-send failure does not double-report (the loss
	// disposition is already folded into the drop counters).
	const ownsRuntime = borrowedRuntime === undefined;
	const runtime =
		borrowedRuntime ?? ManagedRuntime.make(Logger.remove(Logger.defaultLogger));
	// Trace/session lane: a bounded drop-OLDEST sliding queue. The sync emit() path reads
	// unsafeSize() BEFORE offering (at cap ⇒ the imminent offer evicts the oldest) then runs
	// the effectful offer synchronously — sliding offer never suspends, so the eviction stays
	// synchronously countable. unsafeOffer is NOT used: it drops-newest, the opposite contract.
	const traceQ = runtime.runSync(
		Queue.sliding<PublishFrameRequest>(TRACE_QUEUE_CAP),
	);
	// Priority lane: a plain array (OQ-7; effect 3.22.1 ships no primitive that is FIFO,
	// front-reinsertable on a failed batch, and synchronously drainable). Drained AHEAD of trace.
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
		// Snapshot the teardown flag FIRST: the trace drain below yields, and
		// drain() can set `ended` inside that window, which would misclassify a
		// batch taken while the spine was still live as a drain batch.
		const takenAfterTeardown = ended;
		const batch: PublishFrameRequest[] = [];
		while (batch.length < PUBLISH_BATCH_MAX && priority.length > 0) {
			// biome-ignore lint/style/noNonNullAssertion: length checked
			batch.push(priority.shift()!);
		}
		const priorityCount = batch.length;
		// Sample the trace backlog at the moment of the take, BEFORE draining it,
		// so the gauge reports the depth at the take (the saturation a depth gauge
		// exists to surface) rather than the post-drain residual (Decision 2 gauge).
		yield* Metric.set(traceQueueDepth, traceSize());
		const room = PUBLISH_BATCH_MAX - batch.length;
		if (room > 0) {
			const traceFrames = yield* Queue.takeUpTo(traceQ, room);
			for (const frame of traceFrames) batch.push(frame);
		}
		return { batch, priorityCount, takenAfterTeardown };
	});

	// The pump: drain the lanes one cycled stream at a time. Each iteration opens a fresh
	// publish() over a generator that yields the batch then RETURNS, so the stream ends and
	// bun flushes. Priority-first, cap PUBLISH_BATCH_MAX. Terminal exit = ended && both lanes
	// empty; the fiber returning is what resolves drain()'s join.
	const pumpLoop = Effect.gen(function* () {
		// Consecutive failed batches that carried priority frames, reset on any successful
		// send. This pump-run-scoped budget bounds the priority retry so a dead socket cannot
		// wedge drain(): it caps total retry delay across ALL queued priority batches at O(1)
		// (a per-batch Schedule.fromDelays would make drain() O(N) on a dead socket).
		let priorityRetries = 0;
		// Defer the first batch one scheduler yield so a synchronous burst of emit()s — a
		// saturated trace backlog followed by the terminal STOPPED, all in one tick — is fully
		// queued before the first takeBatch. takeBatch drains priority-first, so STOPPED leads
		// the batch AHEAD of the trace backlog rather than a trace frame racing out first.
		yield* Effect.yieldNow();
		while (true) {
			// Block while idle: an agent that never emits opens no stream. A stale
			// coalesced wake causes at most one immediate take before re-blocking.
			while (priority.length === 0 && traceSize() === 0 && !ended) {
				yield* Queue.take(wake);
			}
			if (ended && priority.length === 0 && traceSize() === 0) return;
			const { batch, priorityCount, takenAfterTeardown } = yield* takeBatch;
			// Flush shape, classified on the flag as it stood AT the take. `drain`
			// means taken after teardown began, so a full batch during teardown
			// still reads `full` — saturation is the more specific fact.
			yield* Metric.increment(
				batch.length === PUBLISH_BATCH_MAX
					? batchesFlushed.full
					: takenAfterTeardown
						? batchesFlushed.drain
						: batchesFlushed.short,
			);
			yield* Metric.update(
				priorityCount === batch.length
					? batchSize.priority
					: priorityCount === 0
						? batchSize.trace
						: batchSize.mixed,
				batch.length,
			);
			async function* oneBatch(): AsyncGenerator<PublishFrameRequest> {
				for (const frame of batch) yield frame;
			}
			const result = yield* Effect.either(
				Effect.tryPromise(() => publish(oneBatch())),
			).pipe(
				// Decision 1 span: one per cycled batch send. Attributes are all known
				// at span open (batch composition + pump-scoped retry level). The span
				// wraps only the Effect.either(tryPromise) send — a failed batch is a
				// normal counted event (Either resolves Left), so span status stays ok.
				Effect.withSpan("compass_agent.transport.publish.batch", {
					attributes: {
						batch_size: batch.length,
						priority_count: priorityCount,
						retry_index: priorityRetries,
					},
				}),
			);
			if (Either.isRight(result)) {
				priorityRetries = 0;
				// Successful send resets the pump-scoped retry-depth level.
				yield* Metric.set(priorityRetryDepth, 0);
				continue;
			}
			// A batch send failed (socket dropped, Runner mid-restart). Trace frames
			// are the loss-tolerable class: a failed trace batch is NOT retried (the
			// durable conversation path is a separate awaited unary) and the dropped
			// frames are counted with the overflow drops.
			failedBatchFrames += batch.length - priorityCount;
			// Trace loss on a failed batch (Decision 2 counter, reason=failed_batch).
			yield* Metric.incrementBy(
				traceFramesLostFailedBatch,
				batch.length - priorityCount,
			);
			if (priorityCount === 0) continue;
			// Priority frames (lifecycle/STOPPED) are NOT loss-tolerable: re-enqueue them at
			// the FRONT and retry on a bounded backoff rather than dropping, so a transient
			// blip on the final flush does not abandon STOPPED. After the retry budget they
			// are counted definitively failed, kept SEPARATE from trace drops, and retrying stops.
			if (priorityRetries >= PRIORITY_BATCH_RETRY_MS.length) {
				failedPriorityFrames += priorityCount;
				// Never-drop priority loss (Decision 2 counter, kept separate).
				yield* Metric.incrementBy(priorityFramesLost, priorityCount);
				continue;
			}
			priority.unshift(...batch.slice(0, priorityCount));
			// The `>= length` guard above is the sole bound, so priorityRetries is
			// in [0, length-1] here — index directly, no clamp needed.
			const delay = PRIORITY_BATCH_RETRY_MS[priorityRetries];
			priorityRetries++;
			// A priority-batch retry attempt (monotone counter) + the new
			// pump-scoped retry-depth level (gauge).
			yield* Metric.increment(priorityBatchRetries);
			yield* Metric.set(priorityRetryDepth, priorityRetries);
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
			if (traceSize() >= TRACE_QUEUE_CAP) {
				dropped++;
				// Overflow drop (Decision 2 counter, reason=overflow). Sync enqueue
				// site, so run it on the runtime like the queue offer below.
				runtime.runSync(Metric.increment(traceFramesLostOverflow));
			}
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
					// Decision 1 span: the spine-side teardown flush (join the pump so
					// every queued frame — notably a terminal STOPPED — is sent before
					// dispose). Closes before drain() resolves, so before dispose() below.
					await runtime.runPromise(
						Fiber.join(pumpFiber).pipe(
							Effect.withSpan("compass_agent.transport.publish.drain"),
						),
					);
				}
			} finally {
				// Dispose ONLY a runtime this spine owns (the fallback path). A borrowed runtime
				// is disposed by transport.close() after the drain barrier — disposing here would
				// break the sibling sink/source (§T5). In a finally so a throwing join cannot
				// strand a self-owned runtime.
				if (ownsRuntime) await runtime.dispose();
			}
		},
	};
}
