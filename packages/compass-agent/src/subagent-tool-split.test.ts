// Pins the capability split: the Manager gets Compass tools; subagents retain only OMP tools and hub.
// This is a capability contract, not a security boundary: task subagents run at Manager trust in the same container.

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
	AgentRegistry,
	type CreateAgentSessionOptions,
	type CustomTool,
	createAgentSession,
	SessionManager,
} from "@oh-my-pi/pi-coding-agent";
import { BoardFreshness, createAgentBoardTools } from "./agent-board";
import { BoardBroker, type BoardTransport, createBoardTools } from "./board";
import { CommsBroker, type CommsTransport, createCommsTools } from "./comms";
import type { CommsCallRequest, CommsCallResult } from "./compassv1";
import { createForgeTools, ForgeBroker, type ForgeTransport } from "./forge";
import type {
	ForgeCallRequest,
	ForgeCallResult,
	LifecycleCallRequest,
	LifecycleCallResult,
} from "./gen/compass/v1/agent_gateway_pb";
import {
	createLifecycleTools,
	LifecycleBroker,
	type LifecycleTransport,
} from "./lifecycle";
import { nativeAsCustomTool } from "./native-tools";
import { TurnSequence } from "./turn-sequence";

// This test reads tool NAMES + registration only — never invokes a tool — so
// every transport method rejects: reaching one is a test bug, not a code path.
const unreached = (leg: string): Promise<never> =>
	Promise.reject(
		new Error(`${leg} transport must not be called in the tool-split pin`),
	);
const boardTransport: BoardTransport = {
	board: (_req) => unreached("board"),
};
const commsTransport: CommsTransport = {
	comms: (_req: CommsCallRequest): Promise<CommsCallResult> =>
		unreached("comms"),
};
const forgeTransport: ForgeTransport = {
	forge: (_req: ForgeCallRequest): Promise<ForgeCallResult> =>
		unreached("forge"),
};
const lifecycleTransport: LifecycleTransport = {
	lifecycle: (_req: LifecycleCallRequest): Promise<LifecycleCallResult> =>
		unreached("lifecycle"),
};

// The EXACT Compass native tool set cli.ts registers on the Manager session.
// Built from the same four factories over real brokers, so the expected set can
// never drift from what ships: a new native tool is automatically part of the
// pinned split.
function compassNativeTools(): CustomTool[] {
	// Mirrors the cli.ts registration seam: natives go through `nativeAsCustomTool`,
	// and `loadMode: "essential"` mirrors cli.ts's stamp on the whole `customTools`
	// array. SDK 18.x defaults an omitted `loadMode` to `"discoverable"`, which
	// would leave these tools inactive and measure the wrong surface.
	return [
		...createCommsTools(new CommsBroker(commsTransport)),
		...createLifecycleTools(new LifecycleBroker(lifecycleTransport)),
		...createForgeTools(new ForgeBroker(forgeTransport)),
		...createBoardTools(new BoardBroker(boardTransport)),
		...createAgentBoardTools(
			new CommsBroker(commsTransport),
			new TurnSequence(SessionManager.inMemory()),
			new BoardFreshness(),
		),
	].map(
		(tool): CustomTool => ({
			...nativeAsCustomTool(tool),
			loadMode: "essential",
		}),
	);
}

function compassNativeToolNames(): Set<string> {
	return new Set(compassNativeTools().map((t) => t.name));
}

const scratchDirs: string[] = [];
function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-tool-split-"));
	scratchDirs.push(dir);
	return dir;
}

let savedHome: string | undefined;
beforeEach(() => {
	// createAgentSession's discovery anchors on os.homedir() → process.env.HOME;
	// pin it to a throwaway so no ambient ~/.omp extension or custom tool leaks
	// into either measured set.
	savedHome = process.env.HOME;
	process.env.HOME = scratch();
});

afterEach(() => {
	if (savedHome === undefined) delete process.env.HOME;
	else process.env.HOME = savedHome;
	for (const dir of scratchDirs.splice(0)) {
		rmSync(dir, { recursive: true, force: true });
	}
	// Recursive /tmp scratch deletes outrun bun's 5s hook default under the
	// parallel pre-push gate; bound as a slow-cleanup detector, not a budget (RIG-3611).
}, 60_000);

