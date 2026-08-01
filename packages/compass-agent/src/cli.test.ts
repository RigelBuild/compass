// The `compass-agent` entrypoint — both halves.
//
// The Runner execs a bare `compass-agent` argv (no flags —
// `go/internal/runner/relay.go` `agentCommand`), so every input reaches the
// process through the environment or a well-known file. Two surfaces:
//
//   - CONSTRUCTION: the pure resolution functions (`AGENT_SOCKET_PATH`,
//     `resolveModelSelector`, `authSeedPath`, `createSeedApiKeyResolver`) —
//     each exercised directly, against a tempfile seed.
//   - COMPOSITION: `main` itself, over the `MainDeps` seam (cli.ts `MainDeps`) — a fake
//     session and a fake `RunnerTransport` stand in for the two unfakeable
//     constructors, and everything between them (the real socket FrameSink, the
//     real ControlSource, the real PublishSpine, the real CompassAgent run loop)
//     is the production code. What `main` uniquely owns and nothing below it can
//     defend is the TEARDOWN BARRIER: `finally { await sink.drain?.();
//     transport.close() }`, which the teardown tests here pin against a captured
//     wire log and a recorded ordering.
//
// Nothing here touches a socket, a real model, or a real credential: the carrier
// is injected (as `agent.test.ts` already does) and the seed is a tempfile. No
// timers, no sleeps — the composition tests gate on events (a deferred resolved
// from the fake carrier's own RPC handlers).

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Model } from "@oh-my-pi/pi-ai";
import type {
	AgentSession,
	AgentSessionEventListener,
} from "@oh-my-pi/pi-coding-agent";
import { SessionManager } from "@oh-my-pi/pi-coding-agent";
import { serializeTitleSlot } from "@oh-my-pi/pi-coding-agent/session/session-title-slot";
import {
	AGENT_SOCKET_PATH,
	authSeedPath,
	createSeedApiKeyResolver,
	type MainDeps,
	main,
	resolveModelSelector,
	resolvePersona,
} from "./cli";
import {
	AgentControlSchema,
	AgentSessionState,
	create,
	PromptControlSchema,
	type AgentControl as WireAgentControl,
} from "./compassv1";
import {
	type PostConversationFrameRequest,
	PostConversationFrameResponseSchema,
	type PublishFrameRequest,
} from "./gen/compass/v1/agent_gateway_pb";
import { createTeeSessionStorage } from "./session-tee";
import type { RunnerTransport } from "./transport/index";
import { createPublishSpine } from "./transport/publish-spine";

const tmpdirs: string[] = [];

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-agent-cli-"));
	tmpdirs.push(dir);
	return dir;
}

// `main` computes its session dir via SessionManager.getDefaultSessionDir(cwd),
// which anchors on the global HOME (os.homedir() → process.env.HOME). Pin it to
// a per-test scratch dir so the tee storage's mkdir + the manager's writes land
// under a throwaway tree, never the developer's real ~/.omp session store. (The
// `env` arg main receives controls the auth-seed lookup, NOT the session dir —
// that reads the process HOME, which is what this pins.)
let savedHome: string | undefined;
beforeEach(() => {
	savedHome = process.env.HOME;
	process.env.HOME = scratch();
});

afterEach(() => {
	if (savedHome === undefined) delete process.env.HOME;
	else process.env.HOME = savedHome;
	for (const dir of tmpdirs.splice(0)) {
		rmSync(dir, { recursive: true, force: true });
	}
});

// The socket path is a CONTRACT with the Runner, not a preference: host.go:33
// bind-mounts the per-container socket at this fixed path precisely "so the
// agent needs no per-session configuration — it always dials the same path"
// (host.go:28-29). A drift here is a launch failure with no error until the
// dial times out, so it is pinned by a test.
describe("AGENT_SOCKET_PATH", () => {
	test("matches the Runner's fixed in-container mount path", () => {
		expect(AGENT_SOCKET_PATH).toBe("/run/compass/agent.sock");
	});
});

// COMPASS_MODEL is the Matt-ruled runtime seam for model selection. The
// entrypoint does not parse it into a Model — the SDK's model registry owns
// that — it forwards it as `modelPattern` to createAgentSession. Absent, the
// session falls back to the SDK's own default, which is a legitimate
// configuration, not an error: an operator who pins nothing gets the SDK
// default rather than a container that refuses to boot.
describe("resolveModelSelector", () => {
	test("returns the COMPASS_MODEL value when set", () => {
		expect(
			resolveModelSelector({ COMPASS_MODEL: "anthropic/claude-opus" }),
		).toBe("anthropic/claude-opus");
	});

	test("returns undefined when COMPASS_MODEL is unset (SDK default applies)", () => {
		expect(resolveModelSelector({})).toBeUndefined();
	});

	test("treats an empty or whitespace-only value as unset", () => {
		expect(resolveModelSelector({ COMPASS_MODEL: "" })).toBeUndefined();
		expect(resolveModelSelector({ COMPASS_MODEL: "   " })).toBeUndefined();
	});

	test("trims surrounding whitespace so a padded env value still resolves", () => {
		expect(resolveModelSelector({ COMPASS_MODEL: "  openai/gpt-5  " })).toBe(
			"openai/gpt-5",
		);
	});
});

// COMPASS_PERSONA is the server-authoritative identity overlay. The entrypoint
// forwards it verbatim (trimmed) as an append customizer; unset or blank leaves
// the agent on its default prompt. Same unset/trim semantics as the model
// selector, extracted as a pure function so `main` composes tested decisions.
describe("resolvePersona", () => {
	test("returns the COMPASS_PERSONA value when set", () => {
		expect(resolvePersona({ COMPASS_PERSONA: "You are Ada." })).toBe(
			"You are Ada.",
		);
	});

	test("returns undefined when COMPASS_PERSONA is unset (default prompt applies)", () => {
		expect(resolvePersona({})).toBeUndefined();
	});

	test("treats an empty or whitespace-only value as unset", () => {
		expect(resolvePersona({ COMPASS_PERSONA: "" })).toBeUndefined();
		expect(resolvePersona({ COMPASS_PERSONA: "   " })).toBeUndefined();
	});

	test("trims surrounding whitespace so a padded env value still resolves", () => {
		expect(resolvePersona({ COMPASS_PERSONA: "  You are Ada.  " })).toBe(
			"You are Ada.",
		);
	});
});

