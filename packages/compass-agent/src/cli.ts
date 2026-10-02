// `compass-agent` — the in-container entrypoint the Runner execs with a bare argv and no
// flags, so it takes its whole configuration from the environment (fixed Runner socket,
// COMPASS_MODEL/ROLE/PERSONA, the 0600 auth-seed + env files). Every decision is a pure
// exported function tested in cli.test.ts; main() is the thin IO composition over MainDeps.

import type { Stats } from "node:fs";
import { lstat, mkdir, readlink, rm, symlink } from "node:fs/promises";
import { join } from "node:path";
import type { ToolLoadMode } from "@oh-my-pi/pi-agent-core";
import type { ApiKey, Model } from "@oh-my-pi/pi-ai";
import {
	type AgentSession,
	type CreateAgentSessionOptions,
	createAgentSession,
	discoverContextFiles,
	type IndexedSessionStorage,
	SessionManager,
	Settings,
	type ToolDefinition,
} from "@oh-my-pi/pi-coding-agent";
import { loadCapability } from "@oh-my-pi/pi-coding-agent/capability";
import {
	type Rule,
	ruleCapability,
} from "@oh-my-pi/pi-coding-agent/capability/rule";
import { MCPManager } from "@oh-my-pi/pi-coding-agent/mcp";
import {
	initTelemetryExport,
	isTelemetryExportEnabled,
} from "@oh-my-pi/pi-coding-agent/telemetry-export";
import { YAML } from "bun";
import { CompassAgent } from "./agent";
import { BoardBroker, createBoardTools } from "./board";
import { CommsBroker, createCommsTools } from "./comms";
import {
	currentConfigDir,
	loadMountedConfig,
	type MountedMcp,
	readMountedRolePrompt,
	resolveConfigMountPath,
} from "./config-reader";
import { createForgeTools, ForgeBroker } from "./forge";
import type { FrameSink } from "./frame";
import { createLifecycleTools, LifecycleBroker } from "./lifecycle";
import {
	createTeeSessionStorage,
	type TranscriptTeeBackend,
	type TranscriptTeeOptions,
} from "./session-tee";
import { createTraceBridge, type TraceBridge } from "./trace-bridge";
import { createSocketControlSource } from "./transport/control-source";
import { createSocketFrameSink } from "./transport/frame-sink";
import {
	createUnixSocketTransport,
	type RunnerTransport,
} from "./transport/index";

/**
 * The in-container path the Runner bind-mounts this agent's socket to. Fixed by
 * contract with `internal/runner/host.go:33` — the agent takes no per-session
 * socket configuration, so this constant IS the rendezvous.
 */
export const AGENT_SOCKET_PATH = "/run/compass/agent.sock";

/**
 * The gateway-socket path this agent dials: the `COMPASS_AGENT_SOCKET_PATH`
 * env override when set, else the fixed `AGENT_SOCKET_PATH` default.
 *
 * The container tiers bind-mount the socket at the fixed default and set no
 * override, so they resolve `AGENT_SOCKET_PATH` unchanged. The host-process tier
 * has no bind mounts — it serves the socket inside the agent handle's own state
 * dir and threads the path here (design "Agent transport: the socket and config
 * paths"). Unset or blank is the default, matching the Runner's empty-omit of an
 * unset var (`go/internal/runner/agent_exec.go` execSpec) and every other
 * `resolve*` here: a blank override is not a valid socket to dial.
 */
export function resolveSocketPath(
	env: Record<string, string | undefined>,
): string {
	return env.COMPASS_AGENT_SOCKET_PATH?.trim() || AGENT_SOCKET_PATH;
}

/** The 0600 provider-credential seed the Runner materializes (design §T5). */
export function authSeedPath(home: string): string {
	return `${home}/.compass/auth-seed.json`;
}

/** The 0600 aggregate env-secret file the Runner materializes (RIG-1327 T5). */
export function envFilePath(home: string): string {
	return `${home}/.compass/env`;
}

/**
 * Keys a file may never set: `HOME` (the agent's Runner-scoped home) and the
 * entire `COMPASS_*` control-var namespace. Only the Runner/agent populate
 * `COMPASS_*` (model/persona/workdir/resume-file, …), so any file-supplied
 * `COMPASS_`-prefixed key is illegitimate and dropped — a prefix rule rather
 * than a list so a control var added later (e.g. `COMPASS_RESUME_SESSION_FILE`)
 * is reserved without editing this filter.
 */
function isReservedEnvKey(key: string): boolean {
	return key === "HOME" || key.startsWith("COMPASS_");
}

/**
 * Parse the materialized env file's `KEY=VALUE` lines. Split on the FIRST `=`
 * (a value may contain `=`); the value is literal to end-of-line, only a
 * trailing `\r` stripped so a CRLF-written file is tolerated. Blank lines,
 * `=`-less lines, and empty-key lines are skipped. Reserved keys (`HOME` and
 * the whole `COMPASS_*` namespace) are excluded so a file KEY can never clobber
 * a Runner-set var — see `isReservedEnvKey`. Pure — the
 * IO + the merge into `process.env` live in `main`.
 */
export function parseEnvFile(contents: string): Record<string, string> {
	const out: Record<string, string> = {};
	for (const rawLine of contents.split("\n")) {
		const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
		const eq = line.indexOf("=");
		if (eq < 1) continue; // no `=`, or an empty key (eq === 0)
		const key = line.slice(0, eq).trim();
		if (key === "" || isReservedEnvKey(key)) continue;
		out[key] = line.slice(eq + 1);
	}
	return out;
}

