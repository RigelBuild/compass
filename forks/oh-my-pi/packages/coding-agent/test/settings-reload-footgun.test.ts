import { afterEach, beforeEach, describe, expect, it } from "bun:test";
import * as fs from "node:fs";
import * as path from "node:path";
import { Settings } from "@oh-my-pi/pi-coding-agent/config/settings";
import { AgentStorage } from "@oh-my-pi/pi-coding-agent/session/agent-storage";
import { getProjectAgentDir, TempDir } from "@oh-my-pi/pi-utils";
import { beginSettingsTest, restoreSettingsTestState, type SettingsTestState } from "./helpers/settings-test-state";

// Settings.reload() is the in-session settings re-read. Its load-bearing
// contract is the "/model reformat footgun": it MUST pick up an on-disk edit
// WITHOUT re-serializing config.yml (the write path strips comments/formatting).
// It also reports whether the merged view changed so callers can skip
// downstream work on a no-op reload.
describe("Settings.reload() footgun-safe re-read", () => {
	let settingsState: SettingsTestState | undefined;
	let tempDir: TempDir;
	let agentDir: string;
	let projectDir: string;
	let configPath: string;

	// A hand-authored config.yml with comments and non-canonical formatting:
	// exactly what the write path would destroy on a round-trip.
	const HAND_AUTHORED_CONFIG = [
		"# Fleet default model — edited by hand, keep the comments.",
		"modelRoles:",
		"  default: anthropic/claude-sonnet-4-5",
		"",
		"# UI knobs below.",
		"autocompleteMaxVisible:    9",
		"",
	].join("\n");

	beforeEach(() => {
		settingsState = beginSettingsTest();
		tempDir = TempDir.createSync("@pi-settings-reload-footgun-");
		agentDir = tempDir.join("agent");
		projectDir = tempDir.join("project");
		fs.mkdirSync(agentDir, { recursive: true });
		fs.mkdirSync(getProjectAgentDir(projectDir), { recursive: true });
		configPath = path.join(agentDir, "config.yml");
		fs.writeFileSync(configPath, HAND_AUTHORED_CONFIG);
	});

	afterEach(async () => {
		AgentStorage.resetInstance();
		restoreSettingsTestState(settingsState);
		settingsState = undefined;
		await tempDir?.remove();
	});

	it("picks up an on-disk edit and leaves config.yml byte-identical (no reformat)", async () => {
		const settings = await Settings.loadIsolated({ cwd: projectDir, agentDir });
		expect(settings.getModelRole("default")).toBe("anthropic/claude-sonnet-4-5");

		// Edit the file the way an operator (or nix sync) would: swap the default
		// model, keeping the surrounding comments/whitespace untouched.
		const edited = HAND_AUTHORED_CONFIG.replace("anthropic/claude-sonnet-4-5", "anthropic/claude-sonnet-4-6");
		fs.writeFileSync(configPath, edited);

		const { changed } = await settings.reload();

		expect(changed).toBe(true);
		// The reloaded value is live in the session.
		expect(settings.getModelRole("default")).toBe("anthropic/claude-sonnet-4-6");
		// The file is byte-for-byte what we wrote — reload never entered the save
		// path, so the comments and irregular spacing survive.
		expect(fs.readFileSync(configPath, "utf8")).toBe(edited);
	});

	it("reports changed=false and touches nothing when nothing changed on disk", async () => {
		const settings = await Settings.loadIsolated({ cwd: projectDir, agentDir });
		const before = fs.readFileSync(configPath, "utf8");

		const { changed } = await settings.reload();

		expect(changed).toBe(false);
		expect(settings.getModelRole("default")).toBe("anthropic/claude-sonnet-4-5");
		expect(fs.readFileSync(configPath, "utf8")).toBe(before);
	});
});
