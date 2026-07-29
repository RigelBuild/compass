// CompassAgent: control application + the frozen replay barrier (§T5). Tests
// inject a fake `AgentSession` — a recording inner `Agent` behind `session.agent`
// plus a `subscribe` on the session (the AgentSessionEvent stream source) — and a
// fake ControlSource (a finite async generator — no stdin, no timers), then
// assert the observable effects: which SDK methods `run()` drove on
// `session.agent`, in what order, and the STARTING/STOPPED/ERRORED `session`
// lifecycle frames bracketing the run. The board lifecycle rides the `session`
// variant (SessionFrame.state); the agent mints no server id. The barrier is the
// load-bearing contract: live input is refused (and surfaced, never dropped)
// until ReplayComplete lifts it.

import { describe, expect, test } from "bun:test";
import type { AgentMessage, AgentTool } from "@oh-my-pi/pi-agent-core";
import type {
	AgentSession,
	AgentSessionEventListener,
} from "@oh-my-pi/pi-coding-agent";
import { CompassAgent } from "./agent";
import {
	AgentSessionState,
	AskQuestionAnswerSchema,
	create,
} from "./compassv1";
import type { AgentControl, ControlSource } from "./control";
import type { OutboundFrame } from "./frame";
import type { UnmappedEvent } from "./mapping";

// A recording fake for the SDK Agent — the control surface CompassAgent drives
// (now reached through `session.agent`). It records the calls the class makes so
// tests assert on observable behavior (what was driven) rather than internals.
// Only the members CompassAgent touches are implemented; the rest of the wide
// Agent surface is never called, so the cast at construction is honest.
interface RecordingAgent {
	readonly prompts: string[];
	readonly steers: AgentMessage[];
	readonly appended: AgentMessage[];
	readonly systemPrompts: (string[] | string)[];
	readonly toolSets: AgentTool[][];
	// The live SDK-shaped state CompassAgent reads its native tool set from at
	// construction. Mirrors `Agent.state` (which returns the live object), so a
	// test can assert the snapshot is a copy and not this array.
	readonly state: { tools: AgentTool[] };
}

// A recording fake for AgentSession — the external boundary CompassAgent
// subscribes and drives. `subscribe` records/returns an unsubscribe (the run
// wires the event stream through it); `agent` is the recording inner Agent the
// control ops drive.
interface RecordingSession {
	readonly agent: RecordingAgent;
	subscribed: number;
	unsubscribed: number;
	listener: AgentSessionEventListener | undefined;
}

function recordingSession(natives: AgentTool[] = []): RecordingSession {
	const agent: RecordingAgent = {
		prompts: [],
		steers: [],
		appended: [],
		systemPrompts: [],
		toolSets: [],
		state: { tools: natives },
	};
	const agentImpl = {
		prompt(input: string): Promise<void> {
			agent.prompts.push(input);
			return Promise.resolve();
		},
		steer(m: AgentMessage): void {
			agent.steers.push(m);
		},
		appendMessage(m: AgentMessage): void {
			agent.appended.push(m);
		},
		setSystemPrompt(v: string[] | string): void {
			agent.systemPrompts.push(v);
		},
		setTools(t: AgentTool[]): void {
			agent.toolSets.push(t);
		},
	};
	Object.assign(agent, agentImpl);
	const rec: RecordingSession = {
		agent,
		subscribed: 0,
		unsubscribed: 0,
		listener: undefined,
	};
	const sessionImpl = {
		subscribe(fn: AgentSessionEventListener): () => void {
			rec.subscribed++;
			rec.listener = fn;
			return () => {
				rec.unsubscribed++;
			};
		},
	};
	Object.assign(rec, sessionImpl);
	return rec;
}

// A UserMessage AgentMessage for replay/steer fixtures.
function userMessage(text: string): AgentMessage {
	return { role: "user", content: text, timestamp: 0 };
}

// A minimally-shaped AgentTool fixture. The merge is keyed purely on `name`, so
// only the name is load-bearing; distinct calls yield distinct instances, which
// is what lets a test assert WHICH instance of a shared name survived.
function tool(name: string): AgentTool {
	return { name, description: name, parameters: {} } as unknown as AgentTool;
}

