// The socket ControlSource contract (transport-consolidation C4, inbound half):
// createSocketControlSource must consume the AgentGateway.Control server-stream
// over a REAL socket, dispatch each AgentControl by variant (representable →
// yielded on the iterable; immediate/empty-shell → counted-unmapped at decode),
// emit apply-then-ack ControlAck / ReplayCompleteAck frames onto the SAME
// ordered Publish spine the FrameSink uses, and reconnect (bounded) on a
// transport drop while ending cleanly on a Runner-initiated close. These tests
// stand up a live connect-node h2c server bound to a Unix socket and drive real
// control()/publishSpine() traffic through it: a mock would restate the source;
// only a live server catches a control op stuck behind a running turn, an ack
// emitted on receipt instead of apply, or a drop that terminates the session
// instead of reconnecting.
//
// NOTE (author-run, not an independent test agent): the wave's Tester spawn hit
// the frozen-session provisioning defect (the sub-delegating phantom), so these
// were authored by the implementer. Each case is verified non-vacuous by the
// mutation described in its header comment.

import { afterEach, expect, test } from "bun:test";
import * as fs from "node:fs";
import * as http2 from "node:http2";
import * as os from "node:os";
import * as path from "node:path";
import { create } from "@bufbuild/protobuf";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import type { AgentControl } from "../control";
import {
	AgentGateway,
	type PublishFrameRequest,
	PublishFrameResponseSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import {
	AgentControlSchema,
	type ControlAck,
	DeliverControlSchema,
	PromptControlSchema,
	ReplayCompleteSchema,
	SteerControlSchema,
	type AgentControl as WireAgentControl,
} from "../gen/compass/v1/agent_pb";
import {
	type Message,
	MessageBlockSchema,
	MessageSchema,
} from "../gen/compass/v1/comms_pb";
import type { UnmappedEvent } from "../mapping";
import {
	CONTROL_RECONNECT_NO_PROGRESS_MAX,
	createSocketControlSource,
} from "./control-source";
import { createUnixSocketTransport, type RunnerTransport } from "./index";

// One server + socket per test, torn down in afterEach so a failing case never
// leaks the socket file or a listening server into the next test.
let activeServer: http2.Http2Server | undefined;
let activeSocketPath: string | undefined;

afterEach(async () => {
	if (activeServer !== undefined) {
		const server = activeServer;
		await new Promise<void>((resolve) => server.close(() => resolve()));
		activeServer = undefined;
	}
	if (activeSocketPath !== undefined && fs.existsSync(activeSocketPath)) {
		fs.unlinkSync(activeSocketPath);
	}
	activeSocketPath = undefined;
});

// What a test server captures: the ack frames received on Publish, plus a
// per-connection count so a reconnect test can assert Control was re-opened.
interface Recorder {
	publishFrames: PublishFrameRequest[];
	controlOpens: number;
}

interface ServerHooks {
	// Drives the Control server-stream: yields the AgentControl ops to push, then
	// returns (clean close) or throws (transport drop). `open` is the 1-based
	// subscription count so a reconnect test can behave differently per open.
	// `signal` is the handler's own AbortSignal — it fires when the CLIENT
	// cancels the stream, which is how the M2 return()-aborts-the-stream test
	// observes cancellation reaching the server (an async generator parked on an
	// `await` cannot be force-returned, so its `finally` alone proves nothing).
	control(open: number, signal: AbortSignal): AsyncIterable<WireAgentControl>;
	// Awaited after each ack frame is recorded — lets a test gate on ack arrival.
	onPublish?(frame: PublishFrameRequest): Promise<void> | void;
}

async function serve(rec: Recorder, hooks: ServerHooks): Promise<string> {
	const socketPath = path.join(
		os.tmpdir(),
		`c4b-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
	);
	const adapter = connectNodeAdapter({
		routes(router) {
			router.rpc(AgentGateway.method.control, async function* (_req, ctx) {
				const open = ++rec.controlOpens;
				yield* hooks.control(open, ctx.signal);
			});
			router.rpc(AgentGateway.method.publish, async (stream) => {
				for await (const frame of stream) {
					rec.publishFrames.push(frame);
					if (hooks.onPublish) await hooks.onPublish(frame);
				}
				return create(PublishFrameResponseSchema, {});
			});
		},
	});
	const server = http2.createServer(adapter);
	activeServer = server;
	activeSocketPath = socketPath;
	await new Promise<void>((resolve) => server.listen(socketPath, resolve));
	return socketPath;
}

function emptyRecorder(): Recorder {
	return { publishFrames: [], controlOpens: 0 };
}

// No-op immediate handle — C4b never invokes it (empty-shell payloads, OQ-2(A)),
// but a test can pass a recording one to prove that.
function recordingImmediate(): {
	immediate: { steer(m: unknown): void; deliver(m: unknown): void };
	steers: unknown[];
	delivers: unknown[];
} {
	const steers: unknown[] = [];
	const delivers: unknown[] = [];
	return {
		immediate: {
			steer: (m) => steers.push(m),
			deliver: (m) => delivers.push(m),
		},
		steers,
		delivers,
	};
}

function promptOp(seq: bigint, input: string): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "prompt", value: create(PromptControlSchema, { input }) },
	});
}

function steerOp(seq: bigint): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "steer", value: create(SteerControlSchema, {}) },
	});
}

// A populated steer op: a SteerControl carrying a comms Message with an id and
// one text block (SEA-1310 §8 / SEA-1569 — the channel `@`-mention wire is no
// longer an empty shell). Mirrors deliverOp.
function populatedSteerOp(
	seq: bigint,
	id: string,
	text: string,
): WireAgentControl {
	const message: Message = create(MessageSchema, {
		id,
		blocks: [
			create(MessageBlockSchema, { block: { case: "text", value: text } }),
		],
	});
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: {
			case: "steer",
			value: create(SteerControlSchema, { message }),
		},
	});
}

// A populated deliver op: a DeliverControl carrying a comms Message with an id
// and one text block (SEA-1310 §8 — the wire is no longer an empty shell).
function deliverOp(seq: bigint, id: string, text: string): WireAgentControl {
	const message: Message = create(MessageSchema, {
		id,
		blocks: [
			create(MessageBlockSchema, { block: { case: "text", value: text } }),
		],
	});
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: {
			case: "deliver",
			value: create(DeliverControlSchema, { message }),
		},
	});
}

function replayCompleteOp(seq: bigint): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: {
			case: "replayComplete",
			value: create(ReplayCompleteSchema, {}),
		},
	});
}

// Read an ack frame's kind + payload off a captured PublishFrameRequest.
function ackOf(
	frame: PublishFrameRequest,
):
	| { kind: "controlAck"; value: ControlAck }
	| { kind: "replayCompleteAck" }
	| undefined {
	const f = frame.frame?.frame;
	if (f?.case === "controlAck") return { kind: "controlAck", value: f.value };
	if (f?.case === "replayCompleteAck") return { kind: "replayCompleteAck" };
	return undefined;
}

function deferred(): { promise: Promise<void>; resolve: () => void } {
	let resolve!: () => void;
	const promise = new Promise<void>((r) => {
		resolve = r;
	});
	return { promise, resolve };
}

// A Control stream that delivers ZERO ops and then drops (throws) — the
// quiet-flap shape the reconnect flap-detector tests are built on. Expressed as
// a plain async function returning an AsyncIterable rather than an `async
// function*`, because a generator whose body only throws never yields.
function dropsImmediately(): AsyncIterable<WireAgentControl> {
	return {
		[Symbol.asyncIterator]: () => ({
			next: () => Promise.reject(new Error("blip")),
		}),
	};
}

// Gates a test on the consumer demonstrably APPLYING an op, not merely on the
// op being pushed. Apply-then-ack means op N is acked when the consumer returns
// for N+1, so "the server yielded it" and "the agent applied it" are separated
// by an unpredictable number of microtasks — a progress-budget test that dropped
// the connection without waiting would race the ack and flake. The gate watches
// the ControlAck cursor on the Publish spine, which is the same signal the
// source's own reconnect budget reads.
//
// `starved` converts the failure mode into a NAMED one. A gate that never
// resolves parks the server generator, so the stream never drops, `collect`
// never settles, and the test would otherwise red as a bare suite timeout with
// no diagnostic — asymmetric with F2(b)/F2(c), which go to deliberate trouble to
// name their branch. Racing `starved` says "the source stopped acking" instead.
function ackGate(): {
	onPublish(frame: PublishFrameRequest): void;
	applied(seq: bigint): Promise<void>;
	starved(ms: number): Promise<void>;
} {
	let cursor = 0n;
	const waiters: { seq: bigint; resolve: () => void }[] = [];
	return {
		onPublish(frame) {
			const ack = ackOf(frame);
			if (ack?.kind !== "controlAck") return;
			if (ack.value.ackedSeq > cursor) cursor = ack.value.ackedSeq;
			for (let i = waiters.length - 1; i >= 0; i--) {
				const w = waiters[i] as { seq: bigint; resolve: () => void };
				if (w.seq <= cursor) {
					waiters.splice(i, 1);
					w.resolve();
				}
			}
		},
		applied(seq) {
			if (seq <= cursor) return Promise.resolve();
			return new Promise<void>((resolve) => {
				waiters.push({ seq, resolve });
			});
		},
		// Resolves only if a waiter is still parked `ms` after the last ack moved
		// the cursor — i.e. the source has gone quiet mid-script. Rearmed on every
		// advance, so a slow-but-progressing run never trips it.
		starved(ms) {
			return new Promise<void>((resolve) => {
				const tick = (): void => {
					const at = cursor;
					setTimeout(() => {
						if (waiters.length > 0 && cursor === at) resolve();
						else tick();
					}, ms).unref?.();
				};
				tick();
			});
		},
	};
}

// Drain the iterable into an array, stopping when it ends. Used by the
// clean-close tests where the stream returns after a fixed script.
async function collect(
	source: AsyncIterable<AgentControl>,
): Promise<AgentControl[]> {
	const out: AgentControl[] = [];
	for await (const op of source) out.push(op);
	return out;
}

test("representable ops are yielded on the iterable in order; a clean close ends it", async () => {
	// Non-vacuity: if the source dropped an op or reordered, the yielded kinds
	// would differ → red; if a clean close did not end the iterable, collect()
	// would hang (test timeout) → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield promptOp(2n, "hello");
			yield promptOp(3n, "world");
		},
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
	);
	const ops = await collect(source);
	expect(ops.map((o) => o.kind)).toEqual([
		"replayComplete",
		"prompt",
		"prompt",
	]);
	expect(ops[1]).toMatchObject({ kind: "prompt", input: "hello" });
	expect(ops[2]).toMatchObject({ kind: "prompt", input: "world" });
});

test("an empty-shell steer is counted-unmapped, not yielded, and immediate.* is not called (OQ-2 A)", async () => {
	// Non-vacuity: if the source fabricated a payload and called immediate.steer,
	// steers would be non-empty → red; if it yielded the steer, the iterable would
	// carry it → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield steerOp(2n);
			yield promptOp(3n, "after");
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const { immediate, steers, delivers } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
		{ onUnmapped: (u) => unmapped.push(u) },
	);
	const ops = await collect(source);
	// The steer never reaches the iterable; only barrier + prompt do.
	expect(ops.map((o) => o.kind)).toEqual(["replayComplete", "prompt"]);
	expect(steers).toEqual([]);
	expect(delivers).toEqual([]);
	// It IS surfaced as a counted unmapped op (staged, not dropped).
	const steerUnmapped = unmapped.find((u) => u.eventType === "control:steer");
	expect(steerUnmapped?.reason).toContain("payload staged");
});

