// O1a of the transport-OTel adoption
// (design docs/designs/platform/compass-agent-effect-otel/design.md): the
// module-private OTel layer scaffold. These cases pin the two posture-neutral
// guarantees the scaffold makes before any exporter exists (O1b):
//
//   1. Off by default — with no OTEL_EXPORTER_OTLP_ENDPOINT the layer IS
//      Layer.empty, so the transport runtime installs no provider and pays no
//      overhead. (Until O1b lands the endpoint-set branch also yields
//      Layer.empty; that stays an O1b concern, so it is not asserted here as a
//      permanent contract.)
//   2. Inert-not-broken — a ManagedRuntime built the way the transport builds it
//      (Layer.merge(Logger.remove(...), makeOtelLayer())) runs Effect.withSpan
//      and a Metric increment with no provider, and disposes cleanly. This is
//      the property that keeps the existing black-box suite green: instrumentation
//      added in O2-O4 no-ops without an endpoint rather than throwing.

import { afterEach, expect, test } from "bun:test";
import { Effect, Layer, Logger, ManagedRuntime, Metric } from "effect";

import { makeOtelLayer } from "./otel-layer";

const ENDPOINT_KEY = "OTEL_EXPORTER_OTLP_ENDPOINT";

afterEach(() => {
	delete process.env[ENDPOINT_KEY];
});

// The env gate is read at call time, and unset is the only path built in O1a.
// Identity against Layer.empty is the assertion: makeOtelLayer returns the empty
// layer itself, not a merged/wrapped one, so the runtime gains no provider.
// Mutation check: a layer that always composed NodeSdk.layer would not be
// referentially Layer.empty.
test("makeOtelLayer returns Layer.empty when no endpoint is configured", () => {
	delete process.env[ENDPOINT_KEY];
	expect(makeOtelLayer()).toBe(Layer.empty);
});

// An empty endpoint string is not a configured endpoint — it must gate off, not
// attempt an export against "". Mutation check: a bare `=== undefined` check
// (dropping the `=== ""` arm) would treat "" as configured and fall through.
test("makeOtelLayer treats an empty endpoint string as unconfigured", () => {
	process.env[ENDPOINT_KEY] = "";
	expect(makeOtelLayer()).toBe(Layer.empty);
});

// The exact runtime shape the transport constructs (index.ts): the OTel layer
// merged with the logger removal. With no provider installed, Effect.withSpan is
// the no-op tracer and Metric.increment accumulates in the in-memory registry —
// both must run and the runtime must dispose without error. Mutation check: a
// layer that failed to build, or an instrumentation call that threw without a
// provider, would reject one of these awaits.
test("a runtime built with the merged OTel layer runs withSpan + Metric and disposes cleanly", async () => {
	delete process.env[ENDPOINT_KEY];
	const runtime = ManagedRuntime.make(
		Layer.merge(Logger.remove(Logger.defaultLogger), makeOtelLayer()),
	);

	const spanned = await runtime.runPromise(
		Effect.succeed(42).pipe(Effect.withSpan("test.span")),
	);
	expect(spanned).toBe(42);

	const requests = Metric.counter("test.requests");
	await runtime.runPromise(Metric.increment(requests));
	const value = await runtime.runPromise(Metric.value(requests));
	expect(value.count).toBe(1);

	await runtime.dispose();
});
