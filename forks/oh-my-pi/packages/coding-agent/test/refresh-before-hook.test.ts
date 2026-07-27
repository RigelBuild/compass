/**
 * SEA-1336 Task 2 — onBeforeRefresh host hook + refresh mutex.
 *
 * The frozen record (docs/designs/agents/sea-1336-omp-sdk-lifecycle-callbacks.md,
 * Test cycle lines 781-825) enumerates the observable contract of the
 * pre-refresh hook and the serialized refresh critical section: the hook runs
 * FIRST, before any surface is re-read; it is awaited; a throw rejects the
 * refresh and leaves every roster global untouched; overlapping refreshes
 * serialize on #refreshTail with no swap interleave; the mutex survives a
 * throwing hook; a latched restart makes refresh() report
 * { refused: "restarting" }; summarizeRefresh renders that marker verbatim;
 * and the raw #activeRefresh handle a concurrent restart drains carries the
 * hook's real rejection (never the failure-swallowing tail) and is
 * identity-cleared on settle.
 *
 * Assertions target observable effects only: the resolved rule://+skill://
 * roster after a reload, the ordered enter/exit trace of the gated hook, the
 * refresh() result, the surfaced summary string, and — for the handle
 * mechanics — the response of a concurrent requestRestart(). No private field
 * (#refreshTail, #activeRefresh, #restarting) is read through a test-only seam.
 */
import { afterEach, beforeEach, describe, expect, it, spyOn, vi } from "bun:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { Agent } from "@oh-my-pi/pi-agent-core";
import { createMockModel } from "@oh-my-pi/pi-ai/providers/mock";
import { buildModel } from "@oh-my-pi/pi-catalog/build";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { AsyncJobManager } from "@oh-my-pi/pi-coding-agent/async";
import { reset as resetCapabilities } from "@oh-my-pi/pi-coding-agent/capability";
import { getActiveRules, resetActiveRulesForTests } from "@oh-my-pi/pi-coding-agent/capability/rule";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
// Register the discovery providers (skills/rules) as a side effect.
import "@oh-my-pi/pi-coding-agent/discovery";
import { TtsrManager } from "@oh-my-pi/pi-coding-agent/export/ttsr";
import type { RefreshScope } from "@oh-my-pi/pi-coding-agent/extensibility/reload";
import { resetActiveSkillsForTests } from "@oh-my-pi/pi-coding-agent/extensibility/skills";
import { RuleProtocolHandler } from "@oh-my-pi/pi-coding-agent/internal-urls/rule-protocol";
import type { InternalUrl } from "@oh-my-pi/pi-coding-agent/internal-urls/types";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import { convertToLlm } from "@oh-my-pi/pi-coding-agent/session/messages";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import type { ToolSession } from "@oh-my-pi/pi-coding-agent/tools";
import { RefreshTool, summarizeRefresh } from "@oh-my-pi/pi-coding-agent/tools/refresh";
import { removeSyncWithRetries, Snowflake, setAgentDir } from "@oh-my-pi/pi-utils";

type HookFn = (scope: RefreshScope) => void | Promise<void>;

// A released-gate promise: the hook blocks at `entered` until `release()` is
// called, so ordering between the hook body and a concurrent caller is asserted
// against an explicit signal rather than a wall-clock sleep.
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

async function waitFor(predicate: () => boolean, timeoutMs = 1000): Promise<void> {
	const deadline = Date.now() + timeoutMs;
	while (Date.now() < deadline) {
		if (predicate()) return;
		await Bun.sleep(1);
	}
	throw new Error("Timed out waiting for condition");
}

