// Tests for the trace-continuity bridge.
// docs/designs/observability/compass-agent-message-trace-continuity/design.md (### T1).
//
// House in-memory OTel recipe: a NodeTracerProvider (installs a real context
// manager, so `context.with` propagates into `fn`) with an InMemorySpanExporter
// + SimpleSpanProcessor, registered via `provider.register()`. `trace.disable()`
// + `context.disable()` in teardown so no global leaks into sibling tests.

import { afterEach, expect, test } from "bun:test";
import type {
	AgentIdentity,
	TelemetryHookContext,
	TelemetrySpanKind,
} from "@oh-my-pi/pi-agent-core";
import { context, type Span, trace } from "@opentelemetry/api";
import {
	InMemorySpanExporter,
	SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import { createTraceBridge, parseTraceparent } from "./trace-bridge";

const TRACE_ID = "0af7651916cd43dd8448eb211c80319c";
const SPAN_ID = "b7ad6b7169203331";
const VALID_HEADER = `00-${TRACE_ID}-${SPAN_ID}-01`;

let provider: NodeTracerProvider | undefined;

function recordingExporter(): InMemorySpanExporter {
	const exporter = new InMemorySpanExporter();
	provider = new NodeTracerProvider({
		spanProcessors: [new SimpleSpanProcessor(exporter)],
	});
	provider.register();
	return exporter;
}

// A live invoke_agent span the bridge can capture: started on the recording
// provider so it exports on end. `agent === undefined` marks the MAIN turn.
function startTurnSpan(): Span {
	return trace.getTracer("test").startSpan("invoke_agent");
}

function hookCtx(
	span: Span,
	kind: TelemetrySpanKind,
	agent: AgentIdentity | undefined,
): TelemetryHookContext {
	return { span, kind, agent, model: undefined, conversationId: undefined };
}

afterEach(async () => {
	trace.disable();
	context.disable();
	await provider?.shutdown();
	provider = undefined;
});

test("parseTraceparent round-trips a valid header's trace-id, span-id, and flags", () => {
	const parsed = parseTraceparent(VALID_HEADER);
	expect(parsed).toBeDefined();
	expect(parsed?.traceId).toBe(TRACE_ID);
	expect(parsed?.spanId).toBe(SPAN_ID);
	expect(parsed?.traceFlags).toBe(1);
	expect(parsed?.isRemote).toBe(true);
});

test("parseTraceparent rejects malformed, empty, all-zero-id, and wrong-version inputs", () => {
	expect(parseTraceparent("")).toBeUndefined();
	expect(parseTraceparent("00-tooshort-b7ad6b7169203331-01")).toBeUndefined();
	expect(parseTraceparent(`00-${TRACE_ID}-b7ad6b7169203331`)).toBeUndefined(); // wrong field count
	expect(parseTraceparent(`${VALID_HEADER}-extra`)).toBeUndefined(); // trailing data (5 fields)
	expect(
		parseTraceparent(`00-${TRACE_ID}-zzzzzzzzzzzzzzzz-01`),
	).toBeUndefined(); // non-hex span-id
	expect(
		parseTraceparent(`00-${"0".repeat(32)}-${SPAN_ID}-01`),
	).toBeUndefined(); // all-zero trace-id
	expect(
		parseTraceparent(`00-${TRACE_ID}-${"0".repeat(16)}-01`),
	).toBeUndefined(); // all-zero span-id
	expect(parseTraceparent(`01-${TRACE_ID}-${SPAN_ID}-01`)).toBeUndefined(); // wrong version
});

test("runWithParent parents an inner span on the remote context and returns fn's value", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const result = bridge.runWithParent(VALID_HEADER, () => {
		trace.getTracer("test").startSpan("child").end();
		return 42;
	});
	expect(result).toBe(42);
	const [child] = exporter.getFinishedSpans();
	expect(child?.parentSpanContext?.traceId).toBe(TRACE_ID);
	expect(child?.parentSpanContext?.spanId).toBe(SPAN_ID);
});

test("runWithParent still runs fn as a root span on a malformed traceparent", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const result = bridge.runWithParent("not-a-traceparent", () => {
		trace.getTracer("test").startSpan("child").end();
		return "ran";
	});
	expect(result).toBe("ran");
	const [child] = exporter.getFinishedSpans();
	expect(child?.parentSpanContext).toBeUndefined();
});

