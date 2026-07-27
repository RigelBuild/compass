/**
 * SEA-1336 Task 1 — AgentSession.requestRestart() contract.
 *
 * The frozen record (docs/designs/agents/sea-1336-omp-sdk-lifecycle-callbacks.md,
 * Test cycle lines 623-702) enumerates the observable contract of the
 * cooperative restart: entry guards (unavailable / no-session-file / busy),
 * the durability barrier, the #restarting latch, coalescing, dispose-first
 * ordering, refresh drain/refuse coordination, and the recoverable-vs-terminal
 * failure split. Every assertion here targets an externally observable effect;
 * private state (#restarting, #restartCall, #activeRefresh) is only ever probed
 * through behavior (refresh() refusal, prompt() no-op, callback invocation),
 * never a test-only production getter.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "bun:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { Agent } from "@oh-my-pi/pi-agent-core";
import { createMockModel } from "@oh-my-pi/pi-ai/providers/mock";
import { AssistantMessageEventStream } from "@oh-my-pi/pi-ai/utils/event-stream";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { AsyncJobManager } from "@oh-my-pi/pi-coding-agent/async";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { type SettingPath, Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import type { RefreshScope } from "@oh-my-pi/pi-coding-agent/extensibility/reload";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import { convertToLlm, USER_INTERRUPT_LABEL } from "@oh-my-pi/pi-coding-agent/session/messages";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { removeSyncWithRetries, Snowflake } from "@oh-my-pi/pi-utils";
import { createAssistantMessage } from "./helpers/agent-session-setup";

interface RestartInfo {
	sessionId: string;
	sessionFile: string;
}

describe("AgentSession.requestRestart", () => {
	let tempDir: string;
	const sessions: AgentSession[] = [];
	const authStorages: AuthStorage[] = [];

	beforeEach(() => {
		tempDir = path.join(os.tmpdir(), `pi-restart-test-${Snowflake.next()}`);
		fs.mkdirSync(tempDir, { recursive: true });
	});

	afterEach(async () => {
		for (const session of sessions.splice(0)) {
			await session.dispose();
		}
		for (const authStorage of authStorages.splice(0)) {
			authStorage.close();
		}
		if (tempDir && fs.existsSync(tempDir)) {
			removeSyncWithRetries(tempDir);
		}
		vi.restoreAllMocks();
		AsyncJobManager.resetForTests();
	});

	// A stream that blocks until abort, so a turn can be held in flight for the
	// latch / wait-window cases.
	function blockingAgent(): Agent {
		const model = getBundledModel("anthropic", "claude-sonnet-4-5")!;
		return new Agent({
			getApiKey: () => "test-key",
			initialState: { model, systemPrompt: ["Test"], tools: [] },
			convertToLlm,
			streamFn: (_model, _context, options) => {
				const stream = new AssistantMessageEventStream();
				const signal = options?.signal;
				queueMicrotask(() => {
					stream.push({ type: "start", partial: createAssistantMessage("") });
					if (signal) {
						signal.addEventListener(
							"abort",
							() => stream.push({ type: "error", reason: "aborted", error: createAssistantMessage("Aborted") }),
							{ once: true },
						);
					}
				});
				return stream;
			},
		});
	}

	// A stream that completes immediately with "Done".
	function completingAgent(): Agent {
		const model = getBundledModel("anthropic", "claude-sonnet-4-5")!;
		return new Agent({
			getApiKey: () => "test-key",
			initialState: { model, systemPrompt: ["Test"], tools: [] },
			convertToLlm,
			streamFn: createMockModel({ handler: () => ({ content: ["Done"] }) }).stream,
		});
	}

	interface HarnessOptions {
		agent?: Agent;
		inMemory?: boolean;
		onRestartRequested?: (info: RestartInfo) => void | Promise<void>;
		onBeforeRefresh?: (scope: RefreshScope) => void | Promise<void>;
		settings?: Partial<Record<SettingPath, unknown>>;
		/** Distinct db + cwd so sibling sessions do not share a manager/file. */
		tag?: string;
	}

	// File-backed by default (real sessionFile). Pass inMemory:true only for the
	// no-session-file case.
	async function createSession(options: HarnessOptions = {}): Promise<AgentSession> {
		const tag = options.tag ?? "s";
		const agent = options.agent ?? completingAgent();
		const cwd = path.join(tempDir, `${tag}-cwd`);
		const sessionDir = path.join(tempDir, `${tag}-sessions`);
		fs.mkdirSync(cwd, { recursive: true });
		fs.mkdirSync(sessionDir, { recursive: true });
		const sessionManager = options.inMemory ? SessionManager.inMemory(cwd) : SessionManager.create(cwd, sessionDir);
		const settings = Settings.isolated({ "compaction.enabled": false, ...options.settings });
		const authStorage = await AuthStorage.create(path.join(tempDir, `${tag}-auth.db`));
		authStorages.push(authStorage);
		const modelRegistry = new ModelRegistry(authStorage, path.join(tempDir, `${tag}-models.yml`));
		authStorage.setRuntimeApiKey("anthropic", "test-key");

		const session = new AgentSession({
			agent,
			sessionManager,
			settings,
			modelRegistry,
			onRestartRequested: options.onRestartRequested,
			onBeforeRefresh: options.onBeforeRefresh,
		});
		sessions.push(session);
		return session;
	}

	async function waitFor(predicate: () => boolean, timeoutMs = 1000): Promise<void> {
		const deadline = Date.now() + timeoutMs;
		while (Date.now() < deadline) {
			if (predicate()) return;
			await Bun.sleep(1);
		}
		throw new Error("Timed out waiting for condition");
	}

	// A released-gate promise: the hook blocks at `entered` until `release()` is
	// called, so ordering between the hook body and a concurrent restart is
	// asserted against an explicit signal rather than a wall-clock sleep.
	function makeGate() {
		let release!: () => void;
		let markEntered!: () => void;
		const released = new Promise<void>(resolve => {
			release = resolve;
		});
		const entered = new Promise<void>(resolve => {
			markEntered = resolve;
		});
		return { released, entered, release, markEntered };
	}

	// 1. callback fires exactly once with the durable identity.
	it("fires the callback once with sessionManager id + session file path", async () => {
		const calls: RestartInfo[] = [];
		const session = await createSession({ onRestartRequested: info => void calls.push(info) });
		const expectedId = session.sessionManager.getSessionId();
		const expectedFile = session.sessionFile;

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: true });
		expect(calls).toHaveLength(1);
		expect(calls[0]!.sessionId).toBe(expectedId);
		expect(calls[0]!.sessionFile).toBe(expectedFile!);
	});

	// 2. unavailable + no flush when no callback is bound.
	it("returns unavailable and does not flush when no callback is bound", async () => {
		const session = await createSession();
		const flush = vi.spyOn(session.sessionManager, "flush");

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: false, reason: "unavailable" });
		expect(flush).not.toHaveBeenCalled();
	});

	// 3. no-session-file for an in-memory manager; callback never invoked.
	it("returns no-session-file for an in-memory session without invoking the callback", async () => {
		let invoked = false;
		const session = await createSession({
			inMemory: true,
			onRestartRequested: () => {
				invoked = true;
			},
		});
		expect(session.sessionFile).toBeUndefined();

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: false, reason: "no-session-file" });
		expect(invoked).toBe(false);
	});

	// 4. durability: a message appended before restart is readable from the file
	// inside the callback.
	it("makes a pre-restart message durable in the file before the callback runs", async () => {
		let readBack: string | undefined;
		const session = await createSession({
			onRestartRequested: async info => {
				const reopened = await SessionManager.open(info.sessionFile);
				readBack = reopened.getEntries().some(entry => {
					if (entry.type !== "message" || entry.message.role !== "assistant") return false;
					const content = entry.message.content;
					const text =
						typeof content === "string"
							? content
							: content
									.filter((block): block is { type: "text"; text: string } => block.type === "text")
									.map(block => block.text)
									.join("");
					return text.includes("Done");
				})
					? "found"
					: "missing";
			},
		});

		await session.prompt("hello");
		await session.waitForIdle();

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: true });
		expect(readBack).toBe("found");
	});

	// 5. async callback is awaited.
	it("does not resolve until an async callback settles", async () => {
		const gate = makeGate();
		let callbackDone = false;
		const session = await createSession({
			onRestartRequested: async () => {
				gate.markEntered();
				await gate.released;
				callbackDone = true;
			},
		});

		const pending = session.requestRestart();
		await gate.entered;
		expect(callbackDone).toBe(false);

		gate.release();
		const result = await pending;

		expect(callbackDone).toBe(true);
		expect(result).toEqual({ ok: true });
	});

	// 6. sibling isolation: A's restart touches only A; B keeps working.
	it("isolates sibling sessions: only the restarting session's callback fires", async () => {
		const aCalls: RestartInfo[] = [];
		let bInvoked = false;
		const sessionA = await createSession({ tag: "A", onRestartRequested: info => void aCalls.push(info) });
		const sessionB = await createSession({
			tag: "B",
			onRestartRequested: () => {
				bInvoked = true;
			},
		});
		const aId = sessionA.sessionManager.getSessionId();

		const result = await sessionA.requestRestart();

		expect(result).toEqual({ ok: true });
		expect(aCalls).toHaveLength(1);
		expect(aCalls[0]!.sessionId).toBe(aId);
		expect(bInvoked).toBe(false);

		// B is untouched — a fresh prompt still runs a turn.
		const before = sessionB.agent.state.messages.length;
		await sessionB.prompt("still alive");
		await sessionB.waitForIdle();
		expect(sessionB.agent.state.messages.length).toBeGreaterThan(before);
	});

	// 7. a throwing callback rejects requestRestart() with that error.
	it("rejects with the callback's error when the callback throws", async () => {
		const boom = new Error("host re-attach exploded");
		const session = await createSession({
			onRestartRequested: () => {
				throw boom;
			},
		});

		await expect(session.requestRestart()).rejects.toBe(boom);
	});

	// 8. re-entrancy: two concurrent calls share one in-flight promise.
	it("coalesces concurrent calls to a single callback invocation and shared result", async () => {
		const gate = makeGate();
		let callbackCount = 0;
		const session = await createSession({
			onRestartRequested: async () => {
				callbackCount++;
				gate.markEntered();
				await gate.released;
			},
		});

		const first = session.requestRestart();
		const second = session.requestRestart();
		await gate.entered;
		gate.release();

		const [r1, r2] = await Promise.all([first, second]);
		expect(callbackCount).toBe(1);
		expect(r1).toEqual({ ok: true });
		expect(r2).toEqual({ ok: true });
	});

	// 9. latch: a concurrently-started prompt does not begin a new turn.
	it("latches out a concurrently-started prompt while a restart is in flight", async () => {
		const gate = makeGate();
		const session = await createSession({
			onRestartRequested: async () => {
				gate.markEntered();
				await gate.released;
			},
		});
		await session.prompt("first");
		await session.waitForIdle();
		const barrier = session.agent.state.messages.length;

		const pending = session.requestRestart();
		await gate.entered;

		// prompt() while latched is a no-op — returns false, appends no turn.
		const started = await session.prompt("blocked by restart");
		expect(started).toBe(false);
		expect(session.agent.state.messages.length).toBe(barrier);

		gate.release();
		await pending;
	});

	// 10. busy guard covers agent.hasQueuedMessages() (raw steer queue) at entry.
	it("refuses busy without disposing when a raw steer is queued at entry", async () => {
		const session = await createSession({
			agent: blockingAgent(),
			onRestartRequested: () => {},
		});
		const flush = vi.spyOn(session.sessionManager, "flush");

		const firstPrompt = session.prompt("first message");
		await waitFor(() => session.isStreaming);
		// A steer while streaming lands in agent's raw queue (an unpersisted buffer).
		await session.steer("queued steer");
		expect(session.agent.hasQueuedMessages()).toBe(true);

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: false, reason: "busy" });
		expect(flush).not.toHaveBeenCalled();

		session.agent.clearAllQueues();
		await session.abort({ reason: USER_INTERRUPT_LABEL });
		await firstPrompt.catch(() => {});
	});

	// 11. dispose-first: the callback observes a disposed session; OMP does not
	// write after the callback.
	it("disposes before the callback fires", async () => {
		let disposedAtCallback: boolean | undefined;
		const session = await createSession({
			onRestartRequested: () => {
				disposedAtCallback = session.isDisposed;
			},
		});

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: true });
		expect(disposedAtCallback).toBe(true);
	});

	// 13a. pre-dispose failure recovery: ensureOnDisk rejects (step 5, before
	// dispose) ⇒ reject, latch dropped, restartCall cleared, retry succeeds.
	it("recovers from a pre-dispose failure: unlatches and a later restart succeeds", async () => {
		let callbackCount = 0;
		const session = await createSession({
			onRestartRequested: () => {
				callbackCount++;
			},
		});
		const failure = new Error("disk full");
		vi.spyOn(session.sessionManager, "ensureOnDisk").mockRejectedValueOnce(failure);

		await expect(session.requestRestart()).rejects.toBe(failure);
		expect(callbackCount).toBe(0);
		expect(session.isDisposed).toBe(false);

		// Latch dropped: refresh() is no longer refused and a prompt starts a turn.
		const refreshResult = await session.refresh("skills");
		expect(refreshResult.refused).toBeUndefined();
		const before = session.agent.state.messages.length;
		const started = await session.prompt("after recovery");
		await session.waitForIdle();
		expect(started).toBe(true);
		expect(session.agent.state.messages.length).toBeGreaterThan(before);

		// #restartCall cleared: a second attempt runs the full sequence.
		const second = await session.requestRestart();
		expect(second).toEqual({ ok: true });
		expect(callbackCount).toBe(1);
	});

	// 13b. terminal: a callback throw AFTER dispose leaves the session terminal —
	// a retry does not re-dispose and does not re-invoke the callback.
	it("stays terminal after a post-dispose callback throw", async () => {
		let callbackCount = 0;
		const session = await createSession({
			onRestartRequested: () => {
				callbackCount++;
				throw new Error("re-attach failed after dispose");
			},
		});

		await expect(session.requestRestart()).rejects.toThrow("re-attach failed after dispose");
		expect(session.isDisposed).toBe(true);
		expect(callbackCount).toBe(1);

		// Coalesced terminal promise: a retry returns the same rejection and never
		// re-invokes the callback (the old session is gone).
		await expect(session.requestRestart()).rejects.toThrow("re-attach failed after dispose");
		expect(callbackCount).toBe(1);
	});

	// 16. agent-initiated turn during restart is gated (decision (l), path B).
	it("gates an agent-initiated triggerTurn message while restarting", async () => {
		const gate = makeGate();
		const session = await createSession({
			onRestartRequested: async () => {
				gate.markEntered();
				await gate.released;
			},
		});
		await session.prompt("first");
		await session.waitForIdle();
		const barrier = session.agent.state.messages.length;

		const pending = session.requestRestart();
		await gate.entered;

		// sendCustomMessage({ triggerTurn: true }) routes through
		// #promptAgentInitiatedMessage, which early-returns while #restarting. The
		// outer sendCustomMessage still returns true (agent-session.ts:9835) — the
		// return value is not the contract. The decision-(l) path-B contract is
		// observable as "no new turn appended past the durability barrier".
		await session.sendCustomMessage(
			{ customType: "agent-initiated", content: "wake up", display: false, attribution: "agent" },
			{ triggerTurn: true },
		);
		expect(session.agent.state.messages.length).toBe(barrier);

		gate.release();
		await pending;
	});

	// 17. deferred-next-turn buffer blocks restart (decision (l), path C).
	it("refuses busy when only the deferred-next-turn buffer is non-empty", async () => {
		const session = await createSession({ agent: blockingAgent(), onRestartRequested: () => {} });
		const flush = vi.spyOn(session.sessionManager, "flush");

		// Hold a turn in flight so sendCustomMessage takes the streaming branch.
		const firstPrompt = session.prompt("first");
		await waitFor(() => session.isStreaming);

		// deliverAs:"nextTurn" + triggerTurn:false while streaming pushes to
		// #pendingNextTurnMessages (agent-session.ts:9794) WITHOUT starting a turn.
		// A non-advisor customType so abort() preserves it (10181-10184).
		await session.sendCustomMessage(
			{ customType: "deferred", content: "next-turn context", display: false, attribution: "agent" },
			{ deliverAs: "nextTurn", triggerTurn: false },
		);

		// End the turn: the non-advisor deferred survives, leaving the session idle
		// with only #pendingNextTurnMessages non-empty (raw queues empty). The
		// user-interrupt reason keeps the content-less blocking abort from being
		// auto-retried (#5375), which would otherwise leak a retry into later tests.
		await session.abort({ reason: USER_INTERRUPT_LABEL });
		await firstPrompt.catch(() => {});
		expect(session.agent.hasQueuedMessages()).toBe(false);

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: false, reason: "busy" });
		expect(flush).not.toHaveBeenCalled();
		expect(session.isDisposed).toBe(false);
	});

	// 18. input queued during the wait window (decision (l), path E): a next-turn
	// message lands while waitForIdle() blocks ⇒ post-wait re-check refuses busy,
	// drops the latch, no dispose.
	it("re-checks after waitForIdle and refuses busy on input queued during the wait", async () => {
		let callbackCount = 0;
		const session = await createSession({
			agent: blockingAgent(),
			onRestartRequested: () => {
				callbackCount++;
			},
		});

		const firstPrompt = session.prompt("first message");
		await waitFor(() => session.isStreaming);

		// Enter restart while the turn is in flight so #doRequestRestart blocks in
		// waitForIdle(). At entry both buffers are empty, so the entry guard passes
		// and the latch is set.
		const restartPending = session.requestRestart();
		// Queue a next-turn message during the wait window: still streaming, so it
		// fills #pendingNextTurnMessages (agent-session.ts:9794) without a turn. It
		// slips past the entry guard but the post-waitForIdle re-check must catch it.
		await session.sendCustomMessage(
			{ customType: "deferred", content: "arrived during wait", display: false, attribution: "agent" },
			{ deliverAs: "nextTurn", triggerTurn: false },
		);
		// Release the held turn so waitForIdle() resolves and the re-check runs. Abort
		// with the user-interrupt reason: a bare abort of the content-less blocking
		// stream is a reason-less abort the session auto-retries (#5375), which would
		// stall waitForIdle() on the retry backoff; the user-interrupt marker makes
		// #isRetryableReasonlessAbort skip it (deterministic settle). The non-advisor
		// deferred survives the abort, so the post-wait re-check sees it.
		await session.abort({ reason: USER_INTERRUPT_LABEL });
		await firstPrompt.catch(() => {});

		const result = await restartPending;
		expect(result).toEqual({ ok: false, reason: "busy" });
		expect(callbackCount).toBe(0);
		expect(session.isDisposed).toBe(false);

		// Latch dropped: a subsequent prompt starts a new turn (a latched session
		// would refuse it). The blocking agent never completes, so observe the turn
		// *begin streaming* rather than awaiting completion, then tear it down.
		session.agent.clearAllQueues();
		const afterPrompt = session.prompt("after refusal");
		await waitFor(() => session.isStreaming);
		expect(session.isStreaming).toBe(true);
		await session.abort({ reason: USER_INTERRUPT_LABEL });
		await afterPrompt.catch(() => {});
	});

	// 12. refresh coordination: a slow onBeforeRefresh in flight ⇒ restart drains
	// it before dispose, AND a refresh started after the latch performs no swap.
	it("drains an in-flight refresh before dispose and refuses a post-latch refresh", async () => {
		const gate = makeGate();
		const scopes: RefreshScope[] = [];
		let refreshHookReleased = false;
		let disposedObserved = false;
		const session = await createSession({
			onBeforeRefresh: async scope => {
				scopes.push(scope);
				gate.markEntered();
				await gate.released;
				refreshHookReleased = true;
			},
			onRestartRequested: () => {
				// Dispose runs (step 6) strictly after the in-flight refresh settled.
				disposedObserved = refreshHookReleased;
			},
		});

		// Start a refresh whose hook blocks on the gate.
		const refreshPending = session.refresh("skills");
		await gate.entered;

		// Concurrent restart: requestRestart() sets #restarting synchronously
		// before returning, so the latch is already observable here.
		const restartPending = session.requestRestart();

		// A refresh started AFTER the latch early-returns refused, without invoking
		// the hook a second time (no additional scope recorded, no swap).
		const postLatch = await session.refresh("rules");
		expect(postLatch).toEqual({ refused: "restarting" });
		expect(scopes).toEqual(["skills"]);

		gate.release();
		const [refreshResult, restartResult] = await Promise.all([refreshPending, restartPending]);
		expect(refreshResult.refused).toBeUndefined();
		expect(restartResult).toEqual({ ok: true });
		// Dispose observed the drained refresh as already settled.
		expect(disposedObserved).toBe(true);
	});

	// 19. stale refresh handle does not poison restart (decision (j) finding A).
	it("does not observe a prior failed refresh once it has fully settled", async () => {
		let hookCalls = 0;
		const session = await createSession({
			onBeforeRefresh: scope => {
				hookCalls++;
				// Only the first (rules) refresh throws; later restarts must not see it.
				if (scope === "rules") throw new Error("staged rule write failed");
			},
			onRestartRequested: () => {},
		});

		// A refresh whose hook rejects, fully settled with no concurrent restart.
		await expect(session.refresh("rules")).rejects.toThrow("staged rule write failed");
		expect(hookCalls).toBe(1);

		// The settled (rejected) handle was identity-cleared, so a later restart
		// with no in-flight refresh does not drain it — it disposes cleanly.
		const first = await session.requestRestart();
		expect(first).toEqual({ ok: true });

		// Restart is coalesced-terminal after a successful dispose; a repeat returns
		// the same ok result (the session is gone but the promise is cached).
		const second = await session.requestRestart();
		expect(second).toEqual({ ok: true });
	});

	// 20. drained refresh failure aborts restart recoverably (decision (j)).
	it("aborts recoverably when the drained refresh's hook throws, then succeeds on retry", async () => {
		const gate = makeGate();
		let failNextHook = true;
		let callbackCount = 0;
		const session = await createSession({
			onBeforeRefresh: async () => {
				gate.markEntered();
				await gate.released;
				if (failNextHook) throw new Error("partial write then throw");
			},
			onRestartRequested: () => {
				callbackCount++;
			},
		});

		// Start a refresh whose hook will throw once released.
		const refreshPending = session.refresh("skills");
		await gate.entered;

		// Concurrent restart latches synchronously and, in #doRequestRestart, will
		// await the in-flight refresh handle before dispose.
		const restartPending = session.requestRestart();

		// Release the hook: the refresh rejects, and the restart drain observes the
		// real rejection (not the swallowing #refreshTail).
		gate.release();
		await expect(refreshPending).rejects.toThrow("partial write then throw");
		await expect(restartPending).rejects.toThrow("partial write then throw");
		expect(callbackCount).toBe(0);
		expect(session.isDisposed).toBe(false);

		// The host re-stages; a second restart (no refresh in flight) succeeds.
		failNextHook = false;
		const retry = await session.requestRestart();
		expect(retry).toEqual({ ok: true });
		expect(callbackCount).toBe(1);
	});

	// 21. Fix F2 — pre-dispose quiescence re-check. Input that arrives AFTER the
	// post-waitForIdle re-check (path E, agent-session.ts:7977) but during the
	// flush / ensureOnDisk / drainedRefresh awaits — the window between that
	// re-check and dispose — must still refuse busy, drop the latch, and leave the
	// session alive. A gated onBeforeRefresh parks the restart at
	// `await drainedRefresh` (agent-session.ts:7993); an ensureOnDisk spy proves the
	// restart is already PAST the second re-check, so the injected steer lands
	// squarely in the F2 window guarded by the third re-check (:8003).
	//
	// RED (pre-fix, no third re-check at :8003): the steer is invisible past :7977,
	// so dispose runs and the callback fires — result {ok:true}, callbackCount 1,
	// session disposed, the in-memory steer silently lost across the recycle.
	it("refuses busy on input queued during the pre-dispose drain window", async () => {
		const gate = makeGate();
		const scopes: RefreshScope[] = [];
		let callbackCount = 0;
		const session = await createSession({
			onBeforeRefresh: async scope => {
				scopes.push(scope);
				gate.markEntered();
				await gate.released;
			},
			onRestartRequested: () => {
				callbackCount++;
			},
		});

		// A refresh whose hook blocks on the gate: #activeRefresh is in flight, so a
		// concurrent restart snapshots it and parks on `await drainedRefresh`.
		const refreshPending = session.refresh("skills");
		await gate.entered;

		// The session is idle (no turn), so the restart's waitForIdle() + first
		// re-check + flush + ensureOnDisk all sail through and it parks on the drained
		// refresh. Flag ensureOnDisk's completion so the steer is injected strictly
		// AFTER the path-E re-check (:7977) — squarely in the F2 window.
		let ensureOnDiskDone = false;
		const ensureOnDisk = session.sessionManager.ensureOnDisk.bind(session.sessionManager);
		vi.spyOn(session.sessionManager, "ensureOnDisk").mockImplementation(async () => {
			await ensureOnDisk();
			ensureOnDiskDone = true;
		});

		const restartPending = session.requestRestart();
		await waitFor(() => ensureOnDiskDone);

		// Inject input into the now-open window. Not streaming here, so the
		// deliverAs:"nextTurn" path would append to the transcript (persisted), NOT
		// #pendingNextTurnMessages — a raw agent.steer is the host-steer analogue
		// that fills #hasUnpersistedInput() without starting a turn (no idle drain is
		// scheduled off the raw agent queue).
		session.agent.steer(createAssistantMessage("arrived during pre-dispose drain"));
		expect(session.agent.hasQueuedMessages()).toBe(true);

		// Release the refresh so `await drainedRefresh` resolves and the third
		// re-check (:8003) runs — it must catch the steer.
		gate.release();
		await refreshPending;
		const result = await restartPending;

		expect(result).toEqual({ ok: false, reason: "busy" });
		expect(callbackCount).toBe(0);
		expect(session.isDisposed).toBe(false);
		expect(scopes).toEqual(["skills"]);

		// Latch dropped: with the queued steer cleared, a fresh prompt starts and
		// completes a turn (a latched session would no-op it).
		session.agent.clearAllQueues();
		const barrier = session.agent.state.messages.length;
		const started = await session.prompt("after refusal");
		await session.waitForIdle();
		expect(started).toBe(true);
		expect(session.agent.state.messages.length).toBeGreaterThan(barrier);
	});

	// 22. Fix F1 — auto-continue turn-start gated on #restarting. A queued-message
	// drain (#scheduleQueuedMessageDrain -> #scheduleAgentContinue -> agent.continue(),
	// a raw turn-start waker that bypasses AgentSession.prompt's latch) must NOT
	// start a turn while a restart is latched. #canAutoContinueForFollowUp() now
	// early-returns false under #restarting (agent-session.ts:9608), so the drain is
	// never even scheduled; the authoritative #scheduleAgentContinue guard (:5299)
	// backs it.
	//
	// A gated onBeforeRefresh parks the restart pre-dispose with #restarting true.
	// session.steer() both enqueues a steer (making the follow-up predicate true but
	// for the latch — peekSteeringQueue().length > 0, :9615) and triggers the idle
	// drain synchronously; if a turn starts, #beginInFlight() flips isStreaming true
	// within that same synchronous chain, so the post-steer check is deterministic
	// with no wall-clock wait.
	//
	// RED (pre-fix, no #restarting check in the predicate/guard): the drain
	// schedules, agent.continue() runs, and isStreaming flips true on the latched,
	// tearing-down session. Post-fix it stays quiescent.
	it("does not auto-continue a queued-message drain while restarting", async () => {
		const gate = makeGate();
		const session = await createSession({
			agent: blockingAgent(),
			onBeforeRefresh: async () => {
				gate.markEntered();
				await gate.released;
			},
			onRestartRequested: () => {},
		});

		// Park a restart pre-dispose: a gated refresh in flight holds
		// #doRequestRestart at `await drainedRefresh`, so #restarting is latched but
		// the session is not yet disposed.
		const refreshPending = session.refresh("skills");
		await gate.entered;
		const restartPending = session.requestRestart();
		const barrier = session.agent.state.messages.length;

		// Enqueue a steer AND trigger the idle drain. While latched, the drain's
		// #canAutoContinueForFollowUp() gate refuses, so no agent.continue() fires.
		await session.steer("queued while restarting");

		expect(session.isStreaming).toBe(false);
		expect(session.agent.state.messages.length).toBe(barrier);
		// The steer stays queued — nothing consumed it into a turn.
		expect(session.agent.hasQueuedMessages()).toBe(true);

		// Teardown: release the refresh (the restart then refuses busy on the still-
		// queued steer via the F2 re-check) and settle both.
		gate.release();
		await Promise.allSettled([refreshPending, restartPending]);
	});
});
