# Deployer contract — compass-agent whole-agent telemetry

Maintainer-facing reference for anyone configuring OpenTelemetry on a
compass-agent deployment. It documents the env-var surface a deployer sets, the
off-by-default guarantee, and the two asymmetries the design record
(`design.md`, T2) requires a deployer to know before wiring a collector.

Scope: this is an internal RigelBuild doc. It describes the contract as of the
loop-OTel activation (T1); the code cites are into the frozen design record and
the reused `@oh-my-pi/pi-coding-agent` module.

## Two independent signals, one collector

compass-agent emits two OpenTelemetry signal trees to the same collector
(Decision 2, ruling (b)):

- the **loop** signal — OMP's native agent-loop tracing, reused via
  `@oh-my-pi/pi-coding-agent/telemetry-export` and activated in
  `cli.ts` `main()`;
- the **transport** signal — the frozen `src/transport/` layer's own OTel
  provider.

They are separate trace trees exported to one collector and correlated there by
`service.name` plus the shared `compass.session.id` resource attribute
(Decision 3a), not by parent/child links.

## Environment variables

| Variable | Effect |
| --- | --- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The endpoint key to set for whole-agent telemetry. Turns on BOTH the loop and the transport signals. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | Traces-only endpoint override honored by the loop; see the F5 asymmetry below before using it. |
| `OTEL_SERVICE_NAME` | Overrides the loop signal's service name (defaults to `compass-agent` on the enabled path); see the F1 asymmetry below. |
| `OTEL_RESOURCE_ATTRIBUTES` | Deployer-set resource attributes. The activation APPENDS `compass.session.id=<id>` (Decision 3a), never clobbering a deployer value. |
| `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` | Opt-in for capturing GenAI message content. Off by default; see the sensitivity warning below. |

### Setting the endpoint

For whole-agent telemetry, set `OTEL_EXPORTER_OTLP_ENDPOINT`. That single key
turns on both signals. Do not reach for the `TRACES_` variant unless you have
read the F5 asymmetry below and specifically want a traces-only, loop-only
configuration.

### Service name override

On the enabled path with no deployer override, the loop signal defaults to
`service.name = compass-agent`
(`cli.ts` sets `process.env.OTEL_SERVICE_NAME ??= "compass-agent"`, Decision 3),
matching the transport, so both signals correlate under one name. A deployer
`OTEL_SERVICE_NAME` override does NOT rename both signals symmetrically; see the
F1 asymmetry below.

### Content-capture opt-in and its sensitivity warning

`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` is off by default; the
activation never sets it
(`pi-agent-core/src/telemetry.ts:58,326-333`). Enabling it makes the model
message content (prompts and completions) leave the container as span
attributes on the OTLP export.

> **Warning — sensitivity.** Turning this on exports message content off-box to
> the collector. Treat it as a debug-only opt-in on a trusted collector, never a
> production default.

## Off-by-default guarantee

With no endpoint configured — neither `OTEL_EXPORTER_OTLP_ENDPOINT` nor
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set — the activation block is skipped whole
(the `isTelemetryEndpointConfigured` gate at `cli.ts:838`): no provider is
registered, no `telemetry` session option is passed, `process.env` is unmutated,
and there is zero network egress (Global Constraints, "Off by default"). The
no-endpoint behavior is bit-identical to a build without this activation, and
the existing test suites stay green unmodified.

## Asymmetries a deployer must know

Both of these are documented rather than code-fixed (the transport is frozen;
ruling (b)). A deployer configuring a collector must know them.

### F5 — endpoint-gate asymmetry

The loop honors `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ?? OTEL_EXPORTER_OTLP_ENDPOINT`
(`telemetry-export.ts:62-63`). The transport gates ONLY on
`OTEL_EXPORTER_OTLP_ENDPOINT` (`otel-layer.ts:49-51`) and never reads the
`TRACES_` variant.

Consequence: `OTEL_EXPORTER_OTLP_ENDPOINT` turns on BOTH signals, but
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set alone lights the loop and leaves the
transport dark.

Contract line: set `OTEL_EXPORTER_OTLP_ENDPOINT` for whole-agent telemetry.

### F1 — service-name split

The loop reads the service name env-first (`OTEL_SERVICE_NAME ?? "oh-my-pi"`,
`telemetry-export.ts:103`), while the transport tags `service.name` in code
(`otel-layer.ts:61`) and the code value WINS over env.

Consequence: a deployer `OTEL_SERVICE_NAME` override renames the LOOP signal
only; the transport stays `compass-agent`. An override therefore splits the two
signals under different names.

Because of that, the durable cross-signal join is NOT the service name. It is
the shared `compass.session.id` resource attribute carried in
`OTEL_RESOURCE_ATTRIBUTES` (Decision 3a), which both providers read natively and
stamp. Correlate on that, not on `service.name`.
