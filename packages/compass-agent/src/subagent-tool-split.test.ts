// The comms/IRC tool split, pinned (design
// docs/designs/agent/compass-per-agent-overrides/design.md §Plan T7).
//
// WHAT THIS DEFENDS. Compass's native tools — the comms surface
// (`comms_post_message`, `compass_roster`, …), the lifecycle surface
// (`agents_spawn_peer`, `agents_despawn_peer`), and the forge surface
// (`forge_*`) — reach the Manager session as `customTools` (cli.ts:896:
// `customTools: [...mcp.tools, ...nativeTools]`). An in-process `task`
// subagent (the design's worker topology) runs at full Manager trust in the
// SAME container, so its Compass reach is NOT a security boundary — it is a
// capability contract. The contract: a subagent gets the OMP builtin toolset
// (edit/read/bash/…) and `irc`, but NONE of Compass's native tools; peers and
// the Manager are reached over OMP-internal IRC, never a Compass channel.
//
// WHY BY-CONSTRUCTION TODAY, AND WHY PIN IT ANYWAY. The exclusion falls out of
// the SDK's subagent construction: a subagent session's `customTools` is
// `mcpProxyTools = options.mcpManager ? createMCPProxyTools(...) : []`
// (executor.ts:2394/:2489), and Compass passes its native tools as
// `customTools`, never via an `mcpManager` — so a child's `customTools` is
// empty and no Compass tool can transit. That is a property of a pinned SDK
// (lockfile 16.5.2), not a guarantee: an SDK upgrade, or a future refactor that
// hands Compass tools to subagents through an `mcpManager`, would silently widen
// the child's reach. This test is the DRIFT ALARM (design §Global Constraints:
// "T7's closed-set pin is a drift alarm, not a fence") — it reddens the day a
// Compass tool leaks into a subagent, or `irc` drops out of one.
//
// THE ASSERTION IS A CLOSED SET ON THE COMPASS DIMENSION, not a deny-list and
// not an exact-equals against the whole OMP builtin list. The builtin list is
// environment- and version-dependent (`bash`/`eval`/`lsp` gate on settings and
// available backends; `browser`/`search_tool_bm25` come and go across SDK
// versions), so an exact-equals against it would be flaky. What is INVARIANT is
// the split: the Manager session carries EXACTLY the Compass native tool set,
// and the subagent session carries EXACTLY NONE of it while still carrying
// `irc`. Asserting the Compass-tool intersection in both directions is the
// closed set that matters — it cannot pass if a Compass tool leaks in, and
// cannot pass if `irc` is missing.
//
// Hermetic: a scratch cwd + scratch HOME (the SDK anchors discovery on the
// launch HOME, os.homedir(); pinning it keeps ambient extensions/tools out) and
// disabled extension/MCP discovery, so the sets reflect the code contract, not
// the developer's ~/.omp tree. No sockets, no model, no timers.

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
	type CreateAgentSessionOptions,
	createAgentSession,
	type ToolDefinition,
} from "@oh-my-pi/pi-coding-agent";
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
function compassNativeTools(): ToolDefinition[] {
	// Mirrors the cli.ts registration seam: the factories return `AgentTool[]`,
	// which the entrypoint widens to `ToolDefinition[]` for the `customTools`
	// option (`customTools?: (CustomTool | ToolDefinition)[]`). The two are
	// structurally compatible for registration but inference will not unify
	// them, so this is the same widening the production callsite uses.
	//
	// `loadMode: "essential"` mirrors the same stamp cli.ts applies. SDK 18.x
	// defaults an omitted `loadMode` at an adapter boundary to `"discoverable"`
	// (registered but not top-level), so without it these tools would not be
	// active and this file would be measuring the wrong surface.
	return [
		...createCommsTools(new CommsBroker(commsTransport)),
		...createLifecycleTools(new LifecycleBroker(lifecycleTransport)),
		...createForgeTools(new ForgeBroker(forgeTransport)),
		...createBoardTools(new BoardBroker(boardTransport)),
	].map((tool) => ({
		...tool,
		loadMode: "essential" as const,
	})) as ToolDefinition[];
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
});

// Both shapes disable ambient discovery so the active toolset reflects only the
// explicit config, exactly as the container entrypoint (Manager, cli.ts:889-897)
// and the SDK subagent path (executor.ts) construct their sessions.
async function activeToolNames(
	options: CreateAgentSessionOptions,
): Promise<string[]> {
	const { session } = await createAgentSession({
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

// The Manager session shape: Compass native tools ride `customTools`
// (cli.ts:896). The subagent session shape: a real in-process subagent
// (`taskDepth: 1`) with no `customTools` — the SDK's subagent path sets
// `customTools` from `mcpProxyTools`, which is empty because Compass passes no
// `mcpManager` (executor.ts:2394/:2489), so a child inherits none of the
// native tools the Manager holds.
function managerActiveToolNames(cwd: string): Promise<string[]> {
	return activeToolNames({ cwd, customTools: compassNativeTools() });
}

function subagentActiveToolNames(cwd: string): Promise<string[]> {
	return activeToolNames({ cwd, taskDepth: 1 });
}

describe("subagent comms/hub tool split (design §T7)", () => {
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
	});

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
	});

	test("a subagent session still carries hub (the COOP-advertised peer channel)", async () => {
		const cwd = scratch();
		const active = new Set(await subagentActiveToolNames(cwd));

		// `hub` is active on this subagent via the availability gate
		// isIrcEnabled(settings, taskDepth) (tools/index.ts:679): at taskDepth 1
		// the `taskDepth > 0` short-circuit returns true
		// (tools/hub/messaging.ts:109-110). (A tool-restricted subagent
		// additionally force-includes it in the task executor.) The split removes
		// Compass's channel tools but MUST leave the OMP-internal peer channel, or
		// workers cannot reach the Manager at all. The SDK renamed this tool
		// `irc` -> `hub`; the gate function kept its original name.
		expect(active.has("hub")).toBe(true);
	});

	test("the Manager carries Compass tools that the subagent drops — the split is real", async () => {
		const cwd = scratch();
		const managerActive = new Set(await managerActiveToolNames(cwd));
		const subagentActive = new Set(await subagentActiveToolNames(cwd));
		const compass = compassNativeToolNames();

		// The set of Compass tools present on the Manager but absent on the
		// subagent is the ENTIRE Compass native set — nothing partial. A single
		// Compass tool surviving into the subagent, or dropping off the Manager,
		// reddens this.
		const droppedForSubagent = [...compass]
			.filter((name) => managerActive.has(name) && !subagentActive.has(name))
			.sort();
		expect(droppedForSubagent).toEqual([...compass].sort());
	});
});
