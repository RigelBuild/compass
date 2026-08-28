// The in-container reader for the Runner-mounted agent-config bundle (design
// compass-agent-config-delivery §CD-3/CD-4).
//
// The transport spine — server store, config RPCs, the Runner's
// fetch/materialize/mount, in-place Reload — lands the bundle READ-ONLY at a
// fixed in-container path. This module is the last mile: it reads that mount and
// maps it into the three `createAgentSession` option surfaces the SDK exposes —
// skills, extensions, and MCP servers — so the agent boots configured.
//
// Layout, under `<mount>/current/` (a symlink the Runner flips atomically):
//
//   - `skills/<name>/…`      — skill trees, each a dir with a SKILL.md
//   - `extensions/<name>/…`  — extension entry files or dirs
//   - `mcp/<name>.json`      — MCP server configs (credential-free by MVP rule;
//                              servers read tokens from the aggregate env file
//                              main() already sources into process.env)
//   - `prompts/<role>/SYSTEM.md` — per-role block-0 system prompts; the
//                              operator-set role label selects one, delivered as
//                              `customSystemPrompt` (REPLACES block-0)
//   - `version`              — bundle hash, observability only
//
// UNCONFIGURED — no `current` symlink, or the whole mount absent — is a VALID
// empty state, not an error: every reader is tolerant (absent/malformed →
// empty, NEVER throws), mirroring the Runner's materialize-empty posture and the
// entrypoint's own `readSeed`/`readEnvFile` idiom. A partially-populated mount
// (e.g. skills but no mcp) is normal — each subtree is read independently.
//
// Structure mirrors `cli.ts`: these are pure/async decisions tested directly in
// `config-reader.test.ts` over tempdir fixtures; `main()` performs the one IO
// composition (build+connect the MCP manager, spread the fields, own teardown).

import type { Dirent } from "node:fs";
import { readdir, stat } from "node:fs/promises";
import { join, resolve } from "node:path";
import { loadSkillsFromDir, type Skill } from "@oh-my-pi/pi-coding-agent";
import type { SourceMeta } from "@oh-my-pi/pi-coding-agent/capability";
import type { Rule } from "@oh-my-pi/pi-coding-agent/capability/rule";
import {
	buildRuleFromMarkdown,
	createSourceMeta,
} from "@oh-my-pi/pi-coding-agent/discovery/helpers";
import type {
	MCPConfigFile,
	MCPServerConfig,
} from "@oh-my-pi/pi-coding-agent/mcp";

/**
 * The in-container path the Runner materializes the agent-config bundle to.
 * Fixed by contract with the Runner's mount (design §CD-3) — the agent takes no
 * per-session config location, so this constant IS the rendezvous. The agent
 * reads through `<mount>/current`, the symlink the Runner flips.
 */
export const AGENT_CONFIG_MOUNT_PATH = "/run/compass/agent-config";

/**
 * The source tag stamped on skills loaded from the mount, in the
 * `provider:level` shape `loadSkillsFromDir` splits (skills.ts). Provenance
 * only: `main()` passes these skills to `createAgentSession`, which SKIPS
 * discovery (sdk.ts:1417), so the mount is the sole skill source and no
 * same-name discovered skill exists for the tag to win precedence against.
 */
const SKILL_SOURCE = "compass-config:user";

/** Source metadata stamped on every server parsed from the mount. */
const MCP_PROVIDER = "compass-config";
const MCP_PROVIDER_NAME = "Compass config";

/** Provenance tag stamped on every rule built from the mount (createSourceMeta). */
const RULE_PROVIDER = "compass-config";

/** The `current/` view the Runner flips; the agent only ever reads through it. */
export function currentConfigDir(mount: string): string {
	return join(mount, "current");
}

/**
 * The mount's skills, as SDK `Skill`s. Providing this array to
 * `createAgentSession` SKIPS discovery entirely (sdk.ts: `options.skills !==
 * undefined` → verbatim), so an explicit `[]` GUARANTEES no skills leak in from
 * a discovery walk — exactly the unconfigured contract.
 *
 * Tolerant: `loadSkillsFromDir` already returns `[]` for an absent dir
 * (scanSkillsFromDir swallows ENOENT), and this wraps it so any other read
 * failure is also empty rather than a boot crash — the `readSeed` posture.
 */
