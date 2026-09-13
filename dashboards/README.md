# Compass Observability — Grafana Dashboards

## Overview

These are the Plane-B operator dashboards for Compass observability: in-repo
Grafana dashboard JSON that renders signals the shipped `compass-agent` emits
today. They ship as JSON only — there is **no provisioning automation** (no
datasource YAML, no docker-compose, no `grafana.ini`) by design, per decision
D3/T5 of the frozen record
[`docs/designs/platform/compass-observability-architecture/design.md`](../docs/designs/platform/compass-observability-architecture/design.md).
Import is the operator's one manual step.

Each dashboard declares its datasource as a templating variable
(`${DS_PROMETHEUS}` or `${DS_TEMPO}`) rather than a hard-coded uid, so the JSON
imports into any Grafana: you pick the concrete datasource at import time.

## `compass-agent-transport.json`

Transport-plane metrics for the agent (Prometheus datasource variable
`DS_PROMETHEUS`). OTLP metrics are exported through the OTel Prometheus
exporter, which mangles names (`.` → `_`) and appends `_total` to monotonic
counters; gauges keep their dotless-to-underscore name with no suffix. Panels
are authored against the Prometheus-mangled names.

Rendered signals (metric constants → Prometheus-mangled name):

- `compass_agent.transport.publish.trace_frames_lost` → `..._total` (by `reason`)
- `compass_agent.transport.publish.priority_frames_lost` → `..._total` (never-drop contract; any nonzero is a breach)
- `compass_agent.transport.publish.priority_batch_retries` → `..._total`
- `compass_agent.transport.publish.priority_retry_depth` (gauge)
- `compass_agent.transport.publish.trace_queue_depth` (gauge)
- `compass_agent.transport.frame_sink.durable_attempts` → `..._total`
- `compass_agent.transport.frame_sink.durable_give_ups` → `..._total`
- `compass_agent.transport.control.reconnects` → `..._total`
- `compass_agent.transport.control.no_progress_depth` (gauge)
- `compass_agent.transport.control.flap_resets` → `..._total`
- `compass_agent.transport.control.unmapped` → `..._total` (by `event_type`)

## `compass-agent-loop-traces.json`

Agent-loop GenAI traces (Tempo datasource variable `DS_TEMPO`). The
pi-agent-core loop emits GenAI-semconv spans; the span hierarchy is
`invoke_agent {agent.name}` → children `chat {model}` / `execute_tool
{tool.name}` / `handoff`. Correlate a trace to a session via span attribute
`gen_ai.conversation.id` (= session id) and resource attribute
`compass.session.id` (stamped via `OTEL_RESOURCE_ATTRIBUTES` on both loop and
transport signals).

Rendered signals (span names, TraceQL filtered to `service.name = compass-agent`):

- `invoke_agent.*` root spans — recent turns table
- `execute_tool.*` spans — tool-call table

## Import

To import a dashboard:

1. In Grafana, go to **Dashboards → New → Import**.
2. Upload the dashboard JSON file (or paste its contents).
3. When prompted, pick the concrete datasource for the dashboard's datasource
   variable (`DS_PROMETHEUS` for the transport dashboard, `DS_TEMPO` for the
   loop-traces dashboard).
4. Click **Import**.

## Server/runner panels — follow-up (T4b)

The Go server/runner observability signals (task T4b of the frozen record) do
not exist yet. Panels for those signals are a follow-up and will be added once
those metrics ship; this directory currently binds only the shipped
`compass-agent` signals listed above.