/**
 * The model selector for this container, from `COMPASS_MODEL`.
 *
 * Returned as an opaque pattern string for `createAgentSession` to resolve
 * against its own model registry — the entrypoint deliberately does not parse
 * provider/id itself, so adding a provider never touches this file.
 *
 * Unset (or blank) is a legitimate configuration, not an error: the session
 * falls back to the SDK's default model rather than refusing to boot.
 */
export function resolveModelSelector(
	env: Record<string, string | undefined>,
): string | undefined {
	const raw = env.COMPASS_MODEL?.trim();
	return raw ? raw : undefined;
}

/**
 * The identity persona for this container, from `COMPASS_PERSONA`.
 *
 * An OVERLAY string appended to the SDK's default system prompt (see `main`),
 * not a replacement — block-0 base instructions and the project footer survive.
 *
 * Unset (or blank) is a legitimate configuration: the Runner empty-omits the
 * env var (`go/internal/runner/agent_exec.go` `execSpec`), so an absent overlay
 * leaves the agent on its default prompt.
 */
export function resolvePersona(
	env: Record<string, string | undefined>,
): string | undefined {
	const raw = env.COMPASS_PERSONA?.trim();
	return raw ? raw : undefined;
}

/**
 * The block-0 role for this container, from `COMPASS_ROLE`.
 *
 * A REPLACEMENT selector, not an overlay: the label names a
 * `prompts/<role>/SYSTEM.md` in the mount, whose text `main` injects as
 * `customSystemPrompt` — replacing OMP's default block-0 (persona still appends
 * AFTER, record §OQ-8). This function only resolves the LABEL; the file lookup +
 * fallback (absent file → today's behavior) live in `main`.
 *
 * Unset (or blank) is a legitimate configuration: the Runner empty-omits the env
 * var (`go/internal/runner/agent_exec.go` `execSpec`), so an absent role leaves
 * the agent on its default block-0. Same unset/trim semantics as `resolvePersona`.
 */
export function resolveRole(
	env: Record<string, string | undefined>,
): string | undefined {
	const raw = env.COMPASS_ROLE?.trim();
	return raw ? raw : undefined;
}

/**
 * The LiteLLM MCP endpoint, DERIVED from the delivered `LITELLM_BASE_URL`
 * (mirroring the wave's SOPS loader `secrets-env.nix`). Compass's keyring
 * delivers `LITELLM_BASE_URL` + `LITELLM_API_KEY` but NOT the MCP URL — it is a
 * derived var, not a stored secret — so the fleet `mcp.json`'s
 * `${LITELLM_MCP_URL}` would otherwise expand empty and the LiteLLM MCP server
 * would fail to connect. Deriving (rather than seeding a second secret) keeps a
 * single source of truth and no base/MCP drift.
 *
 * The rule matches the loader byte-for-byte: strip one trailing `/`, strip a
 * trailing `/v1`, append `/mcp/`. The trailing slash is LOAD-BEARING — LiteLLM
 * 307-redirects `/mcp` and MCP clients do not re-POST across the redirect
 * (`secrets-env.nix:27-29`). So `https://host/v1` → `https://host/mcp/`.
 *
 * Unset (or blank) base is a legitimate configuration: no LiteLLM gateway is
 * configured, so nothing to derive — returns undefined and `main` leaves
 * `LITELLM_MCP_URL` untouched. Same unset/trim semantics as `resolveModelSelector`.
 */
export function deriveLitellmMcpUrl(
	env: Record<string, string | undefined>,
): string | undefined {
	const raw = env.LITELLM_BASE_URL?.trim();
	if (!raw) return undefined;
	const base = raw.replace(/\/$/, "").replace(/\/v1$/, "");
	return `${base}/mcp/`;
}

/**
 * Whether an OTLP trace endpoint is configured — the pre-registration gate for
 * the loop's OpenTelemetry activation (design
 * docs/designs/observability/compass-agent-loop-otel/design.md T1). Mirrors the
 * predicate `initTelemetryExport` applies before it registers a provider
 * (`telemetry-export.ts`): an endpoint present via
 * `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (falling back to the base
 * `OTEL_EXPORTER_OTLP_ENDPOINT`) AND the kill-switches clear
 * (`OTEL_SDK_DISABLED` not `true`, `OTEL_TRACES_EXPORTER` not naming `none`).
 *
 * This is the gate for `main`'s env writes + `init()` call: with no endpoint the
 * whole activation is skipped so `process.env` stays UNMUTATED and the session
 * build is bit-identical to a no-telemetry container (Global Constraints, "Off
 * by default"; F2). The authoritative "did a provider actually register" check
 * is `isTelemetryExportEnabled()` AFTER `init()` — which additionally declines
 * an unsupported transport protocol — and that is what gates the `telemetry`
 * session option.
 */
export function isTelemetryEndpointConfigured(
	env: Record<string, string | undefined>,
): boolean {
	if (env.OTEL_SDK_DISABLED?.trim().toLowerCase() === "true") return false;
	if (
		env.OTEL_TRACES_EXPORTER?.split(",").some(
			(entry) => entry.trim().toLowerCase() === "none",
		)
	) {
		return false;
	}
	const endpoint =
		env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ?? env.OTEL_EXPORTER_OTLP_ENDPOINT;
	return endpoint !== undefined && endpoint !== "";
}

/**
 * The loop-telemetry activation hooks, injectable at the `MainDeps` seam. The
 * default binds the reused `@oh-my-pi/pi-coding-agent/telemetry-export` module.
 * A test overrides it so it can assert `main`'s gating/env logic WITHOUT the
 * real `initTelemetryExport`, which registers a live global TracerProvider + a
 * real OTLP exporter with no teardown and would poison every later test in the
 * shared process (design record F3).
 */
