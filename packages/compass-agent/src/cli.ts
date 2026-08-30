// `compass-agent` — the in-container entrypoint the Runner execs.
//
// The Runner starts the agent with a bare `compass-agent` argv and no flags
// (`go/internal/runner/relay.go` `agentCommand`), so this process takes its
// entire configuration from the environment it is launched into:
//
//   - the Runner socket at a FIXED path — `/run/compass/agent.sock`, bind-mounted
//     per container (`internal/runner/host.go:33`), chosen "so the agent needs no
//     per-session configuration" (`host.go:28-29`);
//   - the model selector from `COMPASS_MODEL`;
//   - the block-0 role selector from `COMPASS_ROLE`, naming a
//     `prompts/<role>/SYSTEM.md` in the mount that REPLACES the agent's default
//     block-0 (delivered as `customSystemPrompt`);
//   - the persona identity overlay from `COMPASS_PERSONA`, appended AFTER the
//     agent's default system prompt (or after the role block-0 when both set);
//   - the provider credential from the 0600 `$HOME/.compass/auth-seed.json` the
//     Runner's materializer writes (design §T5);
//   - the materialized tool/MCP secrets from the 0600 `$HOME/.compass/env` the
//     Runner's materializer writes as `KEY=VALUE` lines (RIG-1327 T5), sourced
//     into the process environment before the session is built.
//
// It composes three things and runs them: an `AgentSession` from
// `createAgentSession` (which loads extensions/MCP/skills/tools/the model
// registry/auth), the socket carrier
// (`createSocketFrameSink` / `createSocketControlSource`), and
// `CompassAgent` over both.
//
// Structure follows the repo's construction/execution split: every decision is a
// pure exported function tested in `cli.test.ts`, and `main()` is the thin
// composition that performs IO. `main` is itself tested there over the `MainDeps`
// seam — the two unfakeable constructors (session, socket carrier) are
// injectable, everything between them is the real thing.