// ───────────────────────────────────────────────────────────────────────────
// Roster harness: a real skills+rules reload path (TtsrManager + skillsSettings)
// under a fake home so the discovery walk-up is bounded and full-suite-safe.
// Mirrors test/refresh-roster-reload.test.ts. Used for the hook-ordering,
// write-then-rescan, throw-no-reread, and mutex cases where the observable is
// the actual roster after a reload.
// ───────────────────────────────────────────────────────────────────────────
describe("onBeforeRefresh hook + refresh mutex (roster path)", () => {
	let tempHome: string;
	let cwd: string;
	let originalAgentDir: string;
	const sessions: AgentSession[] = [];

	function createModel() {
		return buildModel({
			id: "mock",
			name: "mock",
			api: "openai-responses",
			provider: "openai",
			baseUrl: "https://example.invalid",
			reasoning: false,
			input: ["text"],
			cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
			contextWindow: 8192,
			maxTokens: 2048,
		});
	}

	function ruleUrl(name: string): InternalUrl {
		return Object.assign(new URL(`rule://${name}`), { rawHost: name }) as InternalUrl;
	}

	function writeSkill(dir: string, name: string, description: string): void {
		const file = path.join(dir, name, "SKILL.md");
		fs.mkdirSync(path.dirname(file), { recursive: true });
		fs.writeFileSync(file, `---\nname: ${name}\ndescription: ${description}\n---\n\n# ${name}\n\nSkill body.\n`);
	}

	function writeRule(dir: string, name: string, description: string, body: string): void {
		fs.mkdirSync(dir, { recursive: true });
		fs.writeFileSync(path.join(dir, `${name}.md`), `---\ndescription: ${description}\n---\n\n${body}\n`);
	}

	beforeEach(() => {
		tempHome = fs.mkdtempSync(path.join(os.tmpdir(), "omp-before-refresh-home-"));
		cwd = path.join(tempHome, "project");
		fs.mkdirSync(path.join(cwd, ".git"), { recursive: true });
		spyOn(os, "homedir").mockReturnValue(tempHome);
		originalAgentDir = process.env.PI_CODING_AGENT_DIR ?? "";
		setAgentDir(path.join(tempHome, ".omp", "agent"));
		resetCapabilities();
		resetActiveSkillsForTests();
		resetActiveRulesForTests();
	});

	afterEach(async () => {
		for (const session of sessions.splice(0)) {
			await session.dispose();
		}
		spyOn(os, "homedir").mockRestore();
		if (originalAgentDir) setAgentDir(originalAgentDir);
		resetCapabilities();
		resetActiveSkillsForTests();
		resetActiveRulesForTests();
		fs.rmSync(tempHome, { recursive: true, force: true });
	});

	function createSession(onBeforeRefresh?: HookFn): AgentSession {
		const settings = Settings.isolated({ "compaction.enabled": false });
		const agent = new Agent({
			initialState: { model: createModel(), systemPrompt: ["initial"], tools: [], messages: [] },
		});
		const session = new AgentSession({
			agent,
			sessionManager: SessionManager.inMemory(cwd),
			settings,
			modelRegistry: {} as never,
			toolRegistry: new Map(),
			ttsrManager: new TtsrManager(settings.getGroup("ttsr")),
			skillsSettings: settings.getGroup("skills"),
			onBeforeRefresh,
		});
		sessions.push(session);
		return session;
	}

	// Bullet 4 (and bullet 1's ordering): the hook stages a rule to disk, and it
	// resolves via rule:// only AFTER the refresh completes — which can only
	// happen if the hook ran BEFORE the re-scan. This is the strongest ordering
	// proof: were the hook to run after the roster re-read, the new rule would
	// not be active.
	it("runs the hook before the re-scan so a rule the hook writes is picked up", async () => {
		const rulesDir = path.join(cwd, ".agents", "rules");
		const seenScopes: RefreshScope[] = [];
		const session = createSession(scope => {
			seenScopes.push(scope);
			// Stage a fresh rule from inside the hook, before any surface re-read.
			writeRule(rulesDir, "hook-rule", "A rule staged by onBeforeRefresh.", "Follow the hook rule.");
		});
		const handler = new RuleProtocolHandler();

		// Not resolvable before the refresh.
		await expect(handler.resolve(ruleUrl("hook-rule"))).rejects.toThrow(/Unknown rule: hook-rule/);

		const result = await session.refresh("rules");

		// The hook saw the exact scope, and the rule it wrote is now active.
		expect(seenScopes).toEqual(["rules"]);
		expect(result.rules).toBeGreaterThanOrEqual(1);
		const resource = await handler.resolve(ruleUrl("hook-rule"));
		expect(resource.content).toContain("Follow the hook rule.");
		expect(getActiveRules().some(r => r.name === "hook-rule")).toBe(true);
	});

	// Bullet 2: an async hook is awaited — the refresh result is not produced
	// until the hook's promise settles.
	it("does not resolve the refresh until an async hook settles", async () => {
		const gate = makeGate();
		let hookSettled = false;
		const session = createSession(async () => {
			gate.markEntered();
			await gate.released;
			hookSettled = true;
		});

		const pending = session.refresh("skills");
		await gate.entered;
		expect(hookSettled).toBe(false);

		gate.release();
		const result = await pending;
		expect(hookSettled).toBe(true);
		expect(result.refused).toBeUndefined();
	});

	// Bullet 3: no-op when unset — refresh() runs the roster reload normally and
	// returns a populated result (a skill written to disk becomes active).
	it("reloads normally when no hook is set", async () => {
		const session = createSession();
		writeSkill(path.join(cwd, ".agents", "skills"), "plain-skill", "No hook needed.");

		const result = await session.refresh("skills");

		expect(result.refused).toBeUndefined();
		expect(result.skills).toBeGreaterThanOrEqual(1);
		expect(session.skills.some(s => s.name === "plain-skill")).toBe(true);
	});

	// Bullet 5: a throwing hook rejects refresh() and no surface is re-read — a
	// skill sitting on disk stays INACTIVE because the re-scan never ran.
	it("rejects and leaves the roster untouched when the hook throws", async () => {
		const boom = new Error("staging the config failed");
		const session = createSession(() => {
			throw boom;
		});
		writeSkill(path.join(cwd, ".agents", "skills"), "never-loaded", "Should not become active.");

		await expect(session.refresh("skills")).rejects.toBe(boom);

		// The re-scan was never reached, so the on-disk skill is not active.
		expect(session.skills.some(s => s.name === "never-loaded")).toBe(false);
	});

	// Bullet 6: the hook fires on the RefreshTool path too (RefreshTool.execute →
	// session.refresh).
	it("fires the hook when refresh is driven through the RefreshTool", async () => {
		const seenScopes: RefreshScope[] = [];
		const session = createSession(scope => void seenScopes.push(scope));
		const tool = new RefreshTool(session as unknown as ToolSession);

		const out = await tool.execute("call-1", { scope: "skills" });

		expect(seenScopes).toEqual(["skills"]);
		expect(out.isError).toBeUndefined();
		expect(out.content[0]).toMatchObject({ type: "text" });
	});

	// Bullet 7: the mutex serializes overlapping refreshes. The first hook blocks
	// on a gate; the second refresh, admitted behind the tail, must not enter its
	// hook until the first refresh has fully resolved — no interleave of the two
	// surface swaps.
	it("serializes overlapping refreshes so hooks never interleave", async () => {
		const gate = makeGate();
		const events: string[] = [];
		const session = createSession(async scope => {
			events.push(`enter:${scope}`);
			if (scope === "skills") await gate.released;
			events.push(`exit:${scope}`);
		});

		const first = session.refresh("skills");
		await waitFor(() => events.includes("enter:skills"));

		// Admit the second refresh; it chains onto #refreshTail behind the first.
		const second = session.refresh("rules");
		// Give the microtask queue a turn: the second hook still cannot start
		// while the first is blocked on the gate.
		await Promise.resolve();
		expect(events).toEqual(["enter:skills"]);

		gate.release();
		await Promise.all([first, second]);
		expect(events).toEqual(["enter:skills", "exit:skills", "enter:rules", "exit:rules"]);
	});

	// Bullet 8: the mutex survives a throwing refresh — the tail never stays
	// rejected, so a subsequent refresh runs normally.
	it("keeps the mutex usable after a hook throws", async () => {
		let failNext = true;
		const session = createSession(() => {
			if (failNext) throw new Error("first refresh hook failed");
		});

		await expect(session.refresh("skills")).rejects.toThrow("first refresh hook failed");

		// The tail resolved (swallowing the throw), so the next refresh runs.
		failNext = false;
		writeSkill(path.join(cwd, ".agents", "skills"), "post-throw-skill", "Loaded after recovery.");
		const result = await session.refresh("skills");
		expect(result.refused).toBeUndefined();
		expect(session.skills.some(s => s.name === "post-throw-skill")).toBe(true);
	});
});

