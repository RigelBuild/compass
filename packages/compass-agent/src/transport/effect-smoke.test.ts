// Pins the Effect runtime, queue, retry, timeout, and FiberSet semantics used by transport code.
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

// ManagedRuntime runs async and sync Effects and disposes without leaking resources.
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

// Queue.sliding's effectful offer drops the oldest item when full.
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

// In Effect 3.22.1, unsafeOffer bypasses sliding and rejects the newest item when full.
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

// runSync(Queue.offer) is synchronous for a sliding queue and drops the oldest item on overflow.
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

// TestClock pins retry attempts to the configured delay boundaries.
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

// Effect.timeout converts an unresponsive durable call into a retryable fiber failure.
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

// Drain must await every in-flight durable frame before shutdown completes.
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

// A consumer sees the full same-tick burst before it starts draining, so priority sorting sees all frames.
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
