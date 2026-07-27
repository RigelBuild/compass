// AgentControl wire contract: the inbound control envelope's protojson shape is
// what the Runner's Control producer encodes and the agent's ControlSource
// decodes, so a drift here silently breaks control classification. Each test
// defends a frozen wire fact — the camelCase oneof discriminator per variant,
// `controlSeq` as a top-level ENVELOPE field (not nested in the oneof), and the
// two agent→Runner ack frames' scalar shapes (uint64 → protojson string). Same
// philosophy as frame.test.ts for the outbound AgentFrame.

import { describe, expect, test } from "bun:test";
import {
	AgentControlSchema,
	AgentFrameSchema,
	AskAnswerControlSchema,
	AskQuestionAnswerSchema,
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
	// The wire discriminator: each control op maps to exactly one AgentControl
	// `control` oneof field. protobuf-es toJson renders the set oneof member as
	// its canonical proto3-JSON camelCase key (the cross-language interop
	// default; Go protojson agrees). A drifted key breaks the agent's control
	// dispatch, so these keys are asserted literally, and each round-trips back
	// through fromJson to an equal message.
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
			name: "askAnswer",
			key: "askAnswer",
			msg: create(AgentControlSchema, {
				control: {
					case: "askAnswer",
					value: create(AskAnswerControlSchema, {
						askId: "a1",
						answers: [
							create(AskQuestionAnswerSchema, {
								questionId: "q1",
								chosenOptionIds: ["opt1", "opt2"],
							}),
							create(AskQuestionAnswerSchema, {
								questionId: "q2",
								customText: "other",
							}),
						],
					}),
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
	test("ControlAck carries acked_seq + applied_above as uint64 JSON strings", () => {
		const ack = create(ControlAckSchema, {
			ackedSeq: 7n,
			appliedAbove: [9n, 11n],
		});
		const json = toJson(ControlAckSchema, ack);
		const view = json as {
			ackedSeq?: unknown;
			appliedAbove?: unknown;
		};
		// uint64 → JSON string; repeated uint64 → array of strings.
		expect(view.ackedSeq).toBe("7");
		expect(view.appliedAbove).toEqual(["9", "11"]);
		const back: ControlAck = fromJson(ControlAckSchema, json);
		expect(back.ackedSeq).toBe(7n);
		expect(back.appliedAbove).toEqual([9n, 11n]);
	});

	test("ReplayCompleteAck (empty) round-trips", () => {
		const ack = create(ReplayCompleteAckSchema, {});
		const json = toJson(ReplayCompleteAckSchema, ack);
		expect(fromJson(ReplayCompleteAckSchema, json)).toEqual(ack);
	});
});

describe("AgentFrame — ack variants carry the frozen oneof discriminator", () => {
	// The two agent→Runner ack frames ride the Publish spine as AgentFrame
	// variants (agent.proto oneof: replay_complete_ack = 4, control_ack = 5).
	// The oneof field name IS the wire discriminator C2 routes off, so it is
	// asserted literally at the AgentFrame level — the scalar-shape tests above
	// only exercise the standalone messages, never their envelope placement.
	// A rename or oneof-slot drift on these new variants passes buf breaking
	// (an addition stays non-breaking) and drift (schema regenerates cleanly),
	// so only a literal-key assertion catches it.
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
