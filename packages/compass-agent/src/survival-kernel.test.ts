import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
	type CreateAgentSessionOptions,
	createAgentSession,
	SessionManager,
} from "@oh-my-pi/pi-coding-agent";

const kernel = "KERNEL-SENTINEL evidence and safe tool arguments";
const rolePrompt = "ROLE-SENTINEL manager role";
const persona = "PERSONA-SENTINEL lane context";
const kernelRule = {
	name: "survival-kernel",
	description: "Role-invariant evidence and safe tool arguments",
	alwaysApply: true,
	content: kernel,
	path: "/rules/survival-kernel.md",
};

function scratch(): string {
	return mkdtempSync(join(tmpdir(), "compass-kernel-"));
}

async function makeSession(cwd: string, manager: SessionManager) {
	return createAgentSession({
		cwd,
		sessionManager: manager,
		customSystemPrompt: rolePrompt,
		systemPrompt: (blocks) => [...blocks, persona],
		taskDepth: 1,
		rules: [kernelRule],
		skills: [],
		additionalExtensionPaths: [],
		disableExtensionDiscovery: true,
		enableMCP: false,
	});
}

function userMessage(): Parameters<SessionManager["appendMessage"]>[0] {
	return { role: "user", content: "persist this turn" };
}

describe("survival kernel delivery", () => {
	test("persisted session resume rebuilds kernel, role, and persona prompt", async () => {
		const cwd = scratch();
		const sessionDir = join(cwd, "sessions");
		const firstManager = SessionManager.create(cwd, sessionDir);
		const first = await makeSession(cwd, firstManager);
		const sessionFile = firstManager.getSessionFile();
		if (!sessionFile) throw new Error("session manager did not persist a file");
		firstManager.appendMessage(userMessage());
		await firstManager.flush();
		await first.session.dispose();
		await firstManager.close();

		const resumedManager = await SessionManager.open(sessionFile, sessionDir);
		const resumed = await makeSession(cwd, resumedManager);
		try {
			const prompt = resumed.session.systemPrompt.join("\n");
			expect(resumedManager.getEntries().length).toBeGreaterThan(0);
			expect(prompt).toContain(kernel);
			expect(prompt).toContain(rolePrompt);
			expect(prompt).toContain(persona);
		} finally {
			await resumed.session.dispose();
			await resumedManager.close();
			rmSync(cwd, { recursive: true, force: true });
		}
	});

	test("taskDepth:1 subagent-shaped session receives the composed kernel rule", async () => {
		const cwd = scratch();
		const manager = SessionManager.create(cwd, join(cwd, "sessions"));
		const options: CreateAgentSessionOptions = {
			cwd,
			sessionManager: manager,
			taskDepth: 1,
			rules: [kernelRule],
			skills: [],
			additionalExtensionPaths: [],
			disableExtensionDiscovery: true,
			enableMCP: false,
		};
		const { session } = await createAgentSession(options);
		try {
			expect(session.systemPrompt.join("\n")).toContain(kernel);
		} finally {
			await session.dispose();
			await manager.close();
			rmSync(cwd, { recursive: true, force: true });
		}
	});
});
