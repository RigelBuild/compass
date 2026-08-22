// Effect runtime smoke + the pinned record of the Effect semantics the transport
// migration (compass-agent-effect-adoption/design.md) depends on. T1 of that
// record: it verifies, under Bun, that the exact primitives T2-T5 rely on behave
// as the design assumes — so if an `effect` upgrade ever changes one of these
// semantics, THIS file reddens first, before the black-box invariant suites do.
//
// It is a semantics ledger, not a behavior test of our code: every case pins one
// property of `effect` itself, with a header naming the design mechanism that
// rests on it. Where a fact contradicts the frozen record, the case pins the
// TRUE behavior and says so in its header — the record is a spec, and a spec that
// misreads the library is a bug the executor surfaces, not silently honors.
//
// The retry-ladder case uses `TestClock` virtual time (never a wall-clock
// sleep): the ladder is asserted by advancing the clock to each boundary and
// observing the attempt count, so load can never perturb the assertion and there
// is no flake to paper over with a re-run. The timeout case uses a short real
// deadline over a never-resolving promise — its outcome (a timeout) is the only
// possible one regardless of load, so it is deterministic without virtual time.

import { expect, test } from "bun:test";
import {
	Deferred,
	Duration,
	Effect,
	Exit,
	Fiber,
	FiberSet,
	Layer,
	ManagedRuntime,
	Option,
	Queue,
	Ref,
	Schedule,
	TestClock,
	TestContext,
} from "effect";

// ManagedRuntime is the transport's composition-point runtime (record OQ-3/T5):
// one runtime per transport, promise seams cross it via runPromise, and the
// synchronous emit() path crosses it via runSync. Pin that a bare runtime runs an
// Effect both ways and disposes cleanly. Mutation check: a runtime that never ran
// the Effect would not yield 42 (the +1 map never fired) or "sync".
test("ManagedRuntime runs Effects across both the promise and the sync seam", async () => {
	const runtime = ManagedRuntime.make(Layer.empty);
	const viaPromise = await runtime.runPromise(
		Effect.succeed(41).pipe(Effect.map((n) => n + 1)),
	);
	const viaSync = runtime.runSync(Effect.sync(() => "sync"));
	expect(viaPromise).toBe(42);
	expect(viaSync).toBe("sync");
	await runtime.dispose();
});

// The trace lane (T3) is loss-tolerable with a drop-OLDEST overflow policy
// (publish-spine.ts overload policy; frame-sink.test.ts:435 pins drop-oldest
// ordinals). The design maps it to Queue.sliding. Pin that sliding's overflow IS
// drop-oldest — on the EFFECTFUL offer path. Mutation check: a drop-newest queue
// would leave survivors [1,2,3].
test("Queue.sliding effectful offer drops the OLDEST element on overflow", async () => {
	const program = Effect.gen(function* () {
		const q = yield* Queue.sliding<number>(3);
		yield* Effect.forEach([1, 2, 3, 4, 5], (n) => Queue.offer(q, n), {
			discard: true,
		});
		const drained = yield* Queue.takeAll(q);
		return Array.from(drained);
	});
	const survivors = await Effect.runPromise(program);
	expect(survivors).toEqual([3, 4, 5]);
});

// CONTRADICTS THE FROZEN RECORD — pinned deliberately. The record's T3 mapping
// (design.md, trace-lane row) specifies Queue.unsafeOffer for the sync emit()
// path and states it "always returns true (never signals eviction)" with
// drop-oldest semantics. In effect 3.22.1 that is FALSE: unsafeOffer bypasses the
// SlidingStrategy and offers straight to the backing bounded queue
// (internal/queue.js unsafeOffer -> this.queue.offer), so on a full sliding queue
// it REJECTS the new element (drop-NEWEST) and returns FALSE. It is drop-oldest
// only through the effectful offer path above. The trace-lane drop-oldest
// invariant therefore CANNOT be served by unsafeOffer; T3 must use
// runtime.runSync(Queue.offer) (next case). Mutation check: were unsafeOffer
// drop-oldest+always-true as the record assumed, survivors would be [3,4,5] and
// every return value true.
test("Queue.unsafeOffer on a full sliding queue rejects-when-full (drop-NEWEST), NOT drop-oldest", async () => {
	const program = Effect.gen(function* () {
		const q = yield* Queue.sliding<number>(3);
		const returns: boolean[] = [];
		for (const n of [1, 2, 3, 4, 5]) returns.push(Queue.unsafeOffer(q, n));
		const drained = yield* Queue.takeAll(q);
		return { returns, survivors: Array.from(drained) };
	});
	const { returns, survivors } = await Effect.runPromise(program);
	expect(survivors).toEqual([1, 2, 3]);
	expect(returns).toEqual([true, true, true, false, false]);
});

// The correct T3 trace-lane mechanism, proven: runtime.runSync(Queue.offer) on a
// sliding queue completes SYNCHRONOUSLY (sliding offer never suspends, so runSync
// does not throw), drops the OLDEST on overflow, and the drop is detectable
// synchronously by reading unsafeSize() BEFORE the offer — size == capacity means
// the imminent offer will evict. This is the sync/void emit() path with an
// exact, race-free drop counter (the producer is synchronous, so the size read
// and the offer share one tick). Mutation check: a suspending offer would make
// runSync throw; a non-evicting queue would grow past 3.
test("runtime.runSync(Queue.offer) is a synchronous drop-oldest enqueue with a pre-offer-countable drop", async () => {
	const runtime = ManagedRuntime.make(Layer.empty);
	const q = runtime.runSync(Queue.sliding<number>(3));
	let drops = 0;
	for (const n of [1, 2, 3, 4, 5]) {
		const sizeBefore = Option.getOrElse(q.unsafeSize(), () => 0);
		if (sizeBefore >= 3) drops += 1;
		runtime.runSync(Queue.offer(q, n)); // synchronous — throws if it ever suspends
	}
	const survivors = Array.from(runtime.runSync(Queue.takeAll(q)));
	expect(survivors).toEqual([3, 4, 5]);
	expect(drops).toBe(2);
	await runtime.dispose();
});

