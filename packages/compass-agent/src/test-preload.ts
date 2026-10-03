// Runs before any test file imports the SDK. Bun fixes os.homedir() at startup,
// so a HOME change in a test never moves the SDK's agent dir; pin it here instead.
import { afterAll } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const agentDir = mkdtempSync(join(tmpdir(), "compass-agent-omp-"));
process.env.PI_CODING_AGENT_DIR = agentDir;
// A named profile makes the SDK ignore PI_CODING_AGENT_DIR.
delete process.env.OMP_PROFILE;
delete process.env.PI_PROFILE;
// A preload hook is global; parallel workers skip process "exit" handlers.
afterAll(() => rmSync(agentDir, { recursive: true, force: true }));
