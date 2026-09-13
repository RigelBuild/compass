// The transport's OpenTelemetry layer: the single place the `src/transport/` tree gets a
// Tracer and Meter provider, composed into the one transport-owned `ManagedRuntime` in
// `index.ts` (design Decision 4). Module-private: NEVER re-exported, so no `@opentelemetry/*`
// type reaches the package `.d.ts` (pinned by a red export-surface test).

// Off by default: keys off `OTEL_EXPORTER_OTLP_ENDPOINT`, read ONCE at call time. Unset ⇒
// `Layer.empty`, so `Effect.withSpan` uses the no-op tracer and metrics accumulate but never
// export — observably inert with no endpoint, no overhead, no network egress.

// When set, the layer installs an OTLP export pipeline via one `NodeSdk.layer` call
// (BatchSpanProcessor + PeriodicExportingMetricReader over OTLP-HTTP/protobuf, tagged
// service.name = compass-agent). The exporters read the endpoint themselves, so the value
// here is purely the on/off switch. This is the agent's first network egress (off by default).

import { NodeSdk } from "@effect/opentelemetry";
import { OTLPMetricExporter } from "@opentelemetry/exporter-metrics-otlp-proto";
import { OTLPTraceExporter } from "@opentelemetry/exporter-trace-otlp-proto";
import { PeriodicExportingMetricReader } from "@opentelemetry/sdk-metrics";
import { BatchSpanProcessor } from "@opentelemetry/sdk-trace-base";
import { Duration, Layer } from "effect";

// The transport-internal OTel layer merged into the single `ManagedRuntime`. Declared
// `Layer.Layer<never>` not the natural `Layer.Layer<Resource.Resource>`: `ROut` is
// contravariant, so the resource output is discarded and the merged runtime stays a
// `ManagedRuntime<never>`. Reading the endpoint once per construction is deliberate.
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
