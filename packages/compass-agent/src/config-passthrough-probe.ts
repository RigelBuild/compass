// Test-support driver for the SEA-1678 T4 acceptance (g) that requires a
// launch-frozen $HOME (design compass-agent-config-passthrough §CP-4).
//
// After the object-injection pivot, only ONE fleet member still reaches the SDK
// by filesystem discovery rather than object injection: subagent definitions
// (`agents/`). The runtime SDK (16.5.2) discovers those by walking the agent dir
// via `discoverAgents` — there is no `createAgentSession` param to inject them —
// so `main()` symlinks `$HOME/.omp/agent/agents` → the mount's current/agents.
//
// `discoverAgents`'s user-level dir anchors on `os.homedir()`, which Bun freezes
// at module load and never re-reads from a mid-process `process.env.HOME`. So
// the ONLY hermetic way to point discovery at a tempdir agent dir is to launch a
// fresh process with `HOME` already set — which is what cli.config-passthrough
// .test.ts does when it spawns this driver.
//
// The driver runs the SAME symlink effect main runs (loadMountedConfig →
// ensureAgentDirLink for `agents`), then queries the SDK exactly as the `task`
// tool would (discoverAgents/getAgent) and prints PROBE_RESULT:<json>.

import { discoverAgents, getAgent } from "@oh-my-pi/pi-coding-agent";
import { ensureAgentDirLink } from "./cli";
import { loadMountedConfig } from "./config-reader";

interface ProbeResult {
	subagentFound: boolean;
}

async function run(): Promise<void> {
	const home = process.env.HOME;
	const mount = process.env.PROBE_MOUNT;
	const cwd = process.env.PROBE_CWD ?? process.cwd();
	const subagentName = process.env.PROBE_SUBAGENT_NAME;
	if (!home || !mount) {
		throw new Error(
			"config-passthrough-probe: HOME and PROBE_MOUNT are required",
		);
	}

	// The exact filesystem effect main() runs for the agents member, over the
	// same reader — symlink $HOME/.omp/agent/agents at the mount's current/agents.
	const mounted = await loadMountedConfig(mount);
	await ensureAgentDirLink(home, "agents", mounted.agentsDir);

	// The SDK discovers subagent defs exactly as the `task` tool would.
	let subagentFound = false;
	if (subagentName) {
		const result = await discoverAgents(cwd);
		subagentFound = getAgent(result.agents, subagentName) !== undefined;
	}

	const out: ProbeResult = { subagentFound };
	process.stdout.write(`PROBE_RESULT:${JSON.stringify(out)}\n`);
}

await run();
