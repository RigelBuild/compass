// Module-private OTel metric constants for the two OUTBOUND transport modules — the
// publish spine and the durable frame sink. Decision 2 of the compass-agent-effect-otel
// design (the metric table) owns these seven names; the control.* rows live in O3.

// `Metric` is from core `effect` (already a dependency). A Metric.counter/gauge value is
// a cheap module-level constant: with no OTel provider it no-ops into Effect's in-memory
// registry, so instrumentation is invisible without a provider. NOT re-exported from the
// package entry (export-surface.test.ts guards this). Names are exact — do not rename.

import { Metric } from "effect";

// Trace/session frames lost, carrying a `reason` label. The reasons sum to
// droppedTraceCount(): "overflow" is the bounded queue evicting the oldest on a full
// offer; "failed_batch" is the trace frames abandoned when a cycled batch send fails.
// Pre-tagged constants (Metric.tagged) so each increment site names its reason directly.
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

// The two publish-spine LEVEL gauges, built through a namespace-prefix factory. A gauge
// is last-writer-wins, so a test reading one must not share its registry key with a
// concurrent writer in a sibling file (the global registry keys on the NAME; bun runs
// files concurrently). Production passes no prefix; a test a unique one. Counters read as a delta.
export const priorityRetryDepthGauge = (
	namespace = "",
): Metric.Metric.Gauge<number> =>
	// The pump-scoped consecutive retry budget as a LEVEL: set to the new retry
	// depth on each retry, reset to 0 on a successful send.
	Metric.gauge(
		`${namespace}compass_agent.transport.publish.priority_retry_depth`,
	);

export const traceQueueDepthGauge = (
	namespace = "",
): Metric.Metric.Gauge<number> =>
	// Trace queue depth, sampled at each batch take.
	Metric.gauge(`${namespace}compass_agent.transport.publish.trace_queue_depth`);

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
// control source (O3) — the INBOUND control-stream lane (control-source.ts). Decision 2
// of the compass-agent-effect-otel design owns these four control.* names.
// -----------------------------------------------------------------------------

// Every reconnect backoff taken on the Control server-stream — the
// `CONTROL_RECONNECT_BACKOFF_MS[attempt++]` take (monotone).
export const reconnects = Metric.counter(
	"compass_agent.transport.control.reconnects",
	{ incremental: true },
);

// The consecutive-no-progress LEVEL gauge, built through the same namespace factory as
// the publish-spine gauges and for the same reason (the cross-file gauge race). Set to
// `noProgress` after each drop's progress check, reset to 0 on progress. Production passes
// no namespace; a test passes a unique prefix.
export const noProgressDepthGauge = (
	namespace = "",
): Metric.Metric.Gauge<number> =>
	Metric.gauge(`${namespace}compass_agent.transport.control.no_progress_depth`);

// Every min-uptime flap reset of the backoff ladder — the reset-on-open
// flap-detector zeroing `attempt` after a past-floor connection dropped
// (monotone).
export const flapResets = Metric.counter(
	"compass_agent.transport.control.flap_resets",
	{ incremental: true },
);

// Control ops counted-unmapped through the single count() funnel, labeled by event type.
// The `event_type` label is DYNAMIC (control:steer, control:replay, …), so unlike O2's
// static reason tags this is the BASE counter, tagged per-call at the increment site with
// Metric.tagged(controlUnmapped, "event_type", eventType).
export const controlUnmapped = Metric.counter(
	"compass_agent.transport.control.unmapped",
	{ incremental: true },
);
