// The transport's OpenTelemetry layer: the single place the whole `src/transport/`
// tree gets a Tracer/Meter/Logger provider, composed into the one transport-owned
// `ManagedRuntime` at the construction seam in `index.ts`
// (design docs/designs/platform/compass-agent-effect-otel/design.md, Decision 4).
//
// This module is module-private: it is NEVER re-exported from `index.ts`, so no
// `@effect/opentelemetry` / `@opentelemetry/*` type reaches the package's public
// `.d.ts`. That containment is the frozen rule of the parent adoption record
// (compass-agent-effect-adoption/design.md, Global Constraints) and is pinned by
// a red export-surface test (`export-surface.test.ts`).
//
// Off by default (design Global Constraints, "Off by default"): the layer keys
// off `OTEL_EXPORTER_OTLP_ENDPOINT`, read ONCE at call time. Unset => `Layer.empty`,
// so the runtime installs no provider: `Effect.withSpan` uses the no-op tracer and
// `Metric` increments accumulate in Effect's in-memory registry but are never
// EXPORTED. Instrumentation is thus observably inert with no endpoint — the
// existing black-box transport suite stays green unmodified, and a self-hosted
// deployment that sets no endpoint pays no overhead and opens no network egress.
//
// The endpoint-set branch is the exporter seam O1b fills (design O1b, "OQ1 ruled
// (a)"): the `NodeSdk.layer` composition with a `BatchSpanProcessor(OTLPTraceExporter)`
// and a `PeriodicExportingMetricReader(OTLPMetricExporter)` over HTTP/protobuf, drawn
// against the destination collector. Until O1b lands, no exporter exists, so the set
// branch also yields `Layer.empty` — the gate is live and tested now; only its
// exporter body is deferred. This keeps O1a posture-neutral: no exporter, no network
// path, no behavioral change whether the endpoint is set or not.

import { Layer } from "effect";

// The transport-internal OTel layer merged into the single `ManagedRuntime` in
// `createUnixSocketTransport` (design Decision 4). Declared `Layer.Layer<never>`,
// not the natural `Layer.Layer<Resource.Resource>` that `NodeSdk.layer` returns:
// `Layer`'s `ROut` is contravariant, so `Layer<Resource.Resource>` is assignable to
// `Layer<never>`, discarding the `Resource.Resource` output so the merged runtime
// stays a `ManagedRuntime<never>` (design Decision 4). Reading the endpoint here,
// once per transport construction, is deliberate: the layer is built at exactly the
// point the runtime is, so a per-container endpoint decision is honored without any
// later re-read.
export function makeOtelLayer(): Layer.Layer<never> {
	const endpoint = process.env.OTEL_EXPORTER_OTLP_ENDPOINT;
	if (endpoint === undefined || endpoint === "") {
		return Layer.empty;
	}
	// Endpoint set: the exporter-wiring seam (design O1b). No exporter exists until
	// O1b lands, so this yields `Layer.empty` today — the env gate is exercised, the
	// exporter body is O1b's. When O1b lands it returns
	// `NodeSdk.layer(() => ({ spanProcessor, metricReader, resource, shutdownTimeout }))`
	// built from `endpoint`.
	return Layer.empty;
}