// The seed path is the frozen T5 placement: a 0600 `$HOME/.compass/auth-seed.json`
// written by the Runner's materializer.
describe("authSeedPath", () => {
	test("resolves under the supplied HOME", () => {
		expect(authSeedPath("/home/agent")).toBe(
			"/home/agent/.compass/auth-seed.json",
		);
	});
});

// getApiKey is called PER LLM CALL (agent.d.ts:66-70: "Resolves an API key ...
// dynamically for each LLM call. Useful for expiring tokens"). That semantic is
// the whole reason rotation works without a restart: T6 rewrites the seed file
// in place and the next call must pick it up. So the resolver re-reads the seed
// rather than closing over a value read at boot — these tests pin that.
describe("createSeedApiKeyResolver", () => {
	test("resolves the key for the model's provider", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: {
				anthropic: { type: "api-key", key: "sk-ant-live" },
				openai: { type: "api-key", key: "sk-oai-live" },
			},
		});

		const getApiKey = createSeedApiKeyResolver(home);

		expect(await getApiKey(model("anthropic"))).toBe("sk-ant-live");
		expect(await getApiKey(model("openai"))).toBe("sk-oai-live");
	});

	test("picks up a rewritten seed on the next call — rotation with no restart", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "old" } },
		});

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("anthropic"))).toBe("old");

		// T6 rotation: the Runner rewrites the seed in place. The very next
		// resolution must see it — no process restart, no cache to invalidate.
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "new" } },
		});
		expect(await getApiKey(model("anthropic"))).toBe("new");
	});

	test("returns undefined for a provider absent from the seed", async () => {
		const home = scratch();
		writeSeed(home, { entries: { anthropic: { type: "api-key", key: "k" } } });

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("google"))).toBeUndefined();
	});

	// A container can legitimately boot before its seed is materialized (provision
	// order) — and an agent with no provider credential must still start and report,
	// not crash on first call. Undefined lets the SDK surface a clean auth error.
	test("returns undefined when the seed file does not exist", async () => {
		const getApiKey = createSeedApiKeyResolver(scratch());
		expect(await getApiKey(model("anthropic"))).toBeUndefined();
	});

	test("returns undefined when the seed is malformed rather than throwing", async () => {
		const home = scratch();
		writeSeedRaw(home, "{not json");

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("anthropic"))).toBeUndefined();
	});

	// Defensive: a seed whose entry is the wrong shape (no string `key`) must not
	// hand a non-string to the SDK, which types ApiKey as `string | ApiKeyResolver`
	// (auth-retry.d.ts:35) and would otherwise send a garbage bearer.
	test("returns undefined when the entry has no string key", async () => {
		const home = scratch();
		writeSeedRaw(
			home,
			JSON.stringify({ entries: { anthropic: { type: "api-key" } } }),
		);

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("anthropic"))).toBeUndefined();
	});
});

function writeSeed(home: string, seed: unknown): void {
	writeSeedRaw(home, JSON.stringify(seed));
}

function writeSeedRaw(home: string, body: string): void {
	const dir = join(home, ".compass");
	mkdirSync(dir, { recursive: true });
	writeFileSync(join(dir, "auth-seed.json"), body, { mode: 0o600 });
}

// The resolver keys off `model.provider` alone; the rest of the wide `Model`
// surface (name/api/baseUrl/reasoning/cost/contextWindow/...) is never read, so
// constructing a full registry model here would assert nothing extra. The cast
// is narrow and honest: it names exactly the field under test.
function model(provider: string): Model {
	return { provider, id: `${provider}/test-model` } as unknown as Model;
}

// ── main(): the composition root ─────────────────────────────────────────────
//
// `main` is exercised over the `MainDeps` seam: a fake `AgentSession` (the
// recording shape `agent.test.ts` established) and a fake `RunnerTransport` whose
// four RPCs are in-process handlers recording what reached "the Runner". The
// carrier is fake; everything main composes over it — createSocketFrameSink,
// createSocketControlSource, createPublishSpine (the REAL one, built here exactly
// as createUnixSocketTransport builds it), CompassAgent.run — is production code.
// So these tests see the actual enqueue/flush behavior, not a restatement of it.

// What the fake carrier saw. `publishFrames` is the wire log of the Publish
// spine (trace + lifecycle + control acks, in arrival order); `durableFrames`
// the COMMITTED conversation unaries — appended only after the handler returns,
// so a frame present here is provably committed, and one still in flight is not.
interface CarrierLog {
	publishFrames: PublishFrameRequest[];
	durableFrames: PostConversationFrameRequest[];
}

interface CarrierHooks {
	// The Control server-stream the source consumes: yields ops then returns
	// (clean close → the run loop ends → STOPPED). Defaults to an immediate clean
	// close.
	control?: () => AsyncIterable<WireAgentControl>;
	// Awaited inside the durable unary BEFORE it commits — lets a test hold a
	// conversation frame uncommitted while `run()` finishes.
	onDurable?: (frame: PostConversationFrameRequest) => Promise<void> | void;
	// Called when the composition root releases the carrier. Records the close
	// so a test can pin it against the drain that must precede it.
	onClose?: () => void;
	// Makes the sink's `drain()` reject. Neither production drain can reject
	// today, but that is an invariant of frame-sink.ts/publish-spine.ts, not of
	// the composition root — this lets a test hold `main` to releasing the
	// carrier even when the drain ahead of it fails.
	drainError?: Error;
}

