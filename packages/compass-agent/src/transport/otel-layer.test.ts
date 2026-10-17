// Pins disabled, inert, and exporting behavior for the optional OTLP layer.

import { afterEach, expect, test } from "bun:test";
import { Effect, Layer, Logger, ManagedRuntime, Metric } from "effect";

import { makeOtelLayer } from "./otel-layer";

const ENDPOINT_KEY = "OTEL_EXPORTER_OTLP_ENDPOINT";

afterEach(() => {
	delete process.env[ENDPOINT_KEY];
});

// With no endpoint, identity with Layer.empty proves the runtime gets no provider.
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

// The transport's merged layer must run span and metric effects without a provider.
test("a runtime built with the merged OTel layer runs withSpan + Metric and disposes cleanly", async () => {
	delete process.env[ENDPOINT_KEY];
	const runtime = ManagedRuntime.make(
		Layer.merge(Logger.layer([Logger.tracerLogger]), makeOtelLayer()),
	);

	const spanned = await runtime.runPromise(
		Effect.succeed(42).pipe(Effect.withSpan("test.span")),
	);
	expect(spanned).toBe(42);

	const requests = Metric.counter("test.requests");
	await runtime.runPromise(Metric.update(requests, 1));
	expect(Effect.runSync(Metric.value(requests)).count).toBe(1);

	await runtime.dispose();
});

// Collector POSTs gate the healthy path; the timeout only bounds a broken or absent export.
// biome-ignore lint/plugin: 15s is the framework deadline turning an absent/broken OTLP export into a bounded failure; the pass event-gates on the collector's traces+metrics POSTs, not the clock.
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
			Layer.merge(Logger.layer([Logger.tracerLogger]), layer),
		);
		await runtime.runPromise(
			Effect.succeed(1).pipe(Effect.withSpan("test.export.span")),
		);
		await runtime.runPromise(Metric.update(Metric.counter("test.export"), 1));
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
