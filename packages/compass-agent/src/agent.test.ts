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
import {
	AgentBusyError,
	type AgentMessage,
	type AgentTool,
} from "@oh-my-pi/pi-agent-core";
import type {
	AgentSession,
	AgentSessionEvent,
	AgentSessionEventListener,
} from "@oh-my-pi/pi-coding-agent";
import { CompassAgent, formatDeliversForPrompt } from "./agent";
import {
	AgentSessionState,
	AskQuestionAnswerSchema,
	AskQuestionSchema,
	AskSchema,
	create,
	type Message,
	MessageBlockSchema,
	MessageSchema,
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
	// test can assert the snapshot is a copy and not this array. `isStreaming`
	// mirrors `Agent.state.isStreaming` (pi-agent-core agent.ts:1072) — mutable so
	// a test can model the control-prompt spin-up window (streaming true before
	// any agent_start event) that the idle-deliver race hinges on.
	readonly state: { tools: AgentTool[]; isStreaming: boolean };
	// How many times CompassAgent.steer woke an idle turn via `agent.continue()`.
	// A mid-turn steer must leave this at 0 (it interrupts the running loop, never
	// starts a turn); an idle steer bumps it to 1 (it wakes a turn to drain the
	// injected steer).
	continueCount: number;
	// Forces the next `continue()` to reject with the empty-history Error
	// ("No messages to continue from"), INDEPENDENT of `state.isStreaming`. Models
	// the genuinely reachable idle-steer rejection: `CompassAgent.steer` is fully
	// synchronous from the idle gate to the `continue()` call, so `isStreaming`
	// cannot change between them and an AgentBusyError spin-up race CANNOT fire on
	// the idle path (pi-agent-core agent.ts:1029 vs :1035). The reachable
	// synchronous rejection is `continue`'s empty-history throw when the inner
	// agent reached ReplayComplete with zero replayed messages. Setting
	// `state.isStreaming` true instead would flip the idle gate to the mid-turn
	// path, so the rejection belt (idle-only) would never run — hence a dedicated
	// trigger.
	continueRejectsEmptyHistory: boolean;
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
	// The authoritative "a turn is live" signal CompassAgent's idle-flush gate
	// consults (pi-coding-agent agent-session.ts:6469). Here it reflects the
	// inner agent's `state.isStreaming` (minus the in-flight count the real getter
	// also folds in — irrelevant to this fake's synchronous drive).
	readonly isStreaming: boolean;
}

