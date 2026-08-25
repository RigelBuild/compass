// O3 metric + span wiring on the INBOUND control source (control-source.ts).
// Decision 1 (span) + Decision 2 (control.* metrics) of
// docs/designs/repo/compass-agent-effect-otel/design.md.
//
// The four control.* metrics no-op WITHOUT an OTel provider but still accumulate
// in Effect's process-global in-memory registry, which every ManagedRuntime
// shares by default (the source's borrowed/fallback runtime installs no
// metric-registry override). So a driven reconnect / flap-reset / no-progress
// drop / unmapped op moves the metric and `Metric.value` reads the delta back
// SYNCHRONOUSLY — the exact pattern otel-metrics.test.ts uses for the O2 lane.
//
// The metric cases reuse the SAME live-server + injected-`now()` harness shape as
// control-source.test.ts (a live connect-node h2c server on a Unix socket, a
// `headerObserver` that advances the injected clock once per established
// connection, an `ackGate` that gates on the ControlAck cursor): a flapping fake
// stream drives the reconnect ladder deterministically, and the injected clock
// models each connection's lifetime so the min-uptime flap-detector fires (or
// not) without any real 5s wait. No wall-clock timer, no poll (ts-no-test-timers):
// each case is event-gated on the source's own promises (collect() ending, or the
// ackGate resolving on an applied op).
//
// The span case (`compass_agent.transport.control.connection`) needs a RECORDING
// runtime — the no-op tracer discards spans — so it injects one through the
// module-private `setTransportRuntime` channel (a test in src/transport/ may call
// it) with an in-memory OTel exporter wired through effect's tracer via
// `NodeSdk.layer` (pattern: forks/oh-my-pi/packages/agent/test/otel.test.ts). A
// fake transport gives the test exact control over whether `onHeader` fires, so
// the `established` span EVENT is present on a header-delivering attempt and
// absent on a hung dial.

import { afterEach, expect, test } from "bun:test";
import * as fs from "node:fs";
import * as http2 from "node:http2";
import * as os from "node:os";
import * as path from "node:path";
import { create } from "@bufbuild/protobuf";
import { connectNodeAdapter } from "@connectrpc/connect-node";
import { NodeSdk } from "@effect/opentelemetry";
import {
	InMemorySpanExporter,
	type ReadableSpan,
	SimpleSpanProcessor,
} from "@opentelemetry/sdk-trace-base";
import { Effect, Layer, Logger, ManagedRuntime, Metric } from "effect";
import type { AgentControl } from "../control";
import {
	AgentGateway,
	type PublishFrameRequest,
	PublishFrameResponseSchema,
} from "../gen/compass/v1/agent_gateway_pb";
import {
	AgentControlSchema,
	ConfigControlSchema,
	type ControlAck,
	PromptControlSchema,
	ReplayCompleteSchema,
	type AgentControl as WireAgentControl,
} from "../gen/compass/v1/agent_pb";
import { createSocketControlSource } from "./control-source";
import { createUnixSocketTransport, type RunnerTransport } from "./index";
import {
	controlUnmapped,
	flapResets,
	noProgressDepth,
	reconnects,
} from "./otel-metrics";
import type { PublishSpine } from "./publish-spine";
import { setTransportRuntime } from "./runtime-channel";

// ---------------------------------------------------------------------------
// Metric readers off the process-global registry (the default the source's
// runtime and Effect.runSync all share), mirroring otel-metrics.test.ts.
// ---------------------------------------------------------------------------

function counterCount(metric: Metric.Metric.Counter<number>): number {
	return Effect.runSync(Metric.value(metric)).count;
}

function gaugeValue(metric: Metric.Metric.Gauge<number>): number {
	return Effect.runSync(Metric.value(metric)).value;
}

// The `unmapped` counter is tagged per-call (dynamic `event_type`), so a reader
// must name the SAME tagged instance the increment site created.
function unmappedCount(eventType: string): number {
	return Effect.runSync(
		Metric.value(Metric.tagged(controlUnmapped, "event_type", eventType)),
	).count;
}