test("a populated deliver decodes its Message and dispatches it through immediate.deliver (SEA-1310 §8)", async () => {
	// Non-vacuity: if decodeImmediatePayload still returned undefined for a
	// deliver, delivers would be empty and the op counted "payload staged" → red;
	// if the deliver were yielded on the iterable instead of dispatched
	// immediately, ops would carry it → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield deliverOp(2n, "msg-abc", "channel text");
			yield promptOp(3n, "after");
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const { immediate, delivers } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
		{ onUnmapped: (u) => unmapped.push(u) },
	);
	const ops = await collect(source);
	// The deliver is dispatched immediately, never yielded on the iterable.
	expect(ops.map((o) => o.kind)).toEqual(["replayComplete", "prompt"]);
	// The decoded comms Message (id intact) reached immediate.deliver.
	expect(delivers).toHaveLength(1);
	expect((delivers[0] as Message).id).toBe("msg-abc");
	// A populated deliver is NOT counted "payload staged" — it decoded fine.
	const staged = unmapped.find(
		(u) => u.eventType === "control:deliver" && u.reason.includes("staged"),
	);
	expect(staged).toBeUndefined();
});

test("a populated steer decodes its Message and dispatches it through immediate.steer (SEA-1310 §8)", async () => {
	// Non-vacuity: if decodeImmediatePayload returned undefined for a steer,
	// steers would be empty and the op counted "payload staged" → red; if the
	// steer were yielded on the iterable instead of dispatched immediately, ops
	// would carry it → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield populatedSteerOp(2n, "steer-abc", "mention text");
			yield promptOp(3n, "after");
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const { immediate, steers } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
		{ onUnmapped: (u) => unmapped.push(u) },
	);
	const ops = await collect(source);
	// The steer is dispatched immediately, never yielded on the iterable.
	expect(ops.map((o) => o.kind)).toEqual(["replayComplete", "prompt"]);
	// The decoded comms Message (id intact) reached immediate.steer.
	expect(steers).toHaveLength(1);
	expect((steers[0] as Message).id).toBe("steer-abc");
	// A populated steer is NOT counted "payload staged" — it decoded fine.
	const staged = unmapped.find(
		(u) => u.eventType === "control:steer" && u.reason.includes("staged"),
	);
	expect(staged).toBeUndefined();
});

test("a pre-ReplayComplete immediate op is refused by the barrier and counted (invariant 1)", async () => {
	// Non-vacuity: if the barrier were not enforced on the immediate path, the
	// reason would be the post-barrier "staged" text, not the refusal text → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield steerOp(1n); // before any replayComplete
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const { immediate, steers } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
		{ onUnmapped: (u) => unmapped.push(u) },
	);
	await collect(source);
	expect(steers).toEqual([]);
	const refused = unmapped.find((u) => u.eventType === "control:steer");
	expect(refused?.reason).toContain("refused by replay barrier");
});

test("a ReplayCompleteAck is emitted after replayComplete is applied", async () => {
	// Non-vacuity: if the source acked on mere receipt (not apply), or never
	// emitted the barrier ack, the assertion on the received frame → red.
	const rec = emptyRecorder();
	const gotAck = deferred();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield promptOp(2n, "next"); // consumer pulling this proves replayComplete applied
		},
		onPublish: (frame) => {
			if (ackOf(frame)?.kind === "replayCompleteAck") gotAck.resolve();
		},
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
	);
	// Drain in the background so the consumer pulls past replayComplete.
	const drained = collect(source);
	await gotAck.promise;
	await drained;
	const replayAck = rec.publishFrames.find(
		(f) => ackOf(f)?.kind === "replayCompleteAck",
	);
	expect(replayAck).toBeDefined();
});

test("ControlAck is apply-then-ack: an op received-but-not-pulled-past is NOT acked (P1 #6)", async () => {
	// The stream pushes three ops then parks OPEN (no clean close). The consumer
	// pulls exactly twice — past replayComplete(1) and prompt(2) — leaving
	// prompt(3) received by the pump but NOT yet pulled/applied. Apply-then-ack
	// means: seq 2 is acked (the pull for op 3 proves op 2 applied), seq 3 is NOT
	// (nothing has pulled past it).
	// Non-vacuity (mutation-verified): a source that acks a prompt on RECEIPT (in
	// the pump) acks seq 3 the moment it arrives — while the consumer never pulled
	// past it — so a seq-3 ControlAck appears → the "seq 3 never acked" assertion
	// reds. A source that never acks → the seq-2 gate never resolves → test hangs.
	const rec = emptyRecorder();
	const ackedTwo = deferred();
	const holdStream = deferred();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield promptOp(2n, "a");
			yield promptOp(3n, "b");
			await holdStream.promise; // park the stream open — no clean close
		},
		onPublish: (frame) => {
			const a = ackOf(frame);
			if (a?.kind === "controlAck" && a.value.ackedSeq >= 2n)
				ackedTwo.resolve();
		},
	});
	const transport = createUnixSocketTransport(socketPath);
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(transport, immediate);
	const it = source[Symbol.asyncIterator]();
	const first = await it.next(); // replayComplete(1)
	const second = await it.next(); // prompt(2) — the pull for op 2 does not ack it yet
	const third = it.next(); // pull for op 3 — proves op 2 applied → acks seq 2
	expect(first.value?.kind).toBe("replayComplete");
	expect(second.value?.kind).toBe("prompt");
	// Gate on the seq-2 ack reaching the server (race-free), then flush the spine
	// so any (erroneous) seq-3 ack would also have arrived.
	await ackedTwo.promise;
	await transport.publishSpine().drain();
	const ackedSeqs = rec.publishFrames.flatMap((f) => {
		const a = ackOf(f);
		return a?.kind === "controlAck" ? [Number(a.value.ackedSeq)] : [];
	});
	expect(Math.max(...ackedSeqs)).toBe(2); // seq 2 acked, seq 3 NOT (not pulled past)
	holdStream.resolve();
	await third.catch(() => undefined);
});

test("a transport drop reconnects (bounded) and does NOT end the iterable; a clean close does", async () => {
	// First Control open throws mid-stream (transport drop); the source must
	// re-open (controlOpens === 2) and keep the iterable alive, yielding the op
	// redelivered on the second open, then end cleanly.
	// Non-vacuity: if a drop ended the iterable (iterator-end === STOPPED), the
	// second op would never be yielded and controlOpens would stay 1 → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* (open) {
			if (open === 1) {
				yield replayCompleteOp(1n);
				throw new Error("transport drop"); // non-clean end
			}
			// Reconnect: Runner redelivers from the cursor. seq 1 already applied →
			// deduped; seq 2 is new.
			yield replayCompleteOp(1n);
			yield promptOp(2n, "after-reconnect");
		},
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
	);
	const ops = await collect(source);
	expect(rec.controlOpens).toBe(2);
	// The redelivered replayComplete (seq 1) is deduped, not re-yielded; only the
	// original replayComplete and the new prompt reach the consumer.
	expect(ops.map((o) => o.kind)).toEqual(["replayComplete", "prompt"]);
	expect(ops[1]).toMatchObject({ input: "after-reconnect" });
});

