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

import { afterEach, describe, expect, test } from "bun:test";
import {
	AgentBusyError,
	type AgentIdentity,
	type AgentMessage,
	type AgentTool,
	type TelemetryHookContext,
	type TelemetrySpanKind,
} from "@oh-my-pi/pi-agent-core";
import type {
	AgentSession,
	AgentSessionEvent,
	AgentSessionEventListener,
} from "@oh-my-pi/pi-coding-agent";
import { context, type Span, trace } from "@opentelemetry/api";
import {
	InMemorySpanExporter,
	SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { NodeTracerProvider } from "@opentelemetry/sdk-trace-node";
import {
	CompassAgent,
	formatAskAnswerForPrompt,
	formatDeliversForPrompt,
	formatForgeNotifications,
} from "./agent";
import {
	AgentSessionState,
	type Ask,
	AskAnswerBlockSchema,
	AskOptionSchema,
	type AskQuestion,
	AskQuestionSchema,
	AskSchema,
	ChecksSummarySchema,
	CommentRefSchema,
	create,
	type ForgeNotification,
	ForgeNotificationKind,
	ForgeNotificationSchema,
	ForgeRefSchema,
	type Message,
	MessageBlockSchema,
	type MessageInitShape,
	MessageSchema,
	SessionInjectionKind,
} from "./compassv1";
import type { AgentControl, ControlSource } from "./control";
import type { OutboundFrame } from "./frame";
import type { UnmappedEvent } from "./mapping";
import { createTraceBridge, type TraceBridge } from "./trace-bridge";

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
	// Forces the next `prompt()` to reject with the "No model configured" Error
	// (pi-agent-core agent.ts:990), INDEPENDENT of `state.isStreaming`. Models the
	// one genuinely reachable idle-steer rejection: `CompassAgent.steer` starts an
	// idle turn via `prompt()` (not `continue()`), and prompt rejects only via the
	// already-streaming AgentBusyError (:986 — unreachable on the idle path, which
	// is synchronous from the idle gate so `isStreaming` cannot change) or the
	// no-model throw (:990, pre-injection). Setting `state.isStreaming` true
	// instead would flip the idle gate to the mid-turn path, so the idle-only
	// rejection belt would never run — hence a dedicated trigger.
	promptRejectsNoModel: boolean;
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
	// RIG-2644 — the strand-recovery re-check awaits this (agent.ts
	// #armStrandRecovery). Mirrors AgentSession.waitForIdle (agent-session.ts:6478):
	// resolves once streaming has settled. Deterministic, no timers: resolves at
	// once when already idle, else parks until `settleIdle()` releases it (the test
	// models the untracked probe clearing).
	waitForIdle(): Promise<void>;
	// Test control: release any parked waitForIdle — models a startup
	// probe/prewarm finishing with no agent_end on the stream. Default clears the
	// streaming flag too (fully idle). `keepStreaming: true` resolves the waiters
	// but LEAVES isStreaming true — faithfully modelling production's window where
	// AgentSession.waitForIdle (which awaits the inner agent, agent-session.ts:6478-6481)
	// resolves while `isStreaming` still reads true because a fresh probe holds
	// `#promptInFlightCount > 0` (the fold at :6469-6470). That window is what
	// makes the recovery re-arm branch fire.
	settleIdle(opts?: { keepStreaming?: boolean }): void;
}