// ---------------------------------------------------------------------------
// Live-server harness (copied shape from control-source.test.ts — that file must
// stay unmodified, and its helpers are not exported).
// ---------------------------------------------------------------------------

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

interface Recorder {
	publishFrames: PublishFrameRequest[];
	controlOpens: number;
}

interface ServerHooks {
	control(open: number, signal: AbortSignal): AsyncIterable<WireAgentControl>;
	onPublish?(frame: PublishFrameRequest): Promise<void> | void;
}

async function serve(rec: Recorder, hooks: ServerHooks): Promise<string> {
	const socketPath = path.join(
		os.tmpdir(),
		`o3m-${process.pid}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}.sock`,
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

const noopImmediate = {
	steer: (): void => {},
	deliver: (): void => {},
};

function promptOp(seq: bigint, input: string): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "prompt", value: create(PromptControlSchema, { input }) },
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

// An empty-shell config op — counted-unmapped at decode (control-source.ts
// dispatch, replay/config arm), regardless of the replay barrier.
function configOp(seq: bigint): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "config", value: create(ConfigControlSchema, {}) },
	});
}

// A Control stream that delivers ZERO ops then throws — the sub-floor flap shape.
function dropsImmediately(): AsyncIterable<WireAgentControl> {
	return {
		[Symbol.asyncIterator]: () => ({
			next: () => Promise.reject(new Error("blip")),
		}),
	};
}

// Wraps a transport to bump the injected clock once per established connection,
// immediately after the source's own onHeader has run — uptime is stamped at
// establishment, so a bump made before the header would fold into `openedAt`.
function headerObserver(
	inner: RunnerTransport,
	onEstablished: () => void,
): RunnerTransport {
	return {
		comms: (req) => inner.comms(req),
		lifecycle: (req) => inner.lifecycle(req),
		forge: (req) => inner.forge(req),
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

// Read an ack frame's ControlAck off a captured PublishFrameRequest.
function controlAckOf(frame: PublishFrameRequest): ControlAck | undefined {
	const f = frame.frame?.frame;
	return f?.case === "controlAck" ? f.value : undefined;
}

// Gates on the consumer demonstrably APPLYING an op (the ControlAck cursor
// advancing on the Publish spine) — the same signal the source's reconnect
// budget reads, so a drop-after-progress is deterministic, not a race.
function ackGate(): {
	onPublish(frame: PublishFrameRequest): void;
	applied(seq: bigint): Promise<void>;
} {
	let cursor = 0n;
	const waiters: { seq: bigint; resolve: () => void }[] = [];
	return {
		onPublish(frame) {
			const ack = controlAckOf(frame);
			if (ack === undefined) return;
			if (ack.ackedSeq > cursor) cursor = ack.ackedSeq;
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
	};
}

async function collect(
	source: AsyncIterable<AgentControl>,
): Promise<AgentControl[]> {
	const out: AgentControl[] = [];
	for await (const op of source) out.push(op);
	return out;
}

// Drive a source to settlement, capturing a clean end vs a definitive fail so a
// budget-exhausting scenario reds a NAMED assertion rather than spraying a stack.
async function drive(
	source: AsyncIterable<AgentControl>,
): Promise<{ ended: "cleanly"; ops: AgentControl[] } | { ended: "failed" }> {
	return collect(source).then(
		(ops) => ({ ended: "cleanly" as const, ops }),
		() => ({ ended: "failed" as const }),
	);
}

// ---------------------------------------------------------------------------
// reconnects
// ---------------------------------------------------------------------------

test("reconnects counts one increment per backoff taken across a flap storm", async () => {
	// Three past-floor flaps (each connection outlives the 5000ms floor via the
	// 6000ms clock bump, so the ladder resets and never exhausts), then a clean
	// close. One backoff is taken between each drop and the next open → exactly 3.
	// Mutation: removing `Metric.increment(reconnects)` at the backoff-take site
	// makes the delta 0 → red.
	const rec = emptyRecorder();
	let t = 0;
	const socketPath = await serve(rec, {
		control: async function* (open) {
			if (open < 4) throw new Error("blip"); // healthy zero-op drop
			yield replayCompleteOp(1n);
			yield promptOp(2n, "survived");
		},
	});
	const before = counterCount(reconnects);
	const source = createSocketControlSource(
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000;
		}),
		noopImmediate,
		{ onUnmapped: () => {}, now: () => t },
	);
	const outcome = await drive(source);
	expect(outcome.ended).toBe("cleanly");
	expect(rec.controlOpens).toBe(4);
	expect(counterCount(reconnects) - before).toBe(3);
});

// ---------------------------------------------------------------------------
// no_progress_depth
// ---------------------------------------------------------------------------

test("no_progress_depth tracks the consecutive-no-progress level", async () => {
	// Three past-floor flaps deliver nothing the consumer applies (no progress),
	// so `noProgress` climbs 1→2→3 and the gauge is set each drop; then a clean
	// close (no drop, no set) leaves the gauge at its peak.
	//
	// The gauge is a process-global shared across tests, so seed it to a sentinel
	// (99) this scenario cannot produce immediately before driving. That makes the
	// site self-diagnostic: removing `Metric.set(noProgressDepth, noProgress)`
	// leaves the gauge stuck at 99 (not 3) → red. (The reset-to-0 test below is the
	// sibling guard for the same site on the progress path; the coupling is
	// intentional, not accidental cross-test residue.)
	const rec = emptyRecorder();
	let t = 0;
	const socketPath = await serve(rec, {
		control: async function* (open) {
			if (open < 4) throw new Error("blip"); // no op applied → no progress
			yield* []; // clean close on open 4 (no ops)
		},
	});
	const source = createSocketControlSource(
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000;
		}),
		noopImmediate,
		{ onUnmapped: () => {}, now: () => t },
	);
	Effect.runSync(Metric.set(noProgressDepth, 99));
	const outcome = await drive(source);
	expect(outcome.ended).toBe("cleanly");
	expect(rec.controlOpens).toBe(4);
	expect(gaugeValue(noProgressDepth)).toBe(3);
});