test("a redelivered already-QUEUED op is deduped and NOT re-acked, never re-yielded (amended OQ-6, :288)", async () => {
	// Redeliver seq 2 while it is still QUEUED (decoded + buffered, not yet pulled
	// past → unapplied). The :288 branch counts it "already queued", drops it, and
	// emits NO ack for it (an unapplied op is never acked). The prompt reaches the
	// iterable exactly once. A steer(4) sentinel decoded AFTER the dup gives a
	// dispatch-order signal that fires regardless of which dedup branch runs, so a
	// mutation reds an assertion instead of hanging a dedup-count gate.
	// Non-vacuity (mutation-verified): drop the `queued.has(seq)` dedup branch in
	// `dispatch` → the dup is no longer counted "already queued"
	// AND re-queues → the "counted exactly once as already-queued" assertion reds
	// (and, downstream, seq 2 would then yield a second prompt).
	const rec = emptyRecorder();
	const doRedeliver = deferred();
	const sentinelDispatched = deferred();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield promptOp(2n, "once");
			await doRedeliver.promise; // hold: consumer pulls past rc(1) but not prompt(2)
			yield promptOp(2n, "once"); // dup arrives while seq 2 is queued + unapplied
			yield steerOp(4n); // sentinel: decoded after the dup → proves it dispatched
			// clean close: the iterable ends after the single queued prompt is applied
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const transport = createUnixSocketTransport(socketPath);
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(transport, immediate, {
		onUnmapped: (u) => {
			unmapped.push(u);
			if (u.eventType === "control:steer") sentinelDispatched.resolve();
		},
	});
	const it = source[Symbol.asyncIterator]();
	const first = await it.next(); // replayComplete(1)
	const second = await it.next(); // prompt(2) — applies rc(1); seq 2 now queued+unapplied
	expect(first.value?.kind).toBe("replayComplete");
	expect(second.value?.kind).toBe("prompt");
	// Redeliver seq 2 while queued; the steer(4) sentinel decodes right after it, so
	// its unmapped count proves the dup was dispatched. Flush so any (erroneous) ack
	// for the dup would also have reached the server.
	doRedeliver.resolve();
	await sentinelDispatched.promise;
	await transport.publishSpine().drain();
	const controlAcks = rec.publishFrames.flatMap((f) => {
		const a = ackOf(f);
		return a?.kind === "controlAck" ? [a.value] : [];
	});
	// No ack names seq 2: the queued dup is dropped un-acked (only rc(1)'s ack{1,[]}
	// and the sentinel steer(4)'s ack{1,[4]} exist).
	expect(
		controlAcks.some((a) => a.ackedSeq >= 2n || a.appliedAbove.includes(2n)),
	).toBe(false);
	// Counted exactly once as an already-queued redelivery.
	expect(
		unmapped.filter(
			(u) =>
				u.eventType === "control:prompt" && u.reason.includes("already queued"),
		),
	).toHaveLength(1);
	// The prompt reaches the iterable exactly once: pull 3 applies the single queued
	// copy, then the clean close ends the iterable — the dup never yields again.
	const third = await it.next();
	expect(third.done).toBe(true);
});

test("a redelivered already-APPLIED op is deduped, RE-ACKED, never re-yielded (amended OQ-6, :285)", async () => {
	// Deterministically APPLY prompt(2) first (pull past it, gate on its
	// ControlAck), THEN redeliver seq 2. The :283 branch counts it "already
	// applied" AND re-acks via markApplied (:285) so the Runner retires the
	// retained op — a SECOND ControlAck for seq 2 is published — while the op is
	// never re-yielded. This is the Runner-retirement half the prior single test
	// never exercised.
	// Non-vacuity (mutation-verified): drop the `acks.markApplied(seq)` call in
	// `dispatch`'s immediate-op arm (the count line stays, so the dedup gate still resolves
	// — no hang) → no second seq-2 ControlAck → the "two seq-2 ControlAcks"
	// assertion reds.
	const rec = emptyRecorder();
	const readyToRedeliver = deferred();
	const dedupCounted = deferred();
	const firstApplied = deferred();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield promptOp(2n, "once");
			await readyToRedeliver.promise; // hold until prompt(2) is applied
			yield promptOp(2n, "once"); // dup arrives AFTER seq 2 is applied
			// clean close
		},
		onPublish: (frame) => {
			const a = ackOf(frame);
			if (a?.kind === "controlAck" && a.value.ackedSeq === 2n)
				firstApplied.resolve();
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const transport = createUnixSocketTransport(socketPath);
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(transport, immediate, {
		onUnmapped: (u) => {
			unmapped.push(u);
			if (u.reason.includes("already applied")) dedupCounted.resolve();
		},
	});
	const it = source[Symbol.asyncIterator]();
	const first = await it.next(); // replayComplete(1)
	const second = await it.next(); // prompt(2) — applies rc(1)
	const third = it.next(); // pull past prompt(2) → applies it → first seq-2 ControlAck
	expect(first.value?.kind).toBe("replayComplete");
	expect(second.value?.kind).toBe("prompt");
	await firstApplied.promise; // seq 2 is now applied (cursor === 2)
	// Redeliver seq 2 now that it is applied; gate on the "already applied" count
	// (fires whether or not the re-ack line survives), then flush the spine.
	readyToRedeliver.resolve();
	await dedupCounted.promise;
	await transport.publishSpine().drain();
	// Two ControlAcks name seq 2: the apply ack (pull 3) + the dup RE-ACK (:285).
	const seq2Acks = rec.publishFrames.flatMap((f) => {
		const a = ackOf(f);
		return a?.kind === "controlAck" && a.value.ackedSeq === 2n ? [a.value] : [];
	});
	expect(seq2Acks).toHaveLength(2);
	// Counted exactly once as an already-applied redelivery.
	expect(
		unmapped.filter(
			(u) =>
				u.eventType === "control:prompt" &&
				u.reason.includes("already applied"),
		),
	).toHaveLength(1);
	// The dup never reaches the iterable: the clean close ends it with no extra op.
	expect((await third).done).toBe(true);
});

test("appliedAbove: an immediate op applied ahead of an unfinished queued op carries a non-empty appliedAbove, then collapses (invariant 2)", async () => {
	// The mechanism behind invariant 2: an empty-shell steer(3) is applied at
	// decode (markApplied, :344) while prompt(2) is still queued+unapplied behind
	// the running turn, so the cursor cannot advance past 1 and the ack names 3 in
	// appliedAbove; pulling past prompt(2) then applies 2, the contiguous run
	// 1→2→3 prunes, and the cursor collapses to 3 with appliedAbove empty.
	// Held-open + manual-pull + gate on ack arrival (P1 #6 shape): we must observe
	// the intermediate {ackedSeq:1, appliedAbove:[3]} while prompt(2) is unapplied.
	// Acks flush on their own priority-lane batch as soon as enqueued, so gating on
	// onPublish proves arrival without draining (draining ends the spine, which
	// would drop the later collapse ack).
	//
	// Trace: pull rc(1) → nothing applied yet. pull prompt(2) → rc(1)'s ack block
	// runs (cursor→1); prompt(2) is lastYielded, unapplied. steer(3) decodes →
	// markApplied(3): 3>1 → #above={3}, prune finds no 2 → Ack{ackedSeq:1,
	// appliedAbove:[3]}. pull past prompt(2) → markApplied(2): 2>1 → #above={3,2}
	// → prune deletes 2 (cursor→2) then 3 (cursor→3) → Ack{ackedSeq:3,
	// appliedAbove:[]} — the collapse.
	// Non-vacuity (mutation-verified): break the prune loop in `AckCursor.markApplied`
	// (`this.#cursor + 1n` → `this.#cursor + 2n`) → the contiguous run never picks
	// up cursor+1, so markApplied(3) yields {ackedSeq:2, appliedAbove:[]} rather
	// than {ackedSeq:1, appliedAbove:[3]} → the intermediate appliedAbove
	// assertion reds (and the collapse to ackedSeq:3 never forms either).
	const rec = emptyRecorder();
	const holdStream = deferred();
	const gotAbove = deferred(); // seq-1 ack with 3 in appliedAbove reached the server
	const gotThirdAck = deferred(); // the third controlAck (the collapse) reached the server
	let controlAckCount = 0;
	const socketPath = await serve(rec, {
		control: async function* () {
			yield replayCompleteOp(1n);
			yield promptOp(2n, "running-turn");
			yield steerOp(3n); // empty-shell: applied at decode, ahead of queued prompt(2)
			await holdStream.promise; // park open — no clean close
		},
		onPublish: (frame) => {
			const a = ackOf(frame);
			if (a?.kind !== "controlAck") return;
			controlAckCount += 1;
			// Gate on ARRIVAL by count, never on ackedSeq/appliedAbove content — a
			// broken prune produces different content and would hang a content gate
			// forever, masking the defect. The content is asserted below, so a
			// mutation reds the assertion instead. Two controlAcks exist before we
			// pull past prompt(2): rc(1)'s ack and steer(3)'s out-of-order ack (in
			// either dispatch order); the third is the collapse from applying seq 2.
			if (controlAckCount === 2) gotAbove.resolve();
			if (controlAckCount === 3) gotThirdAck.resolve();
		},
	});
	const transport = createUnixSocketTransport(socketPath);
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(transport, immediate);
	const it = source[Symbol.asyncIterator]();
	const first = await it.next(); // replayComplete(1)
	const second = await it.next(); // prompt(2): applies rc(1); prompt(2) now queued+unapplied
	expect(first.value?.kind).toBe("replayComplete");
	expect(second.value?.kind).toBe("prompt");
	// steer(3) is applied at decode ahead of the unfinished prompt(2). Gate on the
	// resulting {ackedSeq:1, appliedAbove:[3]} ack, then assert that exact ack: the
	// cursor is pinned at 1 (seq 2 not yet applied) with seq 3 out of order above.
	await gotAbove.promise;
	const aboveAck = rec.publishFrames
		.flatMap((f) => {
			const a = ackOf(f);
			return a?.kind === "controlAck" ? [a.value] : [];
		})
		.find((v) => v.ackedSeq === 1n && v.appliedAbove.includes(3n));
	expect(aboveAck).toBeDefined();
	expect(aboveAck?.appliedAbove).toEqual([3n]); // seq 3 applied out of order above 1
	// Now pull past prompt(2): applying it makes 1→2→3 contiguous, so the cursor
	// collapses to 3 and appliedAbove empties. The third controlAck (FIFO) is that
	// collapse ack.
	const third = it.next();
	await gotThirdAck.promise;
	const controlAcks = rec.publishFrames.flatMap((f) => {
		const a = ackOf(f);
		return a?.kind === "controlAck" ? [a.value] : [];
	});
	const collapseAck = controlAcks[2];
	expect(collapseAck?.ackedSeq).toBe(3n); // cursor collapsed to the top of the run
	expect(collapseAck?.appliedAbove).toEqual([]); // contiguous run pruned 2 then 3
	holdStream.resolve();
	await third.catch(() => undefined);
});

