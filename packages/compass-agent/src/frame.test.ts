// ProtojsonLineSink: the frame → wire-bytes boundary. Each test defends the
// frozen wire contract — one newline-terminated JSON line per frame, keyed by
// the exact `AgentFrame` oneof field, payload round-tripping through protojson.
// The oneof field name IS the wire discriminator, so it is asserted literally.

import { describe, expect, test } from "bun:test";
import {
	AgentSessionState,
	create,
	DeliveryAckSchema,
	SessionAssistantTextSchema,
	SessionEventSchema,
	SessionFrameSchema,
	TranscriptEntrySchema,
} from "./compassv1";
import { type OutboundFrame, ProtojsonLineSink } from "./frame";

// Emit one frame through the sink and capture the exact string written.
function emitLine(frame: OutboundFrame): string {
	let captured: string | undefined;
	new ProtojsonLineSink((line) => {
		captured = line;
	}).emit(frame);
	if (captured === undefined) throw new Error("sink.emit did not write a line");
	return captured;
}

// Parse the one JSON object the sink wrote (after stripping the trailing "\n").
function parseLine(frame: OutboundFrame): Record<string, unknown> {
	const line = emitLine(frame);
	expect(line.endsWith("\n")).toBe(true);
	return JSON.parse(line) as Record<string, unknown>;
}

describe("ProtojsonLineSink — one line, frozen oneof key per frame kind", () => {
	// The wire discriminator: each domain frame kind maps to exactly one
	// AgentFrame oneof field. protobuf-es `toJson` renders oneof fields in
	// canonical proto3-JSON camelCase (the cross-language interop default; Go
	// protojson agrees), so the emitted top-level key is the generated `case`
	// name. A drifted key silently breaks the Runner's oneof classification, so
	// these strings are asserted literally: session / transcriptEntry /
	// deliveryAck.
	const session = create(SessionFrameSchema, {
		state: AgentSessionState.WORKING,
	});
	const transcriptEntry = create(TranscriptEntrySchema, { entrySeq: 1n });
	const deliveryAck = create(DeliveryAckSchema, {});

	const rows: { name: string; frame: OutboundFrame; field: string }[] = [
		{
			name: "session",
			frame: { kind: "session", value: session },
			field: "session",
		},
		{
			name: "transcriptEntry",
			frame: { kind: "transcriptEntry", value: transcriptEntry },
			field: "transcriptEntry",
		},
		{
			name: "deliveryAck",
			frame: { kind: "deliveryAck", value: deliveryAck },
			field: "deliveryAck",
		},
	];

	for (const row of rows) {
		test(`${row.name} → single top-level key "${row.field}"`, () => {
			const obj = parseLine(row.frame);
			expect(Object.keys(obj)).toEqual([row.field]);
		});
	}

	test("emit writes exactly one newline-terminated line", () => {
		const line = emitLine({ kind: "session", value: session });
		expect(line.endsWith("\n")).toBe(true);
		// Exactly one line: no interior newline splitting one frame into two.
		expect(line.trimEnd().split("\n")).toHaveLength(1);
	});
});

describe("ProtojsonLineSink — payload round-trips through protojson", () => {
	test("session lifecycle payload survives the wire (state → SCREAMING_SNAKE string, no typed event)", () => {
		const frame: OutboundFrame = {
			kind: "session",
			value: create(SessionFrameSchema, {
				state: AgentSessionState.WORKING,
			}),
		};
		const payload = parseLine(frame).session as Record<string, unknown>;
		// protobuf-es protojson renders enum values as their proto names.
		expect(payload.state).toBe("AGENT_SESSION_STATE_WORKING");
		// A pure lifecycle transition carries no trace event: protojson omits the
		// unset `typed_event` field entirely (proto3 default → not serialized).
		expect(payload.typedEvent).toBeUndefined();
	});

	test("session typed-event payload round-trips the nested typed SessionEvent", () => {
		// A trace frame carries a typed SessionEvent on `typed_event`. protojson
		// renders it as a nested camelCase object; int64 (`at_unix_ms`) becomes a
		// JSON STRING, and the oneof case is the inner `assistantText` key. The
		// nested fields must survive the wire so the render side reads them.
		const typedEvent = create(SessionEventSchema, {
			eventId: "7",
			atUnixMs: 1_700_000_000_000n,
			event: {
				case: "assistantText",
				value: create(SessionAssistantTextSchema, {
					text: "hello there",
					messageId: "3",
				}),
			},
		});
		const frame: OutboundFrame = {
			kind: "session",
			value: create(SessionFrameSchema, { typedEvent }),
		};
		const payload = parseLine(frame).session as {
			typedEvent: {
				eventId: string;
				atUnixMs: string;
				assistantText: { text: string; messageId: string };
			};
			state?: unknown;
		};
		expect(payload.typedEvent.eventId).toBe("7");
		// int64 serializes to a JSON string in proto3 JSON, not a number.
		expect(payload.typedEvent.atUnixMs).toBe("1700000000000");
		expect(typeof payload.typedEvent.atUnixMs).toBe("string");
		// The oneof surfaces as its camelCase field key with the nested payload.
		expect(payload.typedEvent.assistantText.text).toBe("hello there");
		expect(payload.typedEvent.assistantText.messageId).toBe("3");
		// A trace-only frame carries no board transition: state omitted (default).
		expect(payload.state).toBeUndefined();
	});

	test("transcriptEntry payload round-trips the entry json + seq (uint64 → JSON string)", () => {
		const frame: OutboundFrame = {
			kind: "transcriptEntry",
			value: create(TranscriptEntrySchema, {
				entryJson: '{"role":"assistant"}',
				entrySeq: 42n,
				checkpoint: true,
			}),
		};
		const payload = parseLine(frame).transcriptEntry as {
			entryJson: string;
			entrySeq: string;
			checkpoint: boolean;
		};
		expect(payload.entryJson).toBe('{"role":"assistant"}');
		// uint64 serializes to a JSON string in proto3 JSON, not a number.
		expect(payload.entrySeq).toBe("42");
		expect(typeof payload.entrySeq).toBe("string");
		expect(payload.checkpoint).toBe(true);
	});
});