// Both shapes disable ambient discovery so the active toolset reflects only the
// explicit config, exactly as the container entrypoint (Manager, cli.ts:889-897)
// and the SDK subagent path (executor.ts) construct their sessions.
async function activeToolNames(
	options: CreateAgentSessionOptions,
): Promise<string[]> {
	const { session } = await createAgentSession({
		// A private registry: the global one keys every top-level session as
		// "Main", so concurrent boots in one bun process replace each other.
		agentRegistry: new AgentRegistry(),
		skills: [],
		additionalExtensionPaths: [],
		disableExtensionDiscovery: true,
		enableMCP: false,
		...options,
	});
	try {
		return session.getActiveToolNames();
	} finally {
		await session.dispose();
	}
}

// The Manager gets native tools; SDK subagent construction omits them without an mcpManager.
function managerActiveToolNames(cwd: string): Promise<string[]> {
	return activeToolNames({ cwd, customTools: compassNativeTools() });
}

function subagentActiveToolNames(cwd: string): Promise<string[]> {
	return activeToolNames({ cwd, taskDepth: 1 });
}

describe("subagent comms/hub tool split (design §T7)", () => {
	// Real sessions need a generous startup bound; readiness gates the healthy path.
	// biome-ignore lint/plugin: 30s bounds a real session boot (this first case also pays ~7s one-time module init; siblings ~1.2-1.6s); the awaits gate on the session being ready, so the ceiling only bounds a genuine hang.
	test("the Manager session carries exactly the Compass native tool set", async () => {
		const cwd = scratch();
		const active = new Set(await managerActiveToolNames(cwd));
		const compass = compassNativeToolNames();

		// Non-vacuity: there IS a Compass native set to carry.
		expect(compass.size).toBeGreaterThan(0);

		// Every Compass native tool is active on the Manager — the container agent
		// can spawn peers and post to channels.
		const missing = [...compass].filter((name) => !active.has(name)).sort();
		expect(missing).toEqual([]);
	}, 30_000);

	// biome-ignore lint/plugin: 30s bounds a real session boot (~1.2-1.6s warm); the awaits gate on the session being ready, so the ceiling only bounds a genuine hang.
	test("a subagent session carries NONE of the Compass native tools", async () => {
		const cwd = scratch();
		const active = new Set(await subagentActiveToolNames(cwd));
		const compass = compassNativeToolNames();

		// The closed set on the Compass dimension: the intersection of a
		// subagent's active tools with Compass's native tools is EMPTY. This is
		// red if a comms/lifecycle/forge tool ever leaks into a subagent — the
		// exact drift an SDK upgrade or an `mcpManager` handoff would introduce.
		const leaked = [...active].filter((name) => compass.has(name)).sort();
		expect(leaked).toEqual([]);
	}, 30_000);

	// biome-ignore lint/plugin: 30s bounds a real session boot (~1.2-1.6s warm); the awaits gate on the session being ready, so the ceiling only bounds a genuine hang.
	test("a subagent session still carries hub (the COOP-advertised peer channel)", async () => {
		const cwd = scratch();
		const active = new Set(await subagentActiveToolNames(cwd));

		// hub must remain available for the OMP-internal peer channel after Compass tools are excluded.
		expect(active.has("hub")).toBe(true);
	}, 30_000);

	// biome-ignore lint/plugin: 30s bounds two real session boots (~1.2-1.6s warm each); the awaits gate on the sessions being ready, so the ceiling only bounds a genuine hang.
	test("the Manager carries Compass tools that the subagent drops — the split is real", async () => {
		const cwd = scratch();
		// Booted concurrently: each session registers in its own AgentRegistry, so
		// two boots in one process no longer contend for the global "Main" id.
		const [managerNames, subagentNames] = await Promise.all([
			managerActiveToolNames(cwd),
			subagentActiveToolNames(cwd),
		]);
		const managerActive = new Set(managerNames);
		const subagentActive = new Set(subagentNames);
		const compass = compassNativeToolNames();

		// The set of Compass tools present on the Manager but absent on the
		// subagent is the ENTIRE Compass native set — nothing partial. A single
		// Compass tool surviving into the subagent, or dropping off the Manager,
		// reddens this.
		const droppedForSubagent = [...compass]
			.filter((name) => managerActive.has(name) && !subagentActive.has(name))
			.sort();
		expect(droppedForSubagent).toEqual([...compass].sort());
	}, 30_000);
});