test("a control_seq < 1 op is fail-closed (counted 'invalid control_seq < 1', dropped, not yielded, not acked) BEFORE the dedup path (M3)", async () => {
	// The Runner assigns strictly-positive 1-based control_seq; a seq-0 op (proto3
	// uint64 default) is a broken/0-based producer. The guard must sit BEFORE the
	// isApplied dedup: isApplied(0n) is 0n<=0n=true, so without the guard seq 0 is
	// swallowed as an "already-applied duplicate" (wrong reason) AND re-acked
	// (markApplied(0n) emits a ControlAck{ackedSeq:0}). With the guard it is
	// counted once with the invalid-seq reason and dropped un-acked.
	// Non-vacuity (mutation-verified): delete the `if (seq < 1n)` guard in
	// `dispatch` → seq 0 falls into isApplied(0n)=true → counted
	// "duplicate redelivered op — already applied" (NOT "invalid control_seq < 1")
	// and re-acked → the invalid-reason count assertion reds AND a seq-0 ControlAck
	// appears → the no-ack-0 assertion reds.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield promptOp(0n, "x"); // seq 0 — refused before dedup
			yield replayCompleteOp(1n);
			yield promptOp(2n, "after");
			// clean close
		},
	});
	const unmapped: UnmappedEvent[] = [];
	const transport = createUnixSocketTransport(socketPath);
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(transport, immediate, {
		onUnmapped: (u) => unmapped.push(u),
	});
	const ops = await collect(source);
	await transport.publishSpine().drain();
	// The seq-0 op never reaches the iterable; only the two valid ops do.
	expect(ops.map((o) => o.kind)).toEqual(["replayComplete", "prompt"]);
	expect(ops[1]).toMatchObject({ kind: "prompt", input: "after" });
	// Counted exactly once with the fail-closed reason on control:prompt.
	expect(
		unmapped.filter(
			(u) =>
				u.eventType === "control:prompt" &&
				u.reason.includes("invalid control_seq < 1"),
		),
	).toHaveLength(1);
	// It was NOT re-acked as a dup: no ControlAck names seq 0 (the only acks are
	// for the valid ops — cursor jumps 1→2, never 0).
	const controlAcks = rec.publishFrames.flatMap((f) => {
		const a = ackOf(f);
		return a?.kind === "controlAck" ? [a.value] : [];
	});
	expect(
		controlAcks.some((a) => a.ackedSeq === 0n || a.appliedAbove.includes(0n)),
	).toBe(false);
});

test("F2(a): a quiet-but-healthy session flapping MORE than the backoff length still survives — reset-on-open fires on every past-floor drop (M1)", async () => {
	// Opens 1..6 each yield ZERO ops then throw (a healthy connection that blips
	// before redelivering anything); open 7 yields the prompt then clean-closes.
	// Six flaps > the backoff length (4): the OLD reset-on-op-receipt would never
	// reset (zero ops delivered) and would climb the budget to a spurious fail at
	// open 5. The reset-on-open flap-detector resets `attempt` on every drop whose
	// connection stayed up past the floor, so all six reconnects fire at backoff[0]
	// and the prompt on open 7 is yielded.
	// The injected clock is a SETTABLE value, not an auto-advancing counter: the
	// server hook advances it to model each connection's real lifetime (6000ms >=
	// the 5000 floor) no matter how many times the source samples the clock. A
	// counter that bumped per CALL would instead pin the implementation's
	// sampling pattern — add a third `now()` read anywhere in the pump and the
	// modelled uptime would silently change meaning. Deterministic either way:
	// no real 5s wait.
	//
	// The advance happens at the START of the next open, not at the drop. Uptime
	// is stamped when the stream is ESTABLISHED (the response header), and the
	// header lands only once the handler has been entered — so an advance made
	// before throwing would be included in `openedAt` itself and measure an
	// elapsed of ZERO. Advancing on the following open attributes the 6000ms to
	// the connection that just ended, which is what the floor is asking about.
	// Non-vacuity (mutation-verified): replace the reset line in `pump`'s catch
	// (`if (established && now() - openedAt >= CONTROL_RECONNECT_MIN_UPTIME_MS)`)
	// with a no-op (never reset) → attempt climbs the schedule and the iterable
	// FAILS at open 5 → collect() rejects, the prompt is never yielded and
	// controlOpens stays 5 → red.
	const rec = emptyRecorder();
	// Advanced when the source OBSERVES a connection's header, not by the server:
	// uptime is stamped at establishment, so the advance has to land after that
	// stamp or it is absorbed into `openedAt` and the measured elapsed is zero.
	// `headerObserver` below wraps the transport and bumps the clock once per
	// open, immediately after the source's own onHeader has run.
	let t = 0;
	const socketPath = await serve(rec, {
		control: async function* (open) {
			if (open < 7) {
				throw new Error("blip"); // zero-op drop, connection was healthy
			}
			yield promptOp(1n, "survived");
			// clean close
		},
	});
	const { immediate } = recordingImmediate();
	const now = (): number => t;
	const source = createSocketControlSource(
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000; // the connection just established stayed up 6s
		}),
		immediate,
		{ onUnmapped: () => {}, now: now },
	);
	// Capture the outcome rather than letting a regression surface as a raw
	// transport rejection: under the never-reset mutation the source gives up at
	// open 5 and collect() rejects, which should red the NAMED "survived" assertion
	// below, not spray a ConnectError stack.
	const outcome = await collect(source).then(
		(ops) => ({ ended: "cleanly" as const, ops }),
		(err: unknown) => ({ ended: "failed" as const, err }),
	);
	expect(outcome.ended).toBe("cleanly"); // 6 flaps did NOT exhaust the budget
	expect(rec.controlOpens).toBe(7); // survived 6 flaps, re-opened each time
	const ops = outcome.ended === "cleanly" ? outcome.ops : [];
	expect(ops.map((o) => o.kind)).toEqual(["prompt"]);
	expect(ops[0]).toMatchObject({ kind: "prompt", input: "survived" });
});

