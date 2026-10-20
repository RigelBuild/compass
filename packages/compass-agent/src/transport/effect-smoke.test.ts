// Pins the Effect runtime, queue, retry, timeout, and FiberSet semantics used by transport code.
import { expect, spyOn, test } from "bun:test";
import { NodeSdk } from "@effect/opentelemetry";
import {
	InMemorySpanExporter,
	SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import {
	Cause,
	Deferred,
	Duration,
	Effect,
	Exit,
	Fiber,
	FiberSet,
	Layer,
	Logger,
	ManagedRuntime,
	Queue,
	Ref,
} from "effect";
import { TestClock } from "effect/testing";
import {
	DURABLE_RETRY_BACKOFF_MS,
	DURABLE_RETRY_SCHEDULE,
	durableRetryDelay,
} from "./frame-sink";
import { transportLoggerLayer } from "./runtime-channel";

// ManagedRuntime is the transport's composition-point runtime: it runs both the
// promise seam and synchronous emit path, then disposes cleanly.
test("ManagedRuntime runs Effects across both the promise and sync seam", async () => {
	const runtime = ManagedRuntime.make(Layer.empty);
	const viaPromise = await runtime.runPromise(
		Effect.succeed(41).pipe(Effect.map((n) => n + 1)),
	);
	const viaSync = runtime.runSync(Effect.sync(() => "sync"));
	expect(viaPromise).toBe(42);
	expect(viaSync).toBe("sync");
	await runtime.dispose();
});
test("the transport logger layer drops console output and retains tracer span events", async () => {
	const consoleSpies = [
		spyOn(console, "log"),
		spyOn(console, "info"),
		spyOn(console, "warn"),
		spyOn(console, "error"),
		spyOn(console, "debug"),
		spyOn(console, "trace"),
	];
	const exporter = new InMemorySpanExporter();
	const otel: Layer.Layer<never> = NodeSdk.layer(() => ({
		spanProcessor: new SimpleSpanProcessor(exporter),
		resource: { serviceName: "compass-agent-test" },
	}));
	const runtime = ManagedRuntime.make(Layer.merge(transportLoggerLayer, otel));
	try {
		await runtime.runPromise(
			Effect.logInfo("transport log").pipe(
				Effect.withSpan("test.transport.log"),
			),
		);
		expect(consoleSpies.every((spy) => spy.mock.calls.length === 0)).toBe(true);
		const span = exporter
			.getFinishedSpans()
			.find((finished) => finished.name === "test.transport.log");
		expect(span?.events.some((event) => event.name === "transport log")).toBe(
			true,
		);
	} finally {
		await runtime.dispose();
		for (const spy of consoleSpies) spy.mockRestore();
	}
});

// Queue.sliding's effectful offer drops the oldest item when full.
test("Queue.sliding effectful offer drops the OLDEST element on overflow", async () => {
	const q = Effect.runSync(Queue.sliding<number>(3));
	for (const n of [1, 2, 3, 4, 5]) {
		await Effect.runPromise(Queue.offer(q, n));
	}
	const drained = Effect.runSync(Queue.takeAll(q));
	expect(drained).toEqual([3, 4, 5]);
});

test("a failing runtime.runFork writes nothing to the console under the transport logger layer", async () => {
	const consoleSpies = [
		spyOn(console, "log"),
		spyOn(console, "info"),
		spyOn(console, "warn"),
		spyOn(console, "error"),
		spyOn(console, "debug"),
		spyOn(console, "trace"),
	];
	const runtime = ManagedRuntime.make(transportLoggerLayer);
	let exit: Exit.Exit<never, Error> | undefined;
	try {
		const fiber = runtime.runFork(
			Effect.logError("lane failed").pipe(
				Effect.andThen(Effect.fail(new Error("lane failed"))),
			),
		);
		exit = await runtime.runPromise(Fiber.await(fiber));
		// dispose awaits every managed fiber, so any failure report has run by now.
		await runtime.dispose();
		expect(exit !== undefined && Exit.isFailure(exit)).toBe(true);
		expect(consoleSpies.map((spy) => spy.mock.calls.length)).toEqual([
			0, 0, 0, 0, 0, 0,
		]);
	} finally {
		for (const spy of consoleSpies) spy.mockRestore();
	}
});

test("Queue.offerUnsafe on a full sliding queue drops the oldest element synchronously", () => {
	const q = Effect.runSync(Queue.sliding<number>(3));
	let drops = 0;
	const returns: boolean[] = [];
	for (const n of [1, 2, 3, 4, 5]) {
		if (Queue.sizeUnsafe(q) >= 3) drops += 1;
		returns.push(Queue.offerUnsafe(q, n));
	}
	expect(Effect.runSync(Queue.takeAll(q))).toEqual([3, 4, 5]);
	expect(returns).toEqual([true, true, true, true, true]);
	expect(drops).toBe(2);
});

// Pin retry delays and recurrence count under virtual time.
test("Effect.retry exhausts the finite durable delay ladder under virtual time", async () => {
	const program = Effect.gen(function* () {
		const attempts = yield* Ref.make(0);
		const fiber = yield* Effect.forkChild(
			Effect.retry(
				Effect.gen(function* () {
					yield* Ref.update(attempts, (n) => n + 1);
					yield* Effect.fail(new Error("transient"));
				}),
				DURABLE_RETRY_SCHEDULE,
			).pipe(Effect.exit),
		);
		yield* Effect.yieldNow;
		const seen: number[] = [yield* Ref.get(attempts)];
		for (const delay of DURABLE_RETRY_BACKOFF_MS) {
			yield* TestClock.adjust(Duration.millis(delay - 1));
			yield* Effect.yieldNow;
			seen.push(yield* Ref.get(attempts));
			yield* TestClock.adjust(Duration.millis(1));
			yield* Effect.yieldNow;
			seen.push(yield* Ref.get(attempts));
		}
		const exit = yield* Fiber.join(fiber);
		return { exit, seen };
	});
	const { exit, seen } = await Effect.runPromise(
		Effect.provide(program, TestClock.layer()),
	);
	expect(seen).toEqual([1, 1, 2, 2, 3, 3, 4, 4, 5]);
	expect(Exit.isFailure(exit)).toBe(true);
});

test("a durable retry attempt past the delay ladder dies instead of retrying with no delay", () => {
	const past = Effect.runSyncExit(
		durableRetryDelay(DURABLE_RETRY_BACKOFF_MS.length + 1),
	);
	expect(Exit.isFailure(past) && Cause.hasDies(past.cause)).toBe(true);
	const last = Effect.runSync(
		durableRetryDelay(DURABLE_RETRY_BACKOFF_MS.length),
	);
	expect(Duration.toMillis(last)).toBe(2000);
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
		yield* Effect.forkChild(
			Effect.gen(function* () {
				for (let i = 0; i < 3; i++) observed.push(yield* Queue.take(q));
				yield* Deferred.succeed(done, undefined);
			}),
		);
		for (const n of [1, 2, 3]) Queue.offerUnsafe(q, n); // one synchronous tick
		const duringTick = observed.length;
		yield* Deferred.await(done);
		return duringTick;
	}).pipe(Effect.scoped);
	const duringTick = await Effect.runPromise(program);
	expect(duringTick).toBe(0);
	expect(observed).toEqual([1, 2, 3]);
});
