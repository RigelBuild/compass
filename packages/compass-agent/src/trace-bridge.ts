// Trace-continuity bridge: links each injected channel message to the agent
// turn it drives, so a message span and its turn span share one trace.
//
// Design record (frozen contract):
// docs/designs/observability/compass-agent-message-trace-continuity/design.md (### T1).
//
// The bridge is ONE object internally, but its type is SPLIT by consumer: the
// agent sees only the OTel-type-free `TurnTracer`; `cli.ts` alone holds the full
// `TraceBridge` with the `Span`-typed hook members. Only `TurnTracer` ever
// appears on an exported CompassAgent/transport signature, so no OTel type
// crosses the package barrel or the transport fence.

import type { TelemetryHookContext } from "@oh-my-pi/pi-agent-core";
import {
	context,
	type Link,
	type Span,
	type SpanContext,
	trace,
} from "@opentelemetry/api";

const TRACEPARENT_VERSION = "00";
const TRACE_ID_LENGTH = 32;
const SPAN_ID_LENGTH = 16;
const FLAGS_LENGTH = 2;
const ZERO_TRACE_ID = "0".repeat(TRACE_ID_LENGTH);
const ZERO_SPAN_ID = "0".repeat(SPAN_ID_LENGTH);

// Lowercase-only is intentional and W3C-mandated: `traceparent` fields are
// lowercase hex on the wire, and compass-server (the stamper) emits lowercase.
// Matching that exactly is required — a case-insensitive relax would accept
// spec-violating input and yield ids that no longer byte-match the server's
// stamp, breaking the trace join. Do not add the `i` flag.
function isHex(value: string): boolean {
	return /^[0-9a-f]+$/.test(value);
}

/**
 * First-party W3C `traceparent` parser. Grammar:
 * `00-<32-hex trace-id>-<16-hex span-id>-<2-hex flags>`.
 *
 * Returns `undefined` for any malformed / empty / wrong-version / all-zero-id
 * input — a bad header NEVER fails an injection. Pure string parsing only: it
 * does NOT touch the global propagator (a no-op unless registered, and
 * registering one is a banned global side effect).
 */
export function parseTraceparent(header: string): SpanContext | undefined {
	const fields = header.split("-");
	if (fields.length !== 4) return undefined;
	const [version, traceId, spanId, flags] = fields;
	if (version !== TRACEPARENT_VERSION) return undefined;
	if (
		traceId.length !== TRACE_ID_LENGTH ||
		!isHex(traceId) ||
		traceId === ZERO_TRACE_ID
	)
		return undefined;
	if (
		spanId.length !== SPAN_ID_LENGTH ||
		!isHex(spanId) ||
		spanId === ZERO_SPAN_ID
	)
		return undefined;
	if (flags.length !== FLAGS_LENGTH || !isHex(flags)) return undefined;
	return {
		traceId,
		spanId,
		traceFlags: Number.parseInt(flags, 16),
		isRemote: true,
	};
}

/**
 * Agent-facing tracer surface — ZERO OTel types. This is the only member type
 * of the bridge that appears on an exported CompassAgent signature.
 */
export interface TurnTracer {
	runWithParent<T>(traceparent: string, fn: () => T): T;
	linkActiveTurn(traceparent: string, messageId: string): void;
	stampActiveTurn(messageIds: string): void;
}

/**
 * The full bridge held by `cli.ts` alone, adding the telemetry-hook members
 * that carry OTel `Span`s. Kept off every exported CompassAgent/transport
 * signature so the fence holds by construction.
 */
export interface TraceBridge extends TurnTracer {
	onSpanStart(ctx: TelemetryHookContext): void;
	onSpanEnd(ctx: TelemetryHookContext): void;
}

// The single un-identified invoke_agent span is the MAIN turn: every
// task-subagent loop runs with a non-undefined `agent` identity, and
// AgentBusyError on concurrent prompts guarantees at most one un-identified
// invoke_agent live at a time — so one slot is correct.
function isMainTurnSpan(ctx: TelemetryHookContext): boolean {
	return ctx.kind === "invoke_agent" && ctx.agent === undefined;
}

export function createTraceBridge(): TraceBridge {
	let capturedInvokeAgent: Span | undefined;

	return {
		runWithParent<T>(traceparent: string, fn: () => T): T {
			const spanContext = parseTraceparent(traceparent);
			if (spanContext === undefined) return fn();
			return context.with(
				trace.setSpan(context.active(), trace.wrapSpanContext(spanContext)),
				fn,
			);
		},

		linkActiveTurn(traceparent: string, messageId: string): void {
			if (capturedInvokeAgent === undefined) return;
			const spanContext = parseTraceparent(traceparent);
			if (spanContext === undefined) return;
			const link: Link = {
				context: spanContext,
				attributes: { "compass.message.id": messageId },
			};
			capturedInvokeAgent.addLink(link);
		},

		stampActiveTurn(messageIds: string): void {
			if (capturedInvokeAgent === undefined) return;
			capturedInvokeAgent.setAttribute("compass.message.ids", messageIds);
		},

		onSpanStart(ctx: TelemetryHookContext): void {
			if (isMainTurnSpan(ctx)) capturedInvokeAgent = ctx.span;
		},

		onSpanEnd(ctx: TelemetryHookContext): void {
			if (isMainTurnSpan(ctx)) capturedInvokeAgent = undefined;
		},
	};
}