test("F2(b): a rapid sub-floor flap still fails, bounded — the iterable rejects after exactly 5 opens (M1 infinite-loop guard)", async () => {
	// Every open yields nothing then drops immediately. The injected clock is a
	// SETTABLE value the server never advances, so every connection reports uptime
	// 0 < the 5000 floor → the flap never resets. `attempt` climbs
	// [50,200,800,2000] and after the schedule is exhausted the iterable fails:
	// initial open + 4 retries = exactly 5 opens. This is the guard that a dead
	// socket cannot spin forever. It pays the real backoff (~3.05s) once — the
	// actual schedule firing, not test overhead.
	// Non-vacuity (mutation-verified, two ways): (i) shrink the budget check
	// (`attempt >= CONTROL_RECONNECT_BACKOFF_MS.length` → `attempt >= 2`) → 3 opens
	// → the exactly-5 assertion reds; (ii) make the reset predicate always-true
	// (`attempt = 0` unconditionally) → the budget is never reached and the pump
	// spins forever — caught here as a NAMED assertion, not a suite timeout: the
	// server hook resolves `overBudget` the moment a 6th open arrives, and the race
	// below turns that into "opened 6 times — unbounded reconnect spin".
	// The surfaced error text is not pinned (connect renders a pre-first-message
	// server throw as a protocol error — an artifact of the wire, not this
	// source's contract). What IS pinned: the iterable FAILS rather than ending
	// cleanly or spinning, after exactly the budgeted number of opens.
	const rec = emptyRecorder();
	const overBudget = deferred();
	const socketPath = await serve(rec, {
		control: (open) => {
			// A 6th open can only happen if the budget never terminates the climb.
			if (open > 5) overBudget.resolve();
			return dropsImmediately(); // sub-floor drop, every open
		},
	});
	const { immediate } = recordingImmediate();
	const now = (): number => 0; // never advances → uptime 0 << 5000 floor
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
		{ onUnmapped: () => {}, now: now },
	);
	const settled = collect(source).then(
		(ops) => ({ ended: "cleanly" as const, ops }),
		(err: unknown) => ({ ended: "failed" as const, err }),
	);
	const outcome = await Promise.race([
		settled,
		overBudget.promise.then(() => ({ ended: "spinning" as const })),
	]);
	// "spinning" = a 6th open landed → the reconnect budget is not bounded.
	expect(outcome.ended).toBe("failed"); // definitive fail, not clean STOPPED, not a spin
	expect(rec.controlOpens).toBe(5); // initial + 4 bounded retries, then gives up
}, 15000);

test("F2(c): a SLOW-failing socket terminates — past-floor drops reset the backoff every time, so the NO-PROGRESS budget is what bounds it", async () => {
	// The gap F2(a)/F2(b) leave open: the reset-on-open flap-detector clears
	// `attempt` on ANY drop from a connection that outlived the min-uptime floor.
	// A socket that is accepted, stays up past the floor, and THEN fails (a
	// wedged Runner, a server-side deadline, an idle timeout) therefore resets
	// the climb on every attempt and the backoff ladder is never reached —
	// reconnecting forever. Reproduced against a live server on the PRODUCTION
	// clock: 8 opens in 41s and still going.
	//
	// A reconnect-RATE window cannot bound this, which is why the budget counts
	// PROGRESS instead. Every connection here outlives the floor, so the reset
	// fires every time and the ladder delay is always backoff[0]; the budget
	// terminates it anyway, because none of these connections ever delivers an
	// op the agent applies. Crucially the bound is INDIFFERENT to connection
	// lifetime — the clock advance below is 6s, but making it 6 minutes changes
	// nothing, which is exactly what a wall-window could not say.
	//
	// Non-vacuity (mutation-verified): delete the budget check
	// (`noProgress >= CONTROL_RECONNECT_NO_PROGRESS_MAX` → `false`) → the pump
	// spins forever, caught HERE as a named assertion rather than a suite
	// timeout via the same over-budget race F2(b) uses → red.
	//
	// Isolation, and its ONE deliberate exception: deleting the budget check
	// reds exactly this test, so a red here names its branch. But deleting the
	// reset-on-open line itself also reds F2(a) and F2(d). That is structural,
	// not sloppy coupling: the reset is this test's PREMISE. With it gone the
	// 4-entry backoff ladder is exhausted at attempt 4, long before the budget
	// can bound anything, so the scenario "resets every drop, the budget
	// terminates it" is not constructible at all. Decoupling would mean
	// injecting the backoff schedule — production surface added purely for a
	// test — so the dependency is recorded instead. If all three red at once,
	// suspect the reset line; F2(a) is the test that names it.
	const rec = emptyRecorder();
	const overBudget = deferred();
	let t = 0;
	const socketPath = await serve(rec, {
		control: (open) => {
			// An 11th open can only happen if the budget does not terminate it.
			if (open > CONTROL_RECONNECT_NO_PROGRESS_MAX) overBudget.resolve();
			return dropsImmediately();
		},
	});
	const { immediate } = recordingImmediate();
	const now = (): number => t;
	const source = createSocketControlSource(
		// Each connection lasted 6s — past the 5000 floor, so the reset fires and
		// `attempt` returns to 0 on every drop. Advanced when the source observes
		// the header (uptime is stamped at establishment), never by its sampling.
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000;
		}),
		immediate,
		{ onUnmapped: () => {}, now: now },
	);
	const settled = collect(source).then(
		(ops) => ({ ended: "cleanly" as const, ops }),
		(err: unknown) => ({ ended: "failed" as const, err }),
	);
	const outcome = await Promise.race([
		settled,
		overBudget.promise.then(() => ({ ended: "spinning" as const })),
	]);
	// "spinning" = the budget never terminated a socket that resets every drop.
	expect(outcome.ended).toBe("failed");
	expect(rec.controlOpens).toBe(CONTROL_RECONNECT_NO_PROGRESS_MAX);
}, 15000);

test("F2(d): the SAME drop shape as F2(c) survives indefinitely once ops are APPLIED — progress, not rate, is what separates them", async () => {
	// The other half of the budget's contract, and the case a reconnect-RATE
	// window provably could not express. This test is F2(c) with one variable
	// changed: identical connection lifetimes, identical clock advance,
	// identical drop-every-connection shape — the only difference is that each
	// connection delivers an op the consumer applies. F2(c) dies at open 10;
	// this one runs 15 drops past the budget and still delivers. No wall-clock
	// threshold can tell those two sessions apart, because their reconnect rates
	// are the same; progress tells them apart trivially.
	//
	// Non-vacuity (mutation-verified): make the progress test an unconditional
	// increment (`noProgress = noProgress + 1` regardless of
	// `applied > appliedAtLastDrop`) → progress stops resetting the budget, it
	// fills at drop 10 and the source fails → red, and measured to red EXACTLY
	// this test out of the four F2 cases. That mutation is the old rate-only
	// behavior in miniature: it is the defect this replaced.
	//
	// Isolation: the reset-on-open line is the shared premise here as in F2(c)
	// — without it the 4-entry ladder ends the session at open 5, before
	// progress can be demonstrated at all. See F2(c)'s note.
	const rec = emptyRecorder();
	const gate = ackGate();
	// 16 opens: 15 that deliver an op, have it APPLIED, then drop; then a clean
	// close. 15 > NO_PROGRESS_MAX, so a budget that ignored progress would fail.
	const totalOpens = 16;
	let t = 0;
	const socketPath = await serve(rec, {
		control: async function* (open) {
			if (open === totalOpens) return; // clean close ends the iterable
			// Seqs are CONTIGUOUS from 1: the ack cursor only advances through a
			// contiguous run, so a gapped script would never move it and the gate
			// below would never fire.
			const seq = BigInt(open);
			yield promptOp(seq, `op-${open}`);
			// Wait for the consumer to actually APPLY it — apply-then-ack, so the
			// ack lands when the consumer comes back for the next op — then drop.
			// Gating on the ack rather than a timer is what makes "the drop
			// happens after progress" deterministic instead of a race.
			await gate.applied(seq);
			throw new Error("blip after progress");
		},
		onPublish: (frame) => gate.onPublish(frame),
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		// Same 6s-per-connection clock as F2(c): past the floor, so the ladder
		// resets on every drop and cannot be what bounds (or spares) this session.
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000;
		}),
		immediate,
		{ onUnmapped: () => {}, now: () => t },
	);
	const settled = collect(source).then(
		(ops) => ({ ended: "cleanly" as const, ops }),
		(err: unknown) => ({ ended: "failed" as const, err }),
	);
	const outcome = await Promise.race([
		settled,
		// "never-acked" = the source stopped acking mid-script, so the server is
		// parked on a gate that will never open. Named here rather than left to
		// surface as an unexplained 30s suite timeout.
		gate.starved(2000).then(() => ({ ended: "never-acked" as const })),
	]);
	expect(outcome.ended).toBe("cleanly");
	expect(rec.controlOpens).toBe(totalOpens);
	// 15 applied ops across 15 drops — half again the budget, never killed.
	// Asserting the PAYLOADS, not just the kinds: the arity is the property this
	// test exists for, but pinning `op-N` in order buys exactly-once and ordering
	// across 15 reconnects for free — a source that re-yielded one redelivery and
	// dropped another would satisfy a kinds-only assertion unchanged.
	const ops = outcome.ended === "cleanly" ? outcome.ops : [];
	expect(ops.map((o) => (o.kind === "prompt" ? o.input : o.kind))).toEqual(
		Array.from({ length: totalOpens - 1 }, (_, i) => `op-${i + 1}`),
	);
}, 30000);

