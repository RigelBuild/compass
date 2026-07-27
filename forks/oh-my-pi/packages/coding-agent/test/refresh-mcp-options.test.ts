import { afterEach, beforeEach, describe, expect, it, spyOn } from "bun:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { Agent } from "@oh-my-pi/pi-agent-core";
import { buildModel } from "@oh-my-pi/pi-catalog/build";
import { reset as resetCapabilities } from "@oh-my-pi/pi-coding-agent/capability";
import { resetActiveRulesForTests } from "@oh-my-pi/pi-coding-agent/capability/rule";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
// Register the discovery providers (skills/rules) as a side effect.
import "@oh-my-pi/pi-coding-agent/discovery";
import { TtsrManager } from "@oh-my-pi/pi-coding-agent/export/ttsr";
import { resetActiveSkillsForTests } from "@oh-my-pi/pi-coding-agent/extensibility/skills";
import { type MCPDiscoverOptions, type MCPLoadResult, MCPManager } from "@oh-my-pi/pi-coding-agent/mcp/manager";
import { AgentSession } from "@oh-my-pi/pi-coding-agent/session/agent-session";
import { SessionManager } from "@oh-my-pi/pi-coding-agent/session/session-manager";
import { setAgentDir } from "@oh-my-pi/pi-utils";

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

const EMPTY_LOAD: MCPLoadResult = { tools: [], errors: new Map(), connectedServers: [], exaApiKeys: [] };

// A fake MCPManager recording the options each discoverAndConnect receives, so
// the test can assert the exact discovery controls refresh("mcp") threads. Only
// the three methods refresh("mcp") calls are implemented (disconnect, discover,
// getTools); the rest of the surface is unused on this path.
function fakeMcpManager(): { manager: MCPManager; calls: (MCPDiscoverOptions | undefined)[] } {
	const calls: (MCPDiscoverOptions | undefined)[] = [];
	const manager = {
		disconnectAll: async () => {},
		discoverAndConnect: async (options?: MCPDiscoverOptions): Promise<MCPLoadResult> => {
			calls.push(options);
			return EMPTY_LOAD;
		},
		getTools: () => [],
	} as unknown as MCPManager;
	return { manager, calls };
}

// S-2: refresh("mcp") must thread the discovery controls startup uses — not
// discoverAndConnect's own defaults — so a user's `mcp.enableProjectConfig:
// false` (and the deliberate Exa filter) survive a mid-session refresh. Pre-fix
// discoverAndConnect() was called with no args → undefined → defaulted true,
// silently re-enabling project MCP config a user opted out of.
describe("AgentSession refresh('mcp') discovery options", () => {
	let tempHome: string;
	let cwd: string;
	let originalAgentDir: string;
	const sessions: AgentSession[] = [];

	beforeEach(() => {
		tempHome = fs.mkdtempSync(path.join(os.tmpdir(), "omp-refresh-mcp-home-"));
		cwd = path.join(tempHome, "project");
		fs.mkdirSync(path.join(cwd, ".git"), { recursive: true });
		spyOn(os, "homedir").mockReturnValue(tempHome);
		originalAgentDir = process.env.PI_CODING_AGENT_DIR ?? "";
		setAgentDir(path.join(tempHome, ".omp", "agent"));
		resetCapabilities();
		resetActiveSkillsForTests();
		resetActiveRulesForTests();
		MCPManager.resetForTests();
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
		MCPManager.resetForTests();
		fs.rmSync(tempHome, { recursive: true, force: true });
	});

	function createSession(settingsOverrides: Record<string, unknown>): AgentSession {
		const settings = Settings.isolated({ "compaction.enabled": false, ...settingsOverrides });
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
		});
		sessions.push(session);
		return session;
	}

	it("threads enableProjectConfig:false from settings into discoverAndConnect", async () => {
		const { manager, calls } = fakeMcpManager();
		MCPManager.setInstance(manager);
		const session = createSession({ "mcp.enableProjectConfig": false });

		await session.refresh("mcp");

		expect(calls).toHaveLength(1);
		expect(calls[0]?.enableProjectConfig).toBe(false);
		// filterExa is always on for a refresh (startup filters Exa deliberately).
		expect(calls[0]?.filterExa).toBe(true);
	});

	it("honors enableProjectConfig:true from settings into discoverAndConnect", async () => {
		const { manager, calls } = fakeMcpManager();
		MCPManager.setInstance(manager);
		const session = createSession({ "mcp.enableProjectConfig": true });

		await session.refresh("mcp");

		expect(calls).toHaveLength(1);
		expect(calls[0]?.enableProjectConfig).toBe(true);
	});
});