// A RunnerTransport over in-process handlers. Built the same way
// createUnixSocketTransport builds it — one memoized REAL PublishSpine shared by
// the sink and the source — so the priority/trace lanes, the batch cycling, and
// spine.drain() under test are the production implementations.
function fakeCarrier(
	log: CarrierLog,
	hooks: CarrierHooks = {},
): RunnerTransport {
	const real = createPublishSpine(async (stream) => {
		for await (const frame of stream) log.publishFrames.push(frame);
	});
	// One spine object, memoized exactly as createUnixSocketTransport memoizes
	// its own — the sink and the control source must share it. `drainError`
	// swaps in a rejecting drain(): the sink's drain() ends in spine.drain(), so
	// this is what makes the composition root's `await sink.drain?.()` reject.
	const spine =
		hooks.drainError === undefined
			? real
			: { ...real, drain: () => Promise.reject(hooks.drainError) };
	return {
		comms: () => Promise.reject(new Error("comms is not used by main")),
		publishSpine: () => spine,
		postConversationFrame: async (req) => {
			if (hooks.onDurable) await hooks.onDurable(req);
			// Recorded only once the handler completes: presence here IS commitment.
			log.durableFrames.push(req);
			return create(PostConversationFrameResponseSchema, {});
		},
		control: () =>
			hooks.control?.() ??
			(async function* (): AsyncGenerator<WireAgentControl> {})(),
		close: () => hooks.onClose?.(),
	};
}

// The recording AgentSession `main` composes over: `subscribe` hands the listener
// to a gate (so a test can push a session event through the REAL EventMapper the
// way the SDK would, once the run loop has wired it) and `agent` carries the
// members CompassAgent/main touch. Only those are implemented, so the cast is
// honest.
interface FakeSession {
	// Resolves with the listener the moment `run()` subscribes — the event gate a
	// test awaits before pushing session events, so there is no race and no spin.
	readonly subscribed: Promise<AgentSessionEventListener>;
	agent: { getApiKey?: (model: Model) => Promise<string | undefined> };
}

function fakeSession(opts: { promptError?: Error } = {}): FakeSession {
	const gate = Promise.withResolvers<AgentSessionEventListener>();
	const rec: FakeSession = { subscribed: gate.promise, agent: {} };
	Object.assign(rec.agent, {
		// `promptError` makes an SDK op reject, which is how a real turn failure
		// crashes the run loop (agent.ts:163 awaits it inside the try).
		prompt: () =>
			opts.promptError !== undefined
				? Promise.reject(opts.promptError)
				: Promise.resolve(),
		steer: () => {},
		appendMessage: () => {},
		setSystemPrompt: () => {},
		setTools: () => {},
	});
	Object.assign(rec, {
		subscribe(fn: AgentSessionEventListener): () => void {
			gate.resolve(fn);
			return () => {};
		},
	});
	return rec;
}

// The deps `main` runs under: the fake session factory plus the fake carrier.
// `createSessionStorage` is left to its production default — the REAL
// `createTeeSessionStorage`, writing under the per-test scratch HOME (pinned in
// beforeEach) — so `main`'s full composition (build sink → tee storage →
// SessionManager.create) is exercised, not stubbed.
function deps(session: FakeSession, transport: RunnerTransport): MainDeps {
	return {
		createSession: () =>
			Promise.resolve({ session: session as unknown as AgentSession }),
		createTransport: () => transport,
	};
}

function emptyLog(): CarrierLog {
	return { publishFrames: [], durableFrames: [] };
}

// The two control ops these tests need. `replayComplete` lifts the barrier (so a
// following live prompt is applied rather than refused); `prompt` is the op whose
// SDK call the crash test makes reject.
function replayCompleteOp(seq: bigint): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "replayComplete", value: {} },
	});
}

function promptOp(seq: bigint, input: string): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "prompt", value: create(PromptControlSchema, { input }) },
	});
}

// The board states that reached the fake Runner over the Publish spine, in
// arrival order. Empty (UNSPECIFIED) states are trace frames, not transitions.
function statesOf(log: CarrierLog): AgentSessionState[] {
	return log.publishFrames.flatMap((f) => {
		const inner = f.frame?.frame;
		if (inner?.case !== "session") return [];
		return inner.value.state === AgentSessionState.UNSPECIFIED
			? []
			: [inner.value.state];
	});
}