interface TelemetryHooks {
	/** Register the global provider when an endpoint is configured. Idempotent. */
	init: () => Promise<void>;
	/** Whether a real provider registered — gates the `telemetry` session option. */
	isEnabled: () => boolean;
}

const defaultTelemetryHooks: TelemetryHooks = {
	init: initTelemetryExport,
	isEnabled: isTelemetryExportEnabled,
};

/** One provider's credential in the seed file. Mirrors the SDK's `ApiKeyCredential`. */
interface SeedEntry {
	readonly type?: string;
	readonly key?: unknown;
}

/** The seed document: provider id → credential (design §T5 `ProviderSeed`). */
interface Seed {
	readonly entries?: Record<string, SeedEntry | undefined>;
}

/**
 * A `getApiKey` resolver backed by the on-disk seed, layered over the SDK's own
 * per-call resolver.
 *
 * Re-reads the seed on EVERY call, which is the load-bearing behavior: the SDK
 * invokes `getApiKey` per LLM call precisely so an expiring or rotated
 * credential is picked up without a restart (`agent.d.ts:66-70`), and rotation
 * (design §T6) rewrites this file in place. A value cached at construction would
 * silently pin the container to a stale key until it was torn down.
 *
 * A seeded provider's key always wins — that is the T6 rotation contract. When
 * the seed has no key for the provider, resolution FALLS THROUGH to `fallback`,
 * the SDK resolver `createAgentSession` installed (`modelRegistry.resolver`,
 * `sdk.ts:3030`). That resolver is what returns the keyless `"N/A"` sentinel
 * (`kNoAuth`) for a `auth: none` models.yml provider (`model-registry.ts:2305`,
 * `openai-shared.ts` `NO_AUTH_SENTINEL`), so a keyless local/gateway provider
 * dials instead of failing `MissingApiKeyError`. Replacing the SDK resolver
 * outright (seed-only) silently dropped that path — a delivered idle turn ran
 * the full agent loop but never dialed the model. `fallback` is optional so the
 * pure seed lookup stays testable in isolation; `main` always passes it.
 *
 * Every seed failure path returns via the fallback (or `undefined` when none):
 * a missing, unreadable, malformed, or provider-less seed must leave the agent
 * running and able to report, letting the SDK surface a clean auth error on the
 * call that needed the key. A container that crashes at boot because its
 * credential has not been materialized yet is strictly worse — provisioning
 * writes the seed after the container is up.
 */
export function createSeedApiKeyResolver(
	home: string,
	fallback?: (model: Model) => Promise<ApiKey | undefined> | ApiKey | undefined,
): (model: Model) => Promise<ApiKey | undefined> {
	const path = authSeedPath(home);
	return async (model: Model): Promise<ApiKey | undefined> => {
		const seed = await readSeed(path);
		const key = seed?.entries?.[model.provider]?.key;
		if (typeof key === "string") return key;
		return fallback?.(model);
	};
}

async function readSeed(path: string): Promise<Seed | undefined> {
	try {
		return (await Bun.file(path).json()) as Seed;
	} catch {
		// Absent or malformed: indistinguishable to the caller, and both mean "no
		// credential available right now".
		return undefined;
	}
}

/**
 * Read + parse the materialized env-secret file. Absent/empty/unreadable yields
 * no secrets (`{}`), never throws — the same tolerant posture as `readSeed`; an
 * empty file is normal (the writer always writes it, even with zero secrets).
 */
async function readEnvFile(path: string): Promise<Record<string, string>> {
	try {
		return parseEnvFile(await Bun.file(path).text());
	} catch {
		// Absent/unreadable: no env secrets right now (same posture as readSeed).
		return {};
	}
}

/**
 * The connected MCP tools + a teardown handle, for the mount's servers (design
 * §CD-3). Built + connected here rather than handed to `createAgentSession` as
 * an `mcpManager`: a provided manager is stored on the tool session but its
 * `getTools()` is NEVER harvested into the tool registry (the population block
 * runs only in the SDK's `!mcpManager` discovery branch, sdk.ts:1739/1818), so
 * the tools would silently never surface. Instead we connect the manager
 * ourselves and pass `manager.getTools()` as `customTools` with
 * `enableMCP: false` (so the SDK does not ALSO discover a cwd `.mcp.json`).
 *
 * OWN the lifecycle: the SDK is not the owner, so it never disconnects. `main`
 * calls `disconnect` in its teardown. Credential-free by MVP rule — the servers
 * read tokens from `process.env`, which `main` has already sourced from the
 * aggregate env file above; the connector resolves no credentials.
 *
 * An empty config set skips building a manager entirely: no connect, and a
 * no-op disconnect — the unconfigured→none guarantee, with no teardown work.
 */
interface ConnectedMcp {
	tools: NonNullable<CreateAgentSessionOptions["customTools"]>;
	disconnect: () => Promise<void>;
}

async function connectMountedMcp(
	cwd: string,
	mcp: MountedMcp,
): Promise<ConnectedMcp> {
	if (Object.keys(mcp.configs).length === 0) {
		return { tools: [], disconnect: () => Promise.resolve() };
	}
	const manager = new MCPManager(cwd, null);
	await manager.connectServers(mcp.configs, mcp.sources);
	return {
		tools: manager.getTools(),
		disconnect: () => manager.disconnectAll(),
	};
}