test("a progress-making reconnect resets no_progress_depth to 0", async () => {
	// One no-progress drop raises the gauge, then a drop that follows an APPLIED
	// op (gated on the ack cursor, so the apply lands before the drop) makes
	// progress and zeroes it; a clean close leaves the gauge at that reset. The
	// two drops (reconnects delta 2) prove the source did climb-then-reset rather
	// than never leaving 0. Mirrors O2's priority_retry_depth reset test.
	const rec = emptyRecorder();
	const gate = ackGate();
	let t = 0;
	const reconnectsBefore = counterCount(reconnects);
	const socketPath = await serve(rec, {
		control: async function* (open) {
			if (open === 1) throw new Error("blip"); // no progress → gauge climbs
			if (open === 2) {
				yield replayCompleteOp(1n);
				yield promptOp(2n, "applied");
				await gate.applied(2n); // consumer applied it → progress
				throw new Error("blip after progress");
			}
			// clean close on open 3
		},
		onPublish: (frame) => gate.onPublish(frame),
	});
	const source = createSocketControlSource(
		headerObserver(createUnixSocketTransport(socketPath), () => {
			t += 6000;
		}),
		noopImmediate,
		{ onUnmapped: () => {}, now: () => t },
	);
	const outcome = await drive(source);
	expect(outcome.ended).toBe("cleanly");
	expect(rec.controlOpens).toBe(3);
	expect(counterCount(reconnects) - reconnectsBefore).toBe(2);
	expect(gaugeValue(noProgressDepth)).toBe(0);
});

// ---------------------------------------------------------------------------
// flap_resets
// ---------------------------------------------------------------------------