function names(tools: AgentTool[] | undefined): string[] {
	return (tools ?? []).map((t) => t.name);
}

// Run one agent over a fixed control script, capturing sink frames and unmapped
// events. The ControlSource is a finite async generator (no stdin, no timers),
// so `run()` resolves once it ends. The recording session is the only external
// dependency; the cast to AgentSession is honest because CompassAgent touches
// only the members implemented above.
async function runWith(controls: AgentControl[], natives: AgentTool[] = []) {
	const session = recordingSession(natives);
	const frames: OutboundFrame[] = [];
	const unmapped: UnmappedEvent[] = [];
	const control: ControlSource = {
		async *[Symbol.asyncIterator]() {
			for (const c of controls) yield c;
		},
	};
	await new CompassAgent({
		session: session as unknown as AgentSession,
		sink: { emit: (f) => frames.push(f) },
		control,
		onUnmapped: (u) => unmapped.push(u),
	}).run();
	return { session, agent: session.agent, frames, unmapped };
}

// Run one agent over an arbitrary ControlSource, capturing sink frames and
// whether run() rejected. Unlike runWith (which builds a finite generator from a
// fixed script), this takes the source directly so a test can supply one that
// throws mid-stream — the control-loop crash path. run() is awaited defensively:
// on rejection the error is captured, never re-thrown, so the caller asserts on
// both the terminal frames AND the rejection.
async function runWithSource(control: ControlSource) {
	const session = recordingSession();
	const frames: OutboundFrame[] = [];
	let error: unknown;
	try {
		await new CompassAgent({
			session: session as unknown as AgentSession,
			sink: { emit: (f) => frames.push(f) },
			control,
			onUnmapped: () => {},
		}).run();
	} catch (e) {
		error = e;
	}
	return { session, agent: session.agent, frames, error };
}

describe("CompassAgent — lifecycle bracketing (STARTING/STOPPED session frames)", () => {
	test("run() brackets the session with STARTING then STOPPED", async () => {
		const { frames } = await runWith([]);
		const states = frames.flatMap((f) =>
			f.kind === "session" ? [f.value.state] : [],
		);
		expect(states[0]).toBe(AgentSessionState.STARTING);
		expect(states.at(-1)).toBe(AgentSessionState.STOPPED);
	});

	test("run() subscribes the session stream and unsubscribes when control ends", async () => {
		const { session } = await runWith([]);
		expect(session.subscribed).toBe(1);
		expect(session.unsubscribed).toBe(1);
	});
});

describe("CompassAgent — replay barrier refuses live input pre-ReplayComplete", () => {
	test("prompt before ReplayComplete is refused and surfaced, never forwarded", async () => {
		const { agent, unmapped } = await runWith([
			{ kind: "prompt", input: "do the thing" },
		]);
		expect(agent.prompts).toEqual([]);
		expect(unmapped).toHaveLength(1);
		expect(unmapped[0].eventType).toBe("control:prompt");
	});

	test("steer before ReplayComplete is refused and surfaced, never forwarded", async () => {
		const { agent, unmapped } = await runWith([
			{ kind: "steer", message: userMessage("steer early") },
		]);
		expect(agent.steers).toEqual([]);
		expect(unmapped).toHaveLength(1);
		expect(unmapped[0].eventType).toBe("control:steer");
	});
});

describe("CompassAgent — replay applies to context, never as live input", () => {
	test("replay → session.agent.appendMessage(message), never prompt", async () => {
		const msg = userMessage("prior transcript turn");
		const { agent } = await runWith([{ kind: "replay", message: msg }]);
		expect(agent.appended).toEqual([msg]);
		expect(agent.prompts).toEqual([]);
	});
});

