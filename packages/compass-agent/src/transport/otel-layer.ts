// The transport's OpenTelemetry layer: the single place the whole `src/transport/`
// tree gets a Tracer and Meter provider, composed into the one transport-owned
// `ManagedRuntime` at the construction seam in `index.ts`
// (design docs/designs/repo/compass-agent-effect-otel/design.md, Decision 4).
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
// When the endpoint is set, the layer installs an OTLP export pipeline via a
// single `NodeSdk.layer` call (design O1b, "OQ1 ruled (a)"): a `BatchSpanProcessor`
// over an `OTLPTraceExporter` and a `PeriodicExportingMetricReader` over an
// `OTLPMetricExporter`, both exporting OTLP-over-HTTP/protobuf to the configured
// collector, tagged `service.name = compass-agent`. The exporters read
// `OTEL_EXPORTER_OTLP_ENDPOINT` themselves — standard OTLP base-endpoint semantics,
// where each appends its own `/v1/traces` or `/v1/metrics` path — so the endpoint
// value the gate above reads serves purely as the on/off switch; passing it as an
// explicit `url` would defeat that per-signal path suffixing. This is the agent's
// first network egress (design Decision 3): against an egress-sealed substrate it
// stays off unless a deployer sets the endpoint.

import { NodeSdk } from "@effect/opentelemetry";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-proto";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import { PeriodicExportingMetricReader } from "@opentelemetry/sdk-metrics";
import { BatchSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { Duration, Layer } from "effect";

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
	// The SDK flush is bounded by `shutdownTimeout` so `runtime.dispose()` in the
	// transport's `close()` cannot hang teardown on a stuck collector; the drain
	// barrier has already run before close (design Decision 4).
	return NodeSdk.layer(() => ({
		spanProcessor: new BatchSpanProcessor(new OTLPTraceExporter()),
		metricReader: new PeriodicExportingMetricReader({
			exporter: new OTLPMetricExporter(),
		}),
		resource: { serviceName: "compass-agent" },
		shutdownTimeout: Duration.seconds(2),
	}));
}
