// Module-private OTel metric constants for the two OUTBOUND transport modules —
// the publish spine (publish-spine.ts) and the durable frame sink (frame-sink.ts).
// Decision 2 of docs/designs/platform/compass-agent-effect-otel/design.md
// (the metric table) owns these seven names; the control.* rows live in O3.
//
// `Metric` is from core `effect` (already a dependency — this module adds none).
// A `Metric.counter`/`Metric.gauge` value is a cheap module-level constant: with
// no OTel provider installed it no-ops into Effect's in-memory registry, so the
// black-box transport suites stay green and instrumentation is invisible without
// a provider. These constants are NOT re-exported from the package entry, so no
// `effect` type reaches the public `.d.ts` (export-surface.test.ts guards this).
//
// The names are the exact Decision-2 strings — do not rename without the record.

import { Metric } from "effect";

// Trace/session frames lost, carrying a `reason` label. The two reasons sum to
// today's `droppedTraceCount()` (publish-spine.ts): "overflow" is the bounded
// trace queue evicting the oldest on a full offer; "failed_batch" is the trace
// frames abandoned when a cycled batch send fails (loss-tolerable). Pre-tagged
// constants (Metric.tagged) so each increment site names its reason directly.
const traceFramesLost = Metric.counter(
	"compass_agent.transport.publish.trace_frames_lost",
	{ incremental: true },
);
export const traceFramesLostOverflow = Metric.tagged(
	traceFramesLost,
	"reason",
	"overflow",
);
export const traceFramesLostFailedBatch = Metric.tagged(
	traceFramesLost,
	"reason",
	"failed_batch",
);

// Priority frames (lifecycle/STOPPED, control acks) lost after a failed batch
// exhausted the bounded priority retry. A never-drop loss is a contract breach,
// kept a SEPARATE metric mirroring the source's separate `failedPriorityFrames`.
export const priorityFramesLost = Metric.counter(
	"compass_agent.transport.publish.priority_frames_lost",
	{ incremental: true },
);

// Every priority-batch retry attempt (monotone).
export const priorityBatchRetries = Metric.counter(
	"compass_agent.transport.publish.priority_batch_retries",
	{ incremental: true },
);

// The pump-scoped consecutive retry budget as a LEVEL: set to the new retry
// depth on each retry, reset to 0 on a successful send.
export const priorityRetryDepth = Metric.gauge(
	"compass_agent.transport.publish.priority_retry_depth",
);

// Trace queue depth, sampled at each batch take.
export const traceQueueDepth = Metric.gauge(
	"compass_agent.transport.publish.trace_queue_depth",
);

// Every durable send attempt (initial + each retry) on the frame sink.
export const durableAttempts = Metric.counter(
	"compass_agent.transport.frame_sink.durable_attempts",
	{ incremental: true },
);

// The durable send definitively failing (retry budget exhausted) — the
// onSettle error/give-up path.
export const durableGiveUps = Metric.counter(
	"compass_agent.transport.frame_sink.durable_give_ups",
	{ incremental: true },
);

// -----------------------------------------------------------------------------
// control source (O3) — the INBOUND control-stream lane (control-source.ts).
// Decision 2 of docs/designs/platform/compass-agent-effect-otel/design.md owns
// these four control.* names; same style as the O2 constants above.
// -----------------------------------------------------------------------------

// Every reconnect backoff taken on the Control server-stream — the
// `CONTROL_RECONNECT_BACKOFF_MS[attempt++]` take (monotone).
export const reconnects = Metric.counter(
	"compass_agent.transport.control.reconnects",
	{ incremental: true },
);

// The consecutive-no-progress level as a LEVEL: set to `noProgress` after each
// drop's progress check (against CONTROL_RECONNECT_NO_PROGRESS_MAX), reset to 0
// when a reconnect makes progress — a level, not a count, exactly like the
// publish spine's priority_retry_depth gauge above.
export const noProgressDepth = Metric.gauge(
	"compass_agent.transport.control.no_progress_depth",
);

// Every min-uptime flap reset of the backoff ladder — the reset-on-open
// flap-detector zeroing `attempt` after a past-floor connection dropped
// (monotone).
export const flapResets = Metric.counter(
	"compass_agent.transport.control.flap_resets",
	{ incremental: true },
);

// Control ops counted-unmapped through the single `count()` funnel, labeled by
// event type. The `event_type` label is DYNAMIC (the wire eventType varies per
// call — `control:steer`, `control:replay`, …), so unlike O2's static `reason`
// tags this is the BASE counter, tagged per-call at the increment site with
// `Metric.tagged(controlUnmapped, "event_type", eventType)`. Piggybacks the
// existing funnel; the `onUnmapped` callback contract is unchanged.
export const controlUnmapped = Metric.counter(
	"compass_agent.transport.control.unmapped",
	{ incremental: true },
);