describe("CompassAgent — barrier lifts on ReplayComplete", () => {
	test("prompt after ReplayComplete → session.agent.prompt(input), not surfaced", async () => {
		const { agent, unmapped } = await runWith([
			{ kind: "replayComplete" },
			{ kind: "prompt", input: "now run" },
		]);
		expect(agent.prompts).toEqual(["now run"]);
		expect(unmapped).toEqual([]);
	});

	test("steer after ReplayComplete → session.agent.steer(message), not surfaced", async () => {
		const msg = userMessage("steer live");
		const { agent, unmapped } = await runWith([
			{ kind: "replayComplete" },
			{ kind: "steer", message: msg },
		]);
		expect(agent.steers).toEqual([msg]);
		expect(unmapped).toEqual([]);
	});

	test("the same prompt is refused before and forwarded after — the barrier is the only difference", async () => {
		// One prompt before, one after: exactly one reaches the SDK. A broken
		// barrier either forwards both (0 unmapped, 2 prompts) or neither.
		const { agent, unmapped } = await runWith([
			{ kind: "prompt", input: "early" },
			{ kind: "replayComplete" },
			{ kind: "prompt", input: "late" },
		]);
		expect(agent.prompts).toEqual(["late"]);
		expect(unmapped.map((u) => u.eventType)).toEqual(["control:prompt"]);
	});
});

describe("CompassAgent — ask_answer is staged, never delivered to the SDK (SEA-1310)", () => {
	// The frozen 6th AgentControl variant. Both arms surface a counted unmapped
	// op and drive NO SDK action: pre-barrier it is refused like prompt/steer;
	// post-barrier it is STAGED (wiring the answer needs the SEA-1310 correlation
	// key). The two reasons distinguish the arms so a regression that collapses
	// them — or that starts delivering the answer to the SDK — reddens here.
	test("ask_answer before ReplayComplete is refused by the barrier and surfaced, never delivered", async () => {
		const { agent, unmapped } = await runWith([
			{
				kind: "askAnswer",
				askId: "a-1",
				answers: [
					create(AskQuestionAnswerSchema, {
						questionId: "q-1",
						chosenOptionIds: ["opt-1"],
					}),
				],
			},
		]);
		// No SDK action for this frame, on any drive path.
		expect(agent.prompts).toEqual([]);
		expect(agent.steers).toEqual([]);
		expect(agent.appended).toEqual([]);
		expect(unmapped).toHaveLength(1);
		const refused = unmapped[0];
		expect(refused.eventType).toBe("control:ask_answer");
		expect(refused.reason).toBe(
			"live ask_answer arrived before ReplayComplete — refused by replay barrier",
		);
	});

	test("ask_answer after ReplayComplete is staged (awaiting SEA-1310) and surfaced, still not delivered", async () => {
		const { agent, unmapped } = await runWith([
			{ kind: "replayComplete" },
			{
				kind: "askAnswer",
				askId: "a-1",
				answers: [
					create(AskQuestionAnswerSchema, {
						questionId: "q-1",
						chosenOptionIds: ["opt-1", "opt-2"],
					}),
				],
			},
		]);
		// The barrier lifted, yet the answer is NOT wired into the SDK — it is
		// staged, so still no prompt/steer/append for this frame.
		expect(agent.prompts).toEqual([]);
		expect(agent.steers).toEqual([]);
		expect(agent.appended).toEqual([]);
		expect(unmapped).toHaveLength(1);
		const staged = unmapped[0];
		expect(staged.eventType).toBe("control:ask_answer");
		expect(staged.reason).toBe(
			"ask_answer delivery staged — awaiting SEA-1310 ask correlation key",
		);
	});
});

describe("CompassAgent — config applies to the SDK independent of the barrier", () => {
	test("config systemPrompt → setSystemPrompt, without touching tools", async () => {
		const { agent } = await runWith([
			{ kind: "config", systemPrompt: ["be terse"] },
		]);
		expect(agent.systemPrompts).toEqual([["be terse"]]);
		expect(agent.toolSets).toEqual([]);
	});

	test("config tools → setTools, without touching the system prompt", async () => {
		const tools = [tool("read"), tool("write")];
		const { agent } = await runWith([{ kind: "config", tools }]);
		expect(agent.toolSets).toHaveLength(1);
		expect(names(agent.toolSets[0])).toEqual(["read", "write"]);
		expect(agent.systemPrompts).toEqual([]);
	});

	test("config applies before ReplayComplete (config is not gated by the barrier)", async () => {
		const { agent, unmapped } = await runWith([
			{ kind: "config", systemPrompt: ["early cfg"] },
		]);
		expect(agent.systemPrompts).toEqual([["early cfg"]]);
		expect(unmapped).toEqual([]);
	});
});

