// The in-container agent-config reader (design compass-agent-config-delivery
// §CD-3/CD-4). These pin the pure/async mapping from the Runner-mounted bundle
// to the three `createAgentSession` option surfaces — skills, extension entry
// paths, MCP configs — plus the observability version line.
//
// Every fixture is a real tempdir tree under `<mount>/current/…`, the layout the
// Runner materializes, torn down after each test. The dominant contract is
// TOLERANCE: an absent mount, an absent subtree, and a malformed file must each
// yield empty rather than throw — a missing mount must never crash the agent.
// No timers, no sleeps: pure FS fixtures, deterministic results.

import { afterEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
	AGENT_CONFIG_MOUNT_PATH,
	currentConfigDir,
	enumerateMountedExtensions,
	loadMountedConfig,
	readConfigVersion,
	readMountedMcpConfigs,
	readMountedSkills,
} from "./config-reader";

const tmpdirs: string[] = [];

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-config-"));
	tmpdirs.push(dir);
	return dir;
}

afterEach(() => {
	for (const dir of tmpdirs.splice(0)) {
		rmSync(dir, { recursive: true, force: true });
	}
});

// Write a file under `<mount>/current/<rel>`, creating parents. Returns the
// mount root so a test threads it into the reader.
function writeCurrent(mount: string, rel: string, body: string): void {
	const path = join(currentConfigDir(mount), rel);
	mkdirSync(join(path, ".."), { recursive: true });
	writeFileSync(path, body);
}

function skillMd(name: string, description: string): string {
	return `---\nname: ${name}\ndescription: ${description}\n---\n# ${name}\n`;
}

// The mount path is a CONTRACT with the Runner's materializer (design §CD-3),
// not a preference: the agent reads through a fixed location so it takes no
// per-session config placement. A drift is a silent unconfigured boot, so it is
// pinned — beside AGENT_SOCKET_PATH's contract test.
describe("AGENT_CONFIG_MOUNT_PATH", () => {
	test("is the frozen /run/compass/agent-config contract path", () => {
		expect(AGENT_CONFIG_MOUNT_PATH).toBe("/run/compass/agent-config");
	});

	test("currentConfigDir reads through the current/ symlink the Runner flips", () => {
		expect(currentConfigDir("/run/compass/agent-config")).toBe(
			"/run/compass/agent-config/current",
		);
	});
});

// skills: providing the array to createAgentSession SKIPS discovery, so this
// reader is the SOLE source of the session's skills. A populated subtree yields
// its skills; an absent one yields [] — the unconfigured→none guarantee.
describe("readMountedSkills", () => {
	test("loads each skill subtree under current/skills", async () => {
		const mount = scratch();
		writeCurrent(
			mount,
			"skills/alpha/SKILL.md",
			skillMd("alpha", "Alpha skill"),
		);
		writeCurrent(mount, "skills/beta/SKILL.md", skillMd("beta", "Beta skill"));

		const skills = await readMountedSkills(currentConfigDir(mount));
		expect(skills.map((s) => s.name).sort()).toEqual(["alpha", "beta"]);
		// The source tag is the compass-config user-level provenance, so an
		// authored skill of the same name would still win over the mount's.
		expect(skills.every((s) => s.source === "compass-config:user")).toBe(true);
	});

	test("an absent skills subtree yields [] (unconfigured → no skills)", async () => {
		const mount = scratch();
		// No skills/ written — the whole current/ is absent too.
		expect(await readMountedSkills(currentConfigDir(mount))).toEqual([]);
	});
});

// extensions (design wiring choice A): the reader enumerates concrete entry
// FILES, because createAgentSession imports each additionalExtensionPath as a
// module — a raw dir path would fail. Paired with disableExtensionDiscovery, an
// empty enumeration is the unconfigured→none guarantee.
describe("enumerateMountedExtensions", () => {
	test("a top-level .ts/.js file is an entry path", async () => {
		const mount = scratch();
		writeCurrent(mount, "extensions/solo.ts", "export default {};\n");
		expect(await enumerateMountedExtensions(currentConfigDir(mount))).toEqual([
			join(currentConfigDir(mount), "extensions", "solo.ts"),
		]);
	});

	test("a subdir resolves through its index.ts / index.js", async () => {
		const mount = scratch();
		writeCurrent(
			mount,
			"extensions/withindex/index.ts",
			"export default {};\n",
		);
		expect(await enumerateMountedExtensions(currentConfigDir(mount))).toEqual([
			join(currentConfigDir(mount), "extensions", "withindex", "index.ts"),
		]);
	});

	test("a subdir with a package.json omp.extensions manifest resolves to its declared entries", async () => {
		const mount = scratch();
		writeCurrent(
			mount,
			"extensions/pkg/package.json",
			JSON.stringify({ omp: { extensions: ["./entry.js"] } }),
		);
		writeCurrent(mount, "extensions/pkg/entry.js", "module.exports = {};\n");
		expect(await enumerateMountedExtensions(currentConfigDir(mount))).toEqual([
			join(currentConfigDir(mount), "extensions", "pkg", "entry.js"),
		]);
	});

	test("enumeration is deterministic — sorted by entry name", async () => {
		const mount = scratch();
		writeCurrent(mount, "extensions/zeta.ts", "export default {};\n");
		writeCurrent(mount, "extensions/alpha.ts", "export default {};\n");
		const cur = currentConfigDir(mount);
		expect(await enumerateMountedExtensions(cur)).toEqual([
			join(cur, "extensions", "alpha.ts"),
			join(cur, "extensions", "zeta.ts"),
		]);
	});

	test("a non-extension file and an entry-less subdir are skipped", async () => {
		const mount = scratch();
		writeCurrent(mount, "extensions/readme.md", "not an extension\n");
		writeCurrent(mount, "extensions/empty/nothing.txt", "no entry here\n");
		writeCurrent(mount, "extensions/real.ts", "export default {};\n");
		expect(await enumerateMountedExtensions(currentConfigDir(mount))).toEqual([
			join(currentConfigDir(mount), "extensions", "real.ts"),
		]);
	});

	test("an absent extensions subtree yields [] (unconfigured → no extensions)", async () => {
		const mount = scratch();
		expect(await enumerateMountedExtensions(currentConfigDir(mount))).toEqual(
			[],
		);
	});
});

