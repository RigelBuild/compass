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
//   - the persona identity overlay from `COMPASS_PERSONA`, appended to the
//     agent's default system prompt;
//   - the provider credential from the 0600 `$HOME/.compass/auth-seed.json` the
//     Runner's materializer writes (design §T5);
//   - the materialized tool/MCP secrets from the 0600 `$HOME/.compass/env` the
//     Runner's materializer writes as `KEY=VALUE` lines (SEA-1327 T5), sourced
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

import type { Model } from "@oh-my-pi/pi-ai";
import {
	type AgentSession,
	type CreateAgentSessionOptions,
	createAgentSession,
	type IndexedSessionStorage,
	SessionManager,
} from "@oh-my-pi/pi-coding-agent";
import { CompassAgent } from "./agent";
import type { FrameSink } from "./frame";
import {
	createTeeSessionStorage,
	type TranscriptTeeBackend,
} from "./session-tee";
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

/** The 0600 aggregate env-secret file the Runner materializes (SEA-1327 T5). */
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
 * A `getApiKey` resolver backed by the on-disk seed.
 *
 * Re-reads the seed on EVERY call, which is the load-bearing behavior: the SDK
 * invokes `getApiKey` per LLM call precisely so an expiring or rotated
 * credential is picked up without a restart (`agent.d.ts:66-70`), and rotation
 * (design §T6) rewrites this file in place. A value cached at construction would
 * silently pin the container to a stale key until it was torn down.
 *
 * Every failure path returns `undefined` rather than throwing: a missing,
 * unreadable, malformed, or provider-less seed must leave the agent running and
 * able to report, letting the SDK surface a clean auth error on the call that
 * needed the key. A container that crashes at boot because its credential has
 * not been materialized yet is strictly worse — provisioning writes the seed
 * after the container is up.
 */
export function createSeedApiKeyResolver(
	home: string,
): (model: Model) => Promise<string | undefined> {
	const path = authSeedPath(home);
	return async (model: Model): Promise<string | undefined> => {
		const seed = await readSeed(path);
		const key = seed?.entries?.[model.provider]?.key;
		return typeof key === "string" ? key : undefined;
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
	 * Tee-storage constructor (SEA-1570). Defaults to `createTeeSessionStorage`.
	 * A seam for the same reason as the other two: the real one wraps the SDK's
	 * `IndexedSessionStorage` over a filesystem backend and awaits `initialize()`
	 * off disk, so a test composes `main` over a recording storage instead.
	 */
	createSessionStorage?: (
		sink: FrameSink,
		sessionDir: string,
	) => Promise<{
		storage: IndexedSessionStorage;
		backend: TranscriptTeeBackend;
	}>;
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

	// Materialized tool/MCP secrets (SEA-1327 T5): the Runner writes a 0600
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

	// The identity overlay; undefined when unset or whitespace-only. What omits
	// the overlay in that case is the `persona ?` spread guard below (an absent
	// `systemPrompt` key), not any `||`/`??` subtlety — `resolvePersona` has
	// already normalized a blank value to undefined.
	const persona = resolvePersona(env);

	// The workdir the session is keyed to. `||`, not `??`: an empty or
	// whitespace-only COMPASS_WORKDIR is unset, not a valid cwd. The Runner sets
	// it unconditionally (relay.go `execSpec`), so a caller that builds an
	// AgentEnv with a blank Workdir would otherwise hand bun `cwd: ""` — which
	// does not throw, it silently loads the wrong tree.
	const cwd = env.COMPASS_WORKDIR?.trim() || process.cwd();

	// The socket carrier + sink come FIRST: the tee storage backend teems every
	// committed session write onto the sink's DURABLE lane (SEA-1570), so the
	// sink must exist before the storage that holds it.
	const transport = (deps.createTransport ?? createUnixSocketTransport)(
		AGENT_SOCKET_PATH,
	);
	const sink = createSocketFrameSink(transport);

	// The tee session storage, wrapped + initialize()d (its scan of the session
	// dir must complete before SessionManager.create so synchronous resume
	// lookups see the keyspace). SESSION_DIR is the SDK-default HOME-relative dir
	// for this cwd — checkout-independent (anchored on the agent's scoped $HOME,
	// not a populated repo; sealed#1019 no-auto-clone), mirroring the auth-seed
	// anchoring above.
	const sessionDir = SessionManager.getDefaultSessionDir(cwd);
	const { storage } = await (
		deps.createSessionStorage ?? createTeeSessionStorage
	)(sink, sessionDir);
	// SYNCHRONOUS (session-manager.ts:1839 returns SessionManager, not a Promise):
	// do NOT await. The wrapped IndexedSessionStorage is the 3rd arg.
	const manager = SessionManager.create(cwd, sessionDir, storage);

	// Resume (SEA-1570): T8 exports COMPASS_RESUME_SESSION_FILE on the agent exec.
	// When set, load it through the SDK-native path (setSessionFile → drain →
	// loadEntriesFromFile → migrate → resolveBlobRefs → apply) BEFORE creating
	// the session — reads flow through the tee backend's readFull/loadIndex, no
	// replay code. The reconstructed body is authoritative; the load never tees.
	const resumeFile = env.COMPASS_RESUME_SESSION_FILE?.trim();
	if (resumeFile) await manager.setSessionFile(resumeFile);

	const { session } = await (deps.createSession ?? createAgentSession)({
		cwd,
		modelPattern: resolveModelSelector(env),
		// The tee-backed manager, so every session write teems upstream and the
		// resumed history (if any) is already loaded.
		sessionManager: manager,
		// Persona is an identity OVERLAY, not a replacement: append it after the
		// default prompt so block-0 base instructions + project footer survive.
		...(persona
			? {
					systemPrompt: (defaultPrompt: string[]) => [
						...defaultPrompt,
						persona,
					],
				}
			: {}),
	});

	// Post-construction assignment, not a `createAgentSession` option: the SDK
	// declares `getApiKey` as a public mutable field on `Agent` (`agent.d.ts:209`)
	// and does NOT declare it on `CreateAgentSessionOptions` — its docstring
	// example (`sdk.d.ts:368`) advertises the option, but the type does not carry
	// it, so passing it there is a compile error. Assigning the field is the
	// type-safe path to the same per-call resolution semantics.
	session.agent.getApiKey = createSeedApiKeyResolver(home);

	// Construction cycle (SEA-1310 §8): createSocketControlSource needs the
	// ImmediateControl handle at construction, but the handle must forward into
	// the CompassAgent — which is constructed AFTER (it takes `control` as a ctor
	// arg). A mutable holder resolves it: the handle closes over `agent` and the
	// source's pump only dispatches AFTER `run()` starts consuming, by which point
	// `agent` is assigned — so the `agent?.` guard never actually sees undefined.
	let agent: CompassAgent | undefined;
	const control = createSocketControlSource(transport, {
		steer: (msg) => agent?.steer(msg),
		deliver: (msg) => agent?.deliver(msg),
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
		agent = new CompassAgent({ session, sink, control });
		await agent.run();
	} finally {
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