export async function readMountedSkills(currentDir: string): Promise<Skill[]> {
	try {
		const { skills } = await loadSkillsFromDir({
			dir: join(currentDir, "skills"),
			source: SKILL_SOURCE,
		});
		return skills;
	} catch {
		// Absent/unreadable: no skills right now (same posture as readSeed).
		return [];
	}
}

/** A path exists (used to gate an extension entry candidate). */
async function pathExists(path: string): Promise<boolean> {
	try {
		await stat(path);
		return true;
	} catch {
		return false;
	}
}

/**
 * Resolve a directory to its concrete extension entry FILES, mirroring the SDK's
 * private `resolveExtensionEntries` (loader.ts): a `package.json` `omp`/`pi`
 * `.extensions` manifest names them; else `index.ts`, else `index.js`; else this
 * dir declares no entries (`null`). Returns absolute paths.
 */
async function resolveExtensionEntries(dir: string): Promise<string[] | null> {
	const manifestEntries = await readManifestEntries(dir);
	if (manifestEntries) return manifestEntries;

	for (const index of ["index.ts", "index.js"]) {
		const candidate = join(dir, index);
		if (await pathExists(candidate)) return [candidate];
	}
	return null;
}

/**
 * The entry files a dir's `package.json` `omp`/`pi`.extensions manifest declares
 * (loader.ts `readExtensionManifest` + `resolveExtensionEntries`). Absent or
 * malformed manifest → `null`; only entries that actually exist are kept.
 */
async function readManifestEntries(dir: string): Promise<string[] | null> {
	let manifest: { extensions?: string[] } | undefined;
	try {
		const pkg = (await Bun.file(join(dir, "package.json")).json()) as {
			omp?: { extensions?: string[] };
			pi?: { extensions?: string[] };
		};
		manifest = pkg.omp ?? pkg.pi;
	} catch {
		// Absent/malformed package.json: this dir declares no manifest entries.
		return null;
	}
	const declared = manifest?.extensions;
	if (!declared?.length) return null;

	const entries: string[] = [];
	for (const rel of declared) {
		const entryPath = resolve(dir, rel);
		if (await pathExists(entryPath)) entries.push(entryPath);
	}
	return entries.length > 0 ? entries : null;
}

/**
 * Enumerate the mount's extensions into the concrete entry-FILE paths
 * `createAgentSession` must receive (design wiring choice A). It mirrors the
 * SDK's private `discoverExtensionsInDir` (loader.ts): a top-level `.ts`/`.js`
 * is an entry; a subdir resolves through its manifest / `index.*`. Paired with
 * `disableExtensionDiscovery: true`, passing these verbatim GUARANTEES exactly
 * the mount's extensions and NONE when the mount is empty — the reason the SDK's
 * `additionalExtensionPaths` cannot take a raw dir (it imports each path AS a
 * module, so a dir path fails; loader.ts).
 *
 * Tolerant: absent dir → `[]`. Deterministic: entries are sorted by name before
 * traversal so the returned paths do not depend on readdir order.
 */
export async function enumerateMountedExtensions(
	currentDir: string,
): Promise<string[]> {
	const dir = join(currentDir, "extensions");

	// The extensions dir may itself be a single-extension package (manifest or
	// index.*) — the SDK checks this first before scanning children.
	const rootEntries = await resolveExtensionEntries(dir);
	if (rootEntries) return rootEntries;

	let entries: Dirent[];
	try {
		entries = await readdir(dir, { withFileTypes: true });
	} catch {
		// Absent/unreadable mount subtree: no extensions right now.
		return [];
	}
	entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));

	const out: string[] = [];
	for (const entry of entries) {
		const entryPath = join(dir, entry.name);
		if (
			(entry.isFile() || entry.isSymbolicLink()) &&
			(entry.name.endsWith(".ts") || entry.name.endsWith(".js"))
		) {
			out.push(entryPath);
			continue;
		}
		if (entry.isDirectory() || entry.isSymbolicLink()) {
			const resolved = await resolveExtensionEntries(entryPath);
			if (resolved) out.push(...resolved);
		}
	}
	return out;
}

