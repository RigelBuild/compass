import { afterEach, expect, test } from "bun:test";
import { Agent } from "@oh-my-pi/pi-agent-core";
import { createMockModel } from "@oh-my-pi/pi-ai/providers/mock";
import { context, propagation, trace } from "@opentelemetry/api";
import {
	InMemorySpanExporter,
	SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import { createTraceBridge } from "./trace-bridge";

// agent.test.ts models the SDK's span start with a fake; this drives the REAL agent loop, so an
// SDK bump that starts invoke_agent outside the caller's context, or after an await, fails here.

const TP_TRACE_ID = "0af7651916cd43dd8448eb211c80319c";
const TP_SPAN_ID = "b7ad6b7169203331";
const TP_HEADER = `00-${TP_TRACE_ID}-${TP_SPAN_ID}-01`;

let provider: NodeTracerProvider | undefined;

afterEach(async () => {
	trace.disable();
	context.disable();
	propagation.disable();
	await provider?.shutdown();
	provider = undefined;
});

test("a real agent-loop prompt() parents its invoke_agent span on the runWithParent traceparent", async () => {
	const exporter = new InMemorySpanExporter();
	provider = new NodeTracerProvider({
		spanProcessors: [new SimpleSpanProcessor(exporter)],
	});
	provider.register();
	const bridge = createTraceBridge();
	const model = createMockModel({ responses: [{ content: ["ok"] }] });
	const agent = new Agent({
		initialState: { systemPrompt: [], model },
		streamFn: model.stream,
		telemetry: {
			onSpanStart: bridge.onSpanStart,
			onSpanEnd: bridge.onSpanEnd,
		},
	});

	// Production order (agent.ts idle steer): start prompt, stamp before awaiting. A span started
	// after an await would miss the stamp even though parentage still holds.
	const started = bridge.runWithParent(TP_HEADER, () => agent.prompt("hi"));
	bridge.stampActiveTurn("m1");
	await started;

	const turns = exporter
		.getFinishedSpans()
		.filter((s) => s.name === "invoke_agent");
	expect(turns).toHaveLength(1);
	expect(turns[0]?.parentSpanContext?.traceId).toBe(TP_TRACE_ID);
	expect(turns[0]?.parentSpanContext?.spanId).toBe(TP_SPAN_ID);
	expect(turns[0]?.attributes["compass.message.ids"]).toBe("m1");
	// The loop really completed: no error, and the mock's reply is the last message.
	expect(agent.state.error).toBeUndefined();
	const last = agent.state.messages.at(-1);
	expect(last?.role === "assistant" && last.stopReason).toBe("stop");
});