test("a long apply survives >budget socket flaps — an op in flight is progress (SEA-1540)", async () => {
	// The latent kill this fix closes. The source is apply-then-ack and its
	// single consumer (CompassAgent's control loop) awaits the WHOLE turn before
	// pulling the next op, so while a long turn applies op N the ack cursor
	// CANNOT advance — op N is acked only when the consumer returns for N+1.
	// This test models exactly that: the consumer pulls ONE op and then HOLDS
	// it (never pulls again), so appliedCount is frozen at 0 and the op stays in
	// flight, while the Control socket flaps MORE than CONTROL_RECONNECT_NO_PROGRESS_MAX
	// times — each reopen redelivering the same op, deduped as already-queued so
	// nothing new is ever applied. F2(c) is this exact drop shape with NOTHING
	// in flight and dies at open 10; the only difference here is that an op is
	// mid-apply, which is progress, so the session must SURVIVE past the budget.
	//
	// Non-vacuity (mutation-verified): revert the production progress calc to
	// appliedCount-only — drop the `|| applyInFlight` arm so
	// `madeProgress = applied > appliedAtLastDrop` — and this test goes RED: with
	// nothing applied and the in-flight arm gone, `noProgress` climbs one per
	// drop and the source `buffer.fail`s at open 10, so it never re-opens past
	// the budget, `overBudget` never fires, and the opens-stalled detector wins
	// the race → "failed". That reverted calc IS the SEA-1540 defect.
	//
	// Isolation: like F2(c)/F2(d) the reset-on-open line is the shared premise —
	// every connection outlives the min-uptime floor (t += 6000 per header), so
	// the backoff ladder resets on every drop and ONLY the no-progress budget is
	// under test. See F2(c)'s note.
	const rec = emptyRecorder();
	const overBudget = deferred();
	const pulledOp1 = deferred();
	const release = deferred();
	let t = 0;
	const socketPath = await serve(rec, {
		control: async function* (open) {
			// An open past the budget can only happen if the in-flight arm spared
			// the session — the whole point of the test.
			if (open > CONTROL_RECONNECT_NO_PROGRESS_MAX) overBudget.resolve();
			// Every open (re)delivers the SAME op 1: genuinely queued on open 1,
			// deduped as already-queued on every reopen (:303), so appliedCount
			// never advances and the source's only progress signal is that op 1 is
			// in flight.
			yield promptOp(1n, "op-1");
			// On the FIRST open, drop only AFTER the consumer has pulled op 1, so
			// applyInFlight is true from the very first budget check. Later opens
			// need no gate: the consumer holds op 1 for the whole test, so
			// applyInFlight stays true across every drop.
			if (open === 1) await pulledOp1.promise;
			throw new Error("blip mid-apply");
		},
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		// Same 6s-per-connection clock as F2(c)/F2(d): past the floor, so the
		// ladder resets on every drop and cannot be what bounds (or spares) this
		// session — only the no-progress budget can.
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000;
		}),
		immediate,
		{ onUnmapped: () => {}, now: () => t },
	);
	// A consumer that pulls ONE op and then HOLDS it — a long apply / long turn.
	// It never pulls again, so it never acks op 1 (appliedCount stays 0) and the
	// op stays in flight for the whole flap storm.
	const it = source[Symbol.asyncIterator]();
	const held = (async () => {
		await it.next();
		pulledOp1.resolve();
		await release.promise;
		await it.return?.();
	})();
	// The RED-case detector, named rather than left to surface as a suite
	// timeout (the discipline F2(c)/F2(d) follow, mirroring ackGate.starved). If
	// the reverted calc fails the source at the budget, it stops re-opening
	// Control, so `controlOpens` freezes at or below the budget: an unchanged
	// count across a full window, while still <= the budget, is that terminal
	// state. In the GREEN case the count climbs past the budget every
	// ~backoff[0]ms and `overBudget` wins the race long before any window
	// elapses unchanged.
	//
	// Real setTimeout (not fake timers) is deliberate here, as in ackGate: this
	// is a live-socket integration test whose RED terminal state — buffer.fail()
	// with no puller parked — emits no promise or event to await, and the source
	// drives its own reconnects on real Node timers we do not control. Polling
	// the one observable (controlOpens) over a real window is the only signal;
	// fake timers cannot advance the source's out-of-test reconnect clock.
	const opensStalled = (ms: number): Promise<void> =>
		new Promise<void>((resolve) => {
			const tick = (): void => {
				const at = rec.controlOpens;
				setTimeout(() => {
					if (
						rec.controlOpens === at &&
						at <= CONTROL_RECONNECT_NO_PROGRESS_MAX
					)
						resolve();
					else tick();
				}, ms).unref?.();
			};
			tick();
		});
	const outcome = await Promise.race([
		overBudget.promise.then(() => "survived" as const),
		opensStalled(1000).then(() => "failed" as const),
	]);
	expect(outcome).toBe("survived");
	// Re-opened PAST the budget with a single op in flight the whole time and
	// nothing ever applied — the exact case appliedCount-only would have killed.
	expect(rec.controlOpens).toBeGreaterThan(CONTROL_RECONNECT_NO_PROGRESS_MAX);
	release.resolve();
	await held;
}, 15000);