// MCP: the reader PARSES mcp/*.json into the {configs, sources} connectServers
// takes — it does not connect or resolve credentials. Malformed files are
// skipped so one bad file cannot sink the rest or crash the boot.
describe("readMountedMcpConfigs", () => {
	test("parses each mcp/*.json's mcpServers with per-server source provenance", async () => {
		const mount = scratch();
		writeCurrent(
			mount,
			"mcp/db.json",
			JSON.stringify({
				mcpServers: { pg: { command: "pg-mcp", args: ["x"] } },
			}),
		);
		writeCurrent(
			mount,
			"mcp/web.json",
			JSON.stringify({
				mcpServers: { fetch: { type: "http", url: "http://h" } },
			}),
		);

		const { configs, sources } = await readMountedMcpConfigs(
			currentConfigDir(mount),
		);
		expect(Object.keys(configs).sort()).toEqual(["fetch", "pg"]);
		expect(configs.pg).toEqual({ command: "pg-mcp", args: ["x"] });
		expect(sources.pg.provider).toBe("compass-config");
		expect(sources.pg.level).toBe("user");
		expect(sources.pg.path).toBe(
			join(currentConfigDir(mount), "mcp", "db.json"),
		);
	});

	test("a malformed json file is skipped; the others still load", async () => {
		const mount = scratch();
		writeCurrent(mount, "mcp/bad.json", "{ not valid json");
		writeCurrent(
			mount,
			"mcp/good.json",
			JSON.stringify({ mcpServers: { ok: { command: "ok" } } }),
		);
		const { configs } = await readMountedMcpConfigs(currentConfigDir(mount));
		// The bad file contributes nothing; the good one still parses.
		expect(Object.keys(configs)).toEqual(["ok"]);
	});

	test("an absent mcp subtree yields empty configs (unconfigured → no MCP)", async () => {
		const mount = scratch();
		expect(await readMountedMcpConfigs(currentConfigDir(mount))).toEqual({
			configs: {},
			sources: {},
		});
	});
});

// The version file is observability-only: a value when present, undefined when
// absent — nothing gates on it.
describe("readConfigVersion", () => {
	test("returns the trimmed version hash when present", async () => {
		const mount = scratch();
		writeCurrent(mount, "version", "deadbeef\n");
		expect(await readConfigVersion(currentConfigDir(mount))).toBe("deadbeef");
	});

	test("returns undefined when absent", async () => {
		const mount = scratch();
		expect(await readConfigVersion(currentConfigDir(mount))).toBeUndefined();
	});
});

// loadMountedConfig composes the four readers into the one shape main() spreads.
// disableExtensionDiscovery is always true — the choice-A guarantee.
describe("loadMountedConfig", () => {
	test("composes a fully-populated mount into the option surfaces", async () => {
		const mount = scratch();
		writeCurrent(mount, "skills/s/SKILL.md", skillMd("s", "S skill"));
		writeCurrent(mount, "extensions/e.ts", "export default {};\n");
		writeCurrent(
			mount,
			"mcp/m.json",
			JSON.stringify({ mcpServers: { srv: { command: "s" } } }),
		);
		writeCurrent(mount, "version", "v1\n");

		const cfg = await loadMountedConfig(mount);
		expect(cfg.skills.map((s) => s.name)).toEqual(["s"]);
		expect(cfg.additionalExtensionPaths).toEqual([
			join(currentConfigDir(mount), "extensions", "e.ts"),
		]);
		expect(cfg.disableExtensionDiscovery).toBe(true);
		expect(Object.keys(cfg.mcp.configs)).toEqual(["srv"]);
		expect(cfg.version).toBe("v1");
	});

	test("a partial mount (skills only) leaves the other surfaces empty", async () => {
		const mount = scratch();
		writeCurrent(mount, "skills/only/SKILL.md", skillMd("only", "Only skill"));

		const cfg = await loadMountedConfig(mount);
		expect(cfg.skills.map((s) => s.name)).toEqual(["only"]);
		expect(cfg.additionalExtensionPaths).toEqual([]);
		expect(cfg.mcp.configs).toEqual({});
		expect(cfg.version).toBeUndefined();
	});

	test("an unconfigured mount (no current/) yields every surface empty", async () => {
		const mount = scratch();
		// No current/ dir at all — the mount is present but never populated.
		const cfg = await loadMountedConfig(mount);
		expect(cfg.skills).toEqual([]);
		expect(cfg.additionalExtensionPaths).toEqual([]);
		expect(cfg.mcp.configs).toEqual({});
		expect(cfg.mcp.sources).toEqual({});
		expect(cfg.disableExtensionDiscovery).toBe(true);
		expect(cfg.version).toBeUndefined();
	});

	test("a wholly-absent mount path yields every surface empty (no throw)", async () => {
		const cfg = await loadMountedConfig(
			join(scratch(), "does", "not", "exist"),
		);
		expect(cfg.skills).toEqual([]);
		expect(cfg.additionalExtensionPaths).toEqual([]);
		expect(cfg.mcp.configs).toEqual({});
		expect(cfg.version).toBeUndefined();
	});
});
