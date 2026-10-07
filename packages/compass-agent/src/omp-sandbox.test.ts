// The SDK's model cache (models.db) is shared by every omp agent on the host; a
// test that writes an empty provider list there breaks real agent launches for
// 24h. test-preload.ts pins the agent dir to a scratch dir; these fail if not.
import { describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { homedir } from "node:os";
import { join, relative } from "node:path";
import { createAgentSession, getAgentDir } from "@oh-my-pi/pi-coding-agent";

describe("omp test sandbox", () => {
	test("the SDK agent dir resolves outside the real ~/.omp", () => {
		const rel = relative(join(homedir(), ".omp"), getAgentDir());
		expect(rel.startsWith("..")).toBe(true);
	});

	// Non-vacuity: a real session does write a model cache, and it lands here.
	// biome-ignore lint/plugin: 30s bounds a real session boot (8.8s seen on loaded CI); the await gates on the session being ready, so the ceiling only bounds a genuine hang.
	test("a real session writes its model cache into the sandbox", async () => {
		const { session } = await createAgentSession({
			skills: [],
			additionalExtensionPaths: [],
			disableExtensionDiscovery: true,
			enableMCP: false,
		});
		await session.dispose();
		expect(existsSync(join(getAgentDir(), "models.db"))).toBe(true);
	}, 30_000);
});