/** The mount's MCP servers, keyed by server name, with per-server provenance. */
export interface MountedMcp {
	configs: Record<string, MCPServerConfig>;
	sources: Record<string, SourceMeta>;
}

/**
 * Parse the mount's `mcp/<name>.json` files into the `{configs, sources}` a
 * `MCPManager.connectServers` call takes. Each file is an `MCPConfigFile`
 * (`{mcpServers: Record<name, MCPServerConfig>}`, mcp/types.ts); every server it
 * declares is merged in, tagged with a `SourceMeta` pointing at its file.
 *
 * PURE parse only — it does NOT connect, resolve credentials (MCP servers read
 * tokens from process.env, sourced by main()), or own a manager. Tolerant:
 * absent dir → empty; a malformed/unreadable file is SKIPPED (never throws), so
 * one bad file cannot sink the others or crash the agent.
 */
export async function readMountedMcpConfigs(
	currentDir: string,
): Promise<MountedMcp> {
	const dir = join(currentDir, "mcp");
	const configs: Record<string, MCPServerConfig> = {};
	const sources: Record<string, SourceMeta> = {};

	let entries: Dirent[];
	try {
		entries = await readdir(dir, { withFileTypes: true });
	} catch {
		// Absent/unreadable mount subtree: no MCP servers right now.
		return { configs, sources };
	}

	const files = entries
		.filter(
			(e) => (e.isFile() || e.isSymbolicLink()) && e.name.endsWith(".json"),
		)
		.map((e) => e.name)
		.sort();

	for (const name of files) {
		const path = join(dir, name);
		let parsed: MCPConfigFile;
		try {
			parsed = (await Bun.file(path).json()) as MCPConfigFile;
		} catch {
			// Malformed/unreadable JSON: skip this file, keep the others.
			continue;
		}
		const servers = parsed?.mcpServers;
		if (!servers || typeof servers !== "object") continue;

		for (const [serverName, config] of Object.entries(servers)) {
			if (!config) continue;
			configs[serverName] = config;
			sources[serverName] = {
				provider: MCP_PROVIDER,
				providerName: MCP_PROVIDER_NAME,
				path,
				level: "user",
			};
		}
	}
	return { configs, sources };
}

/**
 * The bundle's version hash, for a single observability log line. Non
 * load-bearing: absent → undefined, and nothing gates on it.
 */
export async function readConfigVersion(
	currentDir: string,
): Promise<string | undefined> {
	try {
		const text = (await Bun.file(join(currentDir, "version")).text()).trim();
		return text ? text : undefined;
	} catch {
		return undefined;
	}
}

/**
 * The absolute path of a mount member when it exists AND is a REGULAR FILE
 * (symlinks resolved by `stat`), else `undefined`. Existence only — no parsing;
 * the SDK's own strict loaders own the content. Tolerant: any read failure
 * (absent/unreadable) yields `undefined`, never throws — the module's
 * absent→empty posture.
 */
async function readMountedFilePath(
	currentDir: string,
	rel: string,
): Promise<string | undefined> {
	const path = join(currentDir, rel);
	try {
		return (await stat(path)).isFile() ? path : undefined;
	} catch {
		return undefined;
	}
}

/**
 * The absolute path of a mount member when it exists AND is a DIRECTORY
 * (symlinks resolved by `stat`), else `undefined`. Existence only — the SDK's
 * own discovery loads the directory's contents through the CP-4 symlinks.
 * Tolerant: any read failure yields `undefined`, never throws.
 */
async function readMountedDirPath(
	currentDir: string,
	rel: string,
): Promise<string | undefined> {
	const path = join(currentDir, rel);
	try {
		return (await stat(path)).isDirectory() ? path : undefined;
	} catch {
		return undefined;
	}
}

/**
 * The fleet OMP settings document `settings/config.yml` (CP-1), when present as
 * a regular file. `main()` sets `PI_CONFIG_FILES` to this path (after its own
 * Bun-parse guard, T4) so the SDK's `Settings` loads it as a read-only overlay.
 * Existence only — the strict overlay loader (settings.ts) parses it.
 */
