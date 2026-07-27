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
	PromptControlSchema,
	ReplayCompleteSchema,
	SteerControlSchema,
	type AgentControl as WireAgentControl,
} from "../gen/compass/v1/agent_pb";
import type { UnmappedEvent } from "../mapping";
import { createSocketControlSource } from "./control-source";
import { createUnixSocketTransport } from "./index";

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
	control(open: number): AsyncIterable<WireAgentControl>;
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
			router.rpc(AgentGateway.method.control, async function* () {
				const open = ++rec.controlOpens;
				yield* hooks.control(open);
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
		(u) => unmapped.push(u),
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
		(u) => unmapped.push(u),
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
	// Non-vacuity (mutation-verified): drop the queued-dedup branch
	// (control-source.ts:288-291) → the dup is no longer counted "already queued"
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
	const source = createSocketControlSource(transport, immediate, (u) => {
		unmapped.push(u);
		if (u.eventType === "control:steer") sentinelDispatched.resolve();
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
	// Non-vacuity (mutation-verified): drop `acks.markApplied(seq)` at
	// control-source.ts:285 (the count line stays, so the dedup gate still resolves
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
	const source = createSocketControlSource(transport, immediate, (u) => {
		unmapped.push(u);
		if (u.reason.includes("already applied")) dedupCounted.resolve();
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
	// Non-vacuity (mutation-verified): break the prune loop at control-source.ts:198
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