test("flap_resets fires only when a past-floor drop resets the ladder", async () => {
	// Part 1: one past-floor drop (6000ms > 5000 floor) resets the ladder → one
	// flap_reset; then a clean close. delta == 1.
	const rec1 = emptyRecorder();
	let t = 0;
	const before = counterCount(flapResets);
	const socketPath1 = await serve(rec1, {
		control: async function* (open) {
			if (open === 1) throw new Error("blip");
			yield* []; // clean close on open 2 (no ops)
		},
	});
	const source1 = createSocketControlSource(
		headerObserver(createUnixSocketTransport(socketPath1), () => {
			t += 6000;
		}),
		noopImmediate,
		{ onUnmapped: () => {}, now: () => t },
	);
	const outcome1 = await drive(source1);
	expect(outcome1.ended).toBe("cleanly");
	expect(counterCount(flapResets) - before).toBe(1);

	// Part 2: sub-floor flaps — the injected clock never advances, so no
	// connection outlives the floor and the reset never fires. The ladder
	// exhausts (5 opens) and the source fails. delta == 0.
	// Mutation: hoisting `Metric.increment(flapResets)` out of the reset `if`
	// (counting every drop) makes Part 2's delta non-zero → red.
	const rec2 = emptyRecorder();
	const beforeSub = counterCount(flapResets);
	const socketPath2 = await serve(rec2, {
		control: () => dropsImmediately(),
	});
	const source2 = createSocketControlSource(
		createUnixSocketTransport(socketPath2),
		noopImmediate,
		{ onUnmapped: () => {}, now: () => 0 },
	);
	const outcome2 = await drive(source2);
	expect(outcome2.ended).toBe("failed");
	expect(rec2.controlOpens).toBe(5);
	expect(counterCount(flapResets) - beforeSub).toBe(0);
}, 15_000);

// ---------------------------------------------------------------------------
// unmapped {event_type}
// ---------------------------------------------------------------------------

test("unmapped increments the counter tagged by the op's event_type", async () => {
	// An empty-shell config op lands in the `count()` funnel and is
	// counted-unmapped at decode with event_type=control:config. Read the TAGGED
	// instance's delta. A prompt op alongside proves only the unmapped funnel
	// increments, not every dispatched op.
	// Mutation: removing the `runtime.runSync(Metric.increment(Metric.tagged(...)))`
	// in `count()` makes the tagged delta 0 → red.
	const rec = emptyRecorder();
	const socketPath = await serve(rec, {
		control: async function* () {
			yield configOp(1n); // empty shell → counted-unmapped
			yield promptOp(2n, "mapped"); // representable → yielded, NOT unmapped
		},
	});
	const before = unmappedCount("control:config");
	const source = createSocketControlSource(
		createUnixSocketTransport(socketPath),
		noopImmediate,
		{ onUnmapped: () => {} },
	);
	const ops = await collect(source);
	expect(ops.map((o) => o.kind)).toEqual(["prompt"]);
	expect(unmappedCount("control:config") - before).toBe(1);
});

// ---------------------------------------------------------------------------
// the connection span (recording runtime injected via setTransportRuntime)
// ---------------------------------------------------------------------------

// A recording runtime whose tracer records into an in-memory exporter, built the
// transport's way (OTel layer merged with the logger removal). Declared
// Layer<never> exactly as otel-layer.ts does: NodeSdk.layer returns
// Layer<Resource.Resource>, and Layer's contravariant ROut lets it widen to
// Layer<never>, so the runtime is a ManagedRuntime<never, never> — the
// TransportRuntime shape setTransportRuntime expects.
function recordingLayer(exporter: InMemorySpanExporter): Layer.Layer<never> {
	return NodeSdk.layer(() => ({
		spanProcessor: new SimpleSpanProcessor(exporter),
		resource: { serviceName: "compass-agent-test" },
	}));
}

// A fake transport giving exact control over whether `onHeader` fires per open,
// so the `established` span EVENT is present on a header-delivering attempt and
// absent on a hung dial. The pump runs on the injected recording runtime.
interface SpanOpen {
	established: boolean;
	ops: WireAgentControl[];
	end: "close" | "throw";
}