/**
 * Mark every custom tool `essential` so it lands in the model's top-level
 * callable schema, and return the same array.
 *
 * Assigns in place BY DESIGN — see the `customTools` seam in `main` for why
 * copying is unsafe here (SDK tools are class instances with prototype methods
 * and `#private` state, so a spread or clone yields a tool the model can see
 * and cannot call). `loadMode` is a mutable field on `CustomTool`, so this
 * preserves object identity.
 */
function stampEssential<T extends { loadMode?: ToolLoadMode }>(
	tools: T[],
): T[] {
	for (const tool of tools) {
		tool.loadMode = "essential";
	}
	return tools;
}

/**
 * The user-level agent dir the SDK's native discovery anchors on inside the
 * container: `$HOME/.omp/agent` (`getAgentDir()` default, dirs.ts). The
 * filesystem-based members are symlinked here so the SDK finds them at USER
 * level, composing additively with the checkout's own project-level config.
 */
function agentDirPath(home: string): string {
	return join(home, ".omp", "agent");
}

/**
 * Idempotently reconcile one user-level agent-dir entry against its mounted
 * target. Used ONLY for the two members the runtime SDK (16.5.2) has no object
 * seam for and must load by path:
 *   - `agents` (CP-4): subagent defs, discovered by `discoverAgents` walking the
 *     agent dir — no `createAgentSession` param injects them.
 *   - `models.yml` (CP-4): loaded by the ModelRegistry from
 *     `getAgentDir()/models.yml` — object injection is a flagged gap.
 * (settings, rules, and AGENTS.md now inject as OBJECTS — see `main`.)
 *
 * `target` is the member's `current/`-relative mount path (the T3 reader's
 * field, e.g. `<mount>/current/agents`) — a CONSTANT pointing THROUGH `current/`,
 * so a ConfigVersion flip + Reload re-resolves fresh content with zero extra
 * wiring, or `undefined` when the bundle omits the member.
 *
 * The contract, verbatim from the record:
 *   - `mkdir -p $HOME/.omp/agent`.
 *   - target SET: when `$HOME/.omp/agent/<entry>` is ABSENT or an existing
 *     SYMLINK, (re)point it at `target`. A pre-existing REGULAR FILE or REAL
 *     DIRECTORY is NEVER clobbered — log and leave it (it wins).
 *   - target UNSET: remove a Compass-owned SYMLINK if present; never a real
 *     file/dir a user placed.
 *   - Every failure logs and continues — a tolerant boot never crashes on a
 *     link it could not place.
 */
export async function ensureAgentDirLink(
	home: string,
	entry: string,
	target: string | undefined,
): Promise<void> {
	const dir = agentDirPath(home);
	const linkPath = join(dir, entry);
	try {
		await mkdir(dir, { recursive: true });
		// lstat, NOT stat: we must distinguish a Compass-owned symlink (which we
		// may repoint/remove) from a user-placed real file/dir (which always wins)
		// WITHOUT following the link.
		let existing: Stats | undefined;
		try {
			existing = await lstat(linkPath);
		} catch {
			existing = undefined; // absent
		}

		if (target === undefined) {
			// Unconfigured member: reclaim only a link WE could have written.
			if (existing?.isSymbolicLink()) {
				await rm(linkPath);
			} else if (existing) {
				console.error(
					`[compass-agent] leaving user-placed ${entry} at ${linkPath} in place (no fleet member to link)`,
				);
			}
			return;
		}

		if (existing && !existing.isSymbolicLink()) {
			// A real file or dir the user placed: it wins, never clobbered.
			console.error(
				`[compass-agent] not linking fleet ${entry}: ${linkPath} is a user-placed ${existing.isDirectory() ? "directory" : "file"} (it wins)`,
			);
			return;
		}

		// Absent or an existing symlink: (re)point idempotently. If the link is
		// already correct, skip the rewrite; otherwise remove the stale link and
		// recreate — `symlink` fails EEXIST on any existing path.
		if (existing?.isSymbolicLink()) {
			const current = await readlink(linkPath).catch(() => undefined);
			if (current === target) return;
			await rm(linkPath);
		}
		await symlink(target, linkPath);
	} catch (error) {
		// Tolerant boot: a link we could not place is logged, never fatal.
		console.error(
			`[compass-agent] failed to reconcile agent-dir link ${linkPath}:`,
			error,
		);
	}
}

/**
 * Build the fleet `Settings` for injection as `createAgentSession({ settingsManager })`
 * (CP-1) — object injection, parse-guarded, fail-open (OQ-7).
 *
 * `Settings.init({ cwd, agentDir, configFiles: [settingsPath] })` loads the
 * fleet member as a read-only OVERLAY layer (deepMerged AFTER global+project, so
 * fleet policy beats the checkout's project settings and loses only to runtime
 * overrides — settings.ts `#loadConfigOverlays`→`#rebuildMerged`). `configFiles`
 * is the seam the runtime SDK (16.5.2) actually reads; the design's
 * `PI_CONFIG_FILES` env path is inert against it, so this replaces it.
 *
 * The overlay loader is STRICT — a missing/malformed member is a HARD error at
 * `Settings.init` (settings.ts:801-823). So a member the Go door admitted but
 * Bun rejects would crash EVERY agent at boot, a crash the Reload path cannot
 * see, leaving the fleet dead until an operator pushes a fixed bundle. Guard:
 * Bun-parse the member FIRST; on failure log loudly and build Settings WITHOUT
 * the overlay — fail-open to SDK defaults.
 *
 * Returns `undefined` when there is no fleet member (or it failed the guard),
 * so `main` omits `settingsManager` and the SDK inits its own default Settings.
 */