export async function readMountedSettingsPath(
	currentDir: string,
): Promise<string | undefined> {
	return readMountedFilePath(currentDir, join("settings", "config.yml"));
}

/** A fleet context file as the SDK's `contextFiles` option takes it. */
export interface MountedContextFile {
	path: string;
	content: string;
}

/**
 * The fleet `AGENTS.md` context file (CP-2), read as a `{path, content}` object
 * for direct injection via `createAgentSession({ contextFiles })` — providing
 * the array short-circuits the SDK's cwd walk-up discovery (sdk.ts:1177-1179),
 * so the fleet file lands as a USER-level context file composing with the
 * checkout's own AGENTS.md chain. Absent/unreadable → `undefined` (tolerant).
 */
export async function readMountedAgentsMd(
	currentDir: string,
): Promise<MountedContextFile | undefined> {
	const path = join(currentDir, "AGENTS.md");
	try {
		if (!(await stat(path)).isFile()) return undefined;
		return { path, content: await Bun.file(path).text() };
	} catch {
		return undefined;
	}
}

/**
 * The role's block-0 system prompt `prompts/<role>/SYSTEM.md` (RIG-1732 T10),
 * read as TEXT for direct injection via `createAgentSession({ customSystemPrompt })`
 * — which REPLACES OMP's block-0 (routed through the SDK's custom-system-prompt
 * template, sdk.ts:2727). The operator-set role label (COMPASS_ROLE) selects the
 * subtree; `main()` reads it only when a role is set.
 *
 * Tolerant, mirroring the other per-surface readers: an absent file, an
 * unreadable one, or an EMPTY/whitespace-only body → `undefined`, never throws —
 * so an unconfigured role falls back to the default block-0 rather than injecting
 * a blank customSystemPrompt (which would still route through the replace path).
 */
export async function readMountedRolePrompt(
	currentDir: string,
	role: string,
): Promise<string | undefined> {
	// Guard the label as a path segment: a role is a flat directory name
	// (`manager`, `supervisor`), never a path. Reject a separator or `..` so the
	// label can never traverse outside the `prompts/` subtree. Defense in depth —
	// today `role` is set out-of-band in the store (no RPC populates it, so the
	// value is trusted), but the guard costs nothing and closes the traversal the
	// moment a client-facing setter lands. A rejected label reads as "no prompt"
	// (undefined), so it falls back to the default block-0 like any absent file.
	if (/[/\\]|\.\./.test(role)) return undefined;
	const path = join(currentDir, "prompts", role, "SYSTEM.md");
	try {
		if (!(await stat(path)).isFile()) return undefined;
		const content = await Bun.file(path).text();
		return content.trim() ? content : undefined;
	} catch {
		return undefined;
	}
}

/**
 * The fleet `models.yml` (CP-4), when present as a top-level regular file.
 * `main()` symlinks it to the user-level agent dir so the SDK's `ModelRegistry`
 * loads it (`config-file.ts` `getAgentDir()/models.yml`). Existence only.
 */
export async function readMountedModelsPath(
	currentDir: string,
): Promise<string | undefined> {
	return readMountedFilePath(currentDir, "models.yml");
}

/**
 * The fleet rules (CP-4), read as SDK `Rule[]` for direct injection via
 * `createAgentSession({ rules })` — providing the array SHORT-CIRCUITS the SDK's
 * rule discovery (sdk.ts:1434-1435), so the fleet rules are the sole injected
 * set. Globs flat `rules/*.md`/`*.mdc` (the discovery grammar: flat, those two
 * extensions) and builds each with the SDK's own `buildRuleFromMarkdown` — the
 * same construction the builtin provider uses, so frontmatter (globs,
 * alwaysApply, description, conditions) is parsed identically. Absent dir → `[]`;
 * an unreadable file is skipped, never fatal (tolerant, mirrors the MCP arm).
 */