function fakeSpanTransport(opens: SpanOpen[]): RunnerTransport {
	const noopSpine: PublishSpine = {
		enqueueTrace: () => {},
		enqueuePriority: () => {},
		droppedTraceCount: () => 0,
		failedPriorityCount: () => 0,
		drain: () => Promise.resolve(),
	};
	let openIndex = 0;
	return {
		comms: () => Promise.reject(new Error("comms unused")),
		lifecycle: () => Promise.reject(new Error("lifecycle unused")),
		forge: () => Promise.reject(new Error("forge unused")),
		publishSpine: () => noopSpine,
		postConversationFrame: () =>
			Promise.reject(new Error("postConversationFrame unused")),
		close: () => {},
		control: (_req, options) => {
			const plan = opens[Math.min(openIndex, opens.length - 1)] as SpanOpen;
			openIndex += 1;
			let i = 0;
			let headed = false;
			return {
				[Symbol.asyncIterator]: () => ({
					next: async (): Promise<IteratorResult<WireAgentControl>> => {
						if (!headed) {
							headed = true;
							// Establishment: fire onHeader while the tryPromise (and thus
							// the span) is live, as the Connect stream would.
							if (plan.established) options?.onHeader?.(new Headers());
						}
						if (i < plan.ops.length) {
							return { value: plan.ops[i++] as WireAgentControl, done: false };
						}
						if (plan.end === "close") return { value: undefined, done: true };
						throw new Error("blip");
					},
				}),
			};
		},
	};
}

test("each connection attempt opens a control.connection span with the attempt attribute; the established event is present only on a header-delivering stream", async () => {
	// Two attempts: attempt 0 is a hung dial (no onHeader, then throws) → its span
	// carries attempt=0 and NO established event; the pump takes one backoff and
	// re-opens. Attempt 1 fires onHeader (established event), delivers an op, and
	// clean-closes → its span carries attempt=1 AND the established event.
	// SimpleSpanProcessor exports on span END, so after the source settles both
	// spans are finished in the in-memory exporter.
	// Mutation: dropping `Effect.withSpan("...control.connection", { attributes:
	// { attempt } })` yields zero connection spans → red; dropping the
	// `span.event("established", ...)` call in onHeader drops the event from the
	// attempt-1 span → the established-present assertion reds.
	const exporter = new InMemorySpanExporter();
	const runtime = ManagedRuntime.make(
		Layer.merge(Logger.remove(Logger.defaultLogger), recordingLayer(exporter)),
	);
	const transport = fakeSpanTransport([
		{ established: false, ops: [], end: "throw" }, // hung dial
		{
			established: true,
			ops: [replayCompleteOp(1n), promptOp(2n, "hello")],
			end: "close",
		},
	]);
	// Register the recording runtime BEFORE the source borrows it.
	setTransportRuntime(transport, runtime);
	const source = createSocketControlSource(transport, noopImmediate, {
		onUnmapped: () => {},
		now: () => 0,
	});

	const ops = await collect(source);
	expect(ops.map((o) => o.kind)).toEqual(["replayComplete", "prompt"]);

	// SimpleSpanProcessor exports each span on END (synchronously), so by the
	// time collect() has drained both attempts the finished spans are readable.
	// Read BEFORE dispose: shutting the SDK down clears the in-memory exporter.
	const connectionSpans: ReadableSpan[] = exporter
		.getFinishedSpans()
		.filter((s) => s.name === "compass_agent.transport.control.connection");

	// One span per attempt, each carrying its ladder index as the `attempt`
	// attribute.
	expect(connectionSpans.length).toBe(2);
	const attempt0 = connectionSpans.find((s) => s.attributes.attempt === 0);
	const attempt1 = connectionSpans.find((s) => s.attributes.attempt === 1);
	expect(attempt0).toBeDefined();
	expect(attempt1).toBeDefined();

	// The established event is present ONLY on the header-delivering attempt.
	expect(
		(attempt1 as ReadableSpan).events.some((e) => e.name === "established"),
	).toBe(true);
	expect(
		(attempt0 as ReadableSpan).events.some((e) => e.name === "established"),
	).toBe(false);

	// The source BORROWS the injected runtime (setTransportRuntime registered it),
	// so it never disposes it — this test owns that teardown.
	await runtime.dispose();
});