export async function buildFleetSettings(
	cwd: string,
	agentDir: string,
	settingsPath: string | undefined,
): Promise<Settings | undefined> {
	if (settingsPath === undefined) return undefined;
	try {
		// Parse guard (OQ-7): the same Bun parser the strict overlay loader uses.
		YAML.parse(await Bun.file(settingsPath).text());
	} catch (error) {
		console.error(
			`[compass-agent] fleet settings ${settingsPath} failed Bun YAML.parse — ignoring, booting on SDK defaults:`,
			error,
		);
		return undefined;
	}
	try {
		return await Settings.loadIsolated({
			cwd,
			agentDir,
			configFiles: [settingsPath],
		});
	} catch (error) {
		// Belt for any residual overlay-load divergence past the Bun guard: still
		// fail-open rather than crash the boot.
		console.error(
			`[compass-agent] fleet settings ${settingsPath} failed to load as an overlay — booting on SDK defaults:`,
			error,
		);
		return undefined;
	}
}

/**
 * The two outside-world constructors `main` reaches through. Overridable ONLY so
 * a test can compose the entrypoint over a fake carrier; both default to the
 * production factories, so the Runner's call path — `main()` with no second
 * argument, below — is byte-identical to hard-coding them: same functions, same
 * arguments, same order, same call site.
 *
 * A seam rather than a mock of the SDK because both defaults are unfakeable
 * in-process: `createAgentSession` loads extensions/MCP/skills/the model
 * registry/auth off disk, and `createUnixSocketTransport` dials a socket that
 * only exists inside the container. What they feed — the drain barrier below —
 * is the part that carries a real defect, so it is the part worth reaching.
 */
export interface MainDeps {
	/** Session constructor. Defaults to the SDK's `createAgentSession`. */
	createSession?: (
		options: CreateAgentSessionOptions,
	) => Promise<{ session: AgentSession }>;
	/** Runner-socket carrier. Defaults to `createUnixSocketTransport`. */
	createTransport?: (socketPath: string) => RunnerTransport;
	/**
	 * Tee-storage constructor (RIG-1570). Defaults to `createTeeSessionStorage`.
	 * A seam for the same reason as the other two: the real one wraps the SDK's
	 * `IndexedSessionStorage` over a filesystem backend and awaits `initialize()`
	 * off disk, so a test composes `main` over a recording storage instead.
	 */
	createSessionStorage?: (
		sink: FrameSink,
		sessionDir: string,
		options?: TranscriptTeeOptions,
	) => Promise<{
		storage: IndexedSessionStorage;
		backend: TranscriptTeeBackend;
	}>;
	/**
	 * The agent-config mount root the reader reads through (design §CD-3).
	 * Defaults to the fixed `AGENT_CONFIG_MOUNT_PATH`. Overridable ONLY so a test
	 * can point the reader at a tempdir fixture instead of the container path
	 * (`/run/compass/agent-config`), which does not exist off-container — the same
	 * tempfile posture the seed/env tests use.
	 */
	configMount?: string;
	/**
	 * MCP connector for the mount's servers. Defaults to `connectMountedMcp`,
	 * which builds a real `MCPManager` and dials each server (a stdio subprocess
	 * or HTTP endpoint). A seam for the same unfakeable reason as `createSession`:
	 * a test cannot spawn real MCP servers, so it composes `main` over a
	 * connector that returns recorded tools + a recording disconnect — the only
	 * way to reach the customTools wiring and the teardown-disconnect barrier.
	 */
	connectMcp?: (cwd: string, mcp: MountedMcp) => Promise<ConnectedMcp>;
	/**
	 * Loop-OpenTelemetry activation hooks (design
	 * docs/designs/observability/compass-agent-loop-otel/design.md T1). Defaults to the
	 * reused telemetry-export module. Injectable ONLY so a test asserts the
	 * gating/env logic without the real `initTelemetryExport` — it registers a
	 * live global provider + OTLP exporter with no teardown (F3), so it must never
	 * run in the shared test process.
	 */
	telemetry?: TelemetryHooks;
}

/**
 * Build the session, wire it to the socket carrier, and run until the control
 * stream ends. Resolves when the agent's run loop completes.
 */