import type { Stats } from "node:fs";
import { lstat, mkdir, readlink, rm, symlink } from "node:fs/promises";
import { join } from "node:path";
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
import { CommsBroker, createCommsTools } from "./comms";
import {
	AGENT_CONFIG_MOUNT_PATH,
	currentConfigDir,
	loadMountedConfig,
	type MountedMcp,
	readMountedRolePrompt,
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
 * docs/designs/platform/compass-agent-loop-otel/design.md T1). Mirrors the
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
	 * docs/designs/platform/compass-agent-loop-otel/design.md T1). Defaults to the
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

	// Materialized tool/MCP secrets (RIG-1327 T5): the Runner writes a 0600
	// aggregate KEY=VALUE file inside the container; source it into the process
	// environment so createAgentSession's extensions/MCP/tools — and any
	// subprocess they spawn — inherit the secrets. The merge target is
	// `process.env`, NOT the `env` param (that is only compass-agent's own config
	// reader): createAgentSession reads process.env, so the secrets must land
	// there. File wins for the keys it defines; `HOME` and the whole `COMPASS_*`
	// control-var namespace are never clobbered (filtered parse-side).
	for (const [key, value] of Object.entries(
		await readEnvFile(envFilePath(home)),
	)) {
		process.env[key] = value;
	}

	// Derive LITELLM_MCP_URL from the just-sourced LITELLM_BASE_URL (RIG-2674):
	// Compass's keyring delivers the base URL + API key but not the derived MCP
	// URL, so the fleet mcp.json's `${LITELLM_MCP_URL}` would expand empty and the
	// LiteLLM MCP server would fail to connect. Derive it here — after the env
	// merge, before the MCP connect below — reading from process.env (where the
	// merge landed the base). An explicitly-delivered LITELLM_MCP_URL wins (same
	// file-defines-it posture as the merge); we only fill the gap.
	if (!process.env.LITELLM_MCP_URL) {
		const mcpUrl = deriveLitellmMcpUrl(process.env);
		if (mcpUrl) process.env.LITELLM_MCP_URL = mcpUrl;
	}

	// The identity overlay; undefined when unset or whitespace-only. What omits
	// the overlay in that case is the `persona ?` spread guard below (an absent
	// `systemPrompt` key), not any `||`/`??` subtlety — `resolvePersona` has
	// already normalized a blank value to undefined.
	const persona = resolvePersona(env);

	// The block-0 role selector; undefined when unset or whitespace-only. When
	// set, `main` reads its `prompts/<role>/SYSTEM.md` from the mount (below) and
	// injects it as `customSystemPrompt` — REPLACING OMP's default block-0. The
	// resolve here only yields the LABEL; the file lookup + fallback live below,
	// after the mount is loaded.
	const role = resolveRole(env);

	// The workdir the session is keyed to. `||`, not `??`: an empty or
	// whitespace-only COMPASS_WORKDIR is unset, not a valid cwd. The Runner sets
	// it unconditionally (relay.go `execSpec`), so a caller that builds an
	// AgentEnv with a blank Workdir would otherwise hand bun `cwd: ""` — which
	// does not throw, it silently loads the wrong tree.
	const cwd = env.COMPASS_WORKDIR?.trim() || process.cwd();

	// The socket carrier + sink come FIRST: the tee storage backend teems every
	// committed session write onto the sink's DURABLE lane (RIG-1570), so the
	// sink must exist before the storage that holds it.
	const transport = (deps.createTransport ?? createUnixSocketTransport)(
		AGENT_SOCKET_PATH,
	);
	const sink = createSocketFrameSink(transport);

	// Native comms + lifecycle tools (RIG-1741 gap-1). The existing `transport`
	// is reused directly: `RunnerTransport` structurally satisfies both
	// `CommsTransport` and `LifecycleTransport` (each is a one-method subset —
	// comms.ts:74 / lifecycle.ts), so the brokers wrap it with no adapter. Their
	// tools are merged into `customTools` below, flowing through the same
	// customTools→state.tools→#withNatives natives path as the MCP tools — so the
	// container agent's `comms_post_message` / `agents_spawn_peer` emissions
	// resolve as session natives rather than "unknown tool".
	const commsBroker = new CommsBroker(transport);
	const lifecycleBroker = new LifecycleBroker(transport);
	const forgeBroker = new ForgeBroker(transport);
	// The comms/lifecycle natives are authored as `AgentTool` (pi-agent-core)
	// because CompassAgent's `#withNatives` mechanism (agent.ts) operates on
	// `AgentTool[]`. `createAgentSession`'s `customTools` wants
	// `(CustomTool | ToolDefinition)[]`, and the SDK exposes no dedicated native
	// seam, so we register the `AgentTool[]` through `customTools` with a single
	// documented assertion. The assertion to the `ToolDefinition` arm is
	// TYPE-sound: the only compile-time gap is generic variance on the OPTIONAL
	// renderCall/renderResult (`AgentTool` TTheme=unknown vs `ToolDefinition`
	// Theme/Component) — fields these headless tools never define.
	//
	// RUNTIME mechanism (subtle — do not "simplify" the invariant below away):
	// an `AgentTool` object literal carries no `__isToolDefinition` marker, so
	// the SDK classifies it as a CustomTool (`isCustomTool`, sdk.ts:876) and runs
	// it through `customToolToDefinition` (sdk.ts:915) — NOT the verbatim
	// pass-through arm. That wrapper invokes `execute` with the CustomTool arg
	// convention `(toolCallId, params, onUpdate, ctx, signal)` (sdk.ts:927),
	// whereas `AgentTool.execute` is `(toolCallId, params, signal, onUpdate, ctx)`
	// (pi-agent-core types.ts:612-616) — so args 3-5 arrive SHUFFLED. This is
	// safe ONLY because every native's `execute` body reads solely
	// `(toolCallId, params)` and ignores args 3-5 (comms.ts / lifecycle.ts). A
	// test in cli.test.ts is a TRIPWIRE on the likely regression: it pins each
	// native's `execute.length === 2`, so adding a plain positional 3rd param
	// (`signal`) to consume a shuffled arg reddens it. The pin is not a total
	// guard — a rest (`...args`) or defaulted (`signal = …`) param reads arg 3
	// while keeping `.length === 2` — so the load-bearing rule is this invariant
	// itself, not the arity check. If a native ever needs its AbortSignal or
	// onUpdate (e.g. wiring cancellation), it CANNOT go through this seam — the
	// SDK must gain a real native-registration path, or the tool must be a true
	// `ToolDefinition`. Do not consume args 3-5 here.
	const nativeTools = [
		...createCommsTools(commsBroker),
		...createLifecycleTools(lifecycleBroker),
		...createForgeTools(forgeBroker),
	] as ToolDefinition[];

	// The tee session storage, wrapped + initialize()d (its scan of the session
	// dir must complete before SessionManager.create so synchronous resume
	// lookups see the keyspace). SESSION_DIR is the SDK-default HOME-relative dir
	// for this cwd — checkout-independent (anchored on the agent's scoped $HOME,
	// not a populated repo; sealed#1019 no-auto-clone), mirroring the auth-seed
	// anchoring above.
	const sessionDir = SessionManager.getDefaultSessionDir(cwd);
	// Resume (RIG-1570): T8 exports COMPASS_RESUME_SESSION_FILE on the agent exec.
	// Resolve it BEFORE the storage is built so it can be threaded into the tee
	// backend and indexed at initialize()→loadIndex() — the resume file lives at
	// an absolute path OUTSIDE sessionDir (Option B, T2), so setSessionFile's
	// statSync gate (indexed-session-storage.ts:177) would ENOENT it otherwise.
	const resumeFile = env.COMPASS_RESUME_SESSION_FILE?.trim();
	const { storage } = await (
		deps.createSessionStorage ?? createTeeSessionStorage
	)(sink, sessionDir, resumeFile ? { resumeFile } : undefined);
	// SYNCHRONOUS (session-manager.ts:1839 returns SessionManager, not a Promise):
	// do NOT await. The wrapped IndexedSessionStorage is the 3rd arg.
	const manager = SessionManager.create(cwd, sessionDir, storage);

	// When set, load it through the SDK-native path (setSessionFile → drain →
	// loadEntriesFromFile → migrate → resolveBlobRefs → apply) BEFORE creating
	// the session — reads flow through the tee backend's readFull/loadIndex, no
	// replay code. The reconstructed body is authoritative; the load never tees.
	// The resume file is now also indexed at initialize() (above) so this
	// statSync gate passes for a file outside sessionDir.
	if (resumeFile) await manager.setSessionFile(resumeFile);

	// The Runner-mounted agent-config bundle (design §CD-3): read the mount and
	// map it to the createAgentSession option surfaces below. Unconfigured — no
	// `current` symlink, or the mount absent — yields every field empty, so the
	// session constructs with NONE injected. process.env is already sourced
	// (above), so a connected MCP server inherits its credentials (credential-
	// free configs by MVP rule; the reader resolves none).
	const mounted = await loadMountedConfig(
		deps.configMount ?? AGENT_CONFIG_MOUNT_PATH,
	);
	// The bundle hash, for one observability line. Non load-bearing: absent → no
	// line, and nothing gates on it.
	if (mounted.version) {
		console.error(`[compass-agent] config version: ${mounted.version}`);
	}

	// The role's block-0 prompt (RIG-1732 T10): when a role is set, read its
	// `prompts/<role>/SYSTEM.md` from the same mount and inject it below as
	// `customSystemPrompt` — REPLACING OMP's default block-0. The read is
	// tolerant (absent/empty file → undefined), so a set-but-unshipped role
	// FALLS BACK to today's behavior (no customSystemPrompt) rather than
	// injecting an empty replace. The mount is read through `current/`, the
	// symlink the Runner flips, so a ConfigVersion flip stays live. Persona still
	// appends AFTER this block (record §OQ-8) — see the createSession call.
	const rolePrompt = role
		? await readMountedRolePrompt(
				currentConfigDir(deps.configMount ?? AGENT_CONFIG_MOUNT_PATH),
				role,
			)
		: undefined;

	// Fleet OMP config passthrough (RIG-1678, design compass-agent-config-passthrough
	// §CP-1/CP-2/CP-4), applied AFTER loadMountedConfig and BEFORE
	// createAgentSession. Matt's pivot: the mount stays the delivery vehicle, but
	// the agent CONSUMES it by OBJECT INJECTION wherever the runtime SDK (16.5.2)
	// exposes a `createAgentSession` object seam — the env-var/symlink paths the
	// original design named are inert or absent against this SDK:
	//   - settings (CP-1): built into a `Settings` overlay here, injected as
	//     `settingsManager` below (the SDK reads `configFiles`, NOT PI_CONFIG_FILES).
	//   - rules (CP-4) + AGENTS.md (CP-2): injected as `rules` / `contextFiles`
	//     objects below (from the reader), each short-circuiting SDK discovery.
	// Only the two members with NO object seam stay filesystem-based, symlinked
	// into $HOME/.omp/agent so the SDK's getAgentDir()-anchored load finds them,
	// each pointing through the mount's `current/` so a ConfigVersion flip stays
	// live:
	//   - agents (CP-4): subagent defs, discovered by walking the agent dir.
	//   - models.yml (CP-4): loaded by the ModelRegistry (object seam is a gap).
	// All real-FS over the injectable mount, so MainDeps needs no new member.
	const fleetSettings = await buildFleetSettings(
		cwd,
		agentDirPath(home),
		mounted.settingsPath,
	);
	await Promise.all([
		ensureAgentDirLink(home, "agents", mounted.agentsDir),
		ensureAgentDirLink(home, "models.yml", mounted.modelsPath),
	]);
	// Connect the mount's MCP servers now, before construction, so their tools
	// reach createAgentSession as customTools. `main` OWNS the manager — the SDK
	// never disconnects a manager it did not build — so its `disconnect` is added
	// to the teardown finally below.
	const mcp = await (deps.connectMcp ?? connectMountedMcp)(cwd, mounted.mcp);

	// Fleet AGENTS.md compose (CP-2, Matt-decided): the fleet AGENTS.md is a
	// GLOBAL/user-level working-conventions file, so it must COMPOSE with — never
	// REPLACE — the checkout's own project-level AGENTS.md chain. Providing
	// `contextFiles` short-circuits the SDK's discovery entirely (sdk.ts:1177-1179
	// → discoverContextFiles skipped), and that discovery is PROJECT scope only
	// (the cwd walk-up, sdk.ts:136 loadProjectContextFiles alias). So when the
	// fleet file is present we run that same discovery OURSELVES and prepend the
	// fleet global: it goes FIRST (least prominent — discoverContextFiles sorts
	// farther-from-cwd first so closer files stay last/more-prominent, and a
	// user-level global is less prominent than any project file). When ABSENT we
	// OMIT the key so the SDK runs its own discovery and project files load
	// automatically — identical effect, simpler.
	const contextFiles = mounted.agentsMd
		? [
				mounted.agentsMd,
				...(await discoverContextFiles(cwd, agentDirPath(home))),
			]
		: undefined;

	// Fleet rules compose (CP-4, Matt-decided): the fleet rules/ is a GLOBAL/user-level
	// set that must COMPOSE with — never REPLACE — the checkout's own discovered rules.
	// Providing `rules` short-circuits the SDK's rule discovery entirely
	// (sdk.ts:1434-1436), so we run that discovery OURSELVES and prepend the fleet
	// rules: they go FIRST (least prominent), the checkout's discovered rules follow.
	const discoveredRules = (
		await loadCapability<Rule>(ruleCapability.id, { cwd })
	).items;
	const rules = [...mounted.rules, ...discoveredRules];

	// Loop OpenTelemetry activation (design
	// docs/designs/platform/compass-agent-loop-otel/design.md T1). Gated HARD on
	// an OTLP endpoint being configured: with none set this block is skipped
	// whole, so process.env is UNMUTATED and the createAgentSession path below is
	// bit-identical to a no-telemetry build (Global Constraints, "Off by default";
	// F2 — an unconditional env write would leak to every tool subprocess). The
	// order is load-bearing: the loop provider reads OTEL_SERVICE_NAME /
	// OTEL_RESOURCE_ATTRIBUTES at registration time, so the env defaults must be
	// in place BEFORE init(). The session id the manager owns (fresh-minted, or
	// the resumed header's id after setSessionFile above) is the shared
	// cross-signal join key (Decision 3a): APPENDED to OTEL_RESOURCE_ATTRIBUTES,
	// never clobbering a deployer-set value.
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

	// Trace-continuity bridge (design
	// docs/designs/platform/compass-agent-message-trace-continuity/design.md §T2):
	// built ONLY on the same enabled path that registered the loop provider +
	// context manager above — the parentage the bridge installs presupposes that
	// registration. When telemetry is off, `undefined` flows to both the session
	// telemetry option and the agent's `tracer` dep, so no hook is installed and
	// every agent-side trace call no-ops (bit-identical off). The full
	// `TraceBridge` (with the `Span`-typed hook members) lives here in the cli.ts
	// composition; CompassAgent receives only its narrow `TurnTracer` facet.
	const traceBridge: TraceBridge | undefined = telemetryHooks.isEnabled()
		? createTraceBridge()
		: undefined;

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
		// The Runner-mounted agent-config (design §CD-3). Each field is passed
		// UNCONDITIONALLY, empty when unconfigured, so "unconfigured → none" is a
		// guarantee, not an accident of discovery:
		//   - `skills` provided (even `[]`) SKIPS discovery entirely
		//     (sdk.ts:1417) — no ambient skills leak in.
		//   - `additionalExtensionPaths` are concrete entry FILES the reader
		//     enumerated; `disableExtensionDiscovery: true` passes them verbatim
		//     (sdk.ts:695) and runs no native/plugin/cwd discovery.
		//   - `customTools` are the connected MCP tools; `enableMCP: false` stops
		//     the SDK ALSO discovering a cwd `.mcp.json`. A provided `mcpManager`
		//     would NOT surface its tools (sdk.ts:1739/1818), so we pass tools.
		// Scope: the guarantee covers the mount's three surfaces (skills,
		// extensions, MCP). It does NOT suppress cwd custom-TOOL discovery —
		// sdk.ts:1861 runs discoverCustomToolPaths([], cwd) unconditionally, so a
		// `.omp/tools/` tree in the workdir still loads. Orthogonal to this
		// mount contract; noted so nobody over-reads it as "zero ambient tools".
		skills: mounted.skills,
		additionalExtensionPaths: mounted.additionalExtensionPaths,
		disableExtensionDiscovery: mounted.disableExtensionDiscovery,
		// The connected MCP tools MERGED with the native comms/lifecycle tools
		// (RIG-1741 gap-1, constructed above): all reach the session as natives via
		// the same customTools→state.tools→#withNatives path, so the container
		// agent can spawn peers and post to channels.
		customTools: [...mcp.tools, ...nativeTools],
		enableMCP: false,
		// Headless approval policy (RIG-1741, design compass-agent-comms-tools
		// §"the container runs headless with write-approval tools auto-executing"):
		// the container has NO human to answer an approval prompt, and the native
		// comms/lifecycle tools declare approval:"write" — so without auto-approve
		// a write-approval tool would block forever and never execute. Pin the
		// yolo-default policy here in the entrypoint. Unconditional by design: the
		// safety rests on an EXTERNAL invariant — this bin is exec'd only by the
		// Runner as the in-container headless entrypoint (`if (import.meta.main)`,
		// the sole createAgentSession call in the package), never interactively. If
		// that ever changes, gate this on an explicit headless signal so the
		// auto-approve posture fails safe outside a container.
		autoApprove: true,
		// Fleet config object injection (RIG-1678 pivot):
		//   - `rules` (CP-4): the fleet rules COMPOSED with the checkout's
		//     discovered rules (both load; fleet-first), computed above. Passed
		//     unconditionally — empty fleet set still composes cleanly.
		//   - `contextFiles` (CP-2): [fleet global AGENTS.md, ...project-discovered],
		//     computed above — passed ONLY when the fleet file is present (it
		//     COMPOSES with, never replaces, the checkout's project AGENTS.md).
		//     When absent, `contextFiles` is undefined so the key is omitted and
		//     the SDK runs its own project discovery.
		//   - `settingsManager` (CP-1): the prebuilt fleet Settings overlay, ONLY
		//     when built — omitted lets the SDK init its own default Settings.
		rules,
		...(contextFiles ? { contextFiles } : {}),
		...(fleetSettings ? { settingsManager: fleetSettings } : {}),
		// Role (RIG-1732 T10) + persona compose INDEPENDENTLY, and BOTH apply:
		//   - `customSystemPrompt` (role): the role's block-0 text, routed through
		//     the SDK's custom-system-prompt template (sdk.ts:2727) — REPLACES
		//     OMP's default block-0 while the template STILL injects skills + rules
		//     and the project footer stays a separate block (record §MP-1). Passed
		//     ONLY when a role prompt was found; absent → key omitted → today's
		//     default block-0 (no empty replace).
		//   - `systemPrompt` (persona): the identity OVERLAY, APPENDED after the
		//     built default array (record §OQ-8: persona appends AFTER the role
		//     block). The callback transforms whatever `defaultPrompt` the SDK
		//     built — with a role, that array already carries the role block-0 +
		//     skills/rules/footer, so persona lands LAST, after the role block.
		//     Passed ONLY when a persona is set.
		// The two are orthogonal keys, so all four states compose: neither, role
		// only (replace block-0), persona only (append, today's behavior), both
		// (role replaces block-0, persona appends after).
		...(rolePrompt ? { customSystemPrompt: rolePrompt } : {}),
		...(persona
			? {
					systemPrompt: (defaultPrompt: string[]) => [
						...defaultPrompt,
						persona,
					],
				}
			: {}),
		// Loop telemetry (design docs/designs/platform/compass-agent-loop-otel/
		// design.md T1) + trace continuity (message-trace-continuity §T2): the
		// enabled path installs the bridge's capture hooks INTO the telemetry
		// config, so the bridge sees each `invoke_agent` span start/end and can
		// parent/link injected messages onto the turn. Keyed off the same
		// authoritative post-registration check (`traceBridge` is defined iff
		// enabled), so the key is OMITTED entirely when export is off and the loop
		// keeps its literal-undefined zero-lookup path.
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
	// error — a rejected provider config leaves the registry with a recorded
	// error and the session falls back to built-in-only model resolution, booting
	// model-less when the pinned selector can no longer resolve. The cost surfaces
	// far from the cause: the first prompt throws "No model configured" deep in a
	// turn, with nothing pointing back at the bad config. So surface it here, at
	// boot: log the config error loudly whenever one is present, and refuse to
	// boot when an operator PINNED a model that did not resolve (rather than limp
	// on to the deferred first-turn crash).
	const modelRegistry = session.modelRegistry;
	const modelError = modelRegistry.getError();
	if (modelError !== undefined) {
		console.error(
			`[compass-agent] models.yml config rejected (${modelError.id}): ${modelError.message} — falling back to built-in model resolution`,
		);
	}
	// A pinned pattern sets the SDK's explicit-model flag, which SKIPS the
	// default-role fallback — so pinned-but-unresolvable is exactly `model ===
	// undefined` here, while an unpinned boot gets the SDK default and is left
	// alone. This early throw bypasses the drain→close→disconnect finally below,
	// so release the two holders `main` owns (the connected MCP manager, then the
	// socket) here. Nest them so a rejecting disconnect still closes the socket,
	// and log — never rethrow — that rejection, so it cannot mask the actionable
	// diagnostic this belt exists to surface.
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

	// Post-construction assignment, not a `createAgentSession` option: the SDK
	// declares `getApiKey` as a public mutable field on `Agent` (`agent.d.ts:209`)
	// and does NOT declare it on `CreateAgentSessionOptions` — its docstring
	// example (`sdk.d.ts:368`) advertises the option, but the type does not carry
	// it, so passing it there is a compile error. Assigning the field is the
	// type-safe path to the same per-call resolution semantics.
	//
	// LAYER the seed OVER the SDK resolver rather than replacing it: capture the
	// resolver `createAgentSession` installed (`modelRegistry.resolver`) and pass
	// it as the fall-through. A seeded provider's key still wins (T6 rotation),
	// but a provider absent from the seed — notably an `auth: none` keyless
	// provider — resolves through the SDK path, which yields the keyless `"N/A"`
	// sentinel so the turn dials instead of failing `MissingApiKeyError`.
	const sdkGetApiKey = session.agent.getApiKey?.bind(session.agent);
	session.agent.getApiKey = createSeedApiKeyResolver(home, sdkGetApiKey);

	// Construction cycle (RIG-1310 §8): createSocketControlSource needs the
	// ImmediateControl handle at construction, but the handle must forward into
	// the CompassAgent — which is constructed AFTER (it takes `control` as a ctor
	// arg). A mutable holder resolves it: the handle closes over `agent` and the
	// source's pump only dispatches AFTER `run()` starts consuming, by which point
	// `agent` is assigned — so the `agent?.` guard never actually sees undefined.
	let agent: CompassAgent | undefined;
	const control = createSocketControlSource(transport, {
		steer: (msg, fromHandle, sourceNames) =>
			agent?.steer(msg, fromHandle, "", sourceNames),
		deliver: (msg, fromHandle, sourceNames) =>
			agent?.deliver(msg, fromHandle, "", sourceNames),
		forgeNotification: (notification, ackRail) =>
			agent?.forgeNotification(notification, ackRail),
	});

	// Drain in `finally`, on both the clean and error paths. `run()` emits its
	// terminal status through the sink on its way out, and the socket sink only
	// ENQUEUES lifecycle frames onto the send spine's priority lane — so without
	// this barrier the process can exit with that terminal frame, and any
	// in-flight conversation unary, still uncommitted. `drain()` is what the sink
	// contract (frame.ts:52-58) provides for exactly this, and this composition
	// root is the only place holding the sink to call it.
	//
	// Then CLOSE the carrier, in that order: the transport holds a live HTTP/2
	// session over the Runner socket whose manager keeps an idle connection for
	// 15 minutes, so a clean run that only drained would leave the process
	// lingering on the socket. Closing first would abandon the very frames the
	// drain exists to commit, so drain strictly precedes close.
	//
	// The close is nested in its OWN `finally` so it is unconditional. Neither
	// drain implementation can reject today (frame-sink.ts awaits sends whose
	// producer catches to exhaustion; publish-spine's pump catches every batch
	// error) — but that is an invariant of two other files, and if either ever
	// broke it a skipped `close()` would leak the session, which is the exact
	// defect `close()` was added to fix.
	try {
		// `traceBridge` (undefined when telemetry is off) flows in as the narrow
		// `TurnTracer` facet — off ⇒ every agent-side trace call no-ops.
		agent = new CompassAgent({ session, sink, control, tracer: traceBridge });
		await agent.run();
	} finally {
		// The load-bearing drain→close chain is UNTOUCHED — storage.drain →
		// sink.drain → transport.close, in that exact order (see above). The MCP
		// disconnect wraps it as its OWN outer finally so it runs unconditionally
		// (clean and error paths) AFTER the frame barrier, without reordering it:
		// the MCP connections are independent of the send spine, so tearing them
		// down last cannot abandon a frame the drain exists to commit. `main` owns
		// the manager (the SDK never disconnects one it did not build), so without
		// this the container would leak every MCP subprocess/HTTP session on exit.
		try {
			try {
				// Belt for the APPEND vector: `writeTextSync` tracks drain
				// (indexed-session-storage.ts:143 trackDrain:true) so a queued append's
				// tee send is awaited here; the compaction `writeTextAtomic` checkpoint
				// vector does NOT track drain (:270), but the sink drain below covers
				// its durable send. Storage drain precedes sink drain so a late append's
				// emitDurable is in the sink's in-flight set before it is awaited.
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
	// Both exit paths are explicit so the drain-then-close barrier in `main` is
	// the last thing that runs before the process goes away: without the clean
	// `exit(0)` the process would wait out any straggling handle, and without
	// the barrier ahead of `exit(1)` a crash would discard uncommitted frames.
	main().then(
		() => process.exit(0),
		(err: unknown) => {
			console.error("[compass-agent] fatal:", err);
			process.exit(1);
		},
	);
}
