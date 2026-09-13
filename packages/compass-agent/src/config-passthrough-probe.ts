// Test-support driver for the RIG-1678 T4 acceptance (g) needing a launch-frozen $HOME
// (design compass-agent-config-passthrough §CP-4). After the object-injection pivot, only
// subagent definitions (`agents/`) still reach the SDK by filesystem discovery, via
// `discoverAgents`, which anchors on `os.homedir()` — frozen by Bun at module load.

// The only hermetic way to point discovery at a tempdir is a fresh process with `HOME`
// preset. The driver runs the same symlink effect main runs, then queries the SDK as
// `task` would and prints PROBE_RESULT:<json>.

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