export async function main(
	env: Record<string, string | undefined> = process.env,
	deps: MainDeps = {},
): Promise<void> {
	const home = env.HOME;
	if (!home) {
		throw new Error(
			"compass-agent: HOME is unset — the Runner launches the agent with its scoped home; without it the provider seed cannot be located",
		);
	}

	// Materialized tool/MCP secrets (RIG-1327 T5): source the Runner's 0600 aggregate
	// KEY=VALUE file into process.env — NOT the `env` param — so createAgentSession's
	// extensions/MCP/tools and their subprocesses inherit them. File wins per key it
	// defines; HOME and the COMPASS_* control namespace are never clobbered.
	for (const [key, value] of Object.entries(
		await readEnvFile(envFilePath(home)),
	)) {
		process.env[key] = value;
	}

	// Derive LITELLM_MCP_URL from the just-sourced LITELLM_BASE_URL (RIG-2674): the
	// keyring delivers base URL + key but not the derived MCP URL, so mcp.json's
	// `${LITELLM_MCP_URL}` would expand empty. Fill the gap after the env merge; an
	// explicitly-delivered value wins.
	if (!process.env.LITELLM_MCP_URL) {
		const mcpUrl = deriveLitellmMcpUrl(process.env);
		if (mcpUrl) process.env.LITELLM_MCP_URL = mcpUrl;
	}

	// The identity overlay; undefined when unset or whitespace-only. What omits
	// the overlay in that case is the `persona ?` spread guard below (an absent
	// `systemPrompt` key), not any `||`/`??` subtlety — `resolvePersona` has
	// already normalized a blank value to undefined.
	const persona = resolvePersona(env);

	// The block-0 role selector; undefined when unset/whitespace. When set, `main`
	// reads its `prompts/<role>/SYSTEM.md` and injects it as `customSystemPrompt`,
	// REPLACING OMP's default block-0. This resolve yields only the LABEL; file
	// lookup + fallback live below.
	const role = resolveRole(env);

	// The workdir the session is keyed to. `||`, not `??`: an empty/whitespace
	// COMPASS_WORKDIR is unset, not a valid cwd. The Runner always sets it, so a
	// caller building an AgentEnv with a blank Workdir would otherwise hand bun
	// `cwd: ""` — which silently loads the wrong tree instead of throwing.
	const cwd = env.COMPASS_WORKDIR?.trim() || process.cwd();

	// The socket carrier + sink come FIRST: the tee storage backend teems every
	// committed session write onto the sink's DURABLE lane (RIG-1570), so the
	// sink must exist before the storage that holds it.
	const transport = (deps.createTransport ?? createUnixSocketTransport)(
		resolveSocketPath(env),
	);
	const sink = createSocketFrameSink(transport);

	// The tee session storage, wrapped + initialize()d (its scan of the session dir
	// must finish before SessionManager.create so synchronous resume lookups see the
	// keyspace). SESSION_DIR is the SDK-default HOME-relative dir for this cwd —
	// checkout-independent (anchored on the agent's scoped $HOME; DL-090 no-auto-clone).
	const sessionDir = SessionManager.getDefaultSessionDir(cwd);
	// Resume (RIG-1570): T8 exports COMPASS_RESUME_SESSION_FILE. Resolve it BEFORE the
	// storage is built so it can be threaded into the tee backend and indexed at
	// initialize()→loadIndex() — the resume file lives at an absolute path OUTSIDE
	// sessionDir (Option B, T2), else setSessionFile's statSync gate would ENOENT it.
	const resumeFile = env.COMPASS_RESUME_SESSION_FILE?.trim();
	const { storage } = await (
		deps.createSessionStorage ?? createTeeSessionStorage
	)(sink, sessionDir, resumeFile ? { resumeFile } : undefined);
	// SYNCHRONOUS (session-manager.ts:1839 returns SessionManager, not a Promise):
	// do NOT await. The wrapped IndexedSessionStorage is the 3rd arg.
	const manager = SessionManager.create(cwd, sessionDir, storage);

	// When set, load it through the SDK-native path (setSessionFile → drain → migrate
	// → resolveBlobRefs → apply) BEFORE creating the session; reads flow through the
	// tee backend, no replay code. The reconstructed body is authoritative; the load
	// never tees. The resume file is now indexed at initialize() so this gate passes.
	if (resumeFile) await manager.setSessionFile(resumeFile);

	// The Runner-mounted agent-config bundle (design §CD-3): read the mount and map
	// it to the createAgentSession surfaces below; unconfigured yields every field
	// empty, so NONE is injected. Test seam wins when set; else the
	// COMPASS_AGENT_CONFIG_MOUNT_PATH override, defaulting to the fixed mount path.
	const configMount = deps.configMount ?? resolveConfigMountPath(env);
	const mounted = await loadMountedConfig(configMount);
	// The bundle hash, for one observability line. Non load-bearing: absent → no
	// line, and nothing gates on it.
	if (mounted.version) {
		console.error(`[compass-agent] config version: ${mounted.version}`);
	}

	// The role's block-0 prompt (RIG-1732 T10): when a role is set, read its
	// `prompts/<role>/SYSTEM.md` from the mount and inject it below as
	// `customSystemPrompt`, REPLACING OMP's default block-0. Tolerant read (absent/
	// empty → undefined) so a set-but-unshipped role falls back to the default block-0.
	const rolePrompt = role
		? await readMountedRolePrompt(currentConfigDir(configMount), role)
		: undefined;
	if (role && rolePrompt === undefined) {
		// A role was selected but its prompt did not materialize. Boot degrades to
		// the default block-0, but a role-without-shipped-prompt is an operator
		// config gap the server taxonomy cannot catch (it validates the label, not
		// the bundle), so surface it loudly rather than silently.
		console.error(
			`[compass-agent] role ${role} is set but no prompt was found at prompts/${role}/SYSTEM.md — falling back to the default block-0`,
		);
	}

	// Fleet OMP config passthrough (RIG-1678, design compass-agent-config-passthrough
	// §CP-1/CP-2/CP-4): the mount is the delivery vehicle but the agent CONSUMES it by
	// OBJECT INJECTION at the createAgentSession seams (settings/rules/contextFiles).
	// Only agents + models.yml (no seam) stay FS-based, symlinked through current/.
	const fleetSettings = await buildFleetSettings(
		cwd,
		agentDirPath(home),
		mounted.settingsPath,
	);
	await Promise.all([
		ensureAgentDirLink(home, "agents", mounted.agentsDir),
		ensureAgentDirLink(home, "models.yml", mounted.modelsPath),
	]);
	// Connect the mount's MCP servers now, before construction, so their tools reach
	// createAgentSession as customTools. `main` OWNS the manager — the SDK never
	// disconnects a manager it did not build — so its `disconnect` is in the teardown
	// finally below.
	const mcp = await (deps.connectMcp ?? connectMountedMcp)(cwd, mounted.mcp);

	// Fleet AGENTS.md compose (CP-2, Matt-decided): the fleet AGENTS.md is a global
	// file that must COMPOSE with — never REPLACE — the checkout's project chain.
	// `contextFiles` short-circuits SDK discovery, so we run that discovery ourselves
	// and prepend the fleet global (least prominent); absent → omit so the SDK does it.
	const contextFiles = mounted.agentsMd
		? [
				mounted.agentsMd,
				...(await discoverContextFiles(cwd, agentDirPath(home))),
			]
		: undefined;

	// Fleet rules compose (CP-4, Matt-decided): the fleet rules/ is a global set that
	// must COMPOSE with — never REPLACE — the checkout's discovered rules. `rules`
	// short-circuits SDK rule discovery, so we run it ourselves and prepend the fleet
	// rules (least prominent), checkout rules following.
	const discoveredRules = (
		await loadCapability<Rule>(ruleCapability.id, { cwd })
	).items;
	const rules = [...mounted.rules, ...discoveredRules];

	// Loop OpenTelemetry activation (design compass-agent-loop-otel T1). Gated HARD on
	// an OTLP endpoint: unset → skipped whole, process.env UNMUTATED, bit-identical to
	// no-telemetry. Order matters: the provider reads OTEL_* at registration so env
	// defaults must precede init(). Session id is the join key (3a), APPENDED not clobbered.
	const telemetryHooks = deps.telemetry ?? defaultTelemetryHooks;
	if (isTelemetryEndpointConfigured(process.env)) {
		process.env.OTEL_SERVICE_NAME ??= "compass-agent";
		const joinKey = `compass.session.id=${manager.getSessionId()}`;
		const existing = process.env.OTEL_RESOURCE_ATTRIBUTES;
		process.env.OTEL_RESOURCE_ATTRIBUTES = existing
			? `${existing},${joinKey}`
			: joinKey;
		await telemetryHooks.init();
	}

	// Trace-continuity bridge (design compass-agent-message-trace-continuity §T2):
	// built ONLY on the same enabled path that registered the loop provider above —
	// the parentage it installs presupposes that registration. Telemetry off ⇒
	// undefined flows to both seams, every agent-side trace call no-ops (bit-identical).
	const traceBridge: TraceBridge | undefined = telemetryHooks.isEnabled()
		? createTraceBridge()
		: undefined;

	// Native comms + lifecycle tools (RIG-1741 gap-1): `transport` (RunnerTransport)
	// structurally satisfies both broker transports, so they wrap it with no adapter
	// and their tools merge into customTools below. The comms broker also reads the
	// turn trigger (RIG-2894) via TraceBridge to stamp trigger_traceparent (off ⇒ "").
	const commsBroker = new CommsBroker(transport, traceBridge);
	const lifecycleBroker = new LifecycleBroker(transport);
	const forgeBroker = new ForgeBroker(transport);
	const boardBroker = new BoardBroker(transport);
	// The comms/lifecycle natives are AgentTool, registered via customTools with one
	// type-sound assertion to ToolDefinition. RUNTIME invariant (do NOT simplify): the
	// SDK runs them as CustomTools, so execute receives args 3-5 SHUFFLED — safe ONLY
	// because every native reads solely (id, params); cli.test.ts pins execute.length===2.
	const nativeTools = [
		...createCommsTools(commsBroker),
		...createLifecycleTools(lifecycleBroker),
		...createForgeTools(forgeBroker),
		...createBoardTools(boardBroker),
	] as ToolDefinition[];

	// Resolve the pinned model selector ONCE and share it between the session
	// option and the boot-model-health belt below, so the "was a model pinned?"
	// signal the two consult can never diverge.
	const modelPattern = resolveModelSelector(env);

	const { session } = await (deps.createSession ?? createAgentSession)({
		cwd,
		modelPattern,
		// The tee-backed manager, so every session write teems upstream and the
		// resumed history (if any) is already loaded.
		sessionManager: manager,
		// The Runner-mounted agent-config (design §CD-3): each field passed
		// UNCONDITIONALLY, empty when unconfigured, so "unconfigured → none" is a
		// guarantee (skills [] skips discovery; extensions verbatim; enableMCP:false).
		// Scope covers skills/extensions/MCP, NOT cwd custom-TOOL discovery.
		skills: mounted.skills,
		additionalExtensionPaths: mounted.additionalExtensionPaths,
		disableExtensionDiscovery: mounted.disableExtensionDiscovery,
		// MCP tools MERGED with native comms/lifecycle tools, all reaching the session
		// as natives. `loadMode: "essential"` stamped on the WHOLE array is REQUIRED:
		// SDK 18.x defaults omitted loadMode to "discoverable" (registered but NOT model-
		// callable), and getTools() sets none, so unstamped MCP tools become unreachable.

		// Stamped IN PLACE, never copied: mcp.tools are MCPTool class instances with
		// prototype methods and DeferredMCPTool #private state, so a spread/clone breaks
		// them at the model's first call. `loadMode` is a mutable CustomTool field, so
		// assigning preserves identity (methods, private state, reconnect rebinding).
		customTools: stampEssential([...mcp.tools, ...nativeTools]),
		enableMCP: false,
		// Headless approval policy (RIG-1741): the container has NO human to answer an
		// approval prompt and the native tools declare approval:"write", so without
		// auto-approve they block forever. Unconditional — safety rests on the external
		// invariant that the Runner exec's this bin headless; if that changes, gate it.
		autoApprove: true,
		// Fleet config object injection (RIG-1678 pivot): `rules` (CP-4) = fleet rules
		// composed with discovered rules, always; `contextFiles` (CP-2) = [fleet
		// AGENTS.md, ...project], only when the fleet file exists; `settingsManager`
		// (CP-1) = prebuilt fleet Settings, only when built.
		rules,
		...(contextFiles ? { contextFiles } : {}),
		...(fleetSettings ? { settingsManager: fleetSettings } : {}),
		// Role (RIG-1732 T10) + persona compose INDEPENDENTLY, both apply:
		// customSystemPrompt (role) REPLACES the default block-0 (template still injects
		// skills+rules), passed only when found; systemPrompt (persona) APPENDS the
		// identity overlay after the default array (§OQ-8), passed only when set.
		...(rolePrompt ? { customSystemPrompt: rolePrompt } : {}),
		...(persona
			? {
					systemPrompt: (defaultPrompt: string[]) => [
						...defaultPrompt,
						persona,
					],
				}
			: {}),
		// Loop telemetry (design compass-agent-loop-otel T1) + trace continuity
		// (message-trace-continuity §T2): the enabled path installs the bridge's
		// capture hooks into the telemetry config so it parents/links injected
		// messages. Keyed off `traceBridge` defined iff enabled; omitted when off.
		...(traceBridge !== undefined
			? {
					telemetry: {
						onSpanStart: traceBridge.onSpanStart,
						onSpanEnd: traceBridge.onSpanEnd,
					},
				}
			: {}),
	});

	// Boot-model-health belt. createAgentSession SWALLOWS a models.yml validation
	// error, leaving the registry with a recorded error and booting model-less when
	// the pinned selector no longer resolves — surfacing far away as a first-turn
	// "No model configured". Log it here and refuse to boot on a pinned-but-unresolved.
	const modelRegistry = session.modelRegistry;
	const modelError = modelRegistry.getError();
	if (modelError !== undefined) {
		console.error(
			`[compass-agent] models.yml config rejected (${modelError.id}): ${modelError.message} — falling back to built-in model resolution`,
		);
	}
	// A pinned pattern sets the SDK's explicit-model flag (skips default-role
	// fallback), so pinned-but-unresolvable is exactly `model === undefined`. This
	// early throw bypasses the drain→close finally below, so release the two holders
	// `main` owns (MCP manager, socket); nest so a rejecting disconnect still closes.
	if (modelPattern !== undefined && session.model === undefined) {
		try {
			await mcp.disconnect();
		} catch (disconnectError) {
			console.error(
				"[compass-agent] MCP disconnect failed during fail-closed shutdown:",
				disconnectError,
			);
		} finally {
			transport.close();
		}
		throw new Error(
			`[compass-agent] pinned model "${modelPattern}" did not resolve against the model registry — refusing to boot model-less (would throw "No model configured" on first turn)${modelError !== undefined ? `; config error: ${modelError.message}` : ""}`,
		);
	}

	// Post-construction assignment, not a createAgentSession option: the SDK declares
	// `getApiKey` on Agent but not on CreateAgentSessionOptions, so passing it there
	// is a compile error. LAYER the seed OVER the SDK resolver (captured as fall-
	// through) so a seeded key wins but an auth:none provider resolves the keyless "N/A".
	const sdkGetApiKey = session.agent.getApiKey?.bind(session.agent);
	session.agent.getApiKey = createSeedApiKeyResolver(home, sdkGetApiKey);

	// Construction cycle (RIG-1310 §8): createSocketControlSource needs the control
	// handle at construction, but the handle forwards into CompassAgent, built AFTER.
	// A mutable holder resolves it — the source's pump only dispatches after run()
	// starts consuming, by which point `agent` is assigned, so `agent?.` never sees undefined.
	let agent: CompassAgent | undefined;
	const control = createSocketControlSource(transport, {
		steer: (msg, fromHandle, traceparent, sourceNames) =>
			agent?.steer(msg, fromHandle, traceparent, sourceNames),
		deliver: (msg, fromHandle, traceparent, sourceNames) =>
			agent?.deliver(msg, fromHandle, traceparent, sourceNames),
		forgeNotification: (notification, ackRail) =>
			agent?.forgeNotification(notification, ackRail),
	});

	// Drain in `finally` on both paths, then CLOSE the carrier, in that order: run()
	// emits its terminal status through the sink, which only ENQUEUES lifecycle frames on
	// the send spine, so without this barrier the process can exit uncommitted. The
	// transport holds a 15-min idle HTTP/2 session; closing first abandons those frames.
	try {
		// `traceBridge` (undefined when telemetry is off) flows in as the narrow
		// `TurnTracer` facet — off ⇒ every agent-side trace call no-ops.
		agent = new CompassAgent({ session, sink, control, tracer: traceBridge });
		await agent.run();
	} finally {
		// The load-bearing drain→close chain is UNTOUCHED (storage.drain → sink.drain
		// → transport.close). MCP disconnect wraps it as an OWN outer finally so it
		// runs unconditionally AFTER the frame barrier: MCP is independent of the send
		// spine, so tearing it down last cannot abandon a frame. `main` owns the manager.
		try {
			try {
				// Belt for the APPEND vector: writeTextSync tracks drain so a queued
				// append's tee send is awaited here; the compaction checkpoint does NOT,
				// but the sink drain below covers it. Storage drain precedes sink drain so
				// a late append's emitDurable is in the sink's in-flight set before await.
				await storage.drain();
			} finally {
				try {
					await sink.drain?.();
				} finally {
					transport.close();
				}
			}
		} finally {
			await mcp.disconnect();
		}
	}
}

if (import.meta.main) {
	// Both exit paths are explicit so the drain-then-close barrier in `main` is the
	// last thing to run before exit: without the clean exit(0) the process waits out
	// stragglers, and without the barrier ahead of exit(1) a crash discards frames.
	main().then(
		() => process.exit(0),
		(err: unknown) => {
			console.error("[compass-agent] fatal:", err);
			process.exit(1);
		},
	);
}