function recordingSession(natives: AgentTool[] = []): RecordingSession {
	const agent: RecordingAgent = {
		prompts: [],
		steers: [],
		appended: [],
		systemPrompts: [],
		toolSets: [],
		state: { tools: natives, isStreaming: false },
		continueCount: 0,
		continueRejectsEmptyHistory: false,
	};
	const agentImpl = {
		prompt(input: string): Promise<void> {
			// Mirror the real `Agent.prompt` guard (pi-agent-core agent.ts:985): a
			// prompt issued while already streaming is refused with AgentBusyError,
			// as a promise REJECTION (prompt is async — it never throws sync). This
			// is what lets a test reproduce the injection-refused case the flush
			// belt must survive, using the real error shape.
			if (agent.state.isStreaming) {
				return Promise.reject(new AgentBusyError());
			}
			agent.prompts.push(input);
			return Promise.resolve();
		},
		steer(m: AgentMessage): void {
			// `agent.steers` models the inner Agent's live steering queue (LIFO):
			// `steer` pushes (pi-agent-core agent.ts:864) and `popLastSteer` pops
			// the tail (:942-943). Keeping it live — not append-only — is what lets
			// a test observe the rollback removing exactly the orphaned steer.
			agent.steers.push(m);
		},
		popLastSteer(): AgentMessage | undefined {
			// Mirror the real LIFO pop (pi-agent-core agent.ts:942-943): remove and
			// return the last-enqueued steer. The rejection belt calls this to roll
			// back the steer it pre-pushed when the idle `continue()` rejected.
			return agent.steers.pop();
		},
		continue(): Promise<void> {
			// Mirror the real `Agent.continue()` contract minimally (pi-agent-core
			// agent.ts:1028-1036): it resumes a turn to drain queued (steering)
			// messages. Two rejection shapes it can surface as a promise REJECTION:
			// an AgentBusyError if already streaming (:1029) — unreachable on the
			// idle path but kept for fidelity — and the empty-history Error
			// ("No messages to continue from", :1035) when the inner agent reached
			// ReplayComplete with zero replayed messages. The empty-history throw is
			// the reachable idle-steer rejection the belt must survive; it fires
			// BEFORE any steering dequeue (:1038), so the pre-pushed steer is still
			// at the queue tail when the belt's `popLastSteer` rolls it back.
			// Otherwise it records the wake and resolves; the drain of the steering
			// queue itself is the inner loop's job, so the fake models the resolving
			// `continue` as a no-op beyond the count.
			agent.continueCount++;
			if (agent.state.isStreaming) {
				return Promise.reject(new AgentBusyError());
			}
			if (agent.continueRejectsEmptyHistory) {
				return Promise.reject(new Error("No messages to continue from"));
			}
			return Promise.resolve();
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
		get isStreaming(): boolean {
			return agent.state.isStreaming;
		},
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
		sink: {
			emit: (f) => {
				frames.push(f);
			},
			emitDurable: (f) => {
				frames.push(f);
				return Promise.resolve();
			},
		},
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
			sink: {
				emit: (f) => {
					frames.push(f);
				},
				emitDurable: (f) => {
					frames.push(f);
					return Promise.resolve();
				},
			},
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

	test("a native the control lists (same instance) is not duplicated; the native survives", async () => {
		const nativeSend = tool("cotal_send");
		const { agent } = await runWith(
			[{ kind: "config", tools: [nativeSend] }],
			[nativeSend],
		);
		expect(names(agent.toolSets[0])).toEqual(["cotal_send"]);
		expect(agent.toolSets[0]?.[0]).toBe(nativeSend);
	});

	test("a native wins over a same-named control tool: the native instance replaces it at its position", async () => {
		const controlSend = tool("cotal_send");
		const nativeSend = tool("cotal_send");
		const { agent, unmapped } = await runWith(
			[{ kind: "config", tools: [tool("read"), controlSend, tool("write")] }],
			[nativeSend],
		);
		// Control ordering preserved; the collided slot keeps its index but holds
		// the NATIVE instance, not the control's.
		expect(names(agent.toolSets[0])).toEqual(["read", "cotal_send", "write"]);
		expect(agent.toolSets[0]?.[1]).toBe(nativeSend);
		// The substitution attempt is surfaced as a rejected server misconfig.
		expect(unmapped).toHaveLength(1);
		expect(unmapped[0]?.eventType).toBe("control:config");
		expect(unmapped[0]?.reason).toContain("cotal_send");
	});

	test("re-supplying the exact native instance emits no unmapped event", async () => {
		const nativeSend = tool("cotal_send");
		const { unmapped } = await runWith(
			[{ kind: "config", tools: [nativeSend] }],
			[nativeSend],
		);
		expect(unmapped).toEqual([]);
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

	test("the native snapshot is a copy: mutating the live state.tools in place after construction cannot alter the native set", async () => {
		// Split construction from run so we can mutate the caller-owned
		// `state.tools` array AFTER the constructor snapshots it but BEFORE a
		// config control merges natives. The snapshot is a copy (agent.ts), so
		// the in-place mutation below must not leak through: the native still
		// merges from the construction-time set, and the injected tool never
		// appears. This FAILS if the defensive spread at construction is dropped.
		const nativeSend = tool("cotal_send");
		const session = recordingSession([nativeSend]);
		const frames: OutboundFrame[] = [];
		const unmapped: UnmappedEvent[] = [];
		const control: ControlSource = {
			async *[Symbol.asyncIterator]() {
				yield { kind: "config", tools: [tool("read")] } as AgentControl;
			},
		};
		const agent = new CompassAgent({
			session: session as unknown as AgentSession,
			sink: {
				emit: (f) => {
					frames.push(f);
				},
				emitDurable: (f) => {
					frames.push(f);
					return Promise.resolve();
				},
			},
			control,
			onUnmapped: (u) => unmapped.push(u),
		});
		// Mutate the live source array in place: truncate it and inject an evil
		// tool. If the snapshot aliased this array, the native would vanish and
		// "evil" would merge instead.
		session.agent.state.tools.length = 0;
		session.agent.state.tools.push(tool("evil"));
		await agent.run();
		expect(names(session.agent.toolSets[0])).toEqual(["read", "cotal_send"]);
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

// ---------------------------------------------------------------------------
// SEA-1310 §8 — RT-3 turn-end delivery (DELIVER arm).
//
// deliver() rides the immediate handle (not the control script), so these tests
// construct CompassAgent directly and call `agent.deliver(msg)`, driving turn
// edges through the recorded `session.listener`. run() is started (not awaited)
// so the subscribe listener is registered; a held-open control source keeps the
// run loop parked until the test closes it, so nothing races the assertions.

// A comms Message fixture: id + a single text block. The id is load-bearing
// (dedup + ack key); the text is what the coalesced prompt must contain.
function deliverMsg(id: string, text: string): Message {
	return create(MessageSchema, {
		id,
		blocks: [
			create(MessageBlockSchema, { block: { case: "text", value: text } }),
		],
	});
}

// Start a CompassAgent with the recording harness and a held-open control
// source, so `run()` registers the turn-tracking listener but never terminates
// on its own. Returns the agent, the captured frames/unmapped, a `drive` to
// push session turn edges, and a `close` that ends the run loop cleanly.
function startDeliverAgent(natives: AgentTool[] = []) {
	const session = recordingSession(natives);
	const frames: OutboundFrame[] = [];
	const unmapped: UnmappedEvent[] = [];
	let releaseControl!: () => void;
	const controlClosed = new Promise<void>((resolve) => {
		releaseControl = resolve;
	});
	// A held-open control source: it yields NO ops and resolves (ends the
	// iterable) only when `close()` releases it, so run() registers the turn
	// listener but the control loop parks until the test is done. Expressed as an
	// async iterator whose `next()` resolves once (to done) after the release —
	// no `yield`, mirroring `dropsImmediately` in control-source.test.ts.
	const control: ControlSource = {
		[Symbol.asyncIterator]() {
			return {
				async next(): Promise<IteratorResult<AgentControl>> {
					await controlClosed;
					return { done: true, value: undefined };
				},
			};
		},
	};
	const agent = new CompassAgent({
		session: session as unknown as AgentSession,
		sink: {
			emit: (f) => {
				frames.push(f);
			},
			emitDurable: (f) => {
				frames.push(f);
				return Promise.resolve();
			},
		},
		control,
		onUnmapped: (u) => unmapped.push(u),
	});
	const done = agent.run();
	const drive = (event: AgentSessionEvent): void => {
		session.listener?.(event);
	};
	const close = async (): Promise<void> => {
		releaseControl();
		await done;
	};
	return { agent, session, frames, unmapped, drive, close };
}

// The delivery-ack frames captured, in order, with their message ids.
function ackIds(frames: OutboundFrame[]): string[] {
	return frames.flatMap((f) =>
		f.kind === "deliveryAck" ? [f.value.messageId] : [],
	);
}

// The `content` string of a recorded steer AgentMessage. CompassAgent.steer
// injects a UserMessage ({ role:"user", content: <formatted text> }), but the
// recorded type is the wide AgentMessage union (whose custom arms — e.g.
// BranchSummaryMessage — have no `content`), so a test asserting on the injected
// text must narrow first. Fails loud if the recorded steer is not the expected
// user-message shape rather than silently reading undefined.
function steerContent(m: AgentMessage): string {
	if (!("content" in m) || typeof m.content !== "string") {
		throw new Error("recorded steer is not a string-content user message");
	}
	return m.content;
}

// Drain the microtask queue: the delivery ack is emitted on the microtask right
// after `#flushDelivers` issues its prompt (the injection-accepted point — see
// the method comment in agent.ts), so a test asserting on acks must let that
// microtask run first. Awaiting a resolved promise yields one microtask turn,
// which is enough: the flush's ack microtask and any settled-rejection `.catch`
// were both scheduled synchronously by the `deliver`/`agent_end` call, ahead of
// this await.
async function tick(): Promise<void> {
	await Promise.resolve();
}

describe("CompassAgent — RT-3 turn-end delivery (SEA-1310 §8 deliver arm)", () => {
	test("mid-turn delivers coalesce into ONE turn-end prompt", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "hello one"));
		h.agent.deliver(deliverMsg("m2", "hello two"));
		// No prompt while the turn is active — the deliveries are queued.
		expect(h.session.agent.prompts).toEqual([]);
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		// Exactly one prompt, coalescing both messages' text.
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("hello one");
		expect(h.session.agent.prompts[0]).toContain("hello two");
		await h.close();
	});

	test("an idle deliver starts a turn immediately", async () => {
		const h = startDeliverAgent();
		// No active turn: the deliver flushes at once.
		h.agent.deliver(deliverMsg("m1", "urgent"));
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("urgent");
		await h.close();
	});

	test("one ack per injected message, carrying its message_id", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "one"));
		h.agent.deliver(deliverMsg("m2", "two"));
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		// The ack is emitted at injection time — the microtask after the flush
		// issues its prompt (ack-at-injection, gated on injection-accepted).
		await tick();
		// Two acks, one per injected message, in delivery order.
		expect(ackIds(h.frames)).toEqual(["m1", "m2"]);
		await h.close();
	});

	test("a duplicate deliver of an ALREADY-INJECTED message re-acks (cursor recovery for a lost priority-lane ack)", async () => {
		const h = startDeliverAgent();
		// First deliver flushes immediately (idle) and injects m1.
		h.agent.deliver(deliverMsg("m1", "once"));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["m1"]);
		// The SAME id, redelivered after its ack was lost on the "never-drop"
		// PRIORITY lane (its retry budget exhausted on a >~1s socket outage, or a
		// Runner restart mid-flush — publish-spine.ts:156-158). m1 is ALREADY
		// injected (not in #deliverQueue), so the dedup-drop path RE-ACKS to
		// recover the stranded Server delivery cursor, and does NOT re-inject.
		h.agent.deliver(deliverMsg("m1", "once"));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		// A SECOND ack for m1 — the guarded re-ack (frozen design.md:405-406 "the
		// dedup absorbs the lost ack"; :338 a duplicate ack is a no-op on the
		// Server). Non-vacuity: revert the re-ack → this goes ["m1"] vs ["m1","m1"].
		expect(ackIds(h.frames)).toEqual(["m1", "m1"]);
		const dup = h.unmapped.find(
			(u) => u.eventType === "deliver" && u.reason.includes("duplicate"),
		);
		expect(dup).toBeDefined();
		await h.close();
	});

	test("a duplicate deliver of a STILL-QUEUED (not-yet-injected) message does NOT re-ack, and acks once at turn end", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		// Mid-turn: m2 queues (not flushed → not injected → not acked).
		h.agent.deliver(deliverMsg("m2", "pending"));
		// Redelivered BEFORE the turn-end flush: m2 is still in #deliverQueue, so
		// the queue-membership guard holds — no re-ack for a message that is NOT
		// yet injected ("ack means injected", agent.ts:257-262). Acking it here
		// then losing it to a crash-before-flush would strand it the other way.
		h.agent.deliver(deliverMsg("m2", "pending"));
		await tick();
		// No ack for the still-queued duplicate. Non-vacuity: drop the
		// #deliverQueue-membership guard (re-ack unconditionally) → this reddens
		// (a not-yet-injected message gets acked).
		expect(ackIds(h.frames)).toEqual([]);
		// Turn ends → the queued m2 flushes, injects once, and is acked EXACTLY
		// once.
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(ackIds(h.frames)).toEqual(["m2"]);
		await h.close();
	});

	test("a crash between enqueue and injection re-receives on reconnect (no ack was emitted)", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		// Enqueued while the turn is active — NOT flushed, so NOT injected.
		h.agent.deliver(deliverMsg("m1", "pending"));
		// No prompt and, crucially, NO ack yet: the ack is emitted at injection,
		// so an unflushed message leaves no receipt and the Server redelivers it.
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		await h.close();
	});

	test("a deliver with empty Message.id is counted-unmapped, with no ack and no injection", async () => {
		const h = startDeliverAgent();
		h.agent.deliver(deliverMsg("", "no id"));
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		const missing = h.unmapped.find(
			(u) => u.eventType === "deliver" && u.reason.includes("missing"),
		);
		expect(missing).toBeDefined();
		await h.close();
	});

	// The high-severity race (SEA-1310 §8): a control-driven prompt sets the inner
	// agent streaming SYNCHRONOUSLY (pi-agent-core agent.ts:1072) but flips
	// `#turnActive` only later, off the async `agent_start` event. A deliver that
	// lands in that window must NOT be flushed — flushing would inject into a
	// streaming agent, the prompt would reject (AgentBusyError), and the message
	// would be acked-and-dropped: a false receipt for a never-injected message,
	// the exact frozen-contract violation this gate closes. Modeled by setting the
	// fake's `state.isStreaming = true` WITHOUT firing `agent_start`.
	test("a deliver during the control-prompt spin-up window is queued, not acked-and-dropped", async () => {
		const h = startDeliverAgent();
		// A control prompt has spun up the inner agent: streaming true, but no
		// agent_start event has propagated, so #turnActive is still false.
		h.session.agent.state.isStreaming = true;
		h.agent.deliver(deliverMsg("m1", "channel msg"));
		await tick();
		// The message rode neither an injection nor an ack — it stayed queued.
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		// The turn settles: streaming clears (as the inner loop clears it at
		// agent_end, agent.ts:1254) and the agent_end edge flushes the queue once.
		h.session.agent.state.isStreaming = false;
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("channel msg");
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	// Rejection-safety belt (SEA-1310 §8): if a flush's prompt is REFUSED (the
	// only prompt-rejection shape — a not-injected batch, see #flushDelivers), the
	// batch must not be acked (no false receipt) and its ids must leave the
	// processed set so the Server's redelivery re-injects them. Forced by driving
	// the agent_end flush while the fake is still streaming, so its prompt guard
	// rejects with AgentBusyError.
	test("a refused flush prompt emits no ack, un-dedups the batch, and surfaces it", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "refused"));
		// The agent is (pathologically) still streaming when agent_end fires, so
		// the flush's prompt is refused. #turnActive is cleared by the edge, so the
		// flush is attempted.
		h.session.agent.state.isStreaming = true;
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		// No injection recorded (the guard refused it), and crucially NO ack.
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		// The refusal is surfaced, never a silent drop.
		const refused = h.unmapped.find(
			(u) =>
				u.eventType === "deliver:prompt" && u.reason.includes("not injected"),
		);
		expect(refused).toBeDefined();
		// The id left the processed set: the Server's redelivery (streaming now
		// clear) is NOT deduped away — it injects exactly once.
		h.session.agent.state.isStreaming = false;
		h.agent.deliver(deliverMsg("m1", "refused"));
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("refused");
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	// Multi-turn coalescing: each turn flushes only the delivers queued for it —
	// the queue does not leak a prior turn's batch into the next, and acks
	// accumulate one per injected message across turns.
	test("delivers coalesce per turn across multiple turns", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "first turn"));
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("first turn");
		// A second turn's deliver flushes on its own agent_end — not carrying the
		// first turn's already-flushed message.
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m2", "second turn"));
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		expect(h.session.agent.prompts).toHaveLength(2);
		expect(h.session.agent.prompts[1]).toContain("second turn");
		expect(h.session.agent.prompts[1]).not.toContain("first turn");
		expect(ackIds(h.frames)).toEqual(["m1", "m2"]);
		await h.close();
	});

	// Ask-only message: a delivered Message whose only block is an `ask` (no text)
	// still injects (its empty slot rides the coalesced prompt) and is acked once
	// by id — deliver carries a receipt per Message, independent of block kind.
	test("an ask-only message is injected and acked exactly once", async () => {
		const h = startDeliverAgent();
		const askOnly = create(MessageSchema, {
			id: "ask1",
			blocks: [
				create(MessageBlockSchema, {
					block: {
						case: "ask",
						value: create(AskSchema, {
							askId: "ask1-ask",
							questions: [
								create(AskQuestionSchema, {
									questionId: "q1",
									question: "proceed?",
								}),
							],
						}),
					},
				}),
			],
		});
		h.agent.deliver(askOnly);
		await tick();
		// It flushed idle: exactly one prompt (its empty text slot) and one ack.
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(ackIds(h.frames)).toEqual(["ask1"]);
		await h.close();
	});
});

