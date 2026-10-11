// A native registered through `customTools` must receive the AgentTool argument
// order when the real SDK session invokes it.

import { afterEach, beforeEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { type } from "@oh-my-pi/omptype/ark";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import {
	AgentRegistry,
	type CustomTool,
	createAgentSession,
} from "@oh-my-pi/pi-coding-agent";
import { nativeAsCustomTool } from "./native-tools";

const scratchDirs: string[] = [];
let savedHome: string | undefined;

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-native-tools-"));
	scratchDirs.push(dir);
	return dir;
}

beforeEach(() => {
	// Pin discovery's HOME so no ambient ~/.omp tool joins the session.
	savedHome = process.env.HOME;
	process.env.HOME = scratch();
});

afterEach(() => {
	if (savedHome === undefined) delete process.env.HOME;
	else process.env.HOME = savedHome;
	for (const dir of scratchDirs.splice(0)) {
		rmSync(dir, { recursive: true, force: true });
	}
}, 60_000);

// biome-ignore lint/plugin: 30s bounds a real session boot (one-time module init); the awaits gate on readiness, so the ceiling only bounds a genuine hang.
test("the SDK invokes a native with its signal and onUpdate in AgentTool order", async () => {
	const seen: { signal?: AbortSignal; onUpdate?: unknown }[] = [];
	const parameters = type({ value: "string" });
	const probe: AgentTool<typeof parameters> = {
		name: "compass_probe",
		label: "Probe",
		description: "Records the arguments it receives.",
		parameters,
		approval: "read",
		async execute(_id, params, signal, onUpdate) {
			seen.push({ signal, onUpdate });
			onUpdate?.({ content: [{ type: "text", text: "partial" }] });
			return { content: [{ type: "text", text: params.value }] };
		},
	};
	// Widened like the factories' AgentTool[] return, as cli.ts registers them.
	const natives: AgentTool[] = [probe];
	const { session } = await createAgentSession({
		agentRegistry: new AgentRegistry(),
		cwd: scratch(),
		skills: [],
		additionalExtensionPaths: [],
		disableExtensionDiscovery: true,
		enableMCP: false,
		customTools: natives.map(
			(tool): CustomTool => ({
				...nativeAsCustomTool(tool),
				loadMode: "essential",
			}),
		),
	});
	try {
		const registered = session.agent.state.tools.find(
			(tool) => tool.name === "compass_probe",
		);
		expect(registered).toBeDefined();
		const controller = new AbortController();
		const updates: unknown[] = [];
		const result = await registered?.execute(
			"call-1",
			{ value: "ok" },
			controller.signal,
			(partial) => updates.push(partial),
		);

		expect(result?.content).toEqual([{ type: "text", text: "ok" }]);
		expect(seen).toHaveLength(1);
		expect(seen[0].signal?.aborted).toBe(false);
		controller.abort();
		expect(seen[0].signal?.aborted).toBe(true);
		expect(updates).toHaveLength(1);
	} finally {
		await session.dispose();
	}
}, 30_000);
