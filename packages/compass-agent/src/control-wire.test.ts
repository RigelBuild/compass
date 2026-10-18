// Pins the protojson wire names and scalar shapes shared with the Runner.

import { describe, expect, test } from "bun:test";
import {
	AgentControlSchema,
	AgentFrameSchema,
	type ControlAck,
	ControlAckSchema,
	create,
	fromJson,
	PromptControlSchema,
	ReplayCompleteAckSchema,
	ReplayCompleteSchema,
	SteerControlSchema,
	toJson,
} from "./compassv1";

describe("AgentControl — camelCase oneof discriminator per control variant", () => {
	// Literal camelCase oneof keys catch wire-name drift that would break dispatch.
	const cases = [
		{
			name: "prompt",
			key: "prompt",
			msg: create(AgentControlSchema, {
				control: {
					case: "prompt",
					value: create(PromptControlSchema, { input: "hello" }),
				},
			}),
		},
		{
			name: "replayComplete (empty payload)",
			key: "replayComplete",
			msg: create(AgentControlSchema, {
				control: {
					case: "replayComplete",
					value: create(ReplayCompleteSchema, {}),
				},
			}),
		},
		{
			name: "steer (empty shell)",
			key: "steer",
			msg: create(AgentControlSchema, {
				control: {
					case: "steer",
					value: create(SteerControlSchema, {}),
				},
			}),
		},
	];

	for (const c of cases) {
		test(`${c.name} → oneof key "${c.key}", round-trips`, () => {
			const json = toJson(AgentControlSchema, c.msg);
			const keys = json as Record<string, unknown>;
			// The set oneof member surfaces as its camelCase field key.
			expect(keys[c.key]).toBeDefined();
			// No other variant key leaks onto the wire.
			for (const other of cases) {
				if (other.key !== c.key) expect(keys[other.key]).toBeUndefined();
			}
			// The envelope decodes back to an equal message (case + payload).
			const back = fromJson(AgentControlSchema, json);
			expect(back.control.case).toBe(c.msg.control.case);
			expect(back).toEqual(c.msg);
		});
	}
});

describe("AgentControl — controlSeq is a top-level envelope field", () => {
	test("controlSeq rides beside the oneof key, not nested in the payload", () => {
		const msg = create(AgentControlSchema, {
			controlSeq: 42n,
			control: {
				case: "prompt",
				value: create(PromptControlSchema, { input: "hi" }),
			},
		});
		const json = toJson(AgentControlSchema, msg);
		const view = json as {
			controlSeq?: unknown;
			prompt?: Record<string, unknown>;
		};
		// uint64 serializes to a JSON string in proto3 JSON, not a number.
		expect(view.controlSeq).toBe("42");
		expect(typeof view.controlSeq).toBe("string");
		// It is an ENVELOPE field — a sibling of the oneof key, never nested
		// under the control payload. Nesting it would break retention/redelivery
		// (the Runner reads controlSeq off the envelope, C2/C3).
		expect(view.prompt?.controlSeq).toBeUndefined();
		// Round-trips as a bigint.
		const back = fromJson(AgentControlSchema, json);
		expect(back.controlSeq).toBe(42n);
	});
});

describe("AgentControl ack frames — scalar shapes survive the wire", () => {
	test("ControlAck carries legacy seqs and inclusive ranges as uint64 JSON strings", () => {
		const ack = create(ControlAckSchema, {
			ackedSeq: 7n,
			appliedAbove: [9n, 11n],
			appliedAboveRanges: [12n, 15n, 20n, 20n],
		});
		const json = toJson(ControlAckSchema, ack);
		const view = json as {
			ackedSeq?: unknown;
			appliedAbove?: unknown;
			appliedAboveRanges?: unknown;
		};
		// uint64 → JSON string; ranges are flattened [start, end] pairs.
		expect(view.ackedSeq).toBe("7");
		expect(view.appliedAbove).toEqual(["9", "11"]);
		expect(view.appliedAboveRanges).toEqual(["12", "15", "20", "20"]);
		const back: ControlAck = fromJson(ControlAckSchema, json);
		expect(back.ackedSeq).toBe(7n);
		expect(back.appliedAbove).toEqual([9n, 11n]);
		expect(back.appliedAboveRanges).toEqual([12n, 15n, 20n, 20n]);
	});

	test("ReplayCompleteAck (empty) round-trips", () => {
		const ack = create(ReplayCompleteAckSchema, {});
		const json = toJson(ReplayCompleteAckSchema, ack);
		expect(fromJson(ReplayCompleteAckSchema, json)).toEqual(ack);
	});
});

describe("AgentFrame — ack variants carry the oneof discriminator", () => {
	// Envelope assertions catch dispatch breaks that schema-additive ack variants can introduce.
	test("replayCompleteAck → single top-level AgentFrame key", () => {
		const frame = create(AgentFrameSchema, {
			frame: {
				case: "replayCompleteAck",
				value: create(ReplayCompleteAckSchema, {}),
			},
		});
		const json = toJson(AgentFrameSchema, frame);
		expect(Object.keys(json as Record<string, unknown>)).toEqual([
			"replayCompleteAck",
		]);
		const back = fromJson(AgentFrameSchema, json);
		expect(back.frame.case).toBe("replayCompleteAck");
	});

	test("controlAck → single top-level AgentFrame key, payload survives", () => {
		const frame = create(AgentFrameSchema, {
			frame: {
				case: "controlAck",
				value: create(ControlAckSchema, {
					ackedSeq: 7n,
					appliedAbove: [9n, 11n],
					appliedAboveRanges: [12n, 15n],
				}),
			},
		});
		const json = toJson(AgentFrameSchema, frame);
		expect(Object.keys(json as Record<string, unknown>)).toEqual([
			"controlAck",
		]);
		const back = fromJson(AgentFrameSchema, json);
		expect(back.frame.case).toBe("controlAck");
		if (back.frame.case !== "controlAck") throw new Error("unreachable");
		expect(back.frame.value.ackedSeq).toBe(7n);
		expect(back.frame.value.appliedAbove).toEqual([9n, 11n]);
		expect(back.frame.value.appliedAboveRanges).toEqual([12n, 15n]);
	});
});

describe("AgentControl — empty envelope defaults", () => {
	test("no control set → oneof case undefined, controlSeq 0n", () => {
		const msg = create(AgentControlSchema, {});
		expect(msg.control.case).toBeUndefined();
		expect(msg.controlSeq).toBe(0n);
		// An empty envelope serializes to an empty JSON object (all fields at
		// proto3 default → omitted).
		const json = toJson(AgentControlSchema, msg) as Record<string, unknown>;
		expect(json.controlSeq).toBeUndefined();
	});
});