// The durable lane (T2) retries the unary on a fixed backoff ladder via
// Effect.retry(Schedule.fromDelays(...)) (frame-sink.ts DURABLE_RETRY_BACKOFF_MS).
// Pin that fromDelays fires each attempt at its ladder boundary and no earlier,
// under virtual time. Mutation check: a ladder that retried immediately would
// reach attempt 3 before the clock advanced; a wrong boundary would trip the
// mid-adjust assertions.
test("Effect.retry(Schedule.fromDelays) fires attempts on the fixed ladder under virtual time", async () => {
	const program = Effect.gen(function* () {
		const attempts = yield* Ref.make(0);
		const body = Effect.gen(function* () {
			const n = yield* Ref.updateAndGet(attempts, (x) => x + 1);
			return n < 3 ? yield* Effect.fail(new Error("transient")) : n;
		});
		const fiber = yield* Effect.fork(
			Effect.retry(
				body,
				Schedule.fromDelays(Duration.millis(50), Duration.millis(200)),
			),
		);
		yield* TestClock.adjust(Duration.millis(49));
		const afterFirstDelayMinusOne = yield* Ref.get(attempts); // first attempt fired, still waiting
		yield* TestClock.adjust(Duration.millis(1)); // hit +50ms -> second attempt
		const afterFirstDelay = yield* Ref.get(attempts);
		yield* TestClock.adjust(Duration.millis(200)); // hit +200ms -> third attempt -> success
		const result = yield* Fiber.join(fiber);
		return { afterFirstDelayMinusOne, afterFirstDelay, result };
	}).pipe(Effect.provide(TestContext.TestContext));
	const { afterFirstDelayMinusOne, afterFirstDelay, result } =
		await Effect.runPromise(program);
	expect(afterFirstDelayMinusOne).toBe(1);
	expect(afterFirstDelay).toBe(2);
	expect(result).toBe(3);
});

// The durable unary's per-attempt deadline (frame-sink.ts DURABLE_CALL_TIMEOUT_MS,
// OQ-4) turns a Runner that accepts-but-never-responds into a retryable failure so
// the retry loop can advance past a hang. Pin that Effect.timeout over a
// never-resolving promise produces a failure (the fiber-side deadline). The record
// keeps Connect's timeoutMs for wire cancellation ON TOP of this; this pins only
// the fiber-side conversion. Mutation check: without the timeout the effect never
// settles and the test times out.
test("Effect.timeout converts a never-resolving promise into a failure", async () => {
	const never = Effect.tryPromise({
		try: () => new Promise<never>(() => {}),
		catch: (e) => e,
	});
	const exit = await Effect.runPromise(
		never.pipe(Effect.timeout(Duration.millis(20)), Effect.exit),
	);
	expect(Exit.isFailure(exit)).toBe(true);
});

// The durable in-flight set (T2) becomes a FiberSet the sink's drain() awaits via
// FiberSet.awaitEmpty, so shutdown cannot abandon an uncommitted transcript frame
// (frame-sink.ts drain awaits the in-flight set). Pin that awaitEmpty resolves
// only after every forked fiber has completed. Mutation check: awaitEmpty
// resolving early would leave completed < 3.
test("FiberSet.awaitEmpty resolves only after every forked fiber completes", async () => {
	const program = Effect.gen(function* () {
		const set = yield* FiberSet.make<void>();
		const completed = yield* Ref.make(0);
		for (const n of [1, 2, 3]) {
			yield* FiberSet.run(
				set,
				Ref.update(completed, (x) => x + n),
			);
		}
		yield* FiberSet.awaitEmpty(set);
		return yield* Ref.get(completed);
	}).pipe(Effect.scoped);
	const total = await Effect.runPromise(program);
	expect(total).toBe(6);
});

// The pump (T3) drains a whole synchronously-enqueued burst atomically: a forked
// consumer fiber does not interleave and observe frames mid-burst, it sees the
// full batch once the enqueueing tick yields. This is what lets a same-tick STOPPED
// added after trace frames still lead — the pump sorts priority-first over the
// entire batch (frame-sink.test.ts:322 STOPPED-leads-same-tick). Pin the miniature:
// a burst offered in one synchronous tick is observed by the consumer only after
// the tick yields, and then in full order. Mutation check: a fiber that
// interleaved would make duringTick > 0.
test("a forked consumer observes a synchronous enqueue burst only after the tick yields", async () => {
	const observed: number[] = [];
	const program = Effect.gen(function* () {
		const q = yield* Queue.unbounded<number>();
		const done = yield* Deferred.make<void>();
		yield* Effect.fork(
			Effect.gen(function* () {
				for (let i = 0; i < 3; i++) observed.push(yield* Queue.take(q));
				yield* Deferred.succeed(done, undefined);
			}),
		);
		for (const n of [1, 2, 3]) Queue.unsafeOffer(q, n); // one synchronous tick
		const duringTick = observed.length;
		yield* Deferred.await(done);
		return duringTick;
	}).pipe(Effect.scoped);
	const duringTick = await Effect.runPromise(program);
	expect(duringTick).toBe(0);
	expect(observed).toEqual([1, 2, 3]);
});
