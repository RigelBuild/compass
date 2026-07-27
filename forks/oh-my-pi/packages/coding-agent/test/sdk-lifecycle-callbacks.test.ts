/**
 * SEA-1336 Task 3 — SDK wiring of the lifecycle callbacks.
 *
 * The frozen record (docs/designs/agents/sea-1336-omp-sdk-lifecycle-callbacks.md,
 * Test cycle lines 860-866) requires that the full `createAgentSession` path —
 * not a hand-built AgentSession — threads both host callbacks through to the
 * live session:
 *   - createAgentSession({ onRestartRequested }) → session.requestRestart()
 *     fires the host callback with the session's durable identity;
 *   - createAgentSession({ onBeforeRefresh }) → session.refresh("skills") fires
 *     the hook before the roster re-scan.
 *
 * These are integration tests: they construct through the real SDK entry point
 * (which builds the Agent, session manager, and roster internally) and drive the
 * public session methods. Assertions target the observable callback firing and
 * the durable identity / roster effect, never SDK internals.
 */
import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import type { RefreshScope } from "@oh-my-pi/pi-coding-agent/extensibility/reload";
import { createAgentSession } from "@oh-my-pi/pi-coding-agent/sdk";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import { removeSyncWithRetries, Snowflake } from "@oh-my-pi/pi-utils";

interface RestartInfo {
	sessionId: string;
	sessionFile: string;
}

describe("createAgentSession lifecycle callbacks", () => {
	const tempDirs: string[] = [];
	const authStorages: AuthStorage[] = [];
	let modelRegistry: ModelRegistry;

	beforeEach(async () => {
		// An ambient-credential-free in-memory auth store keeps construction
		// deterministic (no SQLite open / network model probe on the hot path).
		const authStorage = await AuthStorage.create(":memory:");
		authStorages.push(authStorage);
		modelRegistry = new ModelRegistry(authStorage);
	});

	afterEach(() => {
		for (const authStorage of authStorages.splice(0)) {
			authStorage.close();
		}
		for (const tempDir of tempDirs.splice(0)) {
			removeSyncWithRetries(tempDir);
		}
	});

	function makeDirs() {
		const tempDir = fs.mkdtempSync(path.join(os.tmpdir(), `pi-sdk-lifecycle-${Snowflake.next()}-`));
		tempDirs.push(tempDir);
		const cwd = path.join(tempDir, `project-${Snowflake.next()}`);
		const agentDir = path.join(tempDir, "agent");
		fs.mkdirSync(path.join(cwd, ".git"), { recursive: true });
		return { cwd, agentDir };
	}

	function commonOptions(cwd: string, agentDir: string) {
		return {
			cwd,
			agentDir,
			modelRegistry,
			settings: Settings.isolated({ "compaction.enabled": false }),
			disableExtensionDiscovery: true,
			skills: [],
			contextFiles: [],
			promptTemplates: [],
			slashCommands: [],
			enableMCP: false,
			enableLsp: false,
		};
	}

	it("fires onRestartRequested through the full SDK path with the durable identity", async () => {
		const { cwd, agentDir } = makeDirs();
		const calls: RestartInfo[] = [];
		const { session } = await createAgentSession({
			...commonOptions(cwd, agentDir),
			onRestartRequested: info => void calls.push(info),
		});

		// The SDK default is a file-backed session, so requestRestart() can reach
		// ok:true (a callback is bound and a session file exists).
		const expectedId = session.sessionManager.getSessionId();
		const expectedFile = session.sessionFile;
		expect(expectedFile).toBeDefined();

		const result = await session.requestRestart();

		expect(result).toEqual({ ok: true });
		expect(calls).toHaveLength(1);
		expect(calls[0]!.sessionId).toBe(expectedId);
		expect(calls[0]!.sessionFile).toBe(expectedFile!);
		// requestRestart() disposes the session itself; no explicit dispose needed.
	});

	it("fires onBeforeRefresh through the full SDK path before the roster re-scan", async () => {
		const { cwd, agentDir } = makeDirs();
		const skillsDir = path.join(cwd, ".agents", "skills");
		const seenScopes: RefreshScope[] = [];
		// Observed inside the hook: whether the new skill is ALREADY active when the
		// hook runs. It must be false — the hook precedes the re-scan.
		let activeAtHook: boolean | undefined;

		const { session } = await createAgentSession({
			...commonOptions(cwd, agentDir),
			onBeforeRefresh: scope => {
				seenScopes.push(scope);
				activeAtHook = session.skills.some(s => s.name === "late-skill");
				// Stage a fresh skill from inside the hook, before the re-scan.
				const file = path.join(skillsDir, "late-skill", "SKILL.md");
				fs.mkdirSync(path.dirname(file), { recursive: true });
				fs.writeFileSync(
					file,
					"---\nname: late-skill\ndescription: staged by onBeforeRefresh\n---\n\n# late-skill\n\nSkill body.\n",
				);
			},
		});

		try {
			const result = await session.refresh("skills");

			expect(seenScopes).toEqual(["skills"]);
			// The re-scan had not run when the hook fired.
			expect(activeAtHook).toBe(false);
			// After the refresh, the skill the hook staged is active — only possible
			// if the hook ran before the re-scan.
			expect(result.refused).toBeUndefined();
			expect(session.skills.some(s => s.name === "late-skill")).toBe(true);
		} finally {
			await session.dispose();
		}
	});
});