describe("main", () => {
	// HOME is how the entrypoint finds the provider seed (authSeedPath). The
	// Runner always supplies it; if it ever does not, the failure must name the
	// cause at boot rather than surfacing later as an inexplicable "no credential"
	// on the first LLM call. Nothing is constructed before the check, so this
	// needs no injection.
	test("rejects when HOME is unset, naming HOME as the cause", async () => {
		// Non-vacuity: a main that fell back to a default home (or read
		// process.env.HOME instead of the passed env) would construct and hang/
		// resolve instead of rejecting → red.
		await expect(main({})).rejects.toThrow(/HOME/);
	});

	test("rejects when HOME is empty, not just absent", async () => {
		// An empty HOME would build the seed path "/.compass/auth-seed.json" — a
		// path outside the Runner's scoped home. `if (!home)` must catch it; a
		// `home === undefined` check would let it through.
		await expect(main({ HOME: "" })).rejects.toThrow(/HOME/);
	});

	// THE DRAIN BARRIER — what `main` alone owns.
	//
	// `run()` emits its terminal STOPPED through the sink on its way out, and the
	// socket sink only ENQUEUES a lifecycle frame onto the spine's priority lane
	// (frame-sink.ts:131) — the actual wire flush happens in a later batch. So
	// when `main` resolves, STOPPED has reached the Runner only if `main` awaited
	// `sink.drain()`. Without the `finally { await sink.drain?.() }` the process
	// exits with the terminal frame still in the queue, and the board never sees
	// the session stop.
	test("the terminal STOPPED frame has reached the carrier by the time main resolves", async () => {
		const log = emptyLog();
		const session = fakeSession();
		await main(
			{ HOME: scratch() },
			deps(session, fakeCarrier(log, { control: emptyControlStream })),
		);
		// Read at the resolution instant — no extra await, no tick, no timer.
		// Non-vacuity (mutation-verified): with the `finally { await sink.drain?.() }`
		// removed from cli.ts, this reads `[STARTING]` — the queued STOPPED never
		// reaches the wire — and the assertion reds.
		expect(statesOf(log)).toEqual([
			AgentSessionState.STARTING,
			AgentSessionState.STOPPED,
		]);
	});

	// The other half of the sink's teardown contract: a conversation frame is
	// DURABLE (delivered-or-erred on the unary, frame-sink.ts:141-144), and
	// `emit()` is void — it launches the send and returns. A frame emitted during
	// the run is therefore still in flight when `run()` resolves. `drain()` awaits
	// those in-flight commits (frame-sink.ts:152-154); without the barrier `main`
	// resolves — and `import.meta.main` calls `process.exit` — abandoning an
	// uncommitted conversation frame. This is the exact defect the drain fixed.
	//
	// The assertion is an ORDERING, which is the contract itself: the commit
	// strictly precedes main's resolution. The commit is parked one event-loop turn
	// out (never a duration — see nextEventLoopTurn), and `main` without the drain
	// resolves entirely within the microtask phase, so the order inverts.
	test("a conversation frame in flight at teardown is COMMITTED before main resolves", async () => {
		const log = emptyLog();
		const session = fakeSession();
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		// Holds the control stream open so `run()` cannot end before the frame is
		// emitted — the interleaving is gated, never raced.
		const closeControl = Promise.withResolvers<void>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await closeControl.promise;
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
		});
		const runP = main({ HOME: scratch() }, deps(session, carrier));
		// The session emitted a settled assistant text block mid-run: the REAL
		// EventMapper turns it into a durable conversationUpdated frame, which the
		// REAL socket sink launches on the unary.
		await settledText(session, "the answer");
		// Gate on the carrier having ENTERED the unary — the send is provably in
		// flight right now, and provably uncommitted.
		await inFlight.promise;
		expect(log.durableFrames).toHaveLength(0);
		closeControl.resolve();
		await runP;
		order.push("main-resolved");
		expect(order).toEqual(["committed", "main-resolved"]);
		expect(log.durableFrames[0]?.frame?.frame.case).toBe("conversationUpdated");
	});

	// The barrier is in `finally`, so it holds on the ERROR path too — which is
	// where it matters most: a session that died is exactly when the board needs
	// its terminal transition and its last conversation frame. `run()` emits
	// ERRORED (agent.ts:113) and re-throws; main must still drain, then propagate
	// the ORIGINAL error (a `finally` that swallowed it would hide the crash).
	//
	// The crash is an SDK op rejecting mid-loop, NOT a control-stream drop: a drop
	// sends the source through its bounded reconnect backoff (control-source.ts:79),
	// and those timers would incidentally flush the spine before main rejects —
	// making the assertions pass with or without the barrier. An op rejection
	// reaches the `finally` entirely within the microtask phase, so this test is
	// genuinely drain-sensitive.
	test("on the error path the frame is committed and ERRORED delivered before main rejects", async () => {
		const log = emptyLog();
		const boom = new Error("SDK prompt failed mid-turn");
		const session = fakeSession({ promptError: boom });
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		// Holds the crashing op back until the durable send is in flight.
		const crash = Promise.withResolvers<void>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await crash.promise;
				yield promptOp(2n, "go");
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
		});
		const runP = main({ HOME: scratch() }, deps(session, carrier));
		await settledText(session, "half an answer");
		await inFlight.promise;
		crash.resolve();
		await expect(runP).rejects.toBe(boom);
		order.push("main-rejected");
		// Drained on the way out: the frame committed first, and the original
		// error still surfaced.
		expect(order).toEqual(["committed", "main-rejected"]);
		expect(log.durableFrames).toHaveLength(1);
		expect(statesOf(log)).toEqual([
			AgentSessionState.STARTING,
			AgentSessionState.ERRORED,
		]);
	});

	// The carrier is a live HTTP/2 session over the Runner socket, and nothing
	// below `main` holds it — so the composition root must RELEASE it, and must
	// do so strictly AFTER the drain: close abandons open streams, so closing
	// first would discard exactly the frames the barrier exists to commit. The
	// fake carrier holds no socket, so the lingering connection itself is not
	// observable here; what IS the contract, and what this pins, is the ORDER —
	// the durable commit (which only happens because `drain()` awaited it)
	// strictly precedes `close()`, which strictly precedes main's resolution.
	test("the carrier is closed after the drain, before main resolves", async () => {
		const log = emptyLog();
		const session = fakeSession();
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		const closeControl = Promise.withResolvers<void>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await closeControl.promise;
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
			onClose: () => order.push("closed"),
		});
		const runP = main({ HOME: scratch() }, deps(session, carrier));
		await settledText(session, "the answer");
		await inFlight.promise;
		expect(order).toEqual([]);
		closeControl.resolve();
		await runP;
		order.push("main-resolved");
		expect(order).toEqual(["committed", "closed", "main-resolved"]);
	});

	// The release is in the same `finally`, so it holds on the crash path too —
	// a self-terminating agent that died still must not leave the socket held
	// until the session manager's idle timeout. Same crash shape as the error
	// drain test above (an SDK op rejection, not a control-stream drop, so no
	// reconnect timer incidentally flushes the spine), and the original error
	// still propagates past both teardown steps.
	test("the carrier is closed after the drain on the error path too", async () => {
		const log = emptyLog();
		const boom = new Error("SDK prompt failed mid-turn");
		const session = fakeSession({ promptError: boom });
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		const crash = Promise.withResolvers<void>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await crash.promise;
				yield promptOp(2n, "go");
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
			onClose: () => order.push("closed"),
		});
		const runP = main({ HOME: scratch() }, deps(session, carrier));
		await settledText(session, "half an answer");
		await inFlight.promise;
		crash.resolve();
		await expect(runP).rejects.toBe(boom);
		order.push("main-rejected");
		expect(order).toEqual(["committed", "closed", "main-rejected"]);
	});

	// The release must survive a FAILING drain. Neither production drain can
	// reject today, but that no-throw property belongs to frame-sink.ts and
	// publish-spine.ts, not to this composition root — so if either ever lost it,
	// an unguarded `await sink.drain?.(); transport.close()` would skip the close
	// and leak the HTTP/2 session for the manager's whole idle window, which is
	// the exact defect close() was added to fix. The nested
	// `try { drain } finally { close }` is what this pins.
	//
	// Non-vacuity (mutation-verified): with the close moved back out of its own
	// `finally`, `closed` stays false and the assertion reds.
	test("a rejecting drain still closes the carrier, and its error propagates", async () => {
		const log = emptyLog();
		const drainBoom = new Error("drain failed");
		const session = fakeSession();
		let closed = false;
		const carrier = fakeCarrier(log, {
			control: emptyControlStream,
			drainError: drainBoom,
			onClose: () => {
				closed = true;
			},
		});
		await expect(
			main({ HOME: scratch() }, deps(session, carrier)),
		).rejects.toBe(drainBoom);
		expect(closed).toBe(true);
	});

	// The seed resolver is installed on the SESSION'S agent, and installed as a
	// live resolver (called per LLM call) rather than a value read at boot. The
	// wiring is only interesting because of what it resolves TO, so this drives
	// the installed function against a real seed under the passed HOME: a resolver
	// built from the wrong home, or one never installed, reddens.
	test("installs a getApiKey on the session that resolves from the passed HOME's seed", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "sk-from-main" } },
		});
		const session = fakeSession();
		await main(
			{ HOME: home },
			deps(session, fakeCarrier(emptyLog(), { control: emptyControlStream })),
		);
		const getApiKey = session.agent.getApiKey;
		if (getApiKey === undefined) throw new Error("main installed no getApiKey");
		expect(await getApiKey(model("anthropic"))).toBe("sk-from-main");
	});

	// The socket path is not a parameter anywhere: the Runner bind-mounts it at a
	// fixed location (host.go:33) and the agent dials that constant. Pinning it
	// AT THE CALL SITE catches a main that dialed something else — the constant
	// test above only pins the constant's value.
	test("dials the carrier at AGENT_SOCKET_PATH", async () => {
		const dialed: string[] = [];
		const session = fakeSession();
		await main(
			{ HOME: scratch() },
			{
				createSession: () =>
					Promise.resolve({ session: session as unknown as AgentSession }),
				createTransport: (socketPath) => {
					dialed.push(socketPath);
					return fakeCarrier(emptyLog(), { control: emptyControlStream });
				},
			},
		);
		expect(dialed).toEqual([AGENT_SOCKET_PATH]);
	});

	// COMPASS_MODEL / COMPASS_WORKDIR are the container's only two configuration
	// knobs, and `main` is the sole place they become session options. The
	// resolution rules are unit-tested above; this pins that main actually FORWARDS
	// them — a session built with the wrong cwd loads the wrong project context.
	test("forwards COMPASS_MODEL as modelPattern and COMPASS_WORKDIR as cwd", async () => {
		const session = fakeSession();
		const seen: { cwd?: string; modelPattern?: string | string[] }[] = [];
		await main(
			{
				HOME: scratch(),
				COMPASS_MODEL: "anthropic/claude-opus-4-5",
				COMPASS_WORKDIR: "/work/repo",
			},
			{
				createSession: (options) => {
					seen.push({
						cwd: options.cwd,
						modelPattern: options.modelPattern,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toEqual([
			{ cwd: "/work/repo", modelPattern: "anthropic/claude-opus-4-5" },
		]);
	});

	test("treats an empty or whitespace-only COMPASS_WORKDIR as unset, not as a cwd", async () => {
		// Mirrors the empty-HOME case: `??` would forward "" verbatim, and bun does
		// not reject `cwd: ""` — the agent would silently load project context from
		// the wrong tree. A whitespace-only value is truthy, so the `.trim()` is
		// what catches it. The Runner sets COMPASS_WORKDIR unconditionally
		// (relay.go `execSpec`), so a blank AgentEnv.Workdir reaches here directly.
		for (const workdir of ["", "   "]) {
			const session = fakeSession();
			const seen: (string | undefined)[] = [];
			await main(
				{ HOME: scratch(), COMPASS_WORKDIR: workdir },
				{
					createSession: (options) => {
						seen.push(options.cwd);
						return Promise.resolve({
							session: session as unknown as AgentSession,
						});
					},
					createTransport: () =>
						fakeCarrier(emptyLog(), { control: emptyControlStream }),
				},
			);
			expect(seen).toEqual([process.cwd()]);
		}
	});

	// COMPASS_PERSONA is the identity overlay: when set, main must APPEND it to
	// the SDK's default prompt (block-0 base + project footer survive), never
	// replace it. The customizer is a function; drive it with a fake default and
	// assert the persona lands last.
	test("appends COMPASS_PERSONA to the default systemPrompt when set", async () => {
		const session = fakeSession();
		const seen: (
			| string
			| string[]
			| ((p: string[]) => string | string[])
			| undefined
		)[] = [];
		await main(
			{ HOME: scratch(), COMPASS_PERSONA: "You are Ada." },
			{
				createSession: (options) => {
					seen.push(options.systemPrompt);
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		const customizer = seen[0];
		if (typeof customizer !== "function") {
			throw new Error("systemPrompt was not the append customizer function");
		}
		expect(customizer(["base", "project footer"])).toEqual([
			"base",
			"project footer",
			"You are Ada.",
		]);
	});

	test("leaves systemPrompt unset when COMPASS_PERSONA is empty or whitespace", async () => {
		// A whitespace-only persona resolves to undefined (resolvePersona), so the
		// `persona ?` guard omits systemPrompt and the agent keeps its default
		// prompt rather than an overlay of blank identity.
		for (const persona of [undefined, "", "   "]) {
			const session = fakeSession();
			const seen: unknown[] = [];
			await main(
				{
					HOME: scratch(),
					...(persona === undefined ? {} : { COMPASS_PERSONA: persona }),
				},
				{
					createSession: (options) => {
						seen.push(options.systemPrompt);
						return Promise.resolve({
							session: session as unknown as AgentSession,
						});
					},
					createTransport: () =>
						fakeCarrier(emptyLog(), { control: emptyControlStream }),
				},
			);
			expect(seen).toEqual([undefined]);
		}
	});

	// ── SEA-1570: the tee-storage composition + resume ────────────────────────
	//
	// `main` builds the tee storage over the socket sink and injects the
	// resulting IndexedSessionStorage into SessionManager.create, passed to
	// createAgentSession as `sessionManager`. This pins that wiring: what reaches
	// createSession is the manager main built (not the SDK's own default), so
	// every session write teems onto the durable lane.
	test("passes the tee-backed SessionManager to createAgentSession", async () => {
		const session = fakeSession();
		let seenManager: unknown;
		await main(
			{ HOME: scratch(), COMPASS_WORKDIR: "/work/repo" },
			{
				createSession: (options) => {
					seenManager = options.sessionManager;
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// A SessionManager built for the resolved cwd, its session file under the
		// SDK's HOME-relative default dir (the beforeEach-pinned scratch HOME).
		// Non-vacuity: a main that passed no manager (SDK default) leaves this
		// undefined → red.
		const manager = seenManager as SessionManager | undefined;
		if (!manager) throw new Error("main passed no sessionManager");
		expect(manager.getCwd()).toBe("/work/repo");
		expect(
			manager
				.getSessionFile()
				?.startsWith(SessionManager.getDefaultSessionDir("/work/repo")),
		).toBe(true);
	});

	// COMPASS_RESUME_SESSION_FILE (exported by T8) is loaded through the SDK-native
	// setSessionFile path BEFORE the session is created — so the resumed history
	// is already present when createAgentSession runs. This pins that the manager
	// handed to createSession carries the fixture's entries, loaded via the tee
	// backend's readFull/loadIndex (no replay code).
	test("resumes COMPASS_RESUME_SESSION_FILE before creating the session", async () => {
		const session = fakeSession();
		// The resume file must be INDEXED by the tee backend's initialize() scan
		// (the wrapper ENOENTs un-indexed paths, indexed-session-storage.ts:177),
		// so it lives in the SDK default session dir for this cwd and is written
		// before main() builds the storage. Current-version fixture → no load-time
		// migration rewrite, so the resume path emits no checkpoint frame.
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_resume.jsonl");
		writeFileSync(resumeFile, sessionFixture([userLine("resumed turn")]));

		let entriesAtCreate: unknown[] | undefined;
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: (options) => {
					entriesAtCreate = (
						options.sessionManager as SessionManager
					).getEntries();
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// Loaded BEFORE createSession ran: the user turn is already in the manager.
		// Non-vacuity: a main that ignored the env var (today's behavior) leaves
		// entriesAtCreate empty → red.
		const texts = (entriesAtCreate ?? [])
			.filter((e): e is { type: "message"; message: { content: string } } => {
				const entry = e as { type?: string };
				return entry.type === "message";
			})
			.map((e) => e.message.content);
		expect(texts).toContain("resumed turn");
	});

	// The storage drain is in the same `finally` as the sink drain, so it runs on
	// the ERROR path too — a crashed session must still flush its queued append
	// tee-sends before teardown. This pins that main awaits storage.drain() even
	// when run() rejects (and still propagates the original error).
	test("drains the tee storage on the error path", async () => {
		const boom = new Error("SDK prompt failed mid-turn");
		const session = fakeSession({ promptError: boom });
		const dir = scratch();
		let drained = false;
		const { storage } = await createTeeSessionStorage(
			{
				emit: () => {},
				emitDurable: () => Promise.resolve(),
				drain: () => Promise.resolve(),
			},
			dir,
		);
		const realDrain = storage.drain.bind(storage);
		storage.drain = async () => {
			drained = true;
			await realDrain();
		};
		const carrier = fakeCarrier(emptyLog(), {
			control: async function* () {
				yield replayCompleteOp(1n);
				yield promptOp(2n, "go");
			},
		});
		await expect(
			main(
				{ HOME: scratch() },
				{
					createSession: () =>
						Promise.resolve({ session: session as unknown as AgentSession }),
					createTransport: () => carrier,
					createSessionStorage: () =>
						Promise.resolve({ storage, backend: undefined as never }),
				},
			),
		).rejects.toBe(boom);
		// Non-vacuity: a main that only drained on the clean path (or omitted the
		// storage drain) leaves this false → red.
		expect(drained).toBe(true);
	});

	// ── T3: the resume proof-smoke (SDK-native load) ──────────────────────────
	//
	// The full round-trip, no Runner: run one main(), drive two turns through the
	// REAL tee-backed SessionManager, capture the durable TranscriptEntry frames
	// off the carrier; reconstruct the session-JSONL body the way T5 does (latest
	// checkpoint body + later delta lines by entry_seq); write it to the default
	// session dir; start a SECOND main() with COMPASS_RESUME_SESSION_FILE at it;
	// and assert the second session's manager carries the first run's turns,
	// loaded via setSessionFile → loadEntriesFromFile — with NO TranscriptEntry
	// frames emitted during the load (reads never tee), and a post-resume turn
	// emitting deltas with a FRESH per-lifetime entry_seq starting at 1 (the
	// server-rebase model, T4). The load touches only the $HOME session dir, so
	// it is checkout-independent.
	test("a teed run reconstructs into a resumable session (SDK-native load)", async () => {
		// ── Run 1: drive two turns through the real tee manager, in the
		// createSession callback (it holds options.sessionManager — the manager
		// main built over the tee storage). appendMessage → tee → durable lane;
		// flush() awaits storage.drain() so the frames are committed to the
		// carrier before the callback returns. ──
		const log1 = emptyLog();
		const session1 = fakeSession();
		await main(
			{ HOME: process.env.HOME },
			{
				createSession: async (options) => {
					const m = options.sessionManager;
					if (!m) throw new Error("run 1: main passed no sessionManager");
					m.appendMessage(userMsg("first question"));
					m.appendMessage(assistantMsg("first answer"));
					m.appendMessage(userMsg("second question"));
					m.appendMessage(assistantMsg("second answer"));
					await m.flush();
					return { session: session1 as unknown as AgentSession };
				},
				createTransport: () =>
					fakeCarrier(log1, { control: emptyControlStream }),
			},
		);

		// The durable transcript frames the run committed, in commit order: the
		// first assistant append rewrites the body (a checkpoint), later appends
		// ride as deltas — and the per-lifetime entry_seq started at 1n.
		const frames1 = transcriptFramesOf(log1);
		expect(frames1.length).toBeGreaterThan(0);
		expect(frames1[0].entrySeq).toBe(1n);
		expect(frames1.some((f) => f.checkpoint)).toBe(true);

		// ── Reconstruct the body the way T5 does: latest checkpoint body + later
		// delta lines by entry_seq. The checkpoint's entryJson IS the full body. ──
		const body = reconstructSessionBody(frames1);

		// ── Run 2: resume from the reconstructed body. ────────────────────────
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_resumed.jsonl");
		writeFileSync(resumeFile, body);

		const log2 = emptyLog();
		const session2 = fakeSession();
		let entriesAtCreate: unknown[] = [];
		let framesAtCreate = 0;
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: async (options) => {
					const m = options.sessionManager;
					if (!m) throw new Error("run 2: main passed no sessionManager");
					// Snapshot BEFORE any post-resume write: the load already ran
					// (setSessionFile precedes createSession).
					entriesAtCreate = m.getEntries();
					framesAtCreate = transcriptFramesOf(log2).length;
					// A post-resume turn emits fresh deltas.
					m.appendMessage(userMsg("post-resume question"));
					m.appendMessage(assistantMsg("post-resume answer"));
					await m.flush();
					return { session: session2 as unknown as AgentSession };
				},
				createTransport: () =>
					fakeCarrier(log2, { control: emptyControlStream }),
			},
		);

		// The resumed session carries the first run's turns (loaded via
		// setSessionFile before createSession ran).
		const resumedTexts = textsOf(entriesAtCreate);
		expect(resumedTexts).toContain("first question");
		expect(resumedTexts).toContain("second question");

		// NO TranscriptEntry frames were emitted during the load — reads never
		// tee, and the current-version fixture triggers no migration rewrite.
		expect(framesAtCreate).toBe(0);

		// The post-resume turn's deltas carry a FRESH per-lifetime entry_seq
		// starting at 1n (the server rebases per session, T4).
		const frames2 = transcriptFramesOf(log2);
		expect(frames2.length).toBeGreaterThan(0);
		expect(frames2[0].entrySeq).toBe(1n);
	});

	// A compaction round-trip: a fixture body whose file contains a superseded
	// compaction loads through the SDK's own elision — proving the T5
	// reconstruction needs no compaction awareness beyond T4's supersession.
	test("a superseded-compaction fixture loads via the SDK's own elision", async () => {
		const session = fakeSession();
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_compacted.jsonl");
		// Two compactions on the active branch; the earlier one is superseded and
		// the SDK's elideSupersededCompactionEntries collapses its summary on load.
		writeFileSync(
			resumeFile,
			sessionFixture([
				compactionLine("c1", null, "first compaction summary"),
				compactionLine("c2", "c1", "second compaction summary"),
				userLine("after compaction", "e-after", "c2"),
			]),
		);

		let entriesAtCreate: unknown[] = [];
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: (options) => {
					const m = options.sessionManager;
					if (!m) throw new Error("main passed no sessionManager");
					entriesAtCreate = m.getEntries();
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// The session loaded (post-compaction entry present) and the superseded
		// compaction's summary was elided by the SDK loader.
		const summaries = compactionSummariesOf(entriesAtCreate);
		expect(summaries).toHaveLength(2);
		expect(summaries[0]).toBe(
			"[Superseded compaction summary elided during session load]",
		);
		expect(summaries[1]).toBe("second compaction summary");
		expect(textsOf(entriesAtCreate)).toContain("after compaction");
	});
});

// A Control stream that closes cleanly with no ops — the shortest complete run.
async function* emptyControlStream(): AsyncGenerator<WireAgentControl> {}

// Push a settled assistant text block through the session's subscribed listener,
// the way the SDK's event stream would. `main` wired that listener to the REAL
// EventMapper, whose `text_end` arm settles the text to a durable
// conversationUpdated frame — so this is the honest way to make the composition
// produce a conversation frame, rather than reaching into the sink.
//
// The mapper reads only `type`, `assistantMessageEvent.type` and `.content` on
// this path (mapping.ts:147, :292-305); the SDK's event types require a full
// `AssistantMessage` on `message`/`partial` that is never read, so the cast names
// exactly the fields under test.
async function settledText(
	session: FakeSession,
	content: string,
): Promise<void> {
	// Event gate: resolves when run() subscribed. No polling, no timer.
	const listener = await session.subscribed;
	listener({
		type: "message_update",
		assistantMessageEvent: { type: "text_end", contentIndex: 0, content },
	} as unknown as Parameters<AgentSessionEventListener>[0]);
}

// Park the caller until the next MACROtask turn — a single `setImmediate`, not a
// duration and not a poll. It is the coarsest thing that is still not a sleep:
// every microtask already queued (and every one they queue) runs first, so it
// cleanly separates "resolved within the microtask phase" from "resolved after an
// event-loop turn". The drain tests use it to place the durable commit strictly
// after a barrier-less `main` would have resolved, making the ordering assertion
// discriminate the two implementations rather than time them.
function nextEventLoopTurn(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setImmediate(resolve);
	return promise;
}

// ── SEA-1570 session-JSONL fixtures ──────────────────────────────────────────
//
// Build a current-version (v3) session body the SDK loader accepts verbatim: a
// 256-byte title slot, a session header, then one JSONL line per entry. Current
// version means setSessionFile loads it without a migration rewrite — so the
// resume path emits NO checkpoint frame (reads never tee). These mirror the
// bytes the tee backend commits, so a T3 reconstruction feeds this exact shape.
let fixtureSeq = 0;

function titleSlot(): string {
	// The SDK's own fixed-width 256-byte title slot serializer — the exact bytes
	// a real session file's first line carries, so the fixture is byte-faithful.
	return `${serializeTitleSlot({ updatedAt: "2026-01-01T00:00:00.000Z" })}`;
}

function sessionHeader(): string {
	fixtureSeq += 1;
	return `${JSON.stringify({
		type: "session",
		version: 3,
		id: `fixture-${fixtureSeq}`,
		timestamp: "2026-01-01T00:00:00.000Z",
		cwd: process.cwd(),
	})}\n`;
}

// One user-message entry line — content the resumed manager exposes via
// getEntries()[i].message.content.
function userLine(
	content: string,
	id = `e${fixtureSeq}-${content}`,
	parentId: string | null = null,
): string {
	return `${JSON.stringify({
		type: "message",
		id,
		parentId,
		timestamp: "2026-01-01T00:00:01.000Z",
		message: { role: "user", content },
	})}\n`;
}

// One compaction entry line, for the superseded-compaction round-trip.
function compactionLine(
	id: string,
	parentId: string | null,
	summary: string,
): string {
	return `${JSON.stringify({
		type: "compaction",
		id,
		parentId,
		timestamp: "2026-01-01T00:00:02.000Z",
		summary,
		shortSummary: `${summary} (short)`,
		firstKeptEntryId: id,
		tokensBefore: 1000,
	})}\n`;
}

// A full current-version session body: title slot + header + the given entry
// lines (already newline-terminated).
function sessionFixture(entryLines: string[]): string {
	return `${titleSlot()}${sessionHeader()}${entryLines.join("")}`;
}

// A user / assistant Message to drive through the real SessionManager. Only the
// fields the persistence path reads (role + content) matter; the wide pi-ai
// Message surface is never touched, so the cast names exactly what is used.
function userMsg(
	content: string,
): Parameters<SessionManager["appendMessage"]>[0] {
	return { role: "user", content } as Parameters<
		SessionManager["appendMessage"]
	>[0];
}

function assistantMsg(
	content: string,
): Parameters<SessionManager["appendMessage"]>[0] {
	return {
		role: "assistant",
		content: [{ type: "text", text: content }],
	} as Parameters<SessionManager["appendMessage"]>[0];
}

// The committed transcript frames (entry_json + checkpoint + entry_seq) that
// reached the carrier on the durable unary, in commit order.
function transcriptFramesOf(
	log: CarrierLog,
): { entryJson: string; checkpoint: boolean; entrySeq: bigint }[] {
	const out: { entryJson: string; checkpoint: boolean; entrySeq: bigint }[] =
		[];
	for (const req of log.durableFrames) {
		const inner = req.frame?.frame;
		if (inner?.case !== "transcriptEntry") continue;
		out.push({
			entryJson: inner.value.entryJson,
			checkpoint: inner.value.checkpoint,
			entrySeq: inner.value.entrySeq,
		});
	}
	return out;
}

// Reconstruct the session-JSONL body the way T5 does: take the latest checkpoint
// (a full-body snapshot, entryJson IS the whole file), then append every delta
// line committed AFTER it, ordered by entry_seq. A pure function over the frames.
function reconstructSessionBody(
	frames: { entryJson: string; checkpoint: boolean; entrySeq: bigint }[],
): string {
	let latestCheckpoint: { entryJson: string; entrySeq: bigint } | undefined;
	for (const f of frames) {
		if (
			f.checkpoint &&
			(!latestCheckpoint || f.entrySeq > latestCheckpoint.entrySeq)
		) {
			latestCheckpoint = { entryJson: f.entryJson, entrySeq: f.entrySeq };
		}
	}
	if (!latestCheckpoint)
		throw new Error("no checkpoint frame to reconstruct from");
	const laterDeltas = frames
		.filter((f) => !f.checkpoint && f.entrySeq > latestCheckpoint.entrySeq)
		.sort((l, r) =>
			l.entrySeq < r.entrySeq ? -1 : l.entrySeq > r.entrySeq ? 1 : 0,
		);
	return (
		latestCheckpoint.entryJson + laterDeltas.map((f) => f.entryJson).join("")
	);
}

// The message-entry contents from a getEntries() array, narrowing each entry
// with `in`/`typeof` (no fabricated inline shape).
function textsOf(entries: unknown[]): string[] {
	const out: string[] = [];
	for (const entry of entries) {
		if (!entry || typeof entry !== "object") continue;
		if (!("type" in entry) || entry.type !== "message") continue;
		if (
			!("message" in entry) ||
			!entry.message ||
			typeof entry.message !== "object"
		)
			continue;
		const message = entry.message;
		if (!("content" in message) || typeof message.content !== "string")
			continue;
		out.push(message.content);
	}
	return out;
}

// The compaction-entry summaries from a getEntries() array, in order.
function compactionSummariesOf(entries: unknown[]): string[] {
	const out: string[] = [];
	for (const entry of entries) {
		if (!entry || typeof entry !== "object") continue;
		if (!("type" in entry) || entry.type !== "compaction") continue;
		if (!("summary" in entry) || typeof entry.summary !== "string") continue;
		out.push(entry.summary);
	}
	return out;
}