// The natives the container entrypoint builds the session with are not a
// grantable capability — a config control may add and reorder tools, but a tool
// it omits must survive. Anything else lets a control frame silently revoke an
// agent's ability to speak, mid-session, with no error and no log.
describe("CompassAgent — construction-time native tools survive every config control", () => {
	test("a config omitting a native still reaches setTools carrying it", async () => {
		const { agent } = await runWith(
			[{ kind: "config", tools: [tool("read")] }],
			[tool("cotal_send")],
		);
		expect(names(agent.toolSets[0])).toEqual(["read", "cotal_send"]);
	});

	test("a native the control already lists is not duplicated, and the control's instance wins", async () => {
		const controlSend = tool("cotal_send");
		const nativeSend = tool("cotal_send");
		const { agent } = await runWith(
			[{ kind: "config", tools: [controlSend] }],
			[nativeSend],
		);
		expect(names(agent.toolSets[0])).toEqual(["cotal_send"]);
		expect(agent.toolSets[0]?.[0]).toBe(controlSend);
	});

	test("a control may add tools and set their order; natives follow", async () => {
		const { agent } = await runWith(
			[{ kind: "config", tools: [tool("write"), tool("read")] }],
			[tool("cotal_send"), tool("cotal_dm")],
		);
		expect(names(agent.toolSets[0])).toEqual([
			"write",
			"read",
			"cotal_send",
			"cotal_dm",
		]);
	});

	test("with no natives the control's own array reaches setTools untouched", async () => {
		const tools = [tool("read")];
		const { agent } = await runWith([{ kind: "config", tools }]);
		expect(agent.toolSets[0]).toBe(tools);
	});

	test("a config carrying only a systemPrompt never calls setTools, natives or not", async () => {
		const { agent } = await runWith(
			[{ kind: "config", systemPrompt: ["be terse"] }],
			[tool("cotal_send")],
		);
		expect(agent.toolSets).toEqual([]);
	});

	test("the native snapshot is a copy: a later setTools cannot mutate it away", async () => {
		const natives = [tool("cotal_send")];
		const { agent } = await runWith(
			[
				{ kind: "config", tools: [tool("read")] },
				{ kind: "config", tools: [tool("write")] },
			],
			natives,
		);
		expect(names(agent.toolSets[0])).toEqual(["read", "cotal_send"]);
		expect(names(agent.toolSets[1])).toEqual(["write", "cotal_send"]);
	});
});

describe("CompassAgent — terminal status distinguishes failure from clean stop (compass.proto:141)", () => {
	// AgentSessionState.ERRORED (5) is DISTINCT from STOPPED (4): ERRORED marks
	// "an unexpected agent exit (OOM, panic, engine restart)". A crash during the
	// run must terminate in ERRORED and still propagate; only a clean
	// control-stream close terminates in STOPPED.
	test("a control-loop exception terminates in ERRORED and propagates", async () => {
		const boom = new Error("control-loop boom");
		const throwing: ControlSource = {
			async *[Symbol.asyncIterator]() {
				yield { kind: "replayComplete" };
				throw boom;
			},
		};
		const { frames, error } = await runWithSource(throwing);
		// The error must propagate — a swallowed crash is a silent-failure bug.
		expect(error).toBe(boom);
		// The last session frame's state distinguishes the crash from a clean stop.
		const states = frames.flatMap((f) =>
			f.kind === "session" ? [f.value.state] : [],
		);
		expect(states.at(-1)).toBe(AgentSessionState.ERRORED);
	});

	test("a clean control-stream close terminates in STOPPED", async () => {
		// The clean path: control ends without error → STOPPED, run() resolves.
		// Locks the happy path against a regression that would emit ERRORED here.
		const { frames } = await runWith([]);
		const states = frames.flatMap((f) =>
			f.kind === "session" ? [f.value.state] : [],
		);
		expect(states.at(-1)).toBe(AgentSessionState.STOPPED);
	});
});