test("linkActiveTurn links the captured turn span to the parsed header, then no-ops without a span", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const span = startTurnSpan();
	bridge.onSpanStart(hookCtx(span, "invoke_agent", undefined));
	bridge.linkActiveTurn(VALID_HEADER, "msg-1");
	span.end();

	const [exported] = exporter.getFinishedSpans();
	expect(exported?.links).toHaveLength(1);
	expect(exported?.links[0]?.context.traceId).toBe(TRACE_ID);
	expect(exported?.links[0]?.context.spanId).toBe(SPAN_ID);
	expect(exported?.links[0]?.attributes?.["compass.message.id"]).toBe("msg-1");

	// A fresh bridge has never captured a span; linkActiveTurn on the empty
	// slot must no-op, not throw.
	const fresh = createTraceBridge();
	expect(() => fresh.linkActiveTurn(VALID_HEADER, "msg-2")).not.toThrow();
});

test("stampActiveTurn stamps compass.message.ids on the captured span, then no-ops without one", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const span = startTurnSpan();
	bridge.onSpanStart(hookCtx(span, "invoke_agent", undefined));
	bridge.stampActiveTurn("a,b,c");
	span.end();

	const [exported] = exporter.getFinishedSpans();
	expect(exported?.attributes["compass.message.ids"]).toBe("a,b,c");

	const fresh = createTraceBridge();
	expect(() => fresh.stampActiveTurn("x,y")).not.toThrow();
});

test("the subagent filter does not clobber the captured main-turn span", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const mainSpan = startTurnSpan();
	bridge.onSpanStart(hookCtx(mainSpan, "invoke_agent", undefined));

	// A subagent invoke_agent span (agent identity SET) must NOT overwrite the
	// captured main-turn span.
	const subagentSpan = startTurnSpan();
	bridge.onSpanStart(
		hookCtx(subagentSpan, "invoke_agent", { id: "sub", name: "subagent" }),
	);

	// A non-invoke_agent span is ignored too.
	bridge.onSpanStart(hookCtx(startTurnSpan(), "chat", undefined));

	bridge.linkActiveTurn(VALID_HEADER, "msg-main");
	mainSpan.end();
	subagentSpan.end();

	const spans = exporter.getFinishedSpans();
	const mainId = mainSpan.spanContext().spanId;
	const subId = subagentSpan.spanContext().spanId;
	const mainExported = spans.find((s) => s.spanContext().spanId === mainId);
	const subExported = spans.find((s) => s.spanContext().spanId === subId);
	expect(mainExported?.links).toHaveLength(1);
	expect(mainExported?.links[0]?.attributes?.["compass.message.id"]).toBe(
		"msg-main",
	);
	expect(subExported?.links).toHaveLength(0);
});

test("onSpanEnd for a subagent turn does not clear the captured main-turn span", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const mainSpan = startTurnSpan();
	bridge.onSpanStart(hookCtx(mainSpan, "invoke_agent", undefined));

	// A subagent invoke_agent turn starts and ENDS (agent identity SET). Its
	// onSpanEnd must NOT clear the main slot — a regression dropping the
	// `agent === undefined` guard on the clear path would no-op every later steer.
	const subagentSpan = startTurnSpan();
	const subCtx = hookCtx(subagentSpan, "invoke_agent", {
		id: "sub",
		name: "subagent",
	});
	bridge.onSpanStart(subCtx);
	bridge.onSpanEnd(subCtx);
	subagentSpan.end();

	// The main slot must survive the subagent end: this link still lands.
	bridge.linkActiveTurn(VALID_HEADER, "msg-main");
	mainSpan.end();

	const spans = exporter.getFinishedSpans();
	const mainExported = spans.find(
		(s) => s.spanContext().spanId === mainSpan.spanContext().spanId,
	);
	expect(mainExported?.links).toHaveLength(1);
	expect(mainExported?.links[0]?.attributes?.["compass.message.id"]).toBe(
		"msg-main",
	);
});

test("onSpanEnd for the main turn clears the slot so later steers no-op", () => {
	const exporter = recordingExporter();
	const bridge = createTraceBridge();
	const mainSpan = startTurnSpan();
	const mainCtx = hookCtx(mainSpan, "invoke_agent", undefined);
	bridge.onSpanStart(mainCtx);
	bridge.onSpanEnd(mainCtx);

	// The slot is cleared while the main span is still LIVE: a follow-up steer
	// must no-op, not add a link. Steering BEFORE mainSpan.end() is what makes
	// this discriminating -- OTel silently drops addLink on an already-ended
	// span, so ending first would give 0 links whether or not the slot cleared.
	expect(() => bridge.linkActiveTurn(VALID_HEADER, "msg-late")).not.toThrow();
	mainSpan.end();

	const mainExported = exporter
		.getFinishedSpans()
		.find((s) => s.spanContext().spanId === mainSpan.spanContext().spanId);
	expect(mainExported?.links).toHaveLength(0);
});
