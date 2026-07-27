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
//   - the provider credential from the 0600 `$HOME/.compass/auth-seed.json` the
//     Runner's materializer writes (design §T5).
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
} from "@oh-my-pi/pi-coding-agent";
import { CompassAgent } from "./agent";
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

	const { session } = await (deps.createSession ?? createAgentSession)({
		// `||`, not `??`: an empty COMPASS_WORKDIR is unset, not a valid cwd. The
		// Runner sets it unconditionally (relay.go `execSpec`), so a caller that
		// builds an AgentEnv with a blank Workdir would otherwise hand bun
		// `cwd: ""` — which does not throw, it silently loads the wrong tree.
		cwd: env.COMPASS_WORKDIR || process.cwd(),
		modelPattern: resolveModelSelector(env),
	});

	// Post-construction assignment, not a `createAgentSession` option: the SDK
	// declares `getApiKey` as a public mutable field on `Agent` (`agent.d.ts:209`)
	// and does NOT declare it on `CreateAgentSessionOptions` — its docstring
	// example (`sdk.d.ts:368`) advertises the option, but the type does not carry
	// it, so passing it there is a compile error. Assigning the field is the
	// type-safe path to the same per-call resolution semantics.
	session.agent.getApiKey = createSeedApiKeyResolver(home);

	const transport = (deps.createTransport ?? createUnixSocketTransport)(
		AGENT_SOCKET_PATH,
	);
	const sink = createSocketFrameSink(transport);
	const control = createSocketControlSource(transport, {
		steer: (msg) => session.agent.steer(msg),
		deliver: (msg) => session.agent.appendMessage(msg),
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
		await new CompassAgent({ session, sink, control }).run();
	} finally {
		try {
			await sink.drain?.();
		} finally {
			transport.close();
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
