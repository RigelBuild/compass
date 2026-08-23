// The module-private OTel layer
// (design docs/designs/platform/compass-agent-effect-otel/design.md, O1a + O1b).
// These cases pin three guarantees:
//
//   1. Off by default — with no OTEL_EXPORTER_OTLP_ENDPOINT the layer IS
//      Layer.empty, so the transport runtime installs no provider and pays no
//      overhead.
//   2. Inert-not-broken — a ManagedRuntime built the way the transport builds it
//      (Layer.merge(Logger.remove(...), makeOtelLayer())) runs Effect.withSpan
//      and a Metric increment with no provider, and disposes cleanly. This is
//      the property that keeps the existing black-box suite green: instrumentation
//      added in O2-O4 no-ops without an endpoint rather than throwing.
//   3. Live export (O1b) — with the endpoint set, the layer is NOT Layer.empty,
//      and a runtime built with it exports both a span and a metric over OTLP to
//      the configured collector, flushing both on dispose within the shutdown
//      timeout.

import { afterEach, expect, test } from "bun:test";
import { Effect, Layer, Logger, ManagedRuntime, Metric } from "effect";

import { makeOtelLayer } from "./otel-layer";

const ENDPOINT_KEY = "OTEL_EXPORTER_OTLP_ENDPOINT";

afterEach(() => {
	delete process.env[ENDPOINT_KEY];
});

// The env gate is read at call time. Identity against Layer.empty is the
// assertion: makeOtelLayer returns the empty layer itself, not a merged/wrapped
// one, so the runtime gains no provider.
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

// O1b — the endpoint-set branch. With OTEL_EXPORTER_OTLP_ENDPOINT pointing at a
// collector, makeOtelLayer is NOT Layer.empty and installs a live OTLP pipeline:
// a runtime built the transport's way exports a span AND a metric, both flushed
// on dispose. The stub is an in-process HTTP server; each signal resolves its own
// gate promise when its OTLP path is POSTed, so the test is event-gated, not
// timed. Mutation checks: reverting the branch to Layer.empty makes makeOtelLayer
// === Layer.empty (first assertion red) and sends nothing (the awaited export
// promises never resolve → the test's own deadline fires); dropping either
// exporter drops that signal's POST, so its awaited promise hangs the test to a
// deadline failure. The explicit per-test timeout is the framework deadline that
// turns a broken/absent export into a bounded failure rather than a hang.
test("with an endpoint set, the layer exports a span and a metric over OTLP and flushes on dispose", async () => {
	const seen = new Set<string>();
	let resolveTraces!: () => void;
	let resolveMetrics!: () => void;
	const traces = new Promise<void>((resolve) => {
		resolveTraces = resolve;
	});
	const metrics = new Promise<void>((resolve) => {
		resolveMetrics = resolve;
	});
	const collector = Bun.serve({
		port: 0,
		async fetch(req) {
			const { pathname } = new URL(req.url);
			await req.arrayBuffer();
			seen.add(pathname);
			if (pathname.endsWith("/v1/traces")) resolveTraces();
			if (pathname.endsWith("/v1/metrics")) resolveMetrics();
			return new Response(new Uint8Array(), { status: 200 });
		},
	});

	try {
		process.env[ENDPOINT_KEY] = `http://localhost:${collector.port}`;
		const layer = makeOtelLayer();
		// A configured endpoint yields a real provider layer, not the empty gate.
		expect(layer).not.toBe(Layer.empty);

		const runtime = ManagedRuntime.make(
			Layer.merge(Logger.remove(Logger.defaultLogger), layer),
		);
		await runtime.runPromise(
			Effect.succeed(1).pipe(Effect.withSpan("test.export.span")),
		);
		await runtime.runPromise(Metric.increment(Metric.counter("test.export")));
		// dispose drives the BatchSpanProcessor + PeriodicExportingMetricReader
		// final flush, bounded by the layer's shutdownTimeout.
		await runtime.dispose();
		// Await each signal's POST directly — the collector resolves `traces` and
		// `metrics` when their OTLP paths arrive, so the test gates on the real
		// export events, never a wall-clock guess. A never-arriving signal is
		// caught by the per-test timeout below, not a hand-rolled timer.
		await Promise.all([traces, metrics]);
		expect([...seen].some((p) => p.endsWith("/v1/traces"))).toBe(true);
		expect([...seen].some((p) => p.endsWith("/v1/metrics"))).toBe(true);
	} finally {
		collector.stop(true);
	}
}, 15_000);
