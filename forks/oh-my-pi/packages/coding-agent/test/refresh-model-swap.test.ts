import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import * as fs from "node:fs";
import * as path from "node:path";
import { Agent } from "@oh-my-pi/pi-agent-core";
import type { Api, Model } from "@oh-my-pi/pi-ai";
import { getBundledModel } from "@oh-my-pi/pi-catalog/models";
import { ModelRegistry } from "@oh-my-pi/pi-coding-agent/config/model-registry";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { AuthStorage } from "@oh-my-pi/pi-coding-agent/session/auth-storage";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { getProjectAgentDir, TempDir } from "@oh-my-pi/pi-utils";
import { YAML } from "bun";

// refresh("settings") re-reads config.yml and re-resolves the default model,
// swapping the session's active model iff it changed AND the user has not
// pinned a session-only /model override. Both branches are observed through the
// public refresh() surface (result.modelSwapped + session.model) — never a
// private.
describe("AgentSession refresh('settings') default-model swap", () => {
	let tempDir: TempDir;
	let agentDir: string;
	let projectDir: string;
	let configPath: string;
	let authStorage: AuthStorage;
	let modelRegistry: ModelRegistry;
	const sessions: AgentSession[] = [];

	function anthropic(id: string): Model<Api> {
		const model = getBundledModel("anthropic", id);
		if (!model) throw new Error(`Expected bundled anthropic/${id} to exist`);
		return model;
	}

	function modelValue(model: Model<Api>): string {
		return `${model.provider}/${model.id}`;
	}

	function writeConfig(defaultModel: Model<Api>): void {
		fs.writeFileSync(configPath, YAML.stringify({ modelRoles: { default: modelValue(defaultModel) } }, null, 2));
	}

	async function createSession(initialModel: Model<Api>): Promise<AgentSession> {
		const settings = await Settings.loadIsolated({ cwd: projectDir, agentDir });
		const agent = new Agent({
			initialState: { model: initialModel, systemPrompt: ["Test"], tools: [], messages: [] },
		});
		const session = new AgentSession({
			agent,
			sessionManager: SessionManager.inMemory(projectDir),
			settings,
			modelRegistry,
		});
		sessions.push(session);
		return session;
	}

	beforeEach(async () => {
		tempDir = TempDir.createSync("@pi-refresh-model-swap-");
		agentDir = tempDir.join("agent");
		projectDir = tempDir.join("project");
		fs.mkdirSync(agentDir, { recursive: true });
		fs.mkdirSync(getProjectAgentDir(projectDir), { recursive: true });
		configPath = path.join(agentDir, "config.yml");
		authStorage = await AuthStorage.create(path.join(tempDir.path(), "auth.db"));
		authStorage.setRuntimeApiKey("anthropic", "test-key");
		modelRegistry = new ModelRegistry(authStorage, path.join(tempDir.path(), "models.yml"));
	});

	afterEach(async () => {
		for (const session of sessions.splice(0)) {
			await session.dispose();
		}
		authStorage.close();
		tempDir.removeSync();
	});

	it("swaps the active model when the default model changed on disk", async () => {
		const initial = anthropic("claude-sonnet-4-5");
		const next = anthropic("claude-sonnet-4-6");
		writeConfig(initial);
		const session = await createSession(initial);
		expect(session.model?.id).toBe(initial.id);

		// Operator edits the fleet default in config.yml.
		writeConfig(next);

		const result = await session.refresh("settings");

		expect(result.settingsChanged).toBe(true);
		expect(result.modelSwapped).toBe(true);
		expect(session.model?.provider).toBe(next.provider);
		expect(session.model?.id).toBe(next.id);
	});

	it("does not swap when a session /model override is pinned", async () => {
		const initial = anthropic("claude-sonnet-4-5");
		const pinned = anthropic("claude-sonnet-4-6");
		const newDefault = anthropic("claude-opus-4-1");
		writeConfig(initial);
		const session = await createSession(initial);

		// User pins a session-only model (the /model temporary pick records a
		// model_change entry with a non-default role).
		await session.setModelTemporary(pinned);
		expect(session.model?.id).toBe(pinned.id);

		// The on-disk default then changes to a THIRD model.
		writeConfig(newDefault);

		const result = await session.refresh("settings");

		// Settings did change, but the pin is highest precedence: no swap, and the
		// session stays on the user's pinned model — not the new on-disk default.
		expect(result.settingsChanged).toBe(true);
		expect(result.modelSwapped).toBe(false);
		expect(session.model?.id).toBe(pinned.id);
		expect(session.model?.id).not.toBe(newDefault.id);
	});

	it("reports no swap on a no-op settings reload", async () => {
		const initial = anthropic("claude-sonnet-4-5");
		writeConfig(initial);
		const session = await createSession(initial);

		const result = await session.refresh("settings");

		expect(result.settingsChanged).toBe(false);
		expect(result.modelSwapped).toBe(false);
		expect(session.model?.id).toBe(initial.id);
	});
});