function recordingSession(natives: AgentTool[] = []): RecordingSession {
	const agent: RecordingAgent = {
		prompts: [],
		steers: [],
		appended: [],
		systemPrompts: [],
		toolSets: [],
		state: { tools: natives, isStreaming: false },
		promptRejectsNoModel: false,
	};
	const agentImpl = {
		prompt(input: string): Promise<void> {
			// Mirror the real `Agent.prompt` refusal shapes (pi-agent-core agent.ts
			// :985-990), both surfaced as a promise REJECTION before any injection:
			// AgentBusyError if already streaming (:986), and the "No model
			// configured" throw (:990). The streaming guard reproduces the deliver
			// spin-up race; the no-model throw is the one reachable idle-STEER
			// rejection the steer belt must survive (the idle path starts its turn
			// via prompt(), so this is its injection-refused case).
			if (agent.state.isStreaming) {
				return Promise.reject(new AgentBusyError());
			}
			if (agent.promptRejectsNoModel) {
				return Promise.reject(new Error("No model configured"));
			}
			// Faithful to production: `Agent.prompt` sets `#state.isStreaming = true`
			// SYNCHRONOUSLY on the success path (pi-agent-core agent.ts:1072), AFTER
			// both refusal guards above (:985 busy, :990 no-model, which reject
			// BEFORE any injection and never flip streaming). The inner loop clears
			// it again at the `agent_end` edge (:1254) — modeled in the `drive`
			// helpers, which clear it before delivering an `agent_end` event, exactly
			// as production emits that edge with streaming already false. This is
			// what makes a SECOND synchronous `prompt()` on one turn-end edge collide
			// with AgentBusyError — the bug the single-prompt turn-end flush closes.
			agent.state.isStreaming = true;
			agent.prompts.push(input);
			return Promise.resolve();
		},
		steer(m: AgentMessage): void {
			// `agent.steers` models the inner Agent's live steering queue: `steer`
			// pushes (pi-agent-core agent.ts:864). Only the mid-turn steer arm
			// enqueues here (an interrupt drained by the running loop); the idle arm
			// starts a turn via prompt() and never enqueues.
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
	// Resolvers for waitForIdle calls parked while streaming (RIG-2644).
	const idleWaiters: (() => void)[] = [];
	const rec: RecordingSession = {
		agent,
		subscribed: 0,
		unsubscribed: 0,
		listener: undefined,
		get isStreaming(): boolean {
			return agent.state.isStreaming;
		},
		waitForIdle(): Promise<void> {
			if (!agent.state.isStreaming) return Promise.resolve();
			return new Promise<void>((resolve) => {
				idleWaiters.push(resolve);
			});
		},
		settleIdle(opts?: { keepStreaming?: boolean }): void {
			if (!opts?.keepStreaming) agent.state.isStreaming = false;
			const waiters = idleWaiters.splice(0);
			for (const w of waiters) w();
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

// An AskQuestion fixture: id + text + options + inline answer state (the
// chosen option ids / free-text the server records on RespondToAsk). Only the
// axes the answer formatter reads are load-bearing. Option ids are the
// zero-based-index strings Lane 1 mints ("0","1",…).
function askQuestion(
	id: string,
	text: string,
	{
		options = [],
		chosenOptionIds = [],
		customText = "",
	}: {
		options?: { id: string; label: string }[];
		chosenOptionIds?: string[];
		customText?: string;
	} = {},
): AskQuestion {
	return create(AskQuestionSchema, {
		questionId: id,
		question: text,
		options: options.map((o) =>
			create(AskOptionSchema, { id: o.id, label: o.label }),
		),
		chosenOptionIds,
		customText,
	});
}

// An answered `Ask` snapshot — the shape a delivered `ask_answer` block carries.
function answeredAsk(questions: AskQuestion[]): Ask {
	return create(AskSchema, { questions, answered: true });
}

// A CompassAgent over a PUSHABLE control source: `feed` enqueues a control op
// and lets the run loop drain it, `drive` pushes session turn edges through the
// recorded listener, `close` ends the loop. Unlike runWith (a fixed script that
// runs to completion) this interleaves control ops with turn edges — the
// deliver coalescing path needs both.
function startControlAgent(natives: AgentTool[] = []) {
	const session = recordingSession(natives);
	const frames: OutboundFrame[] = [];
	const unmapped: UnmappedEvent[] = [];
	const queue: AgentControl[] = [];
	let notify: (() => void) | undefined;
	let closed = false;
	const control: ControlSource = {
		async *[Symbol.asyncIterator]() {
			while (true) {
				while (queue.length > 0) {
					const next = queue.shift();
					if (next !== undefined) yield next;
				}
				if (closed) return;
				await new Promise<void>((resolve) => {
					notify = resolve;
				});
			}
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
	// Enqueue a control op and drain the microtask turns the `for await` needs to
	// pull + apply it.
	const feed = async (c: AgentControl): Promise<void> => {
		queue.push(c);
		notify?.();
		notify = undefined;
		await tick();
		await tick();
	};
	const drive = (event: AgentSessionEvent): void => {
		// Model production's inner loop clearing `#state.isStreaming = false` at the
		// `agent_end` case (pi-agent-core agent.ts:1254) BEFORE emitting the edge —
		// so a turn-end flush's single prompt is not refused by the just-settled
		// turn's own streaming flag, and a following turn's flush starts clean.
		if (event.type === "agent_end") session.agent.state.isStreaming = false;
		session.listener?.(event);
	};
	const close = async (): Promise<void> => {
		closed = true;
		notify?.();
		await done;
	};
	return { agent, session, frames, unmapped, feed, drive, close };
}

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
// RIG-1310 §8 — RT-3 turn-end delivery (DELIVER arm).
//
// deliver() rides the immediate handle (not the control script), so these tests
// construct CompassAgent directly and call `agent.deliver(msg)`, driving turn
// edges through the recorded `session.listener`. run() is started (not awaited)
// so the subscribe listener is registered; a held-open control source keeps the
// run loop parked until the test closes it, so nothing races the assertions.

// A comms Message fixture: id + a single text block, and its topic. The id is
// load-bearing (dedup + ack key); the text is what the coalesced prompt must
// contain; the topicId is the per-topic digest grouping key (defaults empty,
// as an untopic'd fixture rides one section).
function deliverMsg(id: string, text: string, topicId = ""): Message {
	return create(MessageSchema, {
		id,
		topicId,
		blocks: [
			create(MessageBlockSchema, { block: { case: "text", value: text } }),
		],
	});
}

// A comms Message carrying a single `ask_answer` block — the wire shape a
// delivered answer arrives as on the deliver (and steer) lane (RIG-2257). The
// answered `Ask` snapshot lives inline on the block.
function deliverAskAnswerMsg(id: string, ask: Ask, topicId = ""): Message {
	return create(MessageSchema, {
		id,
		topicId,
		blocks: [
			create(MessageBlockSchema, {
				block: {
					case: "askAnswer",
					value: create(AskAnswerBlockSchema, { ask }),
				},
			}),
		],
	});
}

// Start a CompassAgent with the recording harness and a held-open control
// source, so `run()` registers the turn-tracking listener but never terminates
// on its own. Returns the agent, the captured frames/unmapped, a `drive` to
// push session turn edges, and a `close` that ends the run loop cleanly.
function startDeliverAgent(natives: AgentTool[] = [], tracer?: TraceBridge) {
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
		...(tracer ? { tracer } : {}),
	});
	const done = agent.run();
	const drive = (event: AgentSessionEvent): void => {
		// Model production's inner loop clearing `#state.isStreaming = false` at the
		// `agent_end` case (pi-agent-core agent.ts:1254) BEFORE emitting the edge,
		// so the turn-end flush's single prompt starts from a settled turn.
		if (event.type === "agent_end") session.agent.state.isStreaming = false;
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

// The SessionInjection observation frames captured, in order, as
// {opKind, messageId} pairs. A SessionInjection rides the `session` variant's
// typed_event (the same FrameSink path the trace events use), so it is a
// "session" OutboundFrame whose typedEvent oneof case is "sessionInjection".
function injections(
	frames: OutboundFrame[],
): { opKind: SessionInjectionKind; messageId: string; fromHandle: string }[] {
	return frames.flatMap((f) => {
		if (f.kind !== "session") return [];
		const event = f.value.typedEvent?.event;
		if (event?.case !== "sessionInjection") return [];
		return [
			{
				opKind: event.value.opKind,
				messageId: event.value.messageId,
				fromHandle: event.value.fromHandle,
			},
		];
	});
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

describe("CompassAgent — RT-3 turn-end delivery (RIG-1310 §8 deliver arm)", () => {
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

	// Peer-DM DL-292 (T0 step 2): the source channel + topic NAMES ride the
	// deliver op and are plumbed per message to `formatDeliversForPrompt`, so the
	// idle turn-start prompt names the reply target the agent must now address by
	// name. Plumbed exactly the way `fromHandle` is (4th arg), keyed on Message.id.
	test("an idle deliver renders the plumbed source channel and topic names", async () => {
		const h = startDeliverAgent();
		h.agent.deliver(deliverMsg("m1", "ship it", "t-1"), "@peer", "", {
			channelName: "product",
			topicName: "launch",
		});
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain(
			"Channel product › topic launch:",
		);
		expect(h.session.agent.prompts[0]).toContain("ship it");
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

	// The high-severity race (RIG-1310 §8): a control-driven prompt sets the inner
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

	// Rejection-safety belt (RIG-1310 §8): if a flush's prompt is REFUSED (the
	// only prompt-rejection shape — a not-injected batch), the batch must not be
	// acked (no false receipt) and its ids must leave the processed set so the
	// Server's redelivery re-injects them. Forced via the model-independent
	// `promptRejectsNoModel` trigger (the "No model configured" throw, pi-agent-core
	// agent.ts:990) — a pre-injection rejection that does not hinge on the
	// streaming flag the `agent_end` edge now clears.
	test("a refused flush prompt emits no ack, un-dedups the batch, and surfaces it", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "refused"));
		// The turn-end flush's prompt is refused (no model configured).
		h.session.agent.promptRejectsNoModel = true;
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		// No injection recorded (the prompt rejected), and crucially NO ack.
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		// The refusal is surfaced, never a silent drop.
		const refused = h.unmapped.find(
			(u) =>
				u.eventType === "deliver:prompt" && u.reason.includes("not injected"),
		);
		expect(refused).toBeDefined();
		// The id left the processed set: the Server's redelivery is NOT deduped
		// away — it injects exactly once.
		h.session.agent.promptRejectsNoModel = false;
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
// RIG-2732 W3 — turn-end forge-notification arm. The RT-3 sibling of the deliver
// arm: a forge notification pushed mid-turn coalesces onto the turn-end queue
// and flushes as ONE prompt at agent_end; an idle notification flushes at once.
// BOTH acks fire at flush — the ForgeNotificationAck frame AND the deferred
// control-rail ack (the `ackRail` thunk the control source hands the agent) —
// never at decode (design.md:1006-1013). forgeNotification() rides the immediate
// handle, so these tests construct CompassAgent directly and drive turn edges
// through the recorded listener, exactly as the deliver tests do.

// A forge notification fixture for the agent arm: subscription id + revision (the
// ack correlation + advance target) and a per-kind payload. `change` defaults to
// COMMENT; `overrides` sets the kind-specific fields.
function forgeNote(
	subscriptionId: string,
	revision: string,
	overrides: Omit<
		Partial<MessageInitShape<typeof ForgeNotificationSchema>>,
		"$typeName"
	> = {},
): ForgeNotification {
	return create(ForgeNotificationSchema, {
		subscriptionId,
		revision,
		repo: "o/r",
		number: 42n,
		change: ForgeNotificationKind.COMMENT,
		...overrides,
	});
}

// The forge-notification-ack frames captured, in order, as {subscriptionId,
// revision} pairs — the turn-end forge delivery receipt that advances the
// Server's delivered_revision.
function forgeAcks(
	frames: OutboundFrame[],
): { subscriptionId: string; revision: string }[] {
	return frames.flatMap((f) =>
		f.kind === "forgeNotificationAck"
			? [
					{
						subscriptionId: f.value.subscriptionId,
						revision: f.value.revision,
					},
				]
			: [],
	);
}

// Start a CompassAgent forge harness, mirroring startDeliverAgent. `railAcks`
// records every control-rail ack the agent fires (the `ackRail` thunk the
// control source would hand it), so a test can prove BOTH acks land at flush and
// neither before it.
function startForgeAgent() {
	const session = recordingSession();
	const frames: OutboundFrame[] = [];
	const unmapped: UnmappedEvent[] = [];
	const railAcks: number[] = [];
	let seq = 0;
	let releaseControl!: () => void;
	const controlClosed = new Promise<void>((resolve) => {
		releaseControl = resolve;
	});
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
	// Push a forge notification through the same seam the control source drives,
	// pairing it with a rail-ack thunk that records the seq it retired.
	const push = (notification: ForgeNotification): void => {
		const mySeq = ++seq;
		agent.forgeNotification(notification, () => railAcks.push(mySeq));
	};
	const drive = (event: AgentSessionEvent): void => {
		// Model production's inner loop clearing `#state.isStreaming = false` at the
		// `agent_end` case (pi-agent-core agent.ts:1254) BEFORE emitting the edge,
		// so the turn-end flush's single prompt starts from a settled turn.
		if (event.type === "agent_end") session.agent.state.isStreaming = false;
		session.listener?.(event);
	};
	const close = async (): Promise<void> => {
		releaseControl();
		await done;
	};
	return { agent, session, frames, unmapped, railAcks, push, drive, close };
}

describe("CompassAgent — RIG-2732 W3 turn-end forge-notification arm", () => {
	test("mid-turn forge notifications coalesce into ONE turn-end prompt; NOTHING acked until flush", async () => {
		const h = startForgeAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.push(forgeNote("sub-1", "rev-1", { repo: "o/r", number: 7n }));
		h.push(forgeNote("sub-2", "rev-2", { repo: "o/r", number: 8n }));
		// No prompt while the turn is active — the notifications are queued.
		expect(h.session.agent.prompts).toEqual([]);
		// And crucially NEITHER ack has fired: no forge frame, no rail ack. A
		// decode-ack would show them here (the durability window the defer protects).
		await tick();
		expect(forgeAcks(h.frames)).toEqual([]);
		expect(h.railAcks).toEqual([]);
		// Turn ends: one coalesced prompt, then both acks per notification.
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("o/r#7");
		expect(h.session.agent.prompts[0]).toContain("o/r#8");
		await tick();
		expect(forgeAcks(h.frames)).toEqual([
			{ subscriptionId: "sub-1", revision: "rev-1" },
			{ subscriptionId: "sub-2", revision: "rev-2" },
		]);
		// Both rail acks fire at flush, AFTER the forge frames (order pinned by the
		// per-entry flush loop).
		expect(h.railAcks).toEqual([1, 2]);
		await h.close();
	});

	test("an idle forge notification flushes immediately as a turn-start prompt and acks", async () => {
		const h = startForgeAgent();
		// No active turn, not streaming: the notification flushes at once.
		h.push(forgeNote("sub-1", "rev-1"));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(forgeAcks(h.frames)).toEqual([
			{ subscriptionId: "sub-1", revision: "rev-1" },
		]);
		expect(h.railAcks).toEqual([1]);
		await h.close();
	});

	test("death before the flush leaves NO ack of either kind — the Runner redelivers, the sweep re-notifies", async () => {
		const h = startForgeAgent();
		// Mid-turn: the notification queues, and no agent_end ever arrives (the
		// agent dies before the turn settles).
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.push(forgeNote("sub-1", "rev-1"));
		await tick();
		// No prompt, no forge ack, no rail ack: delivered_revision stays put, so
		// the reconciliation sweep re-notifies from the durable gap.
		expect(h.session.agent.prompts).toEqual([]);
		expect(forgeAcks(h.frames)).toEqual([]);
		expect(h.railAcks).toEqual([]);
		await h.close();
	});

	test("a flush whose prompt is refused emits NO ack of either kind (un-acked for redelivery)", async () => {
		const h = startForgeAgent();
		// The idle flush's prompt rejects (no model): the batch was NOT injected.
		h.session.agent.promptRejectsNoModel = true;
		h.push(forgeNote("sub-1", "rev-1"));
		await tick();
		// The prompt was attempted but rejected — neither ack fires, and the
		// refusal is surfaced (never a silent drop).
		expect(forgeAcks(h.frames)).toEqual([]);
		expect(h.railAcks).toEqual([]);
		const refused = h.unmapped.find((u) => u.eventType === "forge:prompt");
		expect(refused?.reason).toContain("not injected");
		await h.close();
	});

	test("each notification kind renders its per-kind payload at flush (COMMENT/STATE/CHECKS/REVIEW/OPENED)", async () => {
		const h = startForgeAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.push(
			forgeNote("s-comment", "r1", {
				number: 1n,
				change: ForgeNotificationKind.COMMENT,
				comment: create(CommentRefSchema, {
					forgeAccount: "octocat",
					body: "please rebase",
				}),
			}),
		);
		h.push(
			forgeNote("s-state", "r2", {
				number: 2n,
				change: ForgeNotificationKind.STATE,
				state: "merged",
			}),
		);
		h.push(
			forgeNote("s-checks", "r3", {
				number: 3n,
				change: ForgeNotificationKind.CHECKS,
				checks: create(ChecksSummarySchema, { state: "failure" }),
			}),
		);
		h.push(
			forgeNote("s-review", "r4", {
				number: 4n,
				change: ForgeNotificationKind.REVIEW,
				state: "approved",
				comment: create(CommentRefSchema, { body: "LGTM" }),
			}),
		);
		h.push(
			forgeNote("s-opened", "r5", {
				number: 5n,
				change: ForgeNotificationKind.OPENED,
			}),
		);
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		expect(h.session.agent.prompts).toHaveLength(1);
		const prompt = h.session.agent.prompts[0] ?? "";
		// COMMENT: author + body.
		expect(prompt).toContain("@octocat: please rebase");
		// STATE: the new forge state string.
		expect(prompt).toContain("State: merged");
		// CHECKS: the rolled-up state.
		expect(prompt).toContain("Checks: failure");
		// REVIEW: verdict + body.
		expect(prompt).toContain("Verdict: approved");
		expect(prompt).toContain("Review: LGTM");
		// OPENED: "new <kind> repo#number".
		expect(prompt).toContain("new opened o/r#5");
		await tick();
		// One forge ack per notification, all five, in order.
		expect(forgeAcks(h.frames).map((a) => a.subscriptionId)).toEqual([
			"s-comment",
			"s-state",
			"s-checks",
			"s-review",
			"s-opened",
		]);
		expect(h.railAcks).toEqual([1, 2, 3, 4, 5]);
		await h.close();
	});

	// RIG-2732 Piece-2 review HIGH — the mixed-queue turn-end collision. Before
	// the single-prompt fix, `agent_end` issued TWO independent `prompt()` calls
	// (deliver flush then forge flush) on one synchronous edge; the first set the
	// inner agent streaming SYNCHRONOUSLY (pi-agent-core agent.ts:1072), so the
	// second threw AgentBusyError and the forge batch was silently dropped — no
	// ForgeNotificationAck, no rail ack, delivered_revision stranded. The faithful
	// fake now sets `state.isStreaming = true` on the first prompt, so this test
	// REDS against the two-prompt code (the forge acks never fire) and GREENS
	// against the combined single-prompt flush. It asserts (a) exactly ONE prompt
	// carrying BOTH rendered sections, (b) the deliveryAck for the message, (c) the
	// ForgeNotificationAck AND the rail-ack retirement for the notification.
	test("a mixed deliver+forge queue flushes as ONE prompt with BOTH sections and all acks", async () => {
		const h = startForgeAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		// Mid-turn: BOTH a deliver and a forge notification queue for this turn.
		h.agent.deliver(deliverMsg("m1", "hello channel"));
		h.push(forgeNote("sub-1", "rev-1", { repo: "o/r", number: 7n }));
		// Nothing acked mid-turn — both coalesce onto the turn-end queue.
		expect(h.session.agent.prompts).toEqual([]);
		await tick();
		expect(ackIds(h.frames)).toEqual([]);
		expect(forgeAcks(h.frames)).toEqual([]);
		expect(h.railAcks).toEqual([]);
		// Turn ends: EXACTLY ONE prompt, carrying both rendered sections (deliver
		// first, then forge — the stable order the combined flush pins).
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		expect(h.session.agent.prompts).toHaveLength(1);
		const prompt = h.session.agent.prompts[0] ?? "";
		expect(prompt).toContain("hello channel");
		expect(prompt).toContain("o/r#7");
		expect(prompt.indexOf("hello channel")).toBeLessThan(
			prompt.indexOf("o/r#7"),
		);
		await tick();
		// All ack kinds fire in the shared post-injection microtask: the deliver's
		// DeliveryAck, the forge's ForgeNotificationAck, AND the forge rail-ack.
		expect(ackIds(h.frames)).toEqual(["m1"]);
		expect(forgeAcks(h.frames)).toEqual([
			{ subscriptionId: "sub-1", revision: "rev-1" },
		]);
		expect(h.railAcks).toEqual([1]);
		// The deliver's DELIVER injection observation also fired at flush.
		expect(injections(h.frames)).toEqual([
			{ opKind: SessionInjectionKind.DELIVER, messageId: "m1", fromHandle: "" },
		]);
		await h.close();
	});
});

// ---------------------------------------------------------------------------
// RIG-2257 — a delivered ask_answer message renders through the deliver lane.
// The answer arrives as a normal Message carrying an `ask_answer` block; it
// coalesces, dedups by msg.id, and acks exactly like any other deliver — no
// control arm, no registry.
describe("CompassAgent — RIG-2257 delivered ask_answer renders on the deliver lane", () => {
	test("an idle delivered ask_answer renders question text + chosen labels + custom text as one prompt", async () => {
		const h = startDeliverAgent();
		const ask = answeredAsk([
			askQuestion("q-1", "Ship it?", {
				options: [
					{ id: "0", label: "Yes, ship" },
					{ id: "1", label: "Hold" },
				],
				chosenOptionIds: ["0"],
				customText: "after the freeze",
			}),
		]);
		h.agent.deliver(deliverAskAnswerMsg("m1", ask));
		// Idle: flushed at once as one prompt through the deliver path.
		expect(h.session.agent.prompts).toHaveLength(1);
		const prompt = h.session.agent.prompts[0];
		expect(prompt).toContain("Ship it?");
		// The LABEL of the chosen id, not the id or the unchosen option.
		expect(prompt).toContain("Yes, ship");
		expect(prompt).not.toContain("Hold");
		expect(prompt).toContain("after the freeze");
		await tick();
		// It rode the deliver lane's ack, keyed on msg.id.
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	test("redelivery of the same msg.id injects once (deliver-lane dedup)", async () => {
		const h = startDeliverAgent();
		const ask = answeredAsk([
			askQuestion("q-1", "Ship it?", { customText: "go" }),
		]);
		h.agent.deliver(deliverAskAnswerMsg("m1", ask));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["m1"]);
		// A sweep redelivery of the SAME id: no second injection (dedup), and a
		// guarded re-ack recovers the Server delivery cursor.
		h.agent.deliver(deliverAskAnswerMsg("m1", ask));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["m1", "m1"]);
		await h.close();
	});

	test("an answer delivered to a fresh session renders fully — no registry, no correlation needed", async () => {
		// The whole point of RIG-2257: the answered Ask travels inline on the
		// delivered block, so a session that never raised the ask (post-restart,
		// empty in-memory state) still renders it — the old unknown-ask-id
		// degraded arm is gone.
		const h = startDeliverAgent();
		const ask = answeredAsk([
			askQuestion("q-1", "Proceed?", {
				options: [{ id: "0", label: "affirmative" }],
				chosenOptionIds: ["0"],
			}),
		]);
		h.agent.deliver(deliverAskAnswerMsg("m1", ask));
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("Proceed?");
		expect(h.session.agent.prompts[0]).toContain("affirmative");
		// No fabricated-unknown unmapped op: the answer rendered fully.
		expect(h.unmapped).toEqual([]);
		await tick();
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});
});

// ---------------------------------------------------------------------------
// RIG-2644 — idle deliver after replay_complete must start a turn, and a deliver
// against an UNTRACKED stream must not strand. Surfaced investigating RIG-2617
// Defect 2; wire evidence (992f3b5e clean seed): control path binds, drains,
// applies + acks replay_complete(1) + deliver(2,3,4), but the board stays
// STARTING, comms delivery cursor acked_seq=0 (NO DeliveryAck), no turn. The
// first two tests pin the correct idle-flush path; the last two pin the
// strand-recovery fix (an untracked stream that never emits `agent_end`). All
// drive the REAL run loop (startControlAgent) with production ordering:
// replay_complete then a deliver, no agent_start/agent_end in between.
describe("CompassAgent — RIG-2644 idle deliver / strand recovery after replay_complete", () => {
	test("replay_complete then an idle deliver (no turn edges) → prompt turn + DeliveryAck", async () => {
		const h = startControlAgent();
		// Barrier lifts via the control script, exactly as the pump applies it.
		await h.feed({ kind: "replayComplete" });
		// An idle deliver arrives via the immediate handle — no agent_start has
		// fired, no turn is live: the fresh-peer shape. The gate at agent.ts:325
		// must read idle and flush.
		h.agent.deliver(deliverMsg("m1", "channel msg"));
		await tick();
		// GROUND TRUTH: does a turn start and is the deliver acked?
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	test("replay_complete + idle deliver fired synchronously in one batch → turn + ack", async () => {
		// The tightest production ordering: the pump dispatches replay_complete(1)
		// (buffered) and deliver(2) (immediate) in ONE synchronous stream drain,
		// BEFORE run() has pulled + applied replay_complete via #applyControl. The
		// immediate deliver fires agent.deliver() before agent.ts #replayComplete is
		// set — the two-barrier-flag window the supervisor flagged. agent.deliver's
		// gate does not consult #replayComplete, so it must still flush when idle.
		const h = startControlAgent();
		// Fire the deliver in the same tick as the replayComplete feed, without
		// awaiting the feed's drain first — the deliver lands while run() is still
		// mid-pull of replay_complete.
		const fed = h.feed({ kind: "replayComplete" });
		h.agent.deliver(deliverMsg("m1", "channel msg"));
		await fed;
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	test("a deliver against an UNTRACKED stream (startup probe, no agent_end) is recovered: flushes once the stream settles", async () => {
		// THE production shape (supervisor's wire evidence, RIG-2617 Defect 2). The
		// real AgentSession.isStreaming folds in #promptInFlightCount
		// (agent-session.ts:6470), which a startup provider probe/prewarm holds > 0
		// WITHOUT emitting an agent_end through subscribe(). Before the fix the
		// idle gate (agent.ts:325) read !idle, the deliver QUEUED, and nothing ever
		// fired the agent_end that would flush it → permanent strand (no prompt, no
		// DeliveryAck, comms acked_seq 0, board stuck STARTING). The fix arms a
		// waitForIdle-gated recovery: when the untracked stream settles, the queued
		// deliver flushes.
		const h = startControlAgent();
		await h.feed({ kind: "replayComplete" });
		// An untracked in-flight (probe/prewarm) holds isStreaming true; NO
		// agent_start/agent_end ever reaches the subscription.
		h.session.agent.state.isStreaming = true;
		h.agent.deliver(deliverMsg("m1", "channel msg"));
		await tick();
		// Still queued while the probe streams — not flushed prematurely (that
		// would AgentBusyError). Non-vacuity: this is the pre-settle state.
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		// The probe finishes: isStreaming clears, waitForIdle resolves. The armed
		// recovery re-checks and flushes the stranded deliver.
		h.session.settleIdle();
		await tick();
		await tick();
		// RECOVERED: the deliver became a turn and was acked. Non-vacuity: drop the
		// #armStrandRecovery arm in deliver() → this stays [] / [] (the strand).
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("channel msg");
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	test("a real tracked turn flushes on agent_end; the strand recovery does not double-flush", async () => {
		// The spin-up race the isStreaming gate exists to close (RIG-2488/RIG-1310)
		// must stay closed: a deliver landing while a control-prompt has spun the
		// inner agent streaming but agent_start has not yet propagated (#turnActive
		// still false, isStreaming true) queues AND arms a recovery. When the REAL
		// turn's agent_end fires it flushes the queue; the later waitForIdle
		// recovery must then find an empty queue and no-op — exactly ONE flush, no
		// double-inject / AgentBusyError.
		const h = startControlAgent();
		await h.feed({ kind: "replayComplete" });
		// Control-prompt spin-up: streaming true, no agent_start yet.
		h.session.agent.state.isStreaming = true;
		h.agent.deliver(deliverMsg("m1", "spin-up"));
		await tick();
		expect(h.session.agent.prompts).toEqual([]);
		// The real turn's agent_end arrives (the inner loop cleared streaming
		// before emitting it, agent.ts:1254 — model that ordering) and flushes.
		h.session.agent.state.isStreaming = false;
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(ackIds(h.frames)).toEqual(["m1"]);
		// Now release the still-armed recovery's waitForIdle: the queue is already
		// empty, so it must NOT flush again. Non-vacuity: a recovery that ignored
		// the empty-queue guard would push a second (empty) prompt here.
		h.session.settleIdle();
		await tick();
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	test("a second probe still streaming at resolve re-arms the recovery; it flushes once the second settles", async () => {
		// RIG-2644 review M3. The re-arm branch (agent.ts, #armStrandRecovery
		// else-if): waitForIdle can resolve while the session still reads streaming
		// because a FRESH probe holds #promptInFlightCount > 0 (the fold at
		// agent-session.ts:6469-6470). The recovery must NOT flush into that (it
		// would AgentBusyError) — it re-arms and flushes only when the second probe
		// settles. `settleIdle({ keepStreaming: true })` models exactly that window:
		// waiters resolve, isStreaming stays true.
		const h = startControlAgent();
		await h.feed({ kind: "replayComplete" });
		// First untracked probe: streaming, no turn. The deliver queues + arms.
		h.session.agent.state.isStreaming = true;
		h.agent.deliver(deliverMsg("m1", "channel msg"));
		await tick();
		expect(h.session.agent.prompts).toEqual([]);
		// First probe's inner agent settles, BUT a second probe is already in
		// flight → waitForIdle resolves while isStreaming still reads true. The
		// recovery hits the else-if and RE-ARMS rather than flushing.
		h.session.settleIdle({ keepStreaming: true });
		await tick();
		await tick();
		// Still not flushed — proves the re-arm branch fired (a recovery that
		// flushed unconditionally would have rejected AgentBusyError here, leaving
		// prompts empty AND surfacing an unmapped refusal). Non-vacuity: the queue
		// is intact and no refusal was surfaced.
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		// The second probe finishes fully idle: the re-armed recovery flushes once.
		h.session.settleIdle();
		await tick();
		await tick();
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("channel msg");
		expect(ackIds(h.frames)).toEqual(["m1"]);
		await h.close();
	});

	test("a recovery whose waitForIdle resolves AFTER close does not start a post-terminal turn", async () => {
		// RIG-2644 review M2 (close race). run()'s finally sets #closed on the same
		// edge it emits the terminal status. A strand-recovery waitForIdle still
		// pending at close must NOT flush when it later resolves — that would start
		// a turn and emit a DeliveryAck AFTER the terminal STOPPED frame the board
		// already saw. Order: arm the recovery (deliver against an untracked
		// stream), close the agent (control stream ends → STOPPED, #closed set),
		// THEN settle the probe so the pending .then fires.
		const h = startControlAgent();
		await h.feed({ kind: "replayComplete" });
		h.session.agent.state.isStreaming = true;
		h.agent.deliver(deliverMsg("m1", "channel msg"));
		await tick();
		expect(h.session.agent.prompts).toEqual([]);
		// Close: the control loop ends, run() emits STOPPED and its finally sets
		// #closed = true, all while the recovery's waitForIdle is still parked.
		await h.close();
		const stopped = h.frames.some(
			(f) =>
				f.kind === "session" && f.value.state === AgentSessionState.STOPPED,
		);
		expect(stopped).toBe(true);
		// Now the probe settles and the parked recovery .then resolves. The #closed
		// guard must make it no-op. Non-vacuity: drop the `if (this.#closed) return`
		// guard → this flushes a turn + acks m1 AFTER STOPPED, and both asserts red.
		h.session.settleIdle();
		await tick();
		await tick();
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
	});
});

// ---------------------------------------------------------------------------
// RIG-1310 §8 — channel-borne steer arm.
//
// steer() rides the same immediate handle deliver does (not the control script),
// so these tests reuse startDeliverAgent()/deliverMsg()/ackIds()/tick() and call
// `agent.steer(msg)`. Unlike deliver, a steer is an @-mention interrupt: mid-turn
// it injects via `session.agent.steer` (drained by the running loop, no turn
// started); idle it STARTS A TURN with the mention as content via
// `session.agent.prompt`, mirroring the idle-deliver path (design: architecture-lineage idle arm).
describe("CompassAgent — channel-borne steer (RIG-1310 §8 steer arm)", () => {
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
		// No turn was started (the running loop drains the enqueued steer in place).
		// Non-vacuity: the idle path would push exactly one prompt here.
		expect(h.session.agent.prompts).toEqual([]);
		// The ack (means "injected") rides the next microtask.
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		await h.close();
	});

	test("an idle steer starts a turn via prompt (history-agnostic) and emits its STEER injection", async () => {
		// REGRESSION (RIG-2488, the leg-4 e2e catch): the idle arm must START A
		// TURN with the mention as initial content via prompt() — which runs on ANY
		// history, including a fresh agent's EMPTY history — NOT via continue(),
		// which rejects "No messages to continue from" on a zero-history session.
		// The leg-4 peer is spawned idle with empty history: the mention WAS steered
		// to its live session, but the old continue() path threw and rolled back, so
		// its SessionInjection.STEER never fired and the split assertion timed out.
		// The frozen record ties an idle frame to "starts a new turn" (agent.ts
		// idle-arm comment / design: architecture-lineage); prompt() is the idle-deliver
		// path's mechanism too (#flushDelivers), so this mirrors it.
		const h = startDeliverAgent();
		// Idle: no agent_start, no prior turns — the fresh-peer shape.
		h.agent.steer(deliverMsg("s1", "please take a look"), "matt");
		// Started a turn via prompt carrying the mention content — the regression
		// lock: against the pre-fix continue() path, prompts is EMPTY here.
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain("please take a look");
		await tick();
		// Receipt AND observation both fire at injection time.
		expect(ackIds(h.frames)).toEqual(["s1"]);
		expect(injections(h.frames)).toEqual([
			{
				opKind: SessionInjectionKind.STEER,
				messageId: "s1",
				fromHandle: "matt",
			},
		]);
		await h.close();
	});

	// Peer-DM DL-292 (T0 step 2): a steer carries the source channel + topic
	// NAMES on the wire (`SteerControl.channel_name`/`topic_name`), plumbed as the
	// 4th arg exactly the way `fromHandle` is the 2nd. The idle-steer turn-start
	// prompt renders them so the agent can reply naming its target.
	test("an idle steer renders the plumbed source channel and topic names", async () => {
		const h = startDeliverAgent();
		h.agent.steer(deliverMsg("s1", "look here", "t-1"), "matt", "", {
			channelName: "product",
			topicName: "launch",
		});
		expect(h.session.agent.prompts).toHaveLength(1);
		expect(h.session.agent.prompts[0]).toContain(
			"Channel product › topic launch:",
		);
		expect(h.session.agent.prompts[0]).toContain("look here");
		await h.close();
	});

	// Spin-up-window guard on the idle-steer arm (RIG-2488 review follow-up): the
	// idle arm optimistically sets `#turnActive = true` BEFORE `prompt()`'s
	// `agent_start` propagates (agent.ts:428), exactly as `#flushDelivers` does.
	// Without it, a follow-on steer/deliver landing in that window — `isStreaming`
	// still false, no `agent_start` yet — would re-gate as idle and start a SECOND
	// turn (→ AgentBusyError, the message acked-and-dropped). Here the first idle
	// steer starts a turn (one prompt); a second steer fired in the same window
	// must take the MID-TURN arm (enqueue, no new turn). Non-vacuity: drop the
	// optimistic `#turnActive = true` and the second steer re-gates idle → a
	// second prompt (prompts length 2).
	test("a follow-on steer inside the idle-steer spin-up window enqueues, not a second turn", async () => {
		const h = startDeliverAgent();
		// First idle steer starts a turn via prompt and optimistically marks the
		// turn active — no `agent_start` driven, `isStreaming` still false.
		h.agent.steer(deliverMsg("s1", "first"));
		expect(h.session.agent.prompts).toHaveLength(1);
		// A second steer arrives in the spin-up window (still no agent_start,
		// isStreaming still false). The optimistic `#turnActive` flips the gate to
		// the mid-turn arm: it ENQUEUES onto the steering queue, starts no turn.
		h.agent.steer(deliverMsg("s2", "second"));
		expect(h.session.agent.steers).toHaveLength(1);
		expect(steerContent(h.session.agent.steers[0] as AgentMessage)).toContain(
			"second",
		);
		// The prompt count did not grow — no second turn was started.
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		// Both are acked (s1 via the idle prompt path, s2 via the mid-turn arm).
		expect(ackIds(h.frames)).toEqual(["s1", "s2"]);
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
		// First steer (idle) starts a turn via prompt for s1 and acks it.
		h.agent.steer(deliverMsg("s1", "once"));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		// The SAME id, redelivered after its priority-lane ack was lost. s1 is
		// already injected, so the dedup path RE-ACKS to recover the Server cursor
		// and does NOT re-inject (no second turn). Non-vacuity: drop the dedup guard
		// → prompts length goes to 2 (re-inject).
		h.agent.steer(deliverMsg("s1", "once"));
		expect(h.session.agent.prompts).toHaveLength(1);
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
		expect(h.session.agent.prompts).toEqual([]);
		expect(ackIds(h.frames)).toEqual([]);
		const missing = h.unmapped.find(
			(u) => u.eventType === "steer" && u.reason.includes("missing Message.id"),
		);
		expect(missing).toBeDefined();
		await h.close();
	});

	// Idle-steer rejection belt (MEDIUM-1): the idle arm STARTS A TURN with
	// `prompt()`, which can REJECT. The one reachable idle-path rejection is the
	// "No model configured" throw (pi-agent-core agent.ts:990) — NOT an
	// AgentBusyError spin-up race, which cannot fire because steer() is synchronous
	// from the idle gate to the call. On rejection the turn did not start and the
	// idle path never enqueued (prompt carries the mention as turn content), so
	// there is no orphan steer to roll back: the belt simply emits no ack (no false
	// receipt) and un-dedups the id, so the Server's redelivery re-injects EXACTLY
	// ONCE. Modeled by `promptRejectsNoModel` (not `state.isStreaming`, which would
	// flip the idle gate to the mid-turn path and skip the belt entirely).
	test("an idle prompt rejection un-dedups the id and emits no ack; redelivery injects exactly once", async () => {
		const h = startDeliverAgent();
		h.session.agent.promptRejectsNoModel = true;
		h.agent.steer(deliverMsg("s1", "refused"));
		await tick();
		// The turn refused to start — so NO ack, and nothing was enqueued or
		// prompted. Non-vacuity: drop the rejection guard on the ack microtask →
		// this acks a never-injected steer.
		expect(ackIds(h.frames)).toEqual([]);
		expect(h.session.agent.prompts).toEqual([]);
		expect(h.session.agent.steers).toEqual([]);
		// The refusal is surfaced, never a silent drop.
		const refused = h.unmapped.find(
			(u) =>
				u.eventType === "steer:prompt" && u.reason.includes("not injected"),
		);
		expect(refused).toBeDefined();
		// The id left the processed set: the Server's redelivery of the same id
		// starts the turn (not deduped away), and this time it succeeds — exactly
		// one prompt, one ack. Non-vacuity: drop the `#processedMessageIds.delete`
		// → the redelivery is deduped and this reddens (no prompt, no ack).
		h.session.agent.promptRejectsNoModel = false;
		h.agent.steer(deliverMsg("s1", "refused"));
		expect(h.session.agent.prompts).toHaveLength(1);
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

// RIG-2486 (T1) — the cross-process op-kind signal. steer()/deliver() each emit
// a first-class SessionInjection observation frame BESIDE the existing delivery
// ack at injection time (design "steer/deliver split observation seam"). These
// reuse the deliver/steer harness and assert the injection rides the session
// trace path with the right op_kind + message_id, alongside the ack. Before the
// emit arms exist these are RED: injections(h.frames) is empty, so the length
// assertion fails.
describe("CompassAgent — SessionInjection op-kind signal (RIG-2486 T1)", () => {
	test("deliver(msg, fromHandle) emits one DELIVER SessionInjection with the handle beside the ack", async () => {
		const h = startDeliverAgent();
		// Idle deliver flushes immediately and injects m1, carrying the
		// denormalized author handle off the wire deliver control (RIG-2486 T1).
		h.agent.deliver(deliverMsg("m1", "hello"), "matt");
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		// The ack still fires (no regression).
		expect(ackIds(h.frames)).toEqual(["m1"]);
		// And exactly one injection, DELIVER, carrying the message id AND the
		// non-empty from_handle threaded server->wire->emit. Non-vacuity: before
		// the threading, fromHandle was hard-coded "" so this asserted-value fails.
		expect(injections(h.frames)).toEqual([
			{
				opKind: SessionInjectionKind.DELIVER,
				messageId: "m1",
				fromHandle: "matt",
			},
		]);
		await h.close();
	});

	test("steer(msg, fromHandle) idle emits one STEER SessionInjection with the handle beside the ack", async () => {
		const h = startDeliverAgent();
		// Idle steer starts a turn via prompt for s1, and carries the author handle.
		h.agent.steer(deliverMsg("s1", "hey"), "matt");
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		expect(injections(h.frames)).toEqual([
			{
				opKind: SessionInjectionKind.STEER,
				messageId: "s1",
				fromHandle: "matt",
			},
		]);
		await h.close();
	});

	test("steer(msg, fromHandle) mid-turn emits one STEER SessionInjection with the handle beside the ack", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		// Mid-turn steer injects onto the running loop's steering queue.
		h.agent.steer(deliverMsg("s1", "one"), "matt");
		await tick();
		expect(ackIds(h.frames)).toEqual(["s1"]);
		expect(injections(h.frames)).toEqual([
			{
				opKind: SessionInjectionKind.STEER,
				messageId: "s1",
				fromHandle: "matt",
			},
		]);
		await h.close();
	});

	test("deliver with no fromHandle emits an empty from_handle (server store-miss path)", async () => {
		const h = startDeliverAgent();
		// The Server logs a handle-resolution miss and sends an empty from_handle
		// rather than blocking the delivery — the injection still fires, empty.
		h.agent.deliver(deliverMsg("m1", "hello"));
		expect(h.session.agent.prompts).toHaveLength(1);
		await tick();
		expect(injections(h.frames)).toEqual([
			{ opKind: SessionInjectionKind.DELIVER, messageId: "m1", fromHandle: "" },
		]);
		await h.close();
	});

	test("two mid-turn delivers with DISTINCT fromHandles thread through per-message-id, not last-write-wins", async () => {
		const h = startDeliverAgent();
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		// Two mid-turn delivers, each with its OWN author handle, coalesce into
		// the one turn-end flush. #deliverFromHandles is keyed per message id, so
		// each injection carries its own handle — a single scalar (last-write-wins)
		// would collapse both to "jane".
		h.agent.deliver(deliverMsg("m1", "hello one"), "matt");
		h.agent.deliver(deliverMsg("m2", "hello two"), "jane");
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		expect(injections(h.frames)).toEqual([
			{
				opKind: SessionInjectionKind.DELIVER,
				messageId: "m1",
				fromHandle: "matt",
			},
			{
				opKind: SessionInjectionKind.DELIVER,
				messageId: "m2",
				fromHandle: "jane",
			},
		]);
		await h.close();
	});
});

describe("formatDeliversForPrompt — coalescing format (RIG-1310 §8 / peer-DM DL-292)", () => {
	// A per-message source-name map, keyed on Message.id, mirroring the way
	// `#deliverFromHandles` is plumbed. Empty entries exercise the resolve-miss
	// fallback (topic → id, channel → "(unknown channel)").
	const src = (
		entries: Record<string, { channelName: string; topicName: string }>,
	): Map<string, { channelName: string; topicName: string }> =>
		new Map(Object.entries(entries));

	test("renders each message's text, in order, within its (channel, topic) section", () => {
		const batch = [
			deliverMsg("m1", "first", "t-1"),
			deliverMsg("m2", "second", "t-1"),
		];
		const sources = src({
			m1: { channelName: "eng", topicName: "deploys" },
			m2: { channelName: "eng", topicName: "deploys" },
		});
		const out = formatDeliversForPrompt(batch, sources);
		expect(out).toContain("first");
		expect(out).toContain("second");
		// Order is preserved: first appears before second.
		expect(out.indexOf("first")).toBeLessThan(out.indexOf("second"));
		// One (channel, topic) group, so one section header naming both names.
		expect(out.match(/^Channel /gm)).toHaveLength(1);
		expect(out).toContain("Channel eng › topic deploys:");
	});

	test("concatenates multiple text blocks and ignores ask blocks", () => {
		const msg = create(MessageSchema, {
			id: "m1",
			topicId: "t-1",
			blocks: [
				create(MessageBlockSchema, { block: { case: "text", value: "alpha" } }),
				create(MessageBlockSchema, {
					block: {
						case: "ask",
						value: create(AskSchema, {
							askId: "a-1",
							questions: [
								create(AskQuestionSchema, {
									questionId: "q1",
									question: "ignored?",
								}),
							],
						}),
					},
				}),
				create(MessageBlockSchema, { block: { case: "text", value: "beta" } }),
			],
		});
		const out = formatDeliversForPrompt([msg]);
		expect(out).toContain("alpha");
		expect(out).toContain("beta");
		// The ask block is ignored — deliver carries channel text only.
		expect(out).not.toContain("ignored?");
	});

	test("renders an askAnswer block via formatAskAnswerForPrompt, still ignoring bare ask blocks", () => {
		const msg = create(MessageSchema, {
			id: "m1",
			topicId: "t-1",
			blocks: [
				create(MessageBlockSchema, {
					block: {
						case: "askAnswer",
						value: create(AskAnswerBlockSchema, {
							ask: answeredAsk([
								askQuestion("q-1", "Deploy where?", {
									options: [{ id: "0", label: "prod" }],
									chosenOptionIds: ["0"],
								}),
							]),
						}),
					},
				}),
			],
		});
		const out = formatDeliversForPrompt([msg]);
		expect(out).toContain("Deploy where?");
		expect(out).toContain("prod");
	});

	// The source channel + topic NAMES ride the deliver op and are rendered in
	// the section header + reply cue — the peer-DM cutover (DL-292): the agent
	// must name its reply target, and the tool now requires an explicit channel
	// name, so the delivery has to SHOW that name.
	test("names the source channel and topic from the plumbed source names", () => {
		const batch = [deliverMsg("m1", "hello", "t-9")];
		const sources = src({
			m1: { channelName: "product", topicName: "launch" },
		});
		const out = formatDeliversForPrompt(batch, sources);
		expect(out).toContain("Channel product › topic launch:");
		expect(out).toContain("hello");
		// The reply cue names BOTH required post params.
		const cueLine = out.split("\n\n").at(-1) ?? "";
		expect(cueLine).toContain("channel");
		expect(cueLine).toContain("topic");
	});

	// A server-side resolve miss leaves either name empty (a name miss never
	// blocks a delivery); the renderer falls back — topic to its id, channel to a
	// fixed placeholder — rather than printing a bare empty label.
	test("falls back on a resolve miss: topic to id, channel to a placeholder", () => {
		const batch = [deliverMsg("m1", "orphan", "t-42")];
		// No source entry at all → both names missing.
		const out = formatDeliversForPrompt(batch);
		expect(out).toContain("Channel (unknown channel) › topic t-42:");
		expect(out).toContain("orphan");
	});

	// Two delivers in different (channel, topic) groups render as two distinct
	// sections, first-seen order, message order preserved within a section (D4).
	test("groups two topics into two distinct sections", () => {
		const batch = [
			deliverMsg("m1", "alpha msg", "t-alpha"),
			deliverMsg("m2", "beta msg", "t-beta"),
		];
		const sources = src({
			m1: { channelName: "eng", topicName: "alpha" },
			m2: { channelName: "eng", topicName: "beta" },
		});
		const out = formatDeliversForPrompt(batch, sources);

		expect(out).toContain("Channel eng › topic alpha:");
		expect(out).toContain("Channel eng › topic beta:");
		// Two distinct sections.
		expect(out.match(/^Channel /gm)).toHaveLength(2);
		// First-seen order: alpha before beta.
		expect(out.indexOf("topic alpha:")).toBeLessThan(
			out.indexOf("topic beta:"),
		);
		expect(out.indexOf("alpha msg")).toBeLessThan(out.indexOf("beta msg"));
	});

	// Two delivers in the SAME (channel, topic) stay in one section, order kept.
	test("keeps same-topic delivers in one section, in order", () => {
		const batch = [
			deliverMsg("m1", "one", "t-1"),
			deliverMsg("m2", "two", "t-1"),
		];
		const sources = src({
			m1: { channelName: "eng", topicName: "deploys" },
			m2: { channelName: "eng", topicName: "deploys" },
		});
		const out = formatDeliversForPrompt(batch, sources);

		expect(out.match(/^Channel /gm)).toHaveLength(1);
		expect(out.indexOf("one")).toBeLessThan(out.indexOf("two"));
	});

	// RIG-2664 / DL-292: the coalesced prompt carries ONE terse reply cue for the
	// whole batch, pointing at comms_post_message and naming the two required
	// address params. Terse by design: the load-bearing "why" lives once in the
	// manager SYSTEM.md, not re-paid per delivered batch.
	test("appends a single terse reply cue naming channel and topic", () => {
		const batch = [
			deliverMsg("m1", "alpha msg", "t-alpha"),
			deliverMsg("m2", "beta msg", "t-beta"),
		];
		const sources = src({
			m1: { channelName: "eng", topicName: "alpha" },
			m2: { channelName: "eng", topicName: "beta" },
		});
		const out = formatDeliversForPrompt(batch, sources);

		// The cue is the trailing section (sections render first, one cue last).
		const cueLine = out.split("\n\n").at(-1) ?? "";
		// Exact-shape lock: the durable guard. Any re-expansion of the cue breaks
		// this equality.
		expect(cueLine).toBe(
			"Reply via comms_post_message, naming the channel and topic above.",
		);
		// Exactly one cue for the whole batch, not one per message/topic.
		expect(out.match(/comms_post_message/g)).toHaveLength(1);
		// Terse tripwire: the verbose block-0 "why" is NOT re-paid in the cue.
		expect(out).not.toContain("session log");
	});

	test("returns an empty string for an empty batch (no cue with nothing to reply to)", () => {
		expect(formatDeliversForPrompt([])).toBe("");
	});
});

describe("formatAskAnswerForPrompt — answer render (RIG-2257)", () => {
	test("renders the question text, chosen option LABELS, and custom text", () => {
		const ask = answeredAsk([
			askQuestion("q-1", "Deploy target?", {
				options: [
					{ id: "0", label: "staging" },
					{ id: "1", label: "prod" },
				],
				chosenOptionIds: ["1"],
				customText: "after the freeze",
			}),
		]);
		const out = formatAskAnswerForPrompt(ask);
		expect(out).toContain("Deploy target?");
		// The LABEL of the chosen id, not the id itself.
		expect(out).toContain("prod");
		expect(out).toContain("after the freeze");
		// The unchosen option's label is not rendered.
		expect(out).not.toContain("staging");
	});

	test("one section per question, in ask order", () => {
		const ask = answeredAsk([
			askQuestion("q-1", "First?", { customText: "a" }),
			askQuestion("q-2", "Second?", { customText: "b" }),
		]);
		const out = formatAskAnswerForPrompt(ask);
		expect(out.indexOf("First?")).toBeLessThan(out.indexOf("Second?"));
	});

	test("renders an unresolvable option id defensively, never dropped or mislabelled", () => {
		const ask = answeredAsk([
			askQuestion("q-1", "Pick?", {
				options: [{ id: "0", label: "only" }],
				// An id with no recorded option — surfaced by id, marked unknown.
				chosenOptionIds: ["9"],
			}),
		]);
		const out = formatAskAnswerForPrompt(ask);
		expect(out).toContain("option 9");
		expect(out).toContain("unknown");
	});

	test("guards an embedded newline in an option label (flat), keeping the section on one line", () => {
		const ask = answeredAsk([
			askQuestion("q-1", "Pick?", {
				options: [{ id: "0", label: "line one\nline two" }],
				chosenOptionIds: ["0"],
			}),
		]);
		const out = formatAskAnswerForPrompt(ask);
		// The label's newline is collapsed — it cannot forge a new section line.
		expect(out).toContain("line one line two");
		expect(out).not.toContain("line one\nline two");
	});
});

describe("formatForgeNotifications — per-kind render (RIG-2732 W3)", () => {
	test("returns an empty string for an empty batch (nothing to render)", () => {
		expect(formatForgeNotifications([])).toBe("");
	});

	test("renders one section per notification, in order, each carrying its repo#number coord", () => {
		const out = formatForgeNotifications([
			forgeNote("s1", "r1", { repo: "o/a", number: 1n }),
			forgeNote("s2", "r2", { repo: "o/b", number: 2n }),
		]);
		expect(out).toContain("o/a#1");
		expect(out).toContain("o/b#2");
		expect(out.indexOf("o/a#1")).toBeLessThan(out.indexOf("o/b#2"));
	});

	test("closes the batch with a single re-read cue (the act-on-this instruction)", () => {
		const out = formatForgeNotifications([forgeNote("s1", "r1")]);
		const cue = out.split("\n\n").at(-1) ?? "";
		expect(cue).toContain("Re-read the artifact");
	});

	test("uses the ForgeRef host as the display name when set, else 'forge'", () => {
		const withHost = formatForgeNotifications([
			forgeNote("s1", "r1", {
				forge: create(ForgeRefSchema, { host: "github.com" }),
			}),
		]);
		expect(withHost).toContain("github.com");
		const withoutHost = formatForgeNotifications([forgeNote("s1", "r1")]);
		expect(withoutHost).toContain("forge");
	});

	test("a COMMENT with no forge account renders 'comment:' rather than a bare '@'", () => {
		const out = formatForgeNotifications([
			forgeNote("s1", "r1", {
				change: ForgeNotificationKind.COMMENT,
				comment: create(CommentRefSchema, { body: "ping" }),
			}),
		]);
		expect(out).toContain("comment: ping");
		expect(out).not.toContain("@:");
	});

	test("flattens an embedded newline in a comment body, keeping the section intact", () => {
		const out = formatForgeNotifications([
			forgeNote("s1", "r1", {
				change: ForgeNotificationKind.COMMENT,
				comment: create(CommentRefSchema, {
					forgeAccount: "octocat",
					body: "line one\nline two",
				}),
			}),
		]);
		expect(out).toContain("line one line two");
		expect(out).not.toContain("line one\nline two");
	});
});

// ---------------------------------------------------------------------------
// T2 — trace continuity: thread the TurnTracer through the three injection
// shapes (design record
// docs/designs/observability/compass-agent-message-trace-continuity/design.md §T2).
//
// These tests exercise the REAL bridge (createTraceBridge) against a real
// in-memory OTel provider — the same house recipe trace-bridge.test.ts uses (a
// NodeTracerProvider installs a context manager so `context.with` propagates
// into the wrapped prompt). The recording agent below MODELS the SDK's
// synchronous span start: `prompt()` starts an `invoke_agent` span reading
// `context.active()` (so a `runWithParent` wrapper makes the remote context its
// PARENT) and fires the bridge's `onSpanStart` hook, exactly as the loop does
// inside `prompt()` before its first await. The idle-steer-parent test canaries
// the MODELED contract: it pins that `runWithParent` wraps the (modeled)
// synchronous prompt so the remote context is the turn span's PARENT — dropping
// the wrap, or an await slipping in before the fake's span start, reddens it.
// It does NOT guard the REAL SDK's synchronicity (the fake starts the span
// synchronously by construction); that property — startInvokeAgentSpan runs
// before the loop's first await (agent-loop.ts:692, ahead of runInActiveSpan at
// :696) — belongs to a separate real-`prompt()` integration assertion.

const TP_TRACE_ID = "0af7651916cd43dd8448eb211c80319c";
const TP_SPAN_ID = "b7ad6b7169203331";
const TP_HEADER = `00-${TP_TRACE_ID}-${TP_SPAN_ID}-01`;
const TP_TRACE_ID_2 = "4bf92f3577b34da6a3ce929d0e0e4736";
const TP_SPAN_ID_2 = "00f067aa0ba902b7";
const TP_HEADER_2 = `00-${TP_TRACE_ID_2}-${TP_SPAN_ID_2}-01`;

let traceProvider: NodeTracerProvider | undefined;

afterEach(async () => {
	trace.disable();
	context.disable();
	await traceProvider?.shutdown();
	traceProvider = undefined;
});

function traceHookCtx(
	span: Span,
	kind: TelemetrySpanKind,
	agent: AgentIdentity | undefined,
): TelemetryHookContext {
	return { span, kind, agent, model: undefined, conversationId: undefined };
}

// A CompassAgent wired to the REAL bridge + a recording, span-aware session.
// `prompt()` starts an `invoke_agent` span in the active context (so
// `runWithParent` parents it on the remote header) and fires `onSpanStart`;
// `endTurn()` ends that span (exporting it) and fires `onSpanEnd`. The held-open
// control source keeps run() parked, exactly like startDeliverAgent.
function startTracedAgent() {
	const exporter = new InMemorySpanExporter();
	traceProvider = new NodeTracerProvider({
		spanProcessors: [new SimpleSpanProcessor(exporter)],
	});
	traceProvider.register();
	const bridge = createTraceBridge();

	const frames: OutboundFrame[] = [];
	const unmapped: UnmappedEvent[] = [];
	const prompts: string[] = [];
	const steers: AgentMessage[] = [];
	const state = { tools: [] as AgentTool[], isStreaming: false };
	let currentSpan: Span | undefined;

	const startInvokeAgentSpan = (): void => {
		// Faithful to the loop: the span starts SYNCHRONOUSLY inside prompt(),
		// reading context.active() as its parent (agent-loop.ts:691-692), and the
		// capture hook fires for it (agent === undefined ⇒ main turn).
		const span = trace.getTracer("test").startSpan("invoke_agent");
		currentSpan = span;
		bridge.onSpanStart(traceHookCtx(span, "invoke_agent", undefined));
	};
	const agent = {
		prompt(input: string): Promise<void> {
			if (state.isStreaming) return Promise.reject(new AgentBusyError());
			startInvokeAgentSpan();
			state.isStreaming = true;
			prompts.push(input);
			return Promise.resolve();
		},
		steer(m: AgentMessage): void {
			steers.push(m);
		},
		appendMessage(): void {},
		setSystemPrompt(): void {},
		setTools(): void {},
		state,
	};
	// A feedable control source (mirrors startControlAgent): parks awaiting
	// `notify` when the queue drains, so with no feed it behaves exactly like the
	// held-open source the other traced tests rely on; `feed` enqueues a control
	// op (e.g. a control prompt) and drains the pull+apply microtasks.
	const queue: AgentControl[] = [];
	let notify: (() => void) | undefined;
	let closed = false;
	const control: ControlSource = {
		async *[Symbol.asyncIterator]() {
			while (true) {
				while (queue.length > 0) {
					const next = queue.shift();
					if (next !== undefined) yield next;
				}
				if (closed) return;
				await new Promise<void>((resolve) => {
					notify = resolve;
				});
			}
		},
	};
	let listener: AgentSessionEventListener | undefined;
	const session = {
		agent,
		get isStreaming(): boolean {
			return state.isStreaming;
		},
		waitForIdle(): Promise<void> {
			return Promise.resolve();
		},
		subscribe(fn: AgentSessionEventListener): () => void {
			listener = fn;
			return () => {};
		},
	};
	const compass = new CompassAgent({
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
		tracer: bridge,
	});
	const done = compass.run();
	const drive = (event: AgentSessionEvent): void => {
		if (event.type === "agent_end") state.isStreaming = false;
		listener?.(event);
	};
	// End the live turn span (exports it) and fire the matching onSpanEnd, so a
	// test can read the recorded links/attributes/parent off the exporter.
	const endTurn = (): void => {
		if (currentSpan === undefined) return;
		bridge.onSpanEnd(traceHookCtx(currentSpan, "invoke_agent", undefined));
		currentSpan.end();
		currentSpan = undefined;
	};
	// Enqueue a control op and drain the pull+apply microtasks (mirrors
	// startControlAgent.feed).
	const feed = async (c: AgentControl): Promise<void> => {
		queue.push(c);
		notify?.();
		notify = undefined;
		await tick();
		await tick();
	};
	const close = async (): Promise<void> => {
		closed = true;
		notify?.();
		await done;
	};
	return {
		agent: compass,
		prompts,
		steers,
		frames,
		unmapped,
		exporter,
		drive,
		endTurn,
		feed,
		close,
	};
}

// The single exported invoke_agent span (there is exactly one per traced test).
function exportedTurnSpan(exporter: InMemorySpanExporter) {
	const spans = exporter
		.getFinishedSpans()
		.filter((s) => s.name === "invoke_agent");
	expect(spans).toHaveLength(1);
	return spans[0];
}

describe("CompassAgent — T2 trace continuity (message → turn topology)", () => {
	test("idle steer with a valid traceparent PARENTS the turn span on the remote context (modeled runWithParent-wrap contract)", async () => {
		const h = startTracedAgent();
		h.agent.steer(deliverMsg("m1", "hi"), "", TP_HEADER);
		await tick();
		h.endTurn();
		const span = exportedTurnSpan(h.exporter);
		// PARENT, not link: the remote context is the turn span's true parent.
		// The canary — if `runWithParent` did not wrap the SYNCHRONOUS prompt (an
		// SDK await inserted before startInvokeAgentSpan, or the wrap dropped), the
		// span comes out rootless and both assertions redden.
		expect(span?.parentSpanContext?.traceId).toBe(TP_TRACE_ID);
		expect(span?.parentSpanContext?.spanId).toBe(TP_SPAN_ID);
		// No link (parentage, not a link) and the id is stamped as the query key.
		expect(span?.links).toHaveLength(0);
		expect(span?.attributes["compass.message.ids"]).toBe("m1");
		await h.close();
	});

	test("N=1 deliver flush PARENTS the turn span on that message's traceparent", async () => {
		const h = startTracedAgent();
		// Idle deliver of a single message flushes at once with N=1 → parent.
		h.agent.deliver(deliverMsg("m1", "one"), "", TP_HEADER);
		await tick();
		h.endTurn();
		const span = exportedTurnSpan(h.exporter);
		expect(span?.parentSpanContext?.traceId).toBe(TP_TRACE_ID);
		expect(span?.parentSpanContext?.spanId).toBe(TP_SPAN_ID);
		expect(span?.links).toHaveLength(0);
		expect(span?.attributes["compass.message.ids"]).toBe("m1");
		await h.close();
	});

	test("N=2 deliver flush LINKS both messages (no parent) with per-message ids", async () => {
		const h = startTracedAgent();
		// A live turn: both delivers queue and coalesce to the agent_end flush.
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "one"), "", TP_HEADER);
		h.agent.deliver(deliverMsg("m2", "two"), "", TP_HEADER_2);
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		h.endTurn();
		const span = exportedTurnSpan(h.exporter);
		// LINKS, not a parent: a multi-message batch has no single causal parent.
		// A parent-vs-link regression (wrapping the N>1 prompt in runWithParent)
		// would give a parentSpanContext and 0 links — this reddens.
		expect(span?.parentSpanContext).toBeUndefined();
		expect(span?.links).toHaveLength(2);
		const linkByMsg = new Map(
			(span?.links ?? []).map((l) => [
				l.attributes?.["compass.message.id"],
				l.context,
			]),
		);
		expect(linkByMsg.get("m1")?.traceId).toBe(TP_TRACE_ID);
		expect(linkByMsg.get("m1")?.spanId).toBe(TP_SPAN_ID);
		expect(linkByMsg.get("m2")?.traceId).toBe(TP_TRACE_ID_2);
		expect(linkByMsg.get("m2")?.spanId).toBe(TP_SPAN_ID_2);
		// The comma-joined ids are stamped as the topology-independent query key.
		expect(span?.attributes["compass.message.ids"]).toBe("m1,m2");
		await h.close();
	});

	test("mid-turn steer LINKS onto the LIVE turn span, not a new one", async () => {
		const h = startTracedAgent();
		// Start a turn with an idle steer (no traceparent → root turn span).
		h.agent.steer(deliverMsg("m0", "start"), "", "");
		await tick();
		// Now a mid-turn steer (session streaming): links onto the SAME live span.
		h.agent.steer(deliverMsg("m1", "interrupt"), "", TP_HEADER);
		await tick();
		h.endTurn();
		const span = exportedTurnSpan(h.exporter);
		// The mid-turn steer injected into the running loop (a recorded steer),
		// NOT a new prompt — so exactly one turn span exists and it carries the
		// link. If mid-turn were mis-routed to a new-turn PARENT this reddens
		// (two spans, or a parent instead of a link).
		expect(h.steers).toHaveLength(1);
		expect(span?.links).toHaveLength(1);
		expect(span?.links[0]?.attributes?.["compass.message.id"]).toBe("m1");
		expect(span?.links[0]?.context.traceId).toBe(TP_TRACE_ID);
		expect(span?.links[0]?.context.spanId).toBe(TP_SPAN_ID);
		// The query key ACCUMULATES across the turn: m0 STARTED the turn and m1 fed
		// it mid-turn, so both must be answerable by attribute regardless of
		// topology (m0 is the parent, m1 is a link). A delta-only stamp
		// (`stampActiveTurn(msg.id)`) would overwrite to "m1" and drop m0 — this
		// assertion reddens on that regression (design.md:199-200).
		expect(span?.attributes["compass.message.ids"]).toBe("m0,m1");
		await h.close();
	});

	test("empty traceparent yields no parent and no link, and does not throw", async () => {
		const h = startTracedAgent();
		expect(() => h.agent.steer(deliverMsg("m1", "hi"), "", "")).not.toThrow();
		await tick();
		h.endTurn();
		const span = exportedTurnSpan(h.exporter);
		// Empty header ⇒ parse fails ⇒ the turn runs as a ROOT (no parent), no link.
		expect(span?.parentSpanContext).toBeUndefined();
		expect(span?.links).toHaveLength(0);
		await h.close();
	});

	test("a control prompt starts a fresh turn and resets the id accumulator (no prior-turn leak)", async () => {
		const h = startTracedAgent();
		// Turn 1: an idle steer seeds the accumulator with m0 and leaves it that
		// way — the empty agent_end flush returns early WITHOUT resetting it, so m0
		// survives into the next turn-start unless that site clears it.
		h.agent.steer(deliverMsg("m0", "first"), "", TP_HEADER);
		await tick();
		// End turn 1 (clears isStreaming/turnActive so the control prompt can
		// start). Its span is never ended here, so only turn 2's span is exported.
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		// Turn 2: a CONTROL prompt (the fourth prompt-driven turn-start). It must
		// reset the accumulator like every other turn-start site.
		await h.feed({ kind: "replayComplete" });
		await h.feed({ kind: "prompt", input: "go" });
		// A mid-turn steer feeds m1 onto the live control-prompt turn.
		h.agent.steer(deliverMsg("m1", "interrupt"), "", TP_HEADER);
		await tick();
		h.endTurn();
		const span = exportedTurnSpan(h.exporter);
		// Only m1 fed THIS turn, so the topology-independent query key is "m1".
		// If the control-prompt case omitted the accumulator reset, m0 from turn 1
		// would leak → "m1" becomes "m0,m1" and this reddens (design.md:199-200).
		expect(span?.attributes["compass.message.ids"]).toBe("m1");
		await h.close();
	});
});

describe("CompassAgent — T2 tracer absent is bit-identical to today", () => {
	// Drive the SAME deliver script (mid-turn coalesce of two, then an idle
	// single) against a no-tracer agent and a real-bridge agent, and assert the
	// observable frame emission — kind sequence, acks, injections, prompts — is
	// IDENTICAL. The tracer only touches spans, never frames, so any frame delta
	// would be a regression (an errant emit, a reordered/dropped ack or
	// injection). The bridge run needs a provider registered for context.with.
	async function runDeliverScript(tracer?: TraceBridge) {
		const h = startDeliverAgent([], tracer);
		h.drive({ type: "agent_start" } as AgentSessionEvent);
		h.agent.deliver(deliverMsg("m1", "one"), "h1", TP_HEADER);
		h.agent.deliver(deliverMsg("m2", "two"), "h2", TP_HEADER_2);
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		h.agent.deliver(deliverMsg("m3", "three"), "h3", TP_HEADER);
		h.drive({ type: "agent_end" } as AgentSessionEvent);
		await tick();
		await h.close();
		return {
			kinds: h.frames.map((f) => f.kind),
			acks: ackIds(h.frames),
			injections: injections(h.frames),
			prompts: [...h.session.agent.prompts],
			unmapped: h.unmapped.map((u) => u.eventType),
		};
	}

	test("emitted frames, acks, injections, and prompts match a no-tracer construction", async () => {
		const withoutTracer = await runDeliverScript(undefined);
		traceProvider = new NodeTracerProvider();
		traceProvider.register();
		const withTracer = await runDeliverScript(createTraceBridge());
		expect(withTracer.kinds).toEqual(withoutTracer.kinds);
		expect(withTracer.acks).toEqual(withoutTracer.acks);
		expect(withTracer.injections).toEqual(withoutTracer.injections);
		expect(withTracer.prompts).toEqual(withoutTracer.prompts);
		expect(withTracer.unmapped).toEqual(withoutTracer.unmapped);
		// Non-vacuity: the script really produced acks + injections to compare.
		expect(withoutTracer.acks).toEqual(["m1", "m2", "m3"]);
	});
});