test("F3: abandoning the for-await (iterator return()) aborts the pump AND cancels the Control server-stream (M2)", async () => {
	// The server holds the stream open after rc(1) and parks on its OWN handler
	// AbortSignal, which connect fires when the client cancels the RPC. return()
	// must abort.abort() — cancelling the { signal } threaded into
	// transport.control() — so that cancellation reaches the server; and it must
	// NOT reconnect (the aborted pump returns quietly), so controlOpens stays 1.
	// Parking on ctx.signal rather than a generator `finally` is deliberate: an
	// async generator suspended at an `await` cannot be force-returned, so its
	// finally would not run on cancellation and would prove nothing either way.
	// Non-vacuity (mutation-verified): make the iterator's `return()` skip
	// `abort.abort()` (leave only buffer.close()) → the Connect stream
	// stays open, the server's signal never fires → serverCancelled loses the race
	// and the bounded timer rejects → red (an assertion, not a suite hang).
	const rec = emptyRecorder();
	const serverCancelled = deferred();
	const socketPath = await serve(rec, {
		control: async function* (_open, signal) {
			yield replayCompleteOp(1n);
			// Hold the stream open until the client cancels the RPC.
			await new Promise<void>((resolve) => {
				if (signal.aborted) resolve();
				else signal.addEventListener("abort", () => resolve(), { once: true });
			});
			serverCancelled.resolve();
		},
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
	);
	const it = source[Symbol.asyncIterator]();
	const first = await it.next(); // pull rc(1) — the pump is now live on the stream
	expect(first.value?.kind).toBe("replayComplete");
	// The iterator MUST implement return() — that optional protocol member is
	// exactly what JS calls on an abandoned `for await`, and it is where M2 hangs
	// the abort. Asserted, not `!`-asserted away: a source that dropped it would
	// silently leak the pump.
	const ret = await it.return?.();
	expect(ret).toEqual({ value: undefined, done: true });
	// Prove the cancellation reached the server. Real bounded timer
	// (ts-no-test-timers exception): the awaited signal is an HTTP/2 stream
	// cancellation propagating over a real socket into connect's handler context —
	// there is no injectable clock on that path, and the timer exists solely to
	// convert a broken-abort HANG into a named assertion failure. It is cleared
	// the moment the cancellation lands, so a passing run waits zero extra time.
	const guard = new Promise<never>((_, reject) => {
		const t = setTimeout(
			() =>
				reject(
					new Error(
						"server stream not cancelled within 2s — return() did not abort the Control stream",
					),
				),
			2000,
		);
		void serverCancelled.promise.then(() => clearTimeout(t));
	});
	await Promise.race([serverCancelled.promise, guard]);
	expect(rec.controlOpens).toBe(1); // the aborted pump did not reconnect
});

// ---------------------------------------------------------------------------
// Abort-branch observation seam (F4/F5).
//
// Once the consumer abandons the iterable, an aborted pump touches NOTHING the
// helpers above can see: it dispatches no op, emits no ack, and both
// buffer.close() and buffer.fail() are no-ops after return() already closed the
// buffer. So the terminal-state assertions the earlier tests lean on cannot
// discriminate the abort branches at all — which is precisely why those
// branches survived deletion with the suite green.
//
// What DOES discriminate them is the source's OUTBOUND behaviour: how many times
// it opens `Control`, and the AbortSignal it threads into each open. Both are
// public surface — `transport` is an injected collaborator of the frozen C4
// factory signature, not an internal — so observing them is black-box, not a
// reach into the source. This wraps a real transport to report:
//
//   - control() call count, counted CLIENT-side, so a re-open attempt made with
//     an already-aborted signal still registers even when it never reaches the
//     server (the server's own `controlOpens` cannot see that attempt, which is
//     why F4 does not assert on it).
//   - live "abort" listeners on the source-lifetime signal, the only observable
//     that distinguishes a wait which WATCHES the abort from one that ignores it.
//   - a per-open gate on the client-side stream rejection, which marks the pump
//     entering its catch and therefore its backoff wait.
// Runs `onEstablished` immediately after the source's own `onHeader` fires for
// an open. Uptime is stamped at stream ESTABLISHMENT, so a fake clock modelling
// "this connection stayed up N ms" has to advance AFTER that stamp — advancing
// server-side (before the header reaches the client) is absorbed into `openedAt`
// itself and measures an elapsed of zero. This is a source-side seam, so it does
// not pin the pump's sampling pattern: the clock moves once per established
// connection, no matter how many times `now()` is read.
function headerObserver(
	inner: RunnerTransport,
	onEstablished: () => void,
): RunnerTransport {
	return {
		comms: (req) => inner.comms(req),
		lifecycle: (req) => inner.lifecycle(req),
		publishSpine: () => inner.publishSpine(),
		postConversationFrame: (req, options) =>
			inner.postConversationFrame(req, options),
		close: () => inner.close(),
		control: (req, options) =>
			inner.control(req, {
				...options,
				onHeader: (header) => {
					options?.onHeader?.(header);
					onEstablished();
				},
			}),
	};
}

interface TransportObserver {
	transport: RunnerTransport;
	controlCalls(): number;
	liveAbortListeners(): number;
	// Resolves once the client has seen `n` stream rejections.
	streamRejected(n: number): Promise<void>;
}

function observingTransport(inner: RunnerTransport): TransportObserver {
	let controlCalls = 0;
	let rejections = 0;
	// Listener identities, added/removed at runtime → Set, not a Record.
	const liveAbort = new Set<unknown>();
	const gates = new Map<
		number,
		{ promise: Promise<void>; resolve: () => void }
	>();
	let instrumented = false;

	function gateFor(n: number): { promise: Promise<void>; resolve: () => void } {
		const existing = gates.get(n);
		if (existing !== undefined) return existing;
		const fresh = deferred();
		gates.set(n, fresh);
		return fresh;
	}

	// Count net addEventListener/removeEventListener("abort") on the signal. The
	// source holds ONE AbortController for its whole life and threads the same
	// signal into every open, so this is patched once, on first sight.
	function instrument(signal: AbortSignal): void {
		if (instrumented) return;
		instrumented = true;
		const add = signal.addEventListener.bind(signal);
		const remove = signal.removeEventListener.bind(signal);
		Object.assign(signal, {
			addEventListener(type: string, listener: unknown, opts?: unknown): void {
				if (type === "abort") liveAbort.add(listener);
				add(type as "abort", listener as never, opts as never);
			},
			removeEventListener(
				type: string,
				listener: unknown,
				opts?: unknown,
			): void {
				if (type === "abort") liveAbort.delete(listener);
				remove(type as "abort", listener as never, opts as never);
			},
		});
	}

	const transport: RunnerTransport = {
		comms: (req) => inner.comms(req),
		lifecycle: (req) => inner.lifecycle(req),
		publishSpine: () => inner.publishSpine(),
		postConversationFrame: (req, options) =>
			inner.postConversationFrame(req, options),
		close: () => inner.close(),
		control(req, options) {
			controlCalls += 1;
			const signal = options?.signal;
			if (signal !== undefined) instrument(signal);
			const stream = inner.control(req, options);
			return {
				[Symbol.asyncIterator](): AsyncIterator<WireAgentControl> {
					const it = stream[Symbol.asyncIterator]();
					return {
						next: () =>
							it.next().then(
								(r) => r,
								(err: unknown) => {
									rejections += 1;
									gateFor(rejections).resolve();
									throw err;
								},
							),
					};
				},
			};
		},
	};

	return {
		transport,
		controlCalls: () => controlCalls,
		liveAbortListeners: () => liveAbort.size,
		streamRejected: (n) =>
			rejections >= n ? Promise.resolve() : gateFor(n).promise,
	};
}

// Yield past a macrotask boundary, so every already-scheduled microtask has run.
// NOT a duration wait and not a retry: the pump's path from a stream rejection to
// its backoff wait (and, under the F4 mutant, from the abort back around to
// `transport.control()`) is pure microtask work, and `setImmediate` is ordered
// strictly after all of it. So one hop is an EVENT boundary — "the pump has run
// as far as it can without a timer" — not an arbitrary sleep.
function flush(): Promise<void> {
	return new Promise<void>((resolve) => {
		setImmediate(resolve);
	});
}

// Drive a source to the head of its DEEPEST backoff wait (the 2000ms entry), by
// dropping every open. Returns once the 4th drop has been seen client-side and
// the pump has had its macrotask boundary to reach the wait.
//
// Why the 4th: `attempt` climbs 0→4 over the schedule [50,200,800,2000], so drop
// N leaves the pump waiting backoff[N-1]; the 4th leaves 2000ms of slack for the
// abort to land INSIDE the wait rather than racing its expiry. The 5th drop would
// exhaust the budget and fail the iterable instead.
//
// The injected clock never advances, so no connection reaches the min-uptime
// floor (uptime 0 << 5000) and the climb is never reset. Four drops also stay
// under CONTROL_RECONNECT_NO_PROGRESS_MAX (10), so the no-progress budget does
// not terminate the source before the ladder does.
const DROPS_TO_DEEPEST_BACKOFF = 4;

// The catch-side `if (abort.signal.aborted) return;` in `pump`'s catch has NO
// test here, deliberately. It is UNTESTABLE at the public surface, not untested,
// and the distinction matters because the next person to run a mutation sweep
// will see it survive and be tempted to "fix the gap."
//
// Measured, by deleting that line and running this file: 18 pass / 0 fail. The
// reason is that the two abort guards mask each other. With the catch-side guard
// gone, an abort during a stream still runs the no-progress bookkeeping, falls
// through to the backoff wait — which returns at once on an already-aborted
// signal — and then hits the TOP-OF-LOOP guard, which returns. `buffer.fail()`
// in between is a no-op, because the iterator's `return()` already closed the
// buffer. So the mutant reconnects nothing, surfaces nothing, and yields
// nothing: every consumer-visible outcome is identical, and the source is left
// holding no observable difference to assert on.
//
// The one difference is internal — the mutant consumes a slot of the no-progress
// budget (`noProgress` 0 vs 1) for an abort that is not a reconnect. Reaching it
// means asserting on `pump`'s closure state, or inferring it by counting `now()`
// samples; both pin the implementation's shape rather than a contract, and the
// second is the sampling-pattern coupling F2(a)/F2(b) were rewritten to remove.
// A test that reads internals to kill a mutant with no external effect is the
// vacuous coverage this file's mutation discipline exists to reject.
//
// What the line is actually worth: it is a clarity/robustness guard that keeps an
// intentional cancellation from being processed as a transport drop, and it stops
// being redundant the moment anything with an observable effect is added to the
// catch above the backoff. Left in place, documented, unpinned.

test("F4: a source abandoned DURING a reconnect backoff wait never opens another Control subscription (M2 top-of-loop abort guard)", async () => {
	// The abandon-mid-BACKOFF path, which F3 structurally cannot reach: F3's
	// server PARKS the stream open, so its abort lands while the pump is suspended
	// in `for await` and the CATCH-side guard returns first — the top-of-loop
	// guard is never reached, and deleting it leaves F3 green. Only an abort that
	// lands while the pump sits in a backoff WAIT arrives at the head of the loop,
	// where that guard is the one thing standing between an abandoned source and a
	// fresh subscription.
	//
	// Contract: a consumer that walked away mid-backoff must never cause another
	// `Control` open. The real harm the guard prevents is a source the consumer has
	// released re-attaching to the transport and pulling ops nobody will ever read.
	//
	// Non-vacuity (mutation-verified): delete the top-of-loop
	// `if (abort.signal.aborted) return;` at the head of `pump`'s `for(;;)` → the
	// abortable backoff still wakes on the abort, the loop turns, and with no guard
	// the pump calls `transport.control()` a 5th time carrying an already-aborted
	// signal → the "no further open" assertion reds.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: () => dropsImmediately(),
	});
	const obs = observingTransport(createUnixSocketTransport(socketPath));
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(obs.transport, immediate, {
		onUnmapped: () => {},
		// never advances → uptime 0 << the floor → the climb never resets
		now: () => 0,
	});
	const it = source[Symbol.asyncIterator]();
	const pending = it.next();
	await obs.streamRejected(DROPS_TO_DEEPEST_BACKOFF);
	await flush();
	// Precondition: exactly the four opens, so the pump has climbed to its deepest
	// backoff rather than still dialling. Asserted so a harness change that stopped
	// reaching the wait would red HERE, instead of silently turning the assertion
	// below into a tautology about a pump that never got that far.
	//
	// Deliberately NOT asserting the wait's abort-listener here: that is F5's
	// construct, and pinning it in both places would make this test die under F5's
	// mutation too, so a single red could no longer tell the two branches apart.
	// Each of these tests names exactly one branch.
	expect(obs.controlCalls()).toBe(DROPS_TO_DEEPEST_BACKOFF);

	const ret = await it.return?.();
	expect(ret).toEqual({ value: undefined, done: true });
	// The mutant's re-open is pure microtask work off the abort event (the wait
	// resolves, the loop turns, control() is called), so one macrotask boundary is
	// enough for it to have happened — no duration is waited on.
	await flush();
	expect(obs.controlCalls()).toBe(DROPS_TO_DEEPEST_BACKOFF); // no 5th open
	// Not awaited, for the reason given in F5: settling this pull is F6's contract.
	void pending.catch(() => undefined);
});