// ---------------------------------------------------------------------------
// SEA-1310 §8 — channel-borne steer arm.
//
// steer() rides the same immediate handle deliver does (not the control script),
// so these tests reuse startDeliverAgent()/deliverMsg()/ackIds()/tick() and call
// `agent.steer(msg)`. Unlike deliver, a steer is an @-mention interrupt: mid-turn
// it injects via `session.agent.steer` (drained by the running loop, no turn
// started); idle it injects AND wakes a turn via `session.agent.continue()` to
// drain the injected steer (compass-0.6 :399-408).
describe("CompassAgent — channel-borne steer (SEA-1310 §8 steer arm)", () => {
	test("a mid-turn steer injects via session.agent.steer and does NOT start a turn", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.steer(deliverMsg("s1", "hey"));
		// Injected onto the steering queue (the running loop drains it), and NO
		// turn was woken — a live turn interrupts in place. Non-vacuity: drop the
		// `session.agent.steer` inject → steers is empty.
		expect(h.session.agent.steers).toHaveLength(1);
		expect(steerContent(h.session.agent.steers[0] as AgentMessage)).toContain(
			"hey",
		);
		expect(h.session.agent.continueCount).toBe(0);
		// The ack (means "injected") rides the next microtask.
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		await h.close();
	});

	test("an idle steer injects AND starts a turn via continue", async () => {
		const h = startDeliverAgent();
		// No agent_start: the session is idle, so the steer wakes a turn to drain
		// the injected steer.
		h.agent.steer(deliverMsg("s1", "hey"));
		expect(h.session.agent.steers).toHaveLength(1);
		expect(steerContent(h.session.agent.steers[0] as AgentMessage)).toContain(
			"hey",
		);
		// Non-vacuity: drop the idle `continue()` → continueCount stays 0.
		expect(h.session.agent.continueCount).toBe(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		await h.close();
	});

	test("one ack per injected steer, carrying its message_id", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.steer(deliverMsg("s1", "one"));
		await tick();
		// One ack, carrying the steer's id. Non-vacuity: drop the mid-turn ack
		// microtask → this goes [] vs ["s1"].
		expect(ackIds(h.frames)).toEqual(["s1"]);
		await h.close();
	});

	test("a duplicate steer of an already-injected message re-acks and does NOT re-inject", async () => {
		const h = startDeliverAgent();
		// First steer (idle) injects s1 and acks it.
		h.agent.steer(deliverMsg("s1", "once"));
		expect(h.session.agent.steers).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		// The SAME id, redelivered after its priority-lane ack was lost. s1 is
		// already injected (steer is never queued), so the dedup path RE-ACKS to
		// recover the Server cursor and does NOT re-inject. Non-vacuity: drop the
		// dedup guard → steers length goes to 2 (re-inject).
		h.agent.steer(deliverMsg("s1", "once"));
		expect(h.session.agent.steers).toHaveLength(1);
		await tick();
		// A SECOND ack for s1 — the guarded re-ack. Non-vacuity: drop the re-ack →
		// ["s1"] vs ["s1","s1"].
		expect(ackIds(h.frames)).toEqual(["s1", "s1"]);
		const dup = h.unmapped.find(
			(u) => u.eventType === "steer" && u.reason.includes("duplicate"),
		);
		expect(dup).toBeDefined();
		await h.close();
	});

	test("a steer with empty Message.id is counted-unmapped, with no ack and no injection", async () => {
		const h = startDeliverAgent();
		h.agent.steer(deliverMsg("", "no id"));
		// No inject, no wake, no ack. Non-vacuity: drop the empty-id guard → the
		// steer injects (steers length 1) and acks.
		expect(h.session.agent.steers).toEqual([]);
		expect(h.session.agent.continueCount).toBe(0);
		expect(ackIds(h.frames)).toEqual([]);
		const missing = h.unmapped.find(
			(u) => u.eventType === "steer" && u.reason.includes("missing Message.id"),
		);
		expect(missing).toBeDefined();
		await h.close();
	});

	// Idle-steer rejection belt (MEDIUM-1): an idle steer PRE-PUSHES onto the
	// inner steering queue, then wakes a turn with `continue()`, which can REJECT.
	// The reachable idle-path rejection is `continue`'s synchronous empty-history
	// throw ("No messages to continue from") when the inner agent reached
	// ReplayComplete with zero replayed messages — NOT an AgentBusyError spin-up
	// race, which cannot fire because steer() is synchronous from the idle gate to
	// the call. On rejection the turn did not start: the belt rolls back the
	// pre-pushed steer (popLastSteer, LIFO), un-dedups the id, and emits no ack
	// (no false receipt), so the Server's redelivery re-injects EXACTLY ONE copy.
	// Modeled by `continueRejectsEmptyHistory` (not `state.isStreaming`, which
	// would flip the idle gate to the mid-turn path and skip the belt entirely).
	test("an idle continue rejection rolls back the orphan, un-dedups the id, and emits no ack; redelivery re-injects exactly one copy", async () => {
		const h = startDeliverAgent();
		h.session.agent.continueRejectsEmptyHistory = true;
		h.agent.steer(deliverMsg("s1", "refused"));
		await tick();
		// The wake refused — so NO ack, and the pre-pushed orphan was rolled back
		// off the inner steering queue (nothing left to be drained later).
		// Non-vacuity: drop the `popLastSteer` rollback → this steer stays on the
		// queue and the redelivery below leaves it at length 2.
		expect(ackIds(h.frames)).toEqual([]);
		expect(h.session.agent.steers).toHaveLength(0);
		// The refusal is surfaced, never a silent drop.
		const refused = h.unmapped.find(
			(u) =>
				u.eventType === "steer:continue" && u.reason.includes("not injected"),
		);
		expect(refused).toBeDefined();
		// The id left the processed set: the Server's redelivery of the same id
		// injects again (not deduped away), and this time the wake succeeds. The
		// inner steering queue holds EXACTLY ONE copy — the rollback prevented the
		// double-inject the orphan would have caused.
		h.session.agent.continueRejectsEmptyHistory = false;
		h.agent.steer(deliverMsg("s1", "refused"));
		expect(h.session.agent.steers).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		await h.close();
	});

	// Cross-type re-ack guard (MEDIUM-2): `#processedMessageIds` is SHARED between
	// the deliver and steer arms, and an id can cross-arrive as the other type. A
	// steer duplicate of an id still pending in `#deliverQueue` (queued mid-turn by
	// deliver, NOT yet injected) must NOT be re-acked — "ack means injected", and
	// acking a still-queued message then losing it to a crash-before-flush would
	// strand it. Mirrors the deliver duplicate-of-a-queued-message test.
	test("a steer duplicate of a still-QUEUED deliver is not re-acked (only the queued copy acks, when it flushes)", async () => {
		const h = startDeliverAgent();
		// A turn is live, so a deliver of X is QUEUED (not flushed) — X enters
		// #processedMessageIds AND stays in #deliverQueue awaiting agent_end.
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("x1", "queued"));
		expect(h.session.agent.prompts).toEqual([]);
		// Now a steer of the SAME id arrives (cross-type sweep). It is a duplicate
		// (id already processed) BUT X is still queued/un-injected, so NO ack is
		// emitted — only the "duplicate steer" unmapped surface. Non-vacuity: drop
		// the #deliverQueue-membership guard (re-ack unconditionally) → this reddens
		// (a not-yet-injected message gets acked).
		h.agent.steer(deliverMsg("x1", "dup"));
		await tick();
		expect(ackIds(h.frames)).toEqual([]);
		const dup = h.unmapped.find(
			(u) => u.eventType === "steer" && u.reason.includes("duplicate"),
		);
		expect(dup).toBeDefined();
		// The steer did NOT inject (it is a duplicate) — the queued deliver is the
		// single live copy.
		expect(h.session.agent.steers).toEqual([]);
		// The turn ends → the queued x1 flushes, injects once, and is acked EXACTLY
		// once — the deferred ack the guard protected.
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["x1"]);
		await h.close();
	});
});

describe("formatDeliversForPrompt — coalescing format (SEA-1310 §8)", () => {
	test("renders each message's text, in order, into one stable string", () => {
		const batch = [deliverMsg("m1", "first"), deliverMsg("m2", "second")];
		const out = formatDeliversForPrompt(batch);
		expect(out).toContain("first");
		expect(out).toContain("second");
		// Order is preserved: first appears before second.
		expect(out.indexOf("first")).toBeLessThan(out.indexOf("second"));
	});

	test("concatenates multiple text blocks and ignores ask blocks", () => {
		const msg = create(MessageSchema, {
			id: "m1",
			blocks: [
				create(MessageBlockSchema, { block: { case: "text", value: "alpha" } }),
				create(MessageBlockSchema, { block: { case: "text", value: "beta" } }),
			],
		});
		const out = formatDeliversForPrompt([msg]);
		expect(out).toContain("alpha");
		expect(out).toContain("beta");
	});
});