export async function readMountedRules(currentDir: string): Promise<Rule[]> {
	const rulesDir = join(currentDir, "rules");
	let entries: Dirent[];
	try {
		entries = await readdir(rulesDir, { withFileTypes: true });
	} catch {
		return []; // absent/unreadable dir: no rules right now
	}
	const rules: Rule[] = [];
	for (const entry of entries) {
		if (!entry.isFile() && !entry.isSymbolicLink()) continue;
		if (!entry.name.endsWith(".md") && !entry.name.endsWith(".mdc")) continue;
		const filePath = join(rulesDir, entry.name);
		try {
			const content = await Bun.file(filePath).text();
			rules.push(
				buildRuleFromMarkdown(
					entry.name,
					content,
					filePath,
					createSourceMeta(RULE_PROVIDER, filePath, "user"),
				),
			);
		} catch {
			// One unreadable/malformed rule file must not sink the rest or the boot.
		}
	}
	rules.sort((a, b) => a.name.localeCompare(b.name));
	return rules;
}

/**
 * The fleet `agents/` subagent-definitions directory (CP-4), when present as a
 * directory. `main()` symlinks it to the user-level agent dir so the `task`
 * tool resolves the mounted subagent defs by name. Existence only.
 */
export async function readMountedAgentsDir(
	currentDir: string,
): Promise<string | undefined> {
	return readMountedDirPath(currentDir, "agents");
}

/**
 * The mount mapped to the `createAgentSession` option surfaces, in one call so
 * `main()` reads the bundle once. `disableExtensionDiscovery` is fixed `true`:
 * with the enumerated entry paths it makes "the mount's extensions and nothing
 * else" a guarantee (choice A), the unconfigured→none the acceptance demands.
 * The MCP arm stays as parsed configs — `main()` owns the manager's connect and
 * teardown, keeping this a pure read.
 */
export interface MountedConfig {
	skills: Skill[];
	additionalExtensionPaths: string[];
	disableExtensionDiscovery: true;
	mcp: MountedMcp;
	version: string | undefined;
	/**
	 * `settings/config.yml` (CP-1), absolute path when present. `main()` builds a
	 * `Settings` from it via `Settings.init({ configFiles: [settingsPath] })` and
	 * passes it as `settingsManager` — object injection, the seam the runtime SDK
	 * actually reads (`configFiles`), not the inert `PI_CONFIG_FILES` env path.
	 */
	settingsPath: string | undefined;
	/**
	 * Fleet `AGENTS.md` (CP-2) as a `{path, content}` context file, injected via
	 * `createAgentSession({ contextFiles })`. `undefined` when absent.
	 */
	agentsMd: MountedContextFile | undefined;
	/**
	 * Fleet rules (CP-4) as SDK `Rule[]`, injected via `createAgentSession({ rules })`.
	 * Empty when the `rules/` dir is absent.
	 */
	rules: Rule[];
	/**
	 * Top-level `models.yml` (CP-4), absolute path when present. Stays
	 * FILESYSTEM-based: `main()` symlinks it into `$HOME/.omp/agent/models.yml`
	 * (no `createAgentSession` object seam for the ModelRegistry yet — flagged gap).
	 */
	modelsPath: string | undefined;
	/**
	 * `agents/` subagent-definitions dir (CP-4), absolute path when present. Stays
	 * FILESYSTEM-based: the SDK discovers subagent defs by walking the agent dir
	 * (`discoverAgents`), with NO object seam, so `main()` symlinks it into
	 * `$HOME/.omp/agent/agents`.
	 */
	agentsDir: string | undefined;
}

export async function loadMountedConfig(mount: string): Promise<MountedConfig> {
	const currentDir = currentConfigDir(mount);
	const [
		skills,
		additionalExtensionPaths,
		mcp,
		version,
		settingsPath,
		agentsMd,
		rules,
		modelsPath,
		agentsDir,
	] = await Promise.all([
		readMountedSkills(currentDir),
		enumerateMountedExtensions(currentDir),
		readMountedMcpConfigs(currentDir),
		readConfigVersion(currentDir),
		readMountedSettingsPath(currentDir),
		readMountedAgentsMd(currentDir),
		readMountedRules(currentDir),
		readMountedModelsPath(currentDir),
		readMountedAgentsDir(currentDir),
	]);
	return {
		skills,
		additionalExtensionPaths,
		disableExtensionDiscovery: true,
		mcp,
		version,
		settingsPath,
		agentsMd,
		rules,
		modelsPath,
		agentsDir,
	};
}