// ───────────────────────────────────────────────────────────────────────────
// Restart-coordination harness: a real session file (SessionManager.create) so
// requestRestart() latches and can drain the in-flight refresh. Mirrors
// test/agent-session-restart.test.ts. Used for the refused-marker and
// drain/identity-clear handle cases, whose observable is the response of a
// concurrent restart.
// ───────────────────────────────────────────────────────────────────────────
describe("refresh coordination with a latched restart", () => {
	let tempDir: string;
	const sessions: AgentSession[] = [];
	const authStorages: AuthStorage[] = [];

	beforeEach(() => {
		tempDir = path.join(os.tmpdir(), `pi-before-refresh-restart-${Snowflake.next()}`);
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

	function completingAgent(): Agent {
		const model = getBundledModel("anthropic", "claude-sonnet-4-5")!;
		return new Agent({
			getApiKey: () => "test-key",
			initialState: { model, systemPrompt: ["Test"], tools: [] },
			convertToLlm,
			streamFn: createMockModel({ handler: () => ({ content: ["Done"] }) }).stream,
		});
	}

	interface FileHarnessOptions {
		onBeforeRefresh?: HookFn;
		onRestartRequested?: () => void | Promise<void>;
	}

	async function createFileSession(options: FileHarnessOptions = {}): Promise<AgentSession> {
		const cwd = path.join(tempDir, "cwd");
		const sessionDir = path.join(tempDir, "sessions");
		fs.mkdirSync(cwd, { recursive: true });
		fs.mkdirSync(sessionDir, { recursive: true });
		const settings = Settings.isolated({ "compaction.enabled": false });
		const authStorage = await AuthStorage.create(path.join(tempDir, "auth.db"));
		authStorages.push(authStorage);
		const modelRegistry = new ModelRegistry(authStorage, path.join(tempDir, "models.yml"));
		authStorage.setRuntimeApiKey("anthropic", "test-key");
		const session = new AgentSession({
			agent: completingAgent(),
			sessionManager: SessionManager.create(cwd, sessionDir),
			settings,
			modelRegistry,
			onBeforeRefresh: options.onBeforeRefresh,
			onRestartRequested: options.onRestartRequested,
		});
		sessions.push(session);
		return session;
	}

	// Bullet 9: while a restart is latched, refresh() returns
	// { refused: "restarting" } without chaining onto the tail or invoking the
	// hook. requestRestart() sets #restarting synchronously before returning, and
	// refresh()'s first line checks the latch and returns before any dispose
	// await resumes — so this observes the latch, not a disposed session.
	it("refuses a refresh while a restart is latched, without invoking the hook", async () => {
		const gate = makeGate();
		let hookCalls = 0;
		const session = await createFileSession({
			onBeforeRefresh: () => {
				hookCalls++;
			},
			onRestartRequested: async () => {
				gate.markEntered();
				await gate.released;
			},
		});

		const restartPending = session.requestRestart();
		const refused = await session.refresh("skills");

		expect(refused).toEqual({ refused: "restarting" });
		expect(hookCalls).toBe(0);

		gate.release();
		expect(await restartPending).toEqual({ ok: true });
	});

	// Bullet 11: the raw #activeRefresh handle a concurrent restart drains carries
	// the hook's REAL rejection (not the failure-swallowing #refreshTail): the
	// restart observes it and aborts recoverably. And the tail itself resolves, so
	// a later refresh with no restart runs normally — proving both handles.
	it("surfaces the in-flight refresh's hook failure to a draining restart, tail intact", async () => {
		const gate = makeGate();
		let failNextHook = true;
		let callbackCount = 0;
		const session = await createFileSession({
			onBeforeRefresh: async () => {
				gate.markEntered();
				await gate.released;
				if (failNextHook) throw new Error("hook write then throw");
			},
			onRestartRequested: () => {
				callbackCount++;
			},
		});

		const refreshPending = session.refresh("skills");
		await gate.entered;
		const restartPending = session.requestRestart();

		gate.release();
		await expect(refreshPending).rejects.toThrow("hook write then throw");
		// The restart drained the RAW handle, saw the rejection, and aborted
		// recoverably (no dispose, no callback).
		await expect(restartPending).rejects.toThrow("hook write then throw");
		expect(callbackCount).toBe(0);
		expect(session.isDisposed).toBe(false);

		// The tail was not poisoned: a plain refresh (no restart) runs normally.
		failNextHook = false;
		const result = await session.refresh("skills");
		expect(result.refused).toBeUndefined();
	});

	// Bullet 12: the handle clears on settle (identity-checked). After a refresh
	// whose hook rejected has fully settled with no overlapping successor,
	// #activeRefresh is cleared — so a later requestRestart() with no refresh in
	// flight does NOT drain the stale rejected promise; it disposes cleanly.
	it("does not drain a settled, cleared refresh handle on a later restart", async () => {
		let hookCalls = 0;
		let callbackCount = 0;
		const session = await createFileSession({
			onBeforeRefresh: scope => {
				hookCalls++;
				if (scope === "rules") throw new Error("staged rule write failed");
			},
			onRestartRequested: () => {
				callbackCount++;
			},
		});

		// A refresh whose hook rejects, fully settled with no concurrent restart.
		await expect(session.refresh("rules")).rejects.toThrow("staged rule write failed");
		expect(hookCalls).toBe(1);

		// The settled (rejected) handle was identity-cleared, so the restart does
		// not observe it — it disposes and reaches ok.
		const result = await session.requestRestart();
		expect(result).toEqual({ ok: true });
		expect(callbackCount).toBe(1);
	});
});

// ───────────────────────────────────────────────────────────────────────────
// Refused-marker rendering (decision: report the refusal, never "nothing to
// reload"). Pure summarizeRefresh + the RefreshTool surface.
// ───────────────────────────────────────────────────────────────────────────
describe("summarizeRefresh renders the restart-refused marker", () => {
	// Bullet 10: the exact operator string for every scope.
	it("renders 'Refresh skipped (<scope>): restart in progress.'", () => {
		for (const scope of ["skills", "rules", "settings", "mcp", "all"] as const) {
			expect(summarizeRefresh(scope, { refused: "restarting" })).toBe(
				`Refresh skipped (${scope}): restart in progress.`,
			);
		}
	});

	// Bullet 10: the RefreshTool surfaces the refusal string, not the empty-result
	// "nothing to reload" no-op summary.
	it("surfaces the refusal through RefreshTool.execute", async () => {
		const refresh = vi.fn(async (_scope: RefreshScope) => ({ refused: "restarting" }) as const);
		const session = { refresh } as unknown as ToolSession;
		const tool = new RefreshTool(session);

		const out = await tool.execute("call-1", { scope: "all" });

		expect(out.content).toEqual([{ type: "text", text: "Refresh skipped (all): restart in progress." }]);
		expect(out.isError).toBeUndefined();
	});
});
