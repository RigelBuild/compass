# OTel endpoint: deployer contract

This is the deployer-facing contract for enabling transport telemetry export on
the compass-agent. It is a sibling of the frozen design record
(`docs/designs/platform/compass-agent-effect-otel/design.md`, task O5).

## The contract

To enable transport telemetry export, the deployer sets
`OTEL_EXPORTER_OTLP_ENDPOINT` as an ordinary `KEY=VALUE` line in the
Runner-materialized agent env file at `$HOME/.compass/env` — the same `0600`
aggregate file that already carries tool and MCP secrets.

There is nothing else to do:

- No new Runner-side mechanism.
- No Go change.
- No per-session socket configuration.

## Why a plain env key just works

The env key flows to the OTel layer through mechanisms that already exist:

- The key is neither `HOME` nor `COMPASS_*`-prefixed, so
  `isReservedEnvKey` (`packages/compass-agent/src/cli.ts:103-105`) does not drop
  it during parsing.
- The generic env-file sourcing in `main()`
  (`packages/compass-agent/src/cli.ts:519-523`) merges the parsed file keys into
  `process.env` before the session — and thus the transport — is constructed.
- The transport's OTel layer (`packages/compass-agent/src/transport/otel-layer.ts`)
  reads `process.env.OTEL_EXPORTER_OTLP_ENDPOINT` once, at construction, by which
  point the merge has already run.

## Default posture: off by default

An unset endpoint means `makeOtelLayer()` returns `Layer.empty`: no provider is
installed, there is no network egress, and there is zero overhead. Setting the
endpoint opts a deployment into OTLP-over-HTTP/protobuf export to that collector.

## Regression pin

The env-file to OTel-layer reachability is pinned by a regression test in
`packages/compass-agent/src/cli.test.ts` (the
`main sources $HOME/.compass/env` block), which asserts the endpoint key lands in
`process.env` unfiltered while a `COMPASS_`-prefixed key is dropped.