test("F5: the reconnect backoff wait watches the abort signal, so an abandoned source's wait does not outlive it (M2 abortable backoff)", async () => {
	// Contract: the backoff wait is CANCELLABLE. A consumer that abandons the
	// iterable while the pump sits in the deepest (2s) backoff must not leave that
	// wait — and the timer behind it — running to term. In a long-lived agent
	// container this is the difference between an abandoned source releasing its
	// timer at once and one pinning a timer per retry until it expires.
	//
	// The observable is the source-lifetime AbortSignal it threads into
	// `transport.control()`: a wait that watches the abort is REGISTERED on that
	// signal for exactly as long as it waits, and detaches on both paths. A wait
	// that ignores the signal registers nothing at all. This is a behavioural
	// difference and not a timing one on purpose — the pump's post-wake work is
	// silent (the top-of-loop guard returns without touching a single seam), so
	// "woke early" has no observable to time against; "was watching the signal"
	// does.
	//
	// Non-vacuity (mutation-verified): replace the `sleepOrAbort(delay,
	// abort.signal)` call in `pump`'s catch with a plain
	// `await new Promise<void>((resolve) => setTimeout(resolve, delay));` → nothing
	// observes the signal for the duration of the wait, so the live-listener count
	// during the backoff is 0 → the "registered on the abort signal" assertion
	// reds.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: () => dropsImmediately(),
	});
	const obs = observingTransport(createUnixSocketTransport(socketPath));
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(obs.transport, immediate, {
		onUnmapped: () => {},
		now: () => 0,
	});
	const it = source[Symbol.asyncIterator]();
	const pending = it.next();
	await obs.streamRejected(DROPS_TO_DEEPEST_BACKOFF);
	await flush();
	// Mid-wait: the wait holds exactly one listener on the signal. Exactly one, not
	// "at least one": the per-open listener connect attaches for the stream is
	// already detached by the time that stream has rejected, so a surviving second
	// registration would mean a leak — the very accumulation the wait's
	// always-detach exists to prevent.
	expect(obs.liveAbortListeners()).toBe(1);

	await it.return?.();
	await flush();
	// The abort fired: the wait woke and detached. Nothing stays attached to the
	// source-lifetime signal, so an abandoned source retains no listener (and no
	// timer behind it).
	expect(obs.liveAbortListeners()).toBe(0);
	// The abandoned first pull is deliberately NOT awaited: whether it settles is
	// the buffer-close contract F6 owns, and awaiting it here would hang this test
	// under F6's mutation, so one red could no longer name one branch. Detached
	// with a swallow so an abandoned pull can never surface as an unhandled
	// rejection.
	void pending.catch(() => undefined);
});

test("F6: return() is terminal and idempotent — before any pull, twice over, and for a pull that follows it (M2 iterator protocol)", async () => {
	// Contract: `return()` always settles `{ value: undefined, done: true }`, and
	// leaves the iterator terminally done. A consumer that breaks out of its `for
	// await` BEFORE the first op — agent.ts's control loop erroring during setup —
	// must get a clean completion, and a pull that arrives after it must settle
	// done rather than wedge on a buffer no producer will ever fill again (the pump
	// is aborted; nothing will push).
	//
	// Non-vacuity (mutation-verified): drop `buffer.close()` from the iterator's
	// `return()`, leaving only `abort.abort()` → the post-return `next()` finds an
	// un-closed, empty buffer and parks forever; the bounded guard converts that
	// wedge into its named failure → red. (The two `return()` calls themselves
	// still settle, so this is the assertion that carries the mutation.)
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		// Parks open: the source is abandoned before it ever consumes an op, so the
		// stream must never be the thing that ends the iterable here.
		control: async function* (_open, signal) {
			yield* [];
			await new Promise<void>((resolve) => {
				if (signal.aborted) resolve();
				else signal.addEventListener("abort", () => resolve(), { once: true });
			});
		},
	});
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		immediate,
	);
	const it = source[Symbol.asyncIterator]();
	// return() BEFORE the first next(): the pump is already running (the
	// asyncIterator call started it) but nothing has been pulled.
	expect(await it.return?.()).toEqual({ value: undefined, done: true });
	// Idempotent: a second return() is not an error and does not change the answer.
	expect(await it.return?.()).toEqual({ value: undefined, done: true });

	const pulled = it.next();
	// Real bounded timer (ts-no-test-timers exception, same rationale as F3's): it
	// exists solely to convert a WEDGED pull into a named assertion failure, and is
	// cleared the moment the pull settles, so a passing run waits zero extra time.
	const guard = new Promise<never>((_, reject) => {
		const t = setTimeout(
			() =>
				reject(
					new Error(
						"next() after return() did not settle within 2s — the abandoned iterator wedged on a pull no producer will answer",
					),
				),
			2000,
		);
		void pulled.then(
			() => clearTimeout(t),
			() => clearTimeout(t),
		);
	});
	expect(await Promise.race([pulled, guard])).toEqual({
		value: undefined,
		done: true,
	});
});

test("F7: return() while the pump is parked in the deepest backoff wait tears the fiber down and disposes the module runtime — its awaited teardown never wedges on the uninterruptible wait (T4 no-live-runtime)", async () => {
	// Contract (design record §T4, the mandated T4 addition): the pump is a forked
	// fiber on a module-local ManagedRuntime, and the iterator's return() is the
	// AsyncIterable's only teardown seam (there is no drain()). return() must leave
	// NO live fiber and NO live runtime: it interrupts the pump fiber and disposes
	// the runtime, and — critically — that teardown must SETTLE rather than wedge.
	//
	// The failure mode this pins is specific to the Effect migration. The backoff
	// wait is an `Effect.promise(() => sleepOrAbort(delay, abort.signal))`, and
	// `Effect.promise` is UNINTERRUPTIBLE for the duration of its promise — so a
	// fiber parked in the deepest (2s) backoff cannot be ended by `Fiber.interrupt`
	// alone. What unparks it is `abort.abort()`, which resolves `sleepOrAbort`
	// early (clearing its timer, detaching its one live listener). return() must
	// therefore abort BEFORE it awaits the interrupt + dispose, or its own awaited
	// teardown blocks on the 2s timer running to term. Because return() awaits that
	// teardown, a source that skipped the abort (or ordered it after the awaited
	// interrupt) would make return() itself hang ~2s — the observable here.
	//
	// Non-vacuity (mutation-verified): drop `abort.abort()` from the iterator's
	// return() (leaving the `await runtime.runPromise(Fiber.interrupt(pumpFiber))`
	// / `await runtime.dispose()`) → the interrupt cannot end the fiber parked in
	// the uninterruptible Effect.promise until the 2s timer expires, so return()'s
	// awaited teardown blocks past the 2s floor and the bounded guard below reds
	// with its named message rather than a bare suite timeout.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: () => dropsImmediately(),
	});
	const obs = observingTransport(createUnixSocketTransport(socketPath));
	const { immediate } = recordingImmediate();
	const source = createSocketControlSource(obs.transport, immediate, {
		onUnmapped: () => {},
		// never advances → uptime 0 << the floor → the climb never resets, so the
		// pump reaches and parks in its DEEPEST (2000ms) backoff wait.
		now: () => 0,
	});
	const it = source[Symbol.asyncIterator]();
	const pending = it.next();
	await obs.streamRejected(DROPS_TO_DEEPEST_BACKOFF);
	await flush();
	// Precondition: parked in the deepest backoff, holding exactly one live listener
	// on the source-lifetime signal — the wait the teardown must unpark, not
	// outlast. (Same construct F5 pins; asserted here as this test's setup, not its
	// contract.)
	expect(obs.controlCalls()).toBe(DROPS_TO_DEEPEST_BACKOFF);
	expect(obs.liveAbortListeners()).toBe(1);

	// return()'s awaited interrupt + dispose must settle well inside the deepest
	// backoff delay (2000ms). Real bounded timer (ts-no-test-timers exception,
	// same rationale as F3/F6): it converts a WEDGED teardown — a return() blocked
	// on the uninterruptible wait — into a named assertion failure, and is cleared
	// the moment return() settles, so a passing run waits zero extra time. 1000ms
	// is comfortably below the 2000ms the abort-skip mutant would block for and far
	// above the ~16ms a correct teardown takes.
	const returned =
		it.return?.() ?? Promise.resolve({ value: undefined, done: true });
	const guard = new Promise<never>((_, reject) => {
		const t = setTimeout(
			() =>
				reject(
					new Error(
						"return() did not settle within 1s — its awaited teardown wedged on the uninterruptible backoff wait (abort.abort() did not unpark the pump fiber before the interrupt/dispose await)",
					),
				),
			1000,
		);
		void returned.then(
			() => clearTimeout(t),
			() => clearTimeout(t),
		);
	});
	expect(await Promise.race([returned, guard])).toEqual({
		value: undefined,
		done: true,
	});
	await flush();
	// The fiber is down: its backoff wait woke and detached, so nothing remains on
	// the source-lifetime signal. A live fiber (a teardown that did not end it)
	// would still hold its wait's listener here.
	expect(obs.liveAbortListeners()).toBe(0);
	// And the disposed runtime spawns no further work: no reconnect open follows
	// the teardown.
	expect(obs.controlCalls()).toBe(DROPS_TO_DEEPEST_BACKOFF);
	// The abandoned first pull is deliberately NOT awaited (F6 owns the buffer-close
	// contract); swallow so it can never surface as an unhandled rejection.
	void pending.catch(() => undefined);
});
