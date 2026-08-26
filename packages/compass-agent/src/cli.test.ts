// The `compass-agent` entrypoint — both halves.
//
// The Runner execs a bare `compass-agent` argv (no flags —
// `go/internal/runner/relay.go` `agentCommand`), so every input reaches the
// process through the environment or a well-known file. Two surfaces:
//
//   - CONSTRUCTION: the pure resolution functions (`AGENT_SOCKET_PATH`,
//     `resolveModelSelector`, `authSeedPath`, `createSeedApiKeyResolver`) —
//     each exercised directly, against a tempfile seed.
//   - COMPOSITION: `main` itself, over the `MainDeps` seam (cli.ts `MainDeps`) — a fake
//     session and a fake `RunnerTransport` stand in for the two unfakeable
//     constructors, and everything between them (the real socket FrameSink, the
//     real ControlSource, the real PublishSpine, the real CompassAgent run loop)
//     is the production code. What `main` uniquely owns and nothing below it can
//     defend is the TEARDOWN BARRIER: `finally { await sink.drain?.();
//     transport.close() }`, which the teardown tests here pin against a captured
//     wire log and a recorded ordering.
//
// Nothing here touches a socket, a real model, or a real credential: the carrier
// is injected (as `agent.test.ts` already does) and the seed is a tempfile. No
// timers, no sleeps — the composition tests gate on events (a deferred resolved
// from the fake carrier's own RPC handlers).

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Model } from "@oh-my-pi/pi-ai";
import type {
	AgentSession,
	AgentSessionEventListener,
} from "@oh-my-pi/pi-coding-agent";
import { SessionManager } from "@oh-my-pi/pi-coding-agent";
import { serializeTitleSlot } from "@oh-my-pi/pi-coding-agent/session/session-title-slot";
import { buildSystemPrompt } from "@oh-my-pi/pi-coding-agent/system-prompt";
import {
	AGENT_SOCKET_PATH,
	authSeedPath,
	createSeedApiKeyResolver,
	deriveLitellmMcpUrl,
	envFilePath,
	isTelemetryEndpointConfigured,
	type MainDeps,
	main,
	parseEnvFile,
	resolveModelSelector,
	resolvePersona,
	resolveRole,
} from "./cli";
import { CommsBroker, createCommsTools } from "./comms";
import {
	AgentControlSchema,
	AgentSessionState,
	create,
	PromptControlSchema,
	type AgentControl as WireAgentControl,
} from "./compassv1";
import { createForgeTools, ForgeBroker } from "./forge";
import {
	type PostConversationFrameRequest,
	PostConversationFrameResponseSchema,
	type PublishFrameRequest,
} from "./gen/compass/v1/agent_gateway_pb";
import { createLifecycleTools, LifecycleBroker } from "./lifecycle";
import { createTeeSessionStorage } from "./session-tee";
import type { RunnerTransport } from "./transport/index";
import { createPublishSpine } from "./transport/publish-spine";

const tmpdirs: string[] = [];

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-agent-cli-"));
	tmpdirs.push(dir);
	return dir;
}

// `main` computes its session dir via SessionManager.getDefaultSessionDir(cwd),
// which anchors on the global HOME (os.homedir() → process.env.HOME). Pin it to
// a per-test scratch dir so the tee storage's mkdir + the manager's writes land
// under a throwaway tree, never the developer's real ~/.omp session store. (The
// `env` arg main receives controls the auth-seed lookup, NOT the session dir —
// that reads the process HOME, which is what this pins.)
let savedHome: string | undefined;
beforeEach(() => {
	savedHome = process.env.HOME;
	process.env.HOME = scratch();
});

afterEach(() => {
	if (savedHome === undefined) delete process.env.HOME;
	else process.env.HOME = savedHome;
	for (const dir of tmpdirs.splice(0)) {
		rmSync(dir, { recursive: true, force: true });
	}
});

// The socket path is a CONTRACT with the Runner, not a preference: host.go:33
// bind-mounts the per-container socket at this fixed path precisely "so the
// agent needs no per-session configuration — it always dials the same path"
// (host.go:28-29). A drift here is a launch failure with no error until the
// dial times out, so it is pinned by a test.
describe("AGENT_SOCKET_PATH", () => {
	test("matches the Runner's fixed in-container mount path", () => {
		expect(AGENT_SOCKET_PATH).toBe("/run/compass/agent.sock");
	});
});

// COMPASS_MODEL is the Matt-ruled runtime seam for model selection. The
// entrypoint does not parse it into a Model — the SDK's model registry owns
// that — it forwards it as `modelPattern` to createAgentSession. Absent, the
// session falls back to the SDK's own default, which is a legitimate
// configuration, not an error: an operator who pins nothing gets the SDK
// default rather than a container that refuses to boot.
describe("resolveModelSelector", () => {
	test("returns the COMPASS_MODEL value when set", () => {
		expect(
			resolveModelSelector({ COMPASS_MODEL: "anthropic/claude-opus" }),
		).toBe("anthropic/claude-opus");
	});

	test("returns undefined when COMPASS_MODEL is unset (SDK default applies)", () => {
		expect(resolveModelSelector({})).toBeUndefined();
	});

	test("treats an empty or whitespace-only value as unset", () => {
		expect(resolveModelSelector({ COMPASS_MODEL: "" })).toBeUndefined();
		expect(resolveModelSelector({ COMPASS_MODEL: "   " })).toBeUndefined();
	});

	test("trims surrounding whitespace so a padded env value still resolves", () => {
		expect(resolveModelSelector({ COMPASS_MODEL: "  openai/gpt-5  " })).toBe(
			"openai/gpt-5",
		);
	});
});

// COMPASS_PERSONA is the server-authoritative identity overlay. The entrypoint
// forwards it verbatim (trimmed) as an append customizer; unset or blank leaves
// the agent on its default prompt. Same unset/trim semantics as the model
// selector, extracted as a pure function so `main` composes tested decisions.
describe("resolvePersona", () => {
	test("returns the COMPASS_PERSONA value when set", () => {
		expect(resolvePersona({ COMPASS_PERSONA: "You are Ada." })).toBe(
			"You are Ada.",
		);
	});

	test("returns undefined when COMPASS_PERSONA is unset (default prompt applies)", () => {
		expect(resolvePersona({})).toBeUndefined();
	});

	test("treats an empty or whitespace-only value as unset", () => {
		expect(resolvePersona({ COMPASS_PERSONA: "" })).toBeUndefined();
		expect(resolvePersona({ COMPASS_PERSONA: "   " })).toBeUndefined();
	});

	test("trims surrounding whitespace so a padded env value still resolves", () => {
		expect(resolvePersona({ COMPASS_PERSONA: "  You are Ada.  " })).toBe(
			"You are Ada.",
		);
	});
});

// COMPASS_ROLE is the server-authoritative block-0 role selector (SEA-1732 T10).
// The entrypoint resolves the LABEL here (trimmed; blank → unset), then reads its
// `prompts/<role>/SYSTEM.md` from the mount and injects it as customSystemPrompt.
// Same unset/trim semantics as the model selector and persona.
describe("resolveRole", () => {
	test("returns the COMPASS_ROLE value when set", () => {
		expect(resolveRole({ COMPASS_ROLE: "manager" })).toBe("manager");
	});

	test("returns undefined when COMPASS_ROLE is unset (default block-0 applies)", () => {
		expect(resolveRole({})).toBeUndefined();
	});

	test("treats an empty or whitespace-only value as unset", () => {
		expect(resolveRole({ COMPASS_ROLE: "" })).toBeUndefined();
		expect(resolveRole({ COMPASS_ROLE: "   " })).toBeUndefined();
	});

	test("trims surrounding whitespace so a padded env value still resolves", () => {
		expect(resolveRole({ COMPASS_ROLE: "  supervisor  " })).toBe("supervisor");
	});
});

// The LiteLLM MCP URL is DERIVED from LITELLM_BASE_URL (RIG-2674), matching the
// wave's SOPS loader (secrets-env.nix): strip a trailing slash, strip a trailing
// /v1, append /mcp/. The trailing slash is load-bearing (LiteLLM 307-redirects
// /mcp and MCP clients don't re-POST across the redirect).
describe("deriveLitellmMcpUrl", () => {
	test("derives /mcp/ from a /v1 base URL", () => {
		expect(
			deriveLitellmMcpUrl({ LITELLM_BASE_URL: "https://llm.example/v1" }),
		).toBe("https://llm.example/mcp/");
	});

	test("strips a trailing slash before stripping /v1", () => {
		// Non-vacuity: without the trailing-slash strip the /v1$ match misses and
		// the result would be `…/v1//mcp/` — a broken double path.
		expect(
			deriveLitellmMcpUrl({ LITELLM_BASE_URL: "https://llm.example/v1/" }),
		).toBe("https://llm.example/mcp/");
	});

	test("appends /mcp/ to a base with no /v1 suffix", () => {
		expect(
			deriveLitellmMcpUrl({ LITELLM_BASE_URL: "https://llm.example" }),
		).toBe("https://llm.example/mcp/");
	});

	test("keeps the load-bearing trailing slash", () => {
		// The whole point of the derivation: /mcp (no slash) 307-redirects and the
		// MCP client drops the POST body. The result MUST end in a slash.
		const url = deriveLitellmMcpUrl({
			LITELLM_BASE_URL: "https://llm.example/v1",
		});
		expect(url?.endsWith("/mcp/")).toBe(true);
	});

	test("returns undefined when LITELLM_BASE_URL is unset (no gateway configured)", () => {
		expect(deriveLitellmMcpUrl({})).toBeUndefined();
	});

	test("treats an empty or whitespace-only base as unset", () => {
		expect(deriveLitellmMcpUrl({ LITELLM_BASE_URL: "" })).toBeUndefined();
		expect(deriveLitellmMcpUrl({ LITELLM_BASE_URL: "   " })).toBeUndefined();
	});

	test("trims surrounding whitespace so a padded base still derives", () => {
		expect(
			deriveLitellmMcpUrl({ LITELLM_BASE_URL: "  https://llm.example/v1  " }),
		).toBe("https://llm.example/mcp/");
	});
});

// The seed path is the frozen T5 placement: a 0600 `$HOME/.compass/auth-seed.json`
// written by the Runner's materializer.
describe("authSeedPath", () => {
	test("resolves under the supplied HOME", () => {
		expect(authSeedPath("/home/agent")).toBe(
			"/home/agent/.compass/auth-seed.json",
		);
	});
});

// The env-file path is the frozen SEA-1327 T5 placement: a 0600
// `$HOME/.compass/env` written by the Runner's materializer, beside the seed.
describe("envFilePath", () => {
	test("resolves under the supplied HOME", () => {
		expect(envFilePath("/home/agent")).toBe("/home/agent/.compass/env");
	});
});

// The pure parser of the materialized `KEY=VALUE` file. Split on the FIRST `=`
// (values may contain `=`), value literal to EOL (only a trailing \r stripped),
// tolerant of blank/`=`-less/empty-key lines, and reserved keys (`HOME` + the
// whole `COMPASS_*` namespace) excluded so a file KEY can never clobber a
// Runner-set var. No IO, no process.env.
describe("parseEnvFile", () => {
	test("parses basic KEY=VALUE lines", () => {
		expect(parseEnvFile("A=1\nB=2")).toEqual({ A: "1", B: "2" });
	});

	test("splits on the FIRST `=` so a value may contain `=`", () => {
		expect(parseEnvFile("TOKEN=ab=cd=ef")).toEqual({ TOKEN: "ab=cd=ef" });
	});

	test("an empty file yields no entries", () => {
		expect(parseEnvFile("")).toEqual({});
	});

	test("blank lines are skipped", () => {
		expect(parseEnvFile("\nA=1\n\n")).toEqual({ A: "1" });
	});

	test("a line with no `=` is skipped", () => {
		expect(parseEnvFile("NOEQ\nA=1")).toEqual({ A: "1" });
	});

	test("a line with an empty key is skipped", () => {
		expect(parseEnvFile("=orphan\nA=1")).toEqual({ A: "1" });
	});

	test("the value is literal to EOL — never trimmed", () => {
		expect(parseEnvFile("A= spaced ")).toEqual({ A: " spaced " });
	});

	test("a CRLF-written file is tolerated (trailing \\r stripped)", () => {
		expect(parseEnvFile("A=1\r\nB=2\r\n")).toEqual({ A: "1", B: "2" });
	});

	test("HOME and the whole COMPASS_* namespace are excluded", () => {
		expect(
			parseEnvFile(
				// The ratified four, plus a COMPASS_ control var the four-key list
				// predated (COMPASS_RESUME_SESSION_FILE, SEA-1570 T8): the prefix
				// rule reserves it too, so a file can never hijack the resume path.
				"HOME=/evil\nCOMPASS_MODEL=x\nCOMPASS_WORKDIR=y\nCOMPASS_PERSONA=z\nCOMPASS_RESUME_SESSION_FILE=/evil\nCOMPASS_FUTURE_VAR=nope\nOK=1",
			),
		).toEqual({ OK: "1" });
	});
});

// getApiKey is called PER LLM CALL (agent.d.ts:66-70: "Resolves an API key ...
// dynamically for each LLM call. Useful for expiring tokens"). That semantic is
// the whole reason rotation works without a restart: T6 rewrites the seed file
// in place and the next call must pick it up. So the resolver re-reads the seed
// rather than closing over a value read at boot — these tests pin that.
describe("createSeedApiKeyResolver", () => {
	test("resolves the key for the model's provider", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: {
				anthropic: { type: "api-key", key: "sk-ant-live" },
				openai: { type: "api-key", key: "sk-oai-live" },
			},
		});

		const getApiKey = createSeedApiKeyResolver(home);

		expect(await getApiKey(model("anthropic"))).toBe("sk-ant-live");
		expect(await getApiKey(model("openai"))).toBe("sk-oai-live");
	});

	test("picks up a rewritten seed on the next call — rotation with no restart", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "old" } },
		});

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("anthropic"))).toBe("old");

		// T6 rotation: the Runner rewrites the seed in place. The very next
		// resolution must see it — no process restart, no cache to invalidate.
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "new" } },
		});
		expect(await getApiKey(model("anthropic"))).toBe("new");
	});

	test("returns undefined for a provider absent from the seed", async () => {
		const home = scratch();
		writeSeed(home, { entries: { anthropic: { type: "api-key", key: "k" } } });

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("google"))).toBeUndefined();
	});

	// A container can legitimately boot before its seed is materialized (provision
	// order) — and an agent with no provider credential must still start and report,
	// not crash on first call. Undefined lets the SDK surface a clean auth error.
	test("returns undefined when the seed file does not exist", async () => {
		const getApiKey = createSeedApiKeyResolver(scratch());
		expect(await getApiKey(model("anthropic"))).toBeUndefined();
	});

	test("returns undefined when the seed is malformed rather than throwing", async () => {
		const home = scratch();
		writeSeedRaw(home, "{not json");

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("anthropic"))).toBeUndefined();
	});

	// Defensive: a seed whose entry is the wrong shape (no string `key`) must not
	// hand a non-string to the SDK, which types ApiKey as `string | ApiKeyResolver`
	// (auth-retry.d.ts:35) and would otherwise send a garbage bearer.
	test("returns undefined when the entry has no string key", async () => {
		const home = scratch();
		writeSeedRaw(
			home,
			JSON.stringify({ entries: { anthropic: { type: "api-key" } } }),
		);

		const getApiKey = createSeedApiKeyResolver(home);
		expect(await getApiKey(model("anthropic"))).toBeUndefined();
	});

	// The fall-through is what fixes keyless providers: a provider absent from
	// the seed resolves through the SDK resolver `createAgentSession` installed,
	// which returns the `"N/A"` sentinel for an `auth: none` provider so the turn
	// dials instead of failing MissingApiKeyError. These pin the layering.
	test("falls through to the fallback for a provider absent from the seed", async () => {
		const home = scratch();
		writeSeed(home, { entries: { anthropic: { type: "api-key", key: "k" } } });

		const getApiKey = createSeedApiKeyResolver(home, (m) =>
			m.provider === "cannedci" ? "N/A" : undefined,
		);
		expect(await getApiKey(model("cannedci"))).toBe("N/A");
	});

	test("a seeded key wins over the fallback — T6 rotation precedence holds", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "sk-seed" } },
		});

		const getApiKey = createSeedApiKeyResolver(home, () => "N/A");
		expect(await getApiKey(model("anthropic"))).toBe("sk-seed");
	});

	test("falls through to the fallback when the seed file is absent", async () => {
		const getApiKey = createSeedApiKeyResolver(scratch(), () => "N/A");
		expect(await getApiKey(model("cannedci"))).toBe("N/A");
	});

	test("propagates a resolver-function fallback unchanged (ApiKey union)", async () => {
		const resolver = () => "minted";
		const getApiKey = createSeedApiKeyResolver(scratch(), () => resolver);
		expect(await getApiKey(model("cannedci"))).toBe(resolver);
	});
});

function writeSeed(home: string, seed: unknown): void {
	writeSeedRaw(home, JSON.stringify(seed));
}

function writeSeedRaw(home: string, body: string): void {
	const dir = join(home, ".compass");
	mkdirSync(dir, { recursive: true });
	writeFileSync(join(dir, "auth-seed.json"), body, { mode: 0o600 });
}

// The resolver keys off `model.provider` alone; the rest of the wide `Model`
// surface (name/api/baseUrl/reasoning/cost/contextWindow/...) is never read, so
// constructing a full registry model here would assert nothing extra. The cast
// is narrow and honest: it names exactly the field under test.
function model(provider: string): Model {
	return { provider, id: `${provider}/test-model` } as unknown as Model;
}

// ── main(): the composition root ─────────────────────────────────────────────
//
// `main` is exercised over the `MainDeps` seam: a fake `AgentSession` (the
// recording shape `agent.test.ts` established) and a fake `RunnerTransport` whose
// four RPCs are in-process handlers recording what reached "the Runner". The
// carrier is fake; everything main composes over it — createSocketFrameSink,
// createSocketControlSource, createPublishSpine (the REAL one, built here exactly
// as createUnixSocketTransport builds it), CompassAgent.run — is production code.
// So these tests see the actual enqueue/flush behavior, not a restatement of it.

// What the fake carrier saw. `publishFrames` is the wire log of the Publish
// spine (trace + lifecycle + control acks, in arrival order); `durableFrames`
// the COMMITTED conversation unaries — appended only after the handler returns,
// so a frame present here is provably committed, and one still in flight is not.
interface CarrierLog {
	publishFrames: PublishFrameRequest[];
	durableFrames: PostConversationFrameRequest[];
}

interface CarrierHooks {
	// The Control server-stream the source consumes: yields ops then returns
	// (clean close → the run loop ends → STOPPED). Defaults to an immediate clean
	// close.
	control?: () => AsyncIterable<WireAgentControl>;
	// Awaited inside the durable unary BEFORE it commits — lets a test hold a
	// conversation frame uncommitted while `run()` finishes.
	onDurable?: (frame: PostConversationFrameRequest) => Promise<void> | void;
	// Called when the composition root releases the carrier. Records the close
	// so a test can pin it against the drain that must precede it.
	onClose?: () => void;
	// Makes the sink's `drain()` reject. Neither production drain can reject
	// today, but that is an invariant of frame-sink.ts/publish-spine.ts, not of
	// the composition root — this lets a test hold `main` to releasing the
	// carrier even when the drain ahead of it fails.
	drainError?: Error;
}

// A RunnerTransport over in-process handlers. Built the same way
// createUnixSocketTransport builds it — one memoized REAL PublishSpine shared by
// the sink and the source — so the priority/trace lanes, the batch cycling, and
// spine.drain() under test are the production implementations.
function fakeCarrier(
	log: CarrierLog,
	hooks: CarrierHooks = {},
): RunnerTransport {
	const real = createPublishSpine(async (stream) => {
		for await (const frame of stream) log.publishFrames.push(frame);
	});
	// One spine object, memoized exactly as createUnixSocketTransport memoizes
	// its own — the sink and the control source must share it. `drainError`
	// swaps in a rejecting drain(): the sink's drain() ends in spine.drain(), so
	// this is what makes the composition root's `await sink.drain?.()` reject.
	const spine =
		hooks.drainError === undefined
			? real
			: { ...real, drain: () => Promise.reject(hooks.drainError) };
	return {
		comms: () => Promise.reject(new Error("comms is not used by main")),
		lifecycle: () => Promise.reject(new Error("lifecycle is not used by main")),
		forge: () => Promise.reject(new Error("forge is not used by main")),
		publishSpine: () => spine,
		postConversationFrame: async (req) => {
			if (hooks.onDurable) await hooks.onDurable(req);
			// Recorded only once the handler completes: presence here IS commitment.
			log.durableFrames.push(req);
			return create(PostConversationFrameResponseSchema, {});
		},
		control: () =>
			hooks.control?.() ??
			(async function* (): AsyncGenerator<WireAgentControl> {})(),
		close: () => hooks.onClose?.(),
	};
}

// Like `deps`, but captures the tee-backed SessionManager `main` builds — the
// surviving durable rider (SEA-1570) is a transcript frame, launched when a
// session write teems onto the sink's durable lane. A test resolves the gate,
// then drives an `appendMessage` through the captured manager to put a durable
// TranscriptEntry send in flight (the way the removed conversation write-through
// used to, before SEA-1708). The real tee storage is used (default
// createSessionStorage), so the full sink → tee → durable-unary path runs.
function depsCapturingManager(
	session: FakeSession,
	transport: RunnerTransport,
	gate: { resolve: (m: SessionManager) => void },
): MainDeps {
	return {
		createSession: (options) => {
			gate.resolve(options.sessionManager as unknown as SessionManager);
			return Promise.resolve({ session: session as unknown as AgentSession });
		},
		createTransport: () => transport,
	};
}

// The recording AgentSession `main` composes over: `subscribe` hands the listener
// to a gate (so a test can push a session event through the REAL EventMapper the
// way the SDK would, once the run loop has wired it) and `agent` carries the
// members CompassAgent/main touch. Only those are implemented, so the cast is
// honest.
interface FakeSession {
	// Resolves with the listener the moment `run()` subscribes — the event gate a
	// test awaits before pushing session events, so there is no race and no spin.
	readonly subscribed: Promise<AgentSessionEventListener>;
	agent: { getApiKey?: (model: Model) => Promise<string | undefined> };
}

function fakeSession(opts: { promptError?: Error } = {}): FakeSession {
	const gate = Promise.withResolvers<AgentSessionEventListener>();
	const rec: FakeSession = { subscribed: gate.promise, agent: {} };
	Object.assign(rec.agent, {
		// `promptError` makes an SDK op reject, which is how a real turn failure
		// crashes the run loop (agent.ts:163 awaits it inside the try).
		prompt: () =>
			opts.promptError !== undefined
				? Promise.reject(opts.promptError)
				: Promise.resolve(),
		steer: () => {},
		appendMessage: () => {},
		setSystemPrompt: () => {},
		setTools: () => {},
	});
	Object.assign(rec, {
		subscribe(fn: AgentSessionEventListener): () => void {
			gate.resolve(fn);
			return () => {};
		},
	});
	return rec;
}

// The deps `main` runs under: the fake session factory plus the fake carrier.
// `createSessionStorage` is left to its production default — the REAL
// `createTeeSessionStorage`, writing under the per-test scratch HOME (pinned in
// beforeEach) — so `main`'s full composition (build sink → tee storage →
// SessionManager.create) is exercised, not stubbed.
//
// The telemetry seam is a RECORDING NO-OP (never the real `initTelemetryExport`,
// which registers a live global TracerProvider + OTLP exporter with no teardown
// and would poison every later test in this shared process — design record F3).
// It reports enabled iff an OTLP endpoint is configured at call time, so a test
// that sources an endpoint still exercises main's gating without real
// registration. The dedicated telemetry tests below use their own recording
// seam to assert the calls.
function deps(session: FakeSession, transport: RunnerTransport): MainDeps {
	return {
		createSession: () =>
			Promise.resolve({ session: session as unknown as AgentSession }),
		createTransport: () => transport,
		telemetry: {
			init: () => Promise.resolve(),
			isEnabled: () =>
				process.env.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT !== undefined ||
				process.env.OTEL_EXPORTER_OTLP_ENDPOINT !== undefined,
		},
	};
}

function emptyLog(): CarrierLog {
	return { publishFrames: [], durableFrames: [] };
}

// The two control ops these tests need. `replayComplete` lifts the barrier (so a
// following live prompt is applied rather than refused); `prompt` is the op whose
// SDK call the crash test makes reject.
function replayCompleteOp(seq: bigint): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "replayComplete", value: {} },
	});
}

function promptOp(seq: bigint, input: string): WireAgentControl {
	return create(AgentControlSchema, {
		controlSeq: seq,
		control: { case: "prompt", value: create(PromptControlSchema, { input }) },
	});
}

// The board states that reached the fake Runner over the Publish spine, in
// arrival order. Empty (UNSPECIFIED) states are trace frames, not transitions.
function statesOf(log: CarrierLog): AgentSessionState[] {
	return log.publishFrames.flatMap((f) => {
		const inner = f.frame?.frame;
		if (inner?.case !== "session") return [];
		return inner.value.state === AgentSessionState.UNSPECIFIED
			? []
			: [inner.value.state];
	});
}

describe("main", () => {
	// HOME is how the entrypoint finds the provider seed (authSeedPath). The
	// Runner always supplies it; if it ever does not, the failure must name the
	// cause at boot rather than surfacing later as an inexplicable "no credential"
	// on the first LLM call. Nothing is constructed before the check, so this
	// needs no injection.
	test("rejects when HOME is unset, naming HOME as the cause", async () => {
		// Non-vacuity: a main that fell back to a default home (or read
		// process.env.HOME instead of the passed env) would construct and hang/
		// resolve instead of rejecting → red.
		await expect(main({})).rejects.toThrow(/HOME/);
	});

	test("rejects when HOME is empty, not just absent", async () => {
		// An empty HOME would build the seed path "/.compass/auth-seed.json" — a
		// path outside the Runner's scoped home. `if (!home)` must catch it; a
		// `home === undefined` check would let it through.
		await expect(main({ HOME: "" })).rejects.toThrow(/HOME/);
	});

	// THE DRAIN BARRIER — what `main` alone owns.
	//
	// `run()` emits its terminal STOPPED through the sink on its way out, and the
	// socket sink only ENQUEUES a lifecycle frame onto the spine's priority lane
	// (frame-sink.ts:131) — the actual wire flush happens in a later batch. So
	// when `main` resolves, STOPPED has reached the Runner only if `main` awaited
	// `sink.drain()`. Without the `finally { await sink.drain?.() }` the process
	// exits with the terminal frame still in the queue, and the board never sees
	// the session stop.
	test("the terminal STOPPED frame has reached the carrier by the time main resolves", async () => {
		const log = emptyLog();
		const session = fakeSession();
		await main(
			{ HOME: scratch() },
			deps(session, fakeCarrier(log, { control: emptyControlStream })),
		);
		// Read at the resolution instant — no extra await, no tick, no timer.
		// Non-vacuity (mutation-verified): with the `finally { await sink.drain?.() }`
		// removed from cli.ts, this reads `[STARTING]` — the queued STOPPED never
		// reaches the wire — and the assertion reds.
		expect(statesOf(log)).toEqual([
			AgentSessionState.STARTING,
			AgentSessionState.STOPPED,
		]);
	});

	// The other half of the sink's teardown contract: a transcript frame is
	// DURABLE (delivered-or-erred on the unary, frame-sink.ts:141-144). A session
	// write teemed during the run launches a durable send that is still in flight
	// when `run()` resolves. `drain()` awaits those in-flight commits; without the
	// barrier `main` resolves — and `import.meta.main` calls `process.exit` —
	// abandoning an uncommitted transcript frame. This is the exact defect the
	// drain fixed.
	//
	// The assertion is an ORDERING, which is the contract itself: the commit
	// strictly precedes main's resolution. The commit is parked one event-loop turn
	// out (never a duration — see nextEventLoopTurn), and `main` without the drain
	// resolves entirely within the microtask phase, so the order inverts.
	test("a transcript frame in flight at teardown is COMMITTED before main resolves", async () => {
		const log = emptyLog();
		const session = fakeSession();
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		// Holds the control stream open so `run()` cannot end before the frame is
		// emitted — the interleaving is gated, never raced.
		const closeControl = Promise.withResolvers<void>();
		const managerGate = Promise.withResolvers<SessionManager>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await closeControl.promise;
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
		});
		const runP = main(
			{ HOME: scratch() },
			depsCapturingManager(session, carrier, managerGate),
		);
		// A session write teems onto the durable lane mid-run: the REAL tee-backed
		// SessionManager turns an appendMessage into a durable TranscriptEntry
		// frame, which the REAL socket sink launches on the unary.
		const manager = await managerGate.promise;
		manager.appendMessage(assistantMsg("the answer"));
		// Gate on the carrier having ENTERED the unary — the send is provably in
		// flight right now, and provably uncommitted.
		await inFlight.promise;
		expect(log.durableFrames).toHaveLength(0);
		closeControl.resolve();
		await runP;
		order.push("main-resolved");
		expect(order).toEqual(["committed", "main-resolved"]);
		expect(log.durableFrames[0]?.frame?.frame.case).toBe("transcriptEntry");
	});

	// The barrier is in `finally`, so it holds on the ERROR path too — which is
	// where it matters most: a session that died is exactly when the board needs
	// its terminal transition and its last transcript frame. `run()` emits
	// ERRORED (agent.ts:113) and re-throws; main must still drain, then propagate
	// the ORIGINAL error (a `finally` that swallowed it would hide the crash).
	//
	// The crash is an SDK op rejecting mid-loop, NOT a control-stream drop: a drop
	// sends the source through its bounded reconnect backoff (control-source.ts:79),
	// and those timers would incidentally flush the spine before main rejects —
	// making the assertions pass with or without the barrier. An op rejection
	// reaches the `finally` entirely within the microtask phase, so this test is
	// genuinely drain-sensitive.
	test("on the error path the frame is committed and ERRORED delivered before main rejects", async () => {
		const log = emptyLog();
		const boom = new Error("SDK prompt failed mid-turn");
		const session = fakeSession({ promptError: boom });
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		// Holds the crashing op back until the durable send is in flight.
		const crash = Promise.withResolvers<void>();
		const managerGate = Promise.withResolvers<SessionManager>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await crash.promise;
				yield promptOp(2n, "go");
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
		});
		const runP = main(
			{ HOME: scratch() },
			depsCapturingManager(session, carrier, managerGate),
		);
		const manager = await managerGate.promise;
		manager.appendMessage(assistantMsg("half an answer"));
		await inFlight.promise;
		crash.resolve();
		await expect(runP).rejects.toBe(boom);
		order.push("main-rejected");
		// Drained on the way out: the frame committed first, and the original
		// error still surfaced.
		expect(order).toEqual(["committed", "main-rejected"]);
		expect(log.durableFrames).toHaveLength(1);
		expect(statesOf(log)).toEqual([
			AgentSessionState.STARTING,
			AgentSessionState.ERRORED,
		]);
	});

	// The carrier is a live HTTP/2 session over the Runner socket, and nothing
	// below `main` holds it — so the composition root must RELEASE it, and must
	// do so strictly AFTER the drain: close abandons open streams, so closing
	// first would discard exactly the frames the barrier exists to commit. The
	// fake carrier holds no socket, so the lingering connection itself is not
	// observable here; what IS the contract, and what this pins, is the ORDER —
	// the durable commit (which only happens because `drain()` awaited it)
	// strictly precedes `close()`, which strictly precedes main's resolution.
	test("the carrier is closed after the drain, before main resolves", async () => {
		const log = emptyLog();
		const session = fakeSession();
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		const closeControl = Promise.withResolvers<void>();
		const managerGate = Promise.withResolvers<SessionManager>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await closeControl.promise;
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
			onClose: () => order.push("closed"),
		});
		const runP = main(
			{ HOME: scratch() },
			depsCapturingManager(session, carrier, managerGate),
		);
		const manager = await managerGate.promise;
		manager.appendMessage(assistantMsg("the answer"));
		await inFlight.promise;
		expect(order).toEqual([]);
		closeControl.resolve();
		await runP;
		order.push("main-resolved");
		expect(order).toEqual(["committed", "closed", "main-resolved"]);
	});

	// The release is in the same `finally`, so it holds on the crash path too —
	// a self-terminating agent that died still must not leave the socket held
	// until the session manager's idle timeout. Same crash shape as the error
	// drain test above (an SDK op rejection, not a control-stream drop, so no
	// reconnect timer incidentally flushes the spine), and the original error
	// still propagates past both teardown steps.
	test("the carrier is closed after the drain on the error path too", async () => {
		const log = emptyLog();
		const boom = new Error("SDK prompt failed mid-turn");
		const session = fakeSession({ promptError: boom });
		const order: string[] = [];
		const inFlight = Promise.withResolvers<void>();
		const crash = Promise.withResolvers<void>();
		const managerGate = Promise.withResolvers<SessionManager>();
		const carrier = fakeCarrier(log, {
			control: async function* () {
				yield replayCompleteOp(1n);
				await crash.promise;
				yield promptOp(2n, "go");
			},
			onDurable: async () => {
				inFlight.resolve();
				await nextEventLoopTurn();
				order.push("committed");
			},
			onClose: () => order.push("closed"),
		});
		const runP = main(
			{ HOME: scratch() },
			depsCapturingManager(session, carrier, managerGate),
		);
		const manager = await managerGate.promise;
		manager.appendMessage(assistantMsg("half an answer"));
		await inFlight.promise;
		crash.resolve();
		await expect(runP).rejects.toBe(boom);
		order.push("main-rejected");
		expect(order).toEqual(["committed", "closed", "main-rejected"]);
	});

	// The release must survive a FAILING drain. Neither production drain can
	// reject today, but that no-throw property belongs to frame-sink.ts and
	// publish-spine.ts, not to this composition root — so if either ever lost it,
	// an unguarded `await sink.drain?.(); transport.close()` would skip the close
	// and leak the HTTP/2 session for the manager's whole idle window, which is
	// the exact defect close() was added to fix. The nested
	// `try { drain } finally { close }` is what this pins.
	//
	// Non-vacuity (mutation-verified): with the close moved back out of its own
	// `finally`, `closed` stays false and the assertion reds.
	test("a rejecting drain still closes the carrier, and its error propagates", async () => {
		const log = emptyLog();
		const drainBoom = new Error("drain failed");
		const session = fakeSession();
		let closed = false;
		const carrier = fakeCarrier(log, {
			control: emptyControlStream,
			drainError: drainBoom,
			onClose: () => {
				closed = true;
			},
		});
		await expect(
			main({ HOME: scratch() }, deps(session, carrier)),
		).rejects.toBe(drainBoom);
		expect(closed).toBe(true);
	});

	// The seed resolver is installed on the SESSION'S agent, and installed as a
	// live resolver (called per LLM call) rather than a value read at boot. The
	// wiring is only interesting because of what it resolves TO, so this drives
	// the installed function against a real seed under the passed HOME: a resolver
	// built from the wrong home, or one never installed, reddens.
	test("installs a getApiKey on the session that resolves from the passed HOME's seed", async () => {
		const home = scratch();
		writeSeed(home, {
			entries: { anthropic: { type: "api-key", key: "sk-from-main" } },
		});
		const session = fakeSession();
		await main(
			{ HOME: home },
			deps(session, fakeCarrier(emptyLog(), { control: emptyControlStream })),
		);
		const getApiKey = session.agent.getApiKey;
		if (getApiKey === undefined) throw new Error("main installed no getApiKey");
		expect(await getApiKey(model("anthropic"))).toBe("sk-from-main");
	});

	// The socket path is not a parameter anywhere: the Runner bind-mounts it at a
	// fixed location (host.go:33) and the agent dials that constant. Pinning it
	// AT THE CALL SITE catches a main that dialed something else — the constant
	// test above only pins the constant's value.
	test("dials the carrier at AGENT_SOCKET_PATH", async () => {
		const dialed: string[] = [];
		const session = fakeSession();
		await main(
			{ HOME: scratch() },
			{
				createSession: () =>
					Promise.resolve({ session: session as unknown as AgentSession }),
				createTransport: (socketPath) => {
					dialed.push(socketPath);
					return fakeCarrier(emptyLog(), { control: emptyControlStream });
				},
			},
		);
		expect(dialed).toEqual([AGENT_SOCKET_PATH]);
	});

	// COMPASS_MODEL / COMPASS_WORKDIR are the container's only two configuration
	// knobs, and `main` is the sole place they become session options. The
	// resolution rules are unit-tested above; this pins that main actually FORWARDS
	// them — a session built with the wrong cwd loads the wrong project context.
	test("forwards COMPASS_MODEL as modelPattern and COMPASS_WORKDIR as cwd", async () => {
		const session = fakeSession();
		const seen: { cwd?: string; modelPattern?: string | string[] }[] = [];
		await main(
			{
				HOME: scratch(),
				COMPASS_MODEL: "anthropic/claude-opus-4-5",
				COMPASS_WORKDIR: "/work/repo",
			},
			{
				createSession: (options) => {
					seen.push({
						cwd: options.cwd,
						modelPattern: options.modelPattern,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toEqual([
			{ cwd: "/work/repo", modelPattern: "anthropic/claude-opus-4-5" },
		]);
	});

	test("treats an empty or whitespace-only COMPASS_WORKDIR as unset, not as a cwd", async () => {
		// Mirrors the empty-HOME case: `??` would forward "" verbatim, and bun does
		// not reject `cwd: ""` — the agent would silently load project context from
		// the wrong tree. A whitespace-only value is truthy, so the `.trim()` is
		// what catches it. The Runner sets COMPASS_WORKDIR unconditionally
		// (relay.go `execSpec`), so a blank AgentEnv.Workdir reaches here directly.
		for (const workdir of ["", "   "]) {
			const session = fakeSession();
			const seen: (string | undefined)[] = [];
			await main(
				{ HOME: scratch(), COMPASS_WORKDIR: workdir },
				{
					createSession: (options) => {
						seen.push(options.cwd);
						return Promise.resolve({
							session: session as unknown as AgentSession,
						});
					},
					createTransport: () =>
						fakeCarrier(emptyLog(), { control: emptyControlStream }),
				},
			);
			expect(seen).toEqual([process.cwd()]);
		}
	});

	// COMPASS_PERSONA is the identity overlay: when set, main must APPEND it to
	// the SDK's default prompt (block-0 base + project footer survive), never
	// replace it. The customizer is a function; drive it with a fake default and
	// assert the persona lands last.
	test("appends COMPASS_PERSONA to the default systemPrompt when set", async () => {
		const session = fakeSession();
		const seen: (
			| string
			| string[]
			| ((p: string[]) => string | string[])
			| undefined
		)[] = [];
		await main(
			{ HOME: scratch(), COMPASS_PERSONA: "You are Ada." },
			{
				createSession: (options) => {
					seen.push(options.systemPrompt);
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		const customizer = seen[0];
		if (typeof customizer !== "function") {
			throw new Error("systemPrompt was not the append customizer function");
		}
		expect(customizer(["base", "project footer"])).toEqual([
			"base",
			"project footer",
			"You are Ada.",
		]);
	});

	test("leaves systemPrompt unset when COMPASS_PERSONA is empty or whitespace", async () => {
		// A whitespace-only persona resolves to undefined (resolvePersona), so the
		// `persona ?` guard omits systemPrompt and the agent keeps its default
		// prompt rather than an overlay of blank identity.
		for (const persona of [undefined, "", "   "]) {
			const session = fakeSession();
			const seen: unknown[] = [];
			await main(
				{
					HOME: scratch(),
					...(persona === undefined ? {} : { COMPASS_PERSONA: persona }),
				},
				{
					createSession: (options) => {
						seen.push(options.systemPrompt);
						return Promise.resolve({
							session: session as unknown as AgentSession,
						});
					},
					createTransport: () =>
						fakeCarrier(emptyLog(), { control: emptyControlStream }),
				},
			);
			expect(seen).toEqual([undefined]);
		}
	});

	// ── SEA-1570: the tee-storage composition + resume ────────────────────────
	//
	// `main` builds the tee storage over the socket sink and injects the
	// resulting IndexedSessionStorage into SessionManager.create, passed to
	// createAgentSession as `sessionManager`. This pins that wiring: what reaches
	// createSession is the manager main built (not the SDK's own default), so
	// every session write teems onto the durable lane.
	test("passes the tee-backed SessionManager to createAgentSession", async () => {
		const session = fakeSession();
		let seenManager: unknown;
		await main(
			{ HOME: scratch(), COMPASS_WORKDIR: "/work/repo" },
			{
				createSession: (options) => {
					seenManager = options.sessionManager;
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// A SessionManager built for the resolved cwd, its session file under the
		// SDK's HOME-relative default dir (the beforeEach-pinned scratch HOME).
		// Non-vacuity: a main that passed no manager (SDK default) leaves this
		// undefined → red.
		const manager = seenManager as SessionManager | undefined;
		if (!manager) throw new Error("main passed no sessionManager");
		expect(manager.getCwd()).toBe("/work/repo");
		expect(
			manager
				.getSessionFile()
				?.startsWith(SessionManager.getDefaultSessionDir("/work/repo")),
		).toBe(true);
	});

	// COMPASS_RESUME_SESSION_FILE (exported by T8) is loaded through the SDK-native
	// setSessionFile path BEFORE the session is created — so the resumed history
	// is already present when createAgentSession runs. This pins that the manager
	// handed to createSession carries the fixture's entries, loaded via the tee
	// backend's readFull/loadIndex (no replay code).
	test("resumes COMPASS_RESUME_SESSION_FILE before creating the session", async () => {
		const session = fakeSession();
		// The resume file must be INDEXED by the tee backend's initialize() scan
		// (the wrapper ENOENTs un-indexed paths, indexed-session-storage.ts:177),
		// so it lives in the SDK default session dir for this cwd and is written
		// before main() builds the storage. Current-version fixture → no load-time
		// migration rewrite, so the resume path emits no checkpoint frame.
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_resume.jsonl");
		writeFileSync(resumeFile, sessionFixture([userLine("resumed turn")]));

		let entriesAtCreate: unknown[] | undefined;
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: (options) => {
					entriesAtCreate = (
						options.sessionManager as SessionManager
					).getEntries();
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// Loaded BEFORE createSession ran: the user turn is already in the manager.
		// Non-vacuity: a main that ignored the env var (today's behavior) leaves
		// entriesAtCreate empty → red.
		const texts = (entriesAtCreate ?? [])
			.filter((e): e is { type: "message"; message: { content: string } } => {
				const entry = e as { type?: string };
				return entry.type === "message";
			})
			.map((e) => e.message.content);
		expect(texts).toContain("resumed turn");
	});

	// The real Option-B shape (SEA-1570 T2): the Runner materializes the resume
	// file at an absolute path OUTSIDE the SDK default session dir. On the unfixed
	// code loadIndex scans only sessionDir → the file is un-indexed → setSessionFile's
	// statSync gate ENOENTs → silent fresh session → entriesAtCreate empty → RED.
	// After the fix (resumeFile threaded into the tee backend and indexed at
	// initialize()) → GREEN. This is the exact silent-degradation this task prevents.
	test("resumes a COMPASS_RESUME_SESSION_FILE that lives OUTSIDE the session dir", async () => {
		const session = fakeSession();
		// A scratch dir that is NOT the SDK default session dir for this cwd —
		// mirrors the Runner's $HOME/.compass/resume/<id>.jsonl (Option B, T2).
		const resumeDir = mkdtempSync(join(tmpdir(), "compass-resume-"));
		const resumeFile = join(resumeDir, "r.jsonl");
		writeFileSync(resumeFile, sessionFixture([userLine("resumed turn")]));

		let entriesAtCreate: unknown[] | undefined;
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: (options) => {
					entriesAtCreate = (
						options.sessionManager as SessionManager
					).getEntries();
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		rmSync(resumeDir, { recursive: true, force: true });
		// The resume file lived outside sessionDir yet its turn is loaded — the
		// out-of-dir indexing worked. Non-vacuity: un-indexed → empty → red.
		const texts = (entriesAtCreate ?? [])
			.filter((e): e is { type: "message"; message: { content: string } } => {
				const entry = e as { type?: string };
				return entry.type === "message";
			})
			.map((e) => e.message.content);
		expect(texts).toContain("resumed turn");
	});

	// The storage drain is in the same `finally` as the sink drain, so it runs on
	// the ERROR path too — a crashed session must still flush its queued append
	// tee-sends before teardown. This pins that main awaits storage.drain() even
	// when run() rejects (and still propagates the original error).
	test("drains the tee storage on the error path", async () => {
		const boom = new Error("SDK prompt failed mid-turn");
		const session = fakeSession({ promptError: boom });
		const dir = scratch();
		let drained = false;
		const { storage } = await createTeeSessionStorage(
			{
				emit: () => {},
				emitDurable: () => Promise.resolve(),
				drain: () => Promise.resolve(),
			},
			dir,
		);
		const realDrain = storage.drain.bind(storage);
		storage.drain = async () => {
			drained = true;
			await realDrain();
		};
		const carrier = fakeCarrier(emptyLog(), {
			control: async function* () {
				yield replayCompleteOp(1n);
				yield promptOp(2n, "go");
			},
		});
		await expect(
			main(
				{ HOME: scratch() },
				{
					createSession: () =>
						Promise.resolve({ session: session as unknown as AgentSession }),
					createTransport: () => carrier,
					createSessionStorage: () =>
						Promise.resolve({ storage, backend: undefined as never }),
				},
			),
		).rejects.toBe(boom);
		// Non-vacuity: a main that only drained on the clean path (or omitted the
		// storage drain) leaves this false → red.
		expect(drained).toBe(true);
	});

	// ── T3: the resume proof-smoke (SDK-native load) ──────────────────────────
	//
	// The full round-trip, no Runner: run one main(), drive two turns through the
	// REAL tee-backed SessionManager, capture the durable TranscriptEntry frames
	// off the carrier; reconstruct the session-JSONL body the way T5 does (latest
	// checkpoint body + later delta lines by entry_seq); write it to the default
	// session dir; start a SECOND main() with COMPASS_RESUME_SESSION_FILE at it;
	// and assert the second session's manager carries the first run's turns,
	// loaded via setSessionFile → loadEntriesFromFile — with NO TranscriptEntry
	// frames emitted during the load (reads never tee), and a post-resume turn
	// emitting deltas with a FRESH per-lifetime entry_seq starting at 1 (the
	// server-rebase model, T4). The load touches only the $HOME session dir, so
	// it is checkout-independent.
	test("a teed run reconstructs into a resumable session (SDK-native load)", async () => {
		// ── Run 1: drive two turns through the real tee manager, in the
		// createSession callback (it holds options.sessionManager — the manager
		// main built over the tee storage). appendMessage → tee → durable lane;
		// flush() awaits storage.drain() so the frames are committed to the
		// carrier before the callback returns. ──
		const log1 = emptyLog();
		const session1 = fakeSession();
		await main(
			{ HOME: process.env.HOME },
			{
				createSession: async (options) => {
					const m = options.sessionManager;
					if (!m) throw new Error("run 1: main passed no sessionManager");
					m.appendMessage(userMsg("first question"));
					m.appendMessage(assistantMsg("first answer"));
					m.appendMessage(userMsg("second question"));
					m.appendMessage(assistantMsg("second answer"));
					await m.flush();
					return { session: session1 as unknown as AgentSession };
				},
				createTransport: () =>
					fakeCarrier(log1, { control: emptyControlStream }),
			},
		);

		// The durable transcript frames the run committed, in commit order: the
		// first assistant append rewrites the body (a checkpoint), later appends
		// ride as deltas — and the per-lifetime entry_seq started at 1n.
		const frames1 = transcriptFramesOf(log1);
		expect(frames1.length).toBeGreaterThan(0);
		expect(frames1[0].entrySeq).toBe(1n);
		expect(frames1.some((f) => f.checkpoint)).toBe(true);

		// ── Reconstruct the body the way T5 does: latest checkpoint body + later
		// delta lines by entry_seq. The checkpoint's entryJson IS the full body. ──
		const body = reconstructSessionBody(frames1);

		// ── Run 2: resume from the reconstructed body. ────────────────────────
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_resumed.jsonl");
		writeFileSync(resumeFile, body);

		const log2 = emptyLog();
		const session2 = fakeSession();
		let entriesAtCreate: unknown[] = [];
		let framesAtCreate = 0;
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: async (options) => {
					const m = options.sessionManager;
					if (!m) throw new Error("run 2: main passed no sessionManager");
					// Snapshot BEFORE any post-resume write: the load already ran
					// (setSessionFile precedes createSession).
					entriesAtCreate = m.getEntries();
					framesAtCreate = transcriptFramesOf(log2).length;
					// A post-resume turn emits fresh deltas.
					m.appendMessage(userMsg("post-resume question"));
					m.appendMessage(assistantMsg("post-resume answer"));
					await m.flush();
					return { session: session2 as unknown as AgentSession };
				},
				createTransport: () =>
					fakeCarrier(log2, { control: emptyControlStream }),
			},
		);

		// The resumed session carries the first run's turns (loaded via
		// setSessionFile before createSession ran).
		const resumedTexts = textsOf(entriesAtCreate);
		expect(resumedTexts).toContain("first question");
		expect(resumedTexts).toContain("second question");

		// NO TranscriptEntry frames were emitted during the load — reads never
		// tee, and the current-version fixture triggers no migration rewrite.
		expect(framesAtCreate).toBe(0);

		// The post-resume turn's deltas carry a FRESH per-lifetime entry_seq
		// starting at 1n (the server rebases per session, T4).
		const frames2 = transcriptFramesOf(log2);
		expect(frames2.length).toBeGreaterThan(0);
		expect(frames2[0].entrySeq).toBe(1n);
	});

	// A v2 (older-version) fixture resumed via COMPASS_RESUME_SESSION_FILE: the
	// SDK runs a v2→v3 load migration, but that only sets #rewriteRequired as a
	// DEFERRED flag (session-manager.ts:1007) — it does NOT rewrite the non-empty
	// file during load, so the load tees ZERO frames. This is a regression guard
	// on the cli.ts "the load never tees" invariant for the migrated-resume path.
	// Non-vacuity: if a future SDK revision (or a tee change) rewrote the file
	// during a migrated load, a checkpoint frame would land → framesAtCreate > 0
	// → red.
	test("a v2-fixture resume stays migration-deferred and tees no frames on load", async () => {
		const session = fakeSession();
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_v2resume.jsonl");
		writeFileSync(resumeFile, sessionFixtureV2([userLine("v2 turn", "e-v2")]));

		const log = emptyLog();
		let entriesAtCreate: unknown[] = [];
		let framesAtCreate = 0;
		let headerVersionAtCreate: number | undefined;
		let needsRewriteAtCreate = false;
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: (options) => {
					const m = options.sessionManager as SessionManager;
					// Snapshot BEFORE any post-resume write: the load already ran
					// (setSessionFile precedes createSession).
					entriesAtCreate = m.getEntries();
					framesAtCreate = transcriptFramesOf(log).length;
					// captureState() exposes the post-load header + the deferred
					// rewrite flag (session-manager.ts:919-936), proving the v2→v3
					// migration actually RAN (not "no migration was needed").
					const state = m.captureState();
					headerVersionAtCreate = state.header.version;
					needsRewriteAtCreate = state.needsRewrite;
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(log, { control: emptyControlStream }),
			},
		);

		// The resumed manager carries the fixture's turn (loaded via the SDK's own
		// migrating loader).
		expect(textsOf(entriesAtCreate)).toContain("v2 turn");
		// The v2→v3 migration actually RAN: the in-memory header was upgraded to 3
		// (migrateV2ToV3, session-migrations.ts:46) and the rewrite was flagged as
		// required — this distinguishes "migration ran but stayed deferred" from
		// "no migration was needed", so the zero-frames assertion below is a real
		// guard on the migrated-resume path, not a vacuous current-version pass.
		expect(headerVersionAtCreate).toBe(3);
		expect(needsRewriteAtCreate).toBe(true);
		// Migration stayed deferred: no checkpoint (or any) frame teed on load
		// (setSessionFile sets #rewriteRequired = migrated as a flag,
		// session-manager.ts:1007; it does NOT #rewriteAtomically a non-empty file).
		expect(framesAtCreate).toBe(0);
	});

	// A compaction round-trip: a fixture body whose file contains a superseded
	// compaction loads through the SDK's own elision — proving the T5
	// reconstruction needs no compaction awareness beyond T4's supersession.
	test("a superseded-compaction fixture loads via the SDK's own elision", async () => {
		const session = fakeSession();
		const cwd = process.cwd();
		const sessionDir = SessionManager.getDefaultSessionDir(cwd);
		mkdirSync(sessionDir, { recursive: true });
		const resumeFile = join(sessionDir, "20260101-000000_compacted.jsonl");
		// Two compactions on the active branch; the earlier one is superseded and
		// the SDK's elideSupersededCompactionEntries collapses its summary on load.
		writeFileSync(
			resumeFile,
			sessionFixture([
				compactionLine("c1", null, "first compaction summary"),
				compactionLine("c2", "c1", "second compaction summary"),
				userLine("after compaction", "e-after", "c2"),
			]),
		);

		let entriesAtCreate: unknown[] = [];
		await main(
			{ HOME: process.env.HOME, COMPASS_RESUME_SESSION_FILE: resumeFile },
			{
				createSession: (options) => {
					const m = options.sessionManager;
					if (!m) throw new Error("main passed no sessionManager");
					entriesAtCreate = m.getEntries();
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// The session loaded (post-compaction entry present) and the superseded
		// compaction's summary was elided by the SDK loader.
		const summaries = compactionSummariesOf(entriesAtCreate);
		expect(summaries).toHaveLength(2);
		expect(summaries[0]).toBe(
			"[Superseded compaction summary elided during session load]",
		);
		expect(summaries[1]).toBe("second compaction summary");
		expect(textsOf(entriesAtCreate)).toContain("after compaction");
	});
});

// ── main(): sourcing $HOME/.compass/env into process.env ─────────────────────
//
// The materialized env-secret file (SEA-1327 T5) must reach `process.env` before
// createAgentSession, so the session's extensions/MCP/tools inherit the secrets.
// These run over the same composition seam as the `main` tests above, writing
// the env file under the per-test scratch HOME (pinned in beforeEach). Every
// process.env KEY a test writes is saved+restored so the suite stays isolated
// and full-suite safe — mirroring the savedHome pattern.
function writeEnvFile(home: string, body: string): void {
	const dir = join(home, ".compass");
	mkdirSync(dir, { recursive: true });
	writeFileSync(join(dir, "env"), body, { mode: 0o600 });
}

describe("main sources $HOME/.compass/env into process.env", () => {
	// The test-only keys these tests write into process.env, saved before and
	// restored after so a leaked key can never flake a later test.
	const TOUCHED_KEYS = [
		"SOME_TEST_KEY",
		"COMPASS_MODEL",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		// main's enabled-path telemetry activation writes these when an endpoint is
		// sourced (the OTEL-endpoint test below); save+restore so they never leak.
		"OTEL_SERVICE_NAME",
		"OTEL_RESOURCE_ATTRIBUTES",
		"COMPASS_FUTURE_VAR",
		"LITELLM_BASE_URL",
		"LITELLM_MCP_URL",
	] as const;
	let savedEnv: Record<string, string | undefined> = {};
	beforeEach(() => {
		savedEnv = {};
		for (const key of TOUCHED_KEYS) savedEnv[key] = process.env[key];
	});
	afterEach(() => {
		for (const key of TOUCHED_KEYS) {
			const prev = savedEnv[key];
			if (prev === undefined) delete process.env[key];
			else process.env[key] = prev;
		}
	});

	test("a written env file's KEYs reach process.env", async () => {
		const home = process.env.HOME as string;
		writeEnvFile(home, "SOME_TEST_KEY=secretval\n");
		process.env.SOME_TEST_KEY = "from-process";
		// Non-vacuity + ratified precedence: without the merge loop the key keeps
		// its process value → red; the assertion pins "file wins" over an
		// already-present key, not merely "a new key lands".
		await main(
			{ HOME: home },
			deps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
			),
		);
		expect(process.env.SOME_TEST_KEY as string | undefined).toBe("secretval");
	});

	test("a missing env file is tolerated — main resolves without throwing", async () => {
		const home = process.env.HOME as string;
		// No .compass/env written under this scratch home.
		await expect(
			main(
				{ HOME: home },
				deps(
					fakeSession(),
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
				),
			),
		).resolves.toBeUndefined();
	});

	test("a reserved var in the file never clobbers process.env", async () => {
		const home = process.env.HOME as string;
		process.env.COMPASS_MODEL = "from-process";
		writeEnvFile(home, "COMPASS_MODEL=from-file\n");
		await main(
			{ HOME: home },
			deps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
			),
		);
		expect(process.env.COMPASS_MODEL).toBe("from-process");
	});

	test("the OTEL endpoint key from the env file reaches process.env unfiltered (where the transport's OTel layer reads it), while a COMPASS_-prefixed key is dropped", async () => {
		const home = process.env.HOME as string;
		// A non-COMPASS key (the OTEL endpoint) and a COMPASS_-prefixed key in the
		// same file: the endpoint must land (isReservedEnvKey lets it through), the
		// COMPASS_ key must be dropped (prefix rule) — pinning that the deployer's
		// endpoint reaches makeOtelLayer's process.env read while the control-var
		// namespace stays unclobberable, in one assertion pair.
		writeEnvFile(
			home,
			"OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318\nCOMPASS_FUTURE_VAR=nope\n",
		);
		await main(
			{ HOME: home },
			deps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
			),
		);
		expect(process.env.OTEL_EXPORTER_OTLP_ENDPOINT).toBe(
			"http://collector.example:4318",
		);
		expect(process.env.COMPASS_FUTURE_VAR).toBeUndefined();
	});

	test("derives LITELLM_MCP_URL from a delivered LITELLM_BASE_URL (RIG-2674)", async () => {
		const home = process.env.HOME as string;
		// The keyring delivers only the base URL (+ API key). Without the derive,
		// LITELLM_MCP_URL stays unset and the fleet mcp.json's ${LITELLM_MCP_URL}
		// expands empty → the LiteLLM MCP server can't connect. Non-vacuity: with
		// the derive removed this assertion reds (undefined, not the /mcp/ URL).
		delete process.env.LITELLM_MCP_URL;
		writeEnvFile(home, "LITELLM_BASE_URL=https://llm.example/v1\n");
		await main(
			{ HOME: home },
			deps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
			),
		);
		expect(process.env.LITELLM_MCP_URL as string | undefined).toBe(
			"https://llm.example/mcp/",
		);
	});

	test("LITELLM_MCP_URL is derived BEFORE the MCP manager connects (the load-bearing ordering)", async () => {
		// The whole point of the fix is that the MCP connector — which reads
		// ${LITELLM_MCP_URL} from process.env — sees the derived value. Asserting
		// it only after main() resolves would pass even if a future refactor moved
		// the derive AFTER the connect. Capture process.env AT the connect instant
		// via the connectMcp seam (main calls it unconditionally, cli.ts:729, after
		// loadMountedConfig and before session construction). Non-vacuity: move the
		// derive block below the connect and this reds (undefined at capture) while
		// the post-resolve assertion above would still pass.
		const home = process.env.HOME as string;
		delete process.env.LITELLM_MCP_URL;
		writeEnvFile(home, "LITELLM_BASE_URL=https://llm.example/v1\n");
		let atConnect: string | undefined = "UNSET-SENTINEL";
		await main(
			{ HOME: home },
			{
				...deps(
					fakeSession(),
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
				),
				connectMcp: (_cwd, _mcp) => {
					atConnect = process.env.LITELLM_MCP_URL;
					return Promise.resolve({
						tools: [] as never,
						disconnect: () => Promise.resolve(),
					});
				},
			},
		);
		expect(atConnect).toBe("https://llm.example/mcp/");
	});

	test("an explicitly-delivered LITELLM_MCP_URL wins over the derivation", async () => {
		const home = process.env.HOME as string;
		// If a deployer ever delivers the MCP URL directly (env file), the derive
		// must not clobber it — same file-defines-it posture as the merge loop.
		writeEnvFile(
			home,
			"LITELLM_BASE_URL=https://llm.example/v1\nLITELLM_MCP_URL=https://override.example/mcp/\n",
		);
		await main(
			{ HOME: home },
			deps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
			),
		);
		expect(process.env.LITELLM_MCP_URL).toBe("https://override.example/mcp/");
	});
});

// ── main(): loop OpenTelemetry activation ────────────────────────────────────
//
// design docs/designs/platform/compass-agent-loop-otel/design.md T1. These run
// over the MainDeps composition seam with a RECORDING telemetry seam — NEVER the
// real `initTelemetryExport`, which registers a live global TracerProvider + a
// real OTLP exporter with no teardown and would poison every later test in this
// shared process (design record F3). They assert exactly what cli.ts owns: the
// endpoint gate, the enabled-path env writes (defaulted service name + appended
// join key), the bit-identical inertness when off, and the gated `telemetry`
// session option. Span correctness is OMP's own suite; the real registration is
// left to a spawned-subprocess smoke (not run in-process, per F3).
describe("main activates loop OpenTelemetry", () => {
	// Every OTEL_* key these tests read or main may write, saved+restored so a
	// leaked var can never flake a later test — the savedHome/TOUCHED_KEYS pattern.
	const OTEL_KEYS = [
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_SERVICE_NAME",
		"OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_SDK_DISABLED",
		"OTEL_TRACES_EXPORTER",
	] as const;
	let savedOtel: Record<string, string | undefined> = {};
	beforeEach(() => {
		savedOtel = {};
		for (const key of OTEL_KEYS) {
			savedOtel[key] = process.env[key];
			delete process.env[key];
		}
	});
	afterEach(() => {
		for (const key of OTEL_KEYS) {
			const prev = savedOtel[key];
			if (prev === undefined) delete process.env[key];
			else process.env[key] = prev;
		}
	});

	// A recording telemetry seam + the captured createSession options. `isEnabled`
	// mirrors the real module: it reports true only AFTER init() ran (a real
	// provider registered), so the option gate keys off registration, not the
	// endpoint. The seam records call order so a test can pin env-before-init.
	interface TelemetrySpy {
		calls: string[];
		telemetryOption: unknown;
		hasTelemetryKey: boolean;
	}
	function telemetryDeps(
		session: FakeSession,
		transport: RunnerTransport,
		spy: TelemetrySpy,
	): MainDeps {
		let registered = false;
		return {
			createSession: (options) => {
				spy.telemetryOption = options.telemetry;
				spy.hasTelemetryKey = "telemetry" in options;
				return Promise.resolve({
					session: session as unknown as AgentSession,
				});
			},
			createTransport: () => transport,
			telemetry: {
				init: () => {
					// Capture the env the real provider would read at registration —
					// pinning that the defaults are in place BEFORE init runs.
					spy.calls.push(
						`init:${process.env.OTEL_SERVICE_NAME}:${process.env.OTEL_RESOURCE_ATTRIBUTES}`,
					);
					registered = true;
					return Promise.resolve();
				},
				isEnabled: () => registered,
			},
		};
	}

	// The predicate main gates on, exercised directly — the endpoint contract in
	// isolation from the composition (the enum-list + kill-switch branches are
	// awkward to reach through the env file, and this is the authoritative gate).
	describe("isTelemetryEndpointConfigured", () => {
		test("false when no endpoint is set", () => {
			expect(isTelemetryEndpointConfigured({})).toBe(false);
		});
		test("true for the base endpoint", () => {
			expect(
				isTelemetryEndpointConfigured({
					OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector:4318",
				}),
			).toBe(true);
		});
		test("true for the traces-specific endpoint alone", () => {
			expect(
				isTelemetryEndpointConfigured({
					OTEL_EXPORTER_OTLP_TRACES_ENDPOINT: "http://collector:4318/v1/traces",
				}),
			).toBe(true);
		});
		test("false when OTEL_SDK_DISABLED is true, even with an endpoint", () => {
			expect(
				isTelemetryEndpointConfigured({
					OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector:4318",
					OTEL_SDK_DISABLED: "TRUE",
				}),
			).toBe(false);
		});
		test("false when OTEL_TRACES_EXPORTER names none, even with an endpoint", () => {
			expect(
				isTelemetryEndpointConfigured({
					OTEL_EXPORTER_OTLP_ENDPOINT: "http://collector:4318",
					OTEL_TRACES_EXPORTER: "otlp,none",
				}),
			).toBe(false);
		});
	});

	// Endpoint UNSET ⇒ true bit-identical inertness (F2): NO `telemetry` key on
	// the options AND process.env is unmutated. Snapshot the two enabled-path keys
	// before and after so a stray write (an ungated mutation) reddens. Non-vacuity:
	// dropping the `if (isTelemetryEndpointConfigured(...))` gate writes
	// OTEL_SERVICE_NAME here → red.
	test("endpoint unset ⇒ no telemetry key and process.env is unmutated", async () => {
		const beforeName = process.env.OTEL_SERVICE_NAME;
		const beforeAttrs = process.env.OTEL_RESOURCE_ATTRIBUTES;
		const spy: TelemetrySpy = {
			calls: [],
			telemetryOption: "SENTINEL",
			hasTelemetryKey: true,
		};
		await main(
			{ HOME: scratch() },
			telemetryDeps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
				spy,
			),
		);
		expect(spy.calls).toEqual([]);
		expect(spy.hasTelemetryKey).toBe(false);
		expect(spy.telemetryOption).toBeUndefined();
		expect(process.env.OTEL_SERVICE_NAME).toBe(beforeName);
		expect(process.env.OTEL_RESOURCE_ATTRIBUTES).toBe(beforeAttrs);
	});

	// Endpoint SET ⇒ init() ran with the env defaults in place, and the options
	// carry `telemetry: {}`. Pins: OTEL_SERVICE_NAME defaulted, compass.session.id
	// appended to OTEL_RESOURCE_ATTRIBUTES, and — via the recorded init call — that
	// both were set BEFORE init ran (the load-bearing order).
	test("endpoint set ⇒ env defaults set before init, and telemetry:{} on the options", async () => {
		process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://collector:4318";
		const spy: TelemetrySpy = {
			calls: [],
			telemetryOption: "SENTINEL",
			hasTelemetryKey: false,
		};
		await main(
			{ HOME: scratch() },
			telemetryDeps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
				spy,
			),
		);
		expect(spy.hasTelemetryKey).toBe(true);
		expect(spy.telemetryOption).toEqual({});
		expect(process.env.OTEL_SERVICE_NAME).toBe("compass-agent");
		// The join key was appended; the session id is minted, so assert the shape,
		// not a fixed id.
		expect(process.env.OTEL_RESOURCE_ATTRIBUTES).toMatch(
			/^compass\.session\.id=.+/,
		);
		// init ran exactly once, and it observed the defaults already in place —
		// the load-bearing env-before-registration order.
		expect(spy.calls).toHaveLength(1);
		expect(spy.calls[0]).toMatch(/^init:compass-agent:compass\.session\.id=.+/);
	});

	// Deployer-set values are PRESERVED: ??= no-ops the service name, and the join
	// key APPENDS after the deployer's existing OTEL_RESOURCE_ATTRIBUTES (never
	// clobbers it — Decision 3a). Non-vacuity: a `=` instead of `??=`, or an
	// assignment instead of an append, reds one of these.
	test("deployer-set OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES are preserved", async () => {
		process.env.OTEL_EXPORTER_OTLP_ENDPOINT = "http://collector:4318";
		process.env.OTEL_SERVICE_NAME = "deployer-name";
		process.env.OTEL_RESOURCE_ATTRIBUTES = "deployment.environment=prod";
		const spy: TelemetrySpy = {
			calls: [],
			telemetryOption: undefined,
			hasTelemetryKey: false,
		};
		await main(
			{ HOME: scratch() },
			telemetryDeps(
				fakeSession(),
				fakeCarrier(emptyLog(), { control: emptyControlStream }),
				spy,
			),
		);
		// ??= left the deployer's name untouched.
		expect(process.env.OTEL_SERVICE_NAME).toBe("deployer-name");
		// The join key was appended AFTER the deployer's value, comma-joined.
		expect(process.env.OTEL_RESOURCE_ATTRIBUTES).toMatch(
			/^deployment\.environment=prod,compass\.session\.id=.+/,
		);
		expect(spy.hasTelemetryKey).toBe(true);
	});
});

// A Control stream that closes cleanly with no ops — the shortest complete run.
async function* emptyControlStream(): AsyncGenerator<WireAgentControl> {}

// Park the caller until the next MACROtask turn — a single `setImmediate`, not a
// duration and not a poll. It is the coarsest thing that is still not a sleep:
// every microtask already queued (and every one they queue) runs first, so it
// cleanly separates "resolved within the microtask phase" from "resolved after an
// event-loop turn". The drain tests use it to place the durable commit strictly
// after a barrier-less `main` would have resolved, making the ordering assertion
// discriminate the two implementations rather than time them.
function nextEventLoopTurn(): Promise<void> {
	const { promise, resolve } = Promise.withResolvers<void>();
	setImmediate(resolve);
	return promise;
}

// ── SEA-1570 session-JSONL fixtures ──────────────────────────────────────────
//
// Build a current-version (v3) session body the SDK loader accepts verbatim: a
// 256-byte title slot, a session header, then one JSONL line per entry. Current
// version means setSessionFile loads it without a migration rewrite — so the
// resume path emits NO checkpoint frame (reads never tee). These mirror the
// bytes the tee backend commits, so a T3 reconstruction feeds this exact shape.
let fixtureSeq = 0;

function titleSlot(): string {
	// The SDK's own fixed-width 256-byte title slot serializer — the exact bytes
	// a real session file's first line carries, so the fixture is byte-faithful.
	return `${serializeTitleSlot({ updatedAt: "2026-01-01T00:00:00.000Z" })}`;
}

function sessionHeader(): string {
	fixtureSeq += 1;
	return `${JSON.stringify({
		type: "session",
		version: 3,
		id: `fixture-${fixtureSeq}`,
		timestamp: "2026-01-01T00:00:00.000Z",
		cwd: process.cwd(),
	})}\n`;
}

// One user-message entry line — content the resumed manager exposes via
// getEntries()[i].message.content.
function userLine(
	content: string,
	id = `e${fixtureSeq}-${content}`,
	parentId: string | null = null,
): string {
	return `${JSON.stringify({
		type: "message",
		id,
		parentId,
		timestamp: "2026-01-01T00:00:01.000Z",
		message: { role: "user", content },
	})}\n`;
}

// One compaction entry line, for the superseded-compaction round-trip.
function compactionLine(
	id: string,
	parentId: string | null,
	summary: string,
): string {
	return `${JSON.stringify({
		type: "compaction",
		id,
		parentId,
		timestamp: "2026-01-01T00:00:02.000Z",
		summary,
		shortSummary: `${summary} (short)`,
		firstKeptEntryId: id,
		tokensBefore: 1000,
	})}\n`;
}

// A full current-version session body: title slot + header + the given entry
// lines (already newline-terminated).
function sessionFixture(entryLines: string[]): string {
	return `${titleSlot()}${sessionHeader()}${entryLines.join("")}`;
}

// An older-version (v2) session header. Resuming this triggers a v2→v3 load
// migration in the SDK — which sets #rewriteRequired as a DEFERRED flag and does
// NOT rewrite a non-empty file during load (session-manager.ts:1007), so the
// load still tees ZERO frames.
function sessionHeaderV2(): string {
	fixtureSeq += 1;
	return `${JSON.stringify({
		type: "session",
		version: 2,
		id: `fixture-${fixtureSeq}`,
		timestamp: "2026-01-01T00:00:00.000Z",
		cwd: process.cwd(),
	})}\n`;
}

// A full v2 session body: title slot + v2 header + entries. v2→v3 migration only
// renames hookMessage roles (session-migrations.ts), so a plain user line (which
// already carries the v2 id/parentId shape) migrates cleanly.
function sessionFixtureV2(entryLines: string[]): string {
	return `${titleSlot()}${sessionHeaderV2()}${entryLines.join("")}`;
}

// A user / assistant Message to drive through the real SessionManager. Only the
// fields the persistence path reads (role + content) matter; the wide pi-ai
// Message surface is never touched, so the cast names exactly what is used.
function userMsg(
	content: string,
): Parameters<SessionManager["appendMessage"]>[0] {
	return { role: "user", content } as Parameters<
		SessionManager["appendMessage"]
	>[0];
}

function assistantMsg(
	content: string,
): Parameters<SessionManager["appendMessage"]>[0] {
	return {
		role: "assistant",
		content: [{ type: "text", text: content }],
	} as Parameters<SessionManager["appendMessage"]>[0];
}

// The committed transcript frames (entry_json + checkpoint + entry_seq) that
// reached the carrier on the durable unary, in commit order.
function transcriptFramesOf(
	log: CarrierLog,
): { entryJson: string; checkpoint: boolean; entrySeq: bigint }[] {
	const out: { entryJson: string; checkpoint: boolean; entrySeq: bigint }[] =
		[];
	for (const req of log.durableFrames) {
		const inner = req.frame?.frame;
		if (inner?.case !== "transcriptEntry") continue;
		out.push({
			entryJson: inner.value.entryJson,
			checkpoint: inner.value.checkpoint,
			entrySeq: inner.value.entrySeq,
		});
	}
	return out;
}

// Reconstruct the session-JSONL body the way T5 does: take the latest checkpoint
// (a full-body snapshot, entryJson IS the whole file), then append every delta
// line committed AFTER it, ordered by entry_seq. A pure function over the frames.
function reconstructSessionBody(
	frames: { entryJson: string; checkpoint: boolean; entrySeq: bigint }[],
): string {
	let latestCheckpoint: { entryJson: string; entrySeq: bigint } | undefined;
	for (const f of frames) {
		if (
			f.checkpoint &&
			(!latestCheckpoint || f.entrySeq > latestCheckpoint.entrySeq)
		) {
			latestCheckpoint = { entryJson: f.entryJson, entrySeq: f.entrySeq };
		}
	}
	if (!latestCheckpoint)
		throw new Error("no checkpoint frame to reconstruct from");
	const laterDeltas = frames
		.filter((f) => !f.checkpoint && f.entrySeq > latestCheckpoint.entrySeq)
		.sort((l, r) =>
			l.entrySeq < r.entrySeq ? -1 : l.entrySeq > r.entrySeq ? 1 : 0,
		);
	return (
		latestCheckpoint.entryJson + laterDeltas.map((f) => f.entryJson).join("")
	);
}

// The message-entry contents from a getEntries() array, narrowing each entry
// with `in`/`typeof` (no fabricated inline shape).
function textsOf(entries: unknown[]): string[] {
	const out: string[] = [];
	for (const entry of entries) {
		if (!entry || typeof entry !== "object") continue;
		if (!("type" in entry) || entry.type !== "message") continue;
		if (
			!("message" in entry) ||
			!entry.message ||
			typeof entry.message !== "object"
		)
			continue;
		const message = entry.message;
		if (!("content" in message) || typeof message.content !== "string")
			continue;
		out.push(message.content);
	}
	return out;
}

// The compaction-entry summaries from a getEntries() array, in order.
function compactionSummariesOf(entries: unknown[]): string[] {
	const out: string[] = [];
	for (const entry of entries) {
		if (!entry || typeof entry !== "object") continue;
		if (!("type" in entry) || entry.type !== "compaction") continue;
		if (!("summary" in entry) || typeof entry.summary !== "string") continue;
		out.push(entry.summary);
	}
	return out;
}

// ── main(): wiring the Runner-mounted agent-config into createAgentSession ────
//
// The reader (config-reader.ts) maps the mount at COMPASS's fixed path into the
// three createAgentSession option surfaces. These run over the same MainDeps
// composition seam as the `main` tests above, but add two seams the reader
// needs: `configMount` points the reader at a tempdir fixture (the real
// /run/compass/agent-config does not exist off-container), and `connectMcp`
// stands in for the real MCPManager dial (a test cannot spawn MCP servers). What
// reaches createAgentSession is asserted via the createSession spy, mirroring
// the modelPattern/persona option tests.

// Write a file under `<mount>/current/<rel>`, creating parents — the layout the
// Runner materializes. Returns nothing; the caller holds the mount root.
function writeMount(mount: string, rel: string, body: string): void {
	const path = join(mount, "current", rel);
	mkdirSync(join(path, ".."), { recursive: true });
	writeFileSync(path, body);
}

function mountSkill(name: string): string {
	return `---\nname: ${name}\ndescription: ${name} skill\n---\n# ${name}\n`;
}

// The option fields the reader populates, captured off the createSession spy.
interface SeenConfig {
	skills?: unknown[];
	additionalExtensionPaths?: string[];
	disableExtensionDiscovery?: boolean;
	customTools?: unknown[];
	enableMCP?: boolean;
	autoApprove?: boolean;
	customSystemPrompt?: string;
	systemPrompt?:
		| string
		| string[]
		| ((defaultPrompt: string[]) => string | string[]);
}

// The `name` of each captured skill, narrowing with `in`/`typeof` (no fabricated
// inline cast) — mirrors the textsOf/compactionSummariesOf narrowing above.
function skillNames(skills: unknown[] | undefined): string[] {
	const out: string[] = [];
	for (const skill of skills ?? []) {
		if (!skill || typeof skill !== "object") continue;
		if (!("name" in skill) || typeof skill.name !== "string") continue;
		out.push(skill.name);
	}
	return out;
}

// The `name` of each captured custom tool — same `in`/`typeof` narrowing as
// skillNames, over the customTools option array.
function toolNames(tools: unknown[] | undefined): string[] {
	const out: string[] = [];
	for (const t of tools ?? []) {
		if (!t || typeof t !== "object") continue;
		if (!("name" in t) || typeof t.name !== "string") continue;
		out.push(t.name);
	}
	return out;
}

describe("main wires the mounted agent-config into createAgentSession", () => {
	test("a populated mount → skills, extension paths, and MCP tools all reach the options", async () => {
		const mount = scratch();
		writeMount(mount, "skills/alpha/SKILL.md", mountSkill("alpha"));
		writeMount(mount, "extensions/ext.ts", "export default {};\n");
		writeMount(
			mount,
			"mcp/srv.json",
			JSON.stringify({ mcpServers: { db: { command: "db-mcp" } } }),
		);

		const session = fakeSession();
		const mcpTools = [{ name: "db.query" }];
		let connectedWith: Record<string, unknown> | undefined;
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch() },
			{
				configMount: mount,
				connectMcp: (_cwd, mcp) => {
					connectedWith = mcp.configs;
					return Promise.resolve({
						tools: mcpTools as never,
						disconnect: () => Promise.resolve(),
					});
				},
				createSession: (options) => {
					seen.push({
						skills: options.skills,
						additionalExtensionPaths: options.additionalExtensionPaths,
						disableExtensionDiscovery: options.disableExtensionDiscovery,
						customTools: options.customTools,
						enableMCP: options.enableMCP,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);

		expect(seen).toHaveLength(1);
		const opts = seen[0];
		// skills: the mount's skill reached options.skills (a defined array skips
		// discovery). Non-vacuity: dropping `skills: mounted.skills` from cli.ts
		// leaves this undefined → red.
		expect(skillNames(opts.skills)).toEqual(["alpha"]);
		// extensions: the enumerated entry FILE reached additionalExtensionPaths,
		// with disableExtensionDiscovery pinning "exactly these, none else".
		expect(opts.additionalExtensionPaths).toEqual([
			join(mount, "current", "extensions", "ext.ts"),
		]);
		expect(opts.disableExtensionDiscovery).toBe(true);
		// MCP: the parsed config reached the connector, and its tools reached
		// customTools with enableMCP:false (never a passed mcpManager, which would
		// not surface its tools). The array now also carries the native
		// comms/lifecycle tools (merged in main), so this is a containment check,
		// not identity — the dedicated native-tools test below pins those.
		expect(connectedWith).toEqual({ db: { command: "db-mcp" } });
		expect(opts.customTools).toEqual(expect.arrayContaining(mcpTools));
		expect(opts.enableMCP).toBe(false);
	});

	test("the native comms + lifecycle tools reach customTools alongside the MCP tools", async () => {
		// gap-1 (SEA-1741): main constructs the comms/lifecycle brokers from the
		// existing transport and merges their tools into customTools so the
		// container agent can spawn/post. Derive the EXPECTED names at runtime from
		// the same factories main uses (a rename reddens here, never silently
		// skips), rather than hardcode-guessing them. The brokers are never called
		// during registration, so a stub transport whose bodies never run suffices.
		const fakeTransport = {
			comms: async () => ({}) as never,
			lifecycle: async () => ({}) as never,
			forge: async () => ({}) as never,
		};
		const expectedNames = [
			...createCommsTools(new CommsBroker(fakeTransport)),
			...createLifecycleTools(new LifecycleBroker(fakeTransport)),
			...createForgeTools(new ForgeBroker(fakeTransport)),
		].map((t) => t.name);

		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch() },
			{
				configMount: scratch(),
				connectMcp: () =>
					Promise.resolve({
						tools: [] as never,
						disconnect: () => Promise.resolve(),
					}),
				createSession: (options) => {
					seen.push({
						customTools: options.customTools,
						autoApprove: options.autoApprove,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);

		expect(seen).toHaveLength(1);
		const names = toolNames(seen[0].customTools);
		// Every native tool the factories produce reached customTools.
		for (const name of expectedNames) expect(names).toContain(name);
		// Discriminating anchors: two confirmed lifecycle names and two forge names.
		expect(names).toContain("agents_spawn_peer");
		expect(names).toContain("agents_despawn_peer");
		expect(names).toContain("forge_get_issue");
		expect(names).toContain("forge_create_pull_request");
		// Headless approval policy (SEA-1741): the entrypoint pins autoApprove so
		// the write-approval natives auto-execute with no human in the container.
		expect(seen[0].autoApprove).toBe(true);
	});

	test("every native's execute keeps arity 2 — tripwire on the customToolToDefinition arg-shuffle", () => {
		// SEA-1741 seam invariant. The natives are `AgentTool`s registered through
		// `customTools`; the SDK classifies a marker-less AgentTool as a CustomTool
		// and runs it through `customToolToDefinition`, which invokes `execute`
		// with the CustomTool arg order (toolCallId, params, onUpdate, ctx, signal)
		// — NOT the AgentTool order (toolCallId, params, signal, onUpdate, ctx). So
		// args 3-5 arrive SHUFFLED, and the wiring in cli.ts main() is sound ONLY
		// while no native reads past `params`. This is a TRIPWIRE, not a total
		// guard: pinning `execute.length === 2` reddens the LIKELY regression —
		// adding a plain positional 3rd param (`signal`) to consume a shuffled arg.
		// It does NOT catch a rest (`...args`) or defaulted (`signal = …`) param,
		// which read arg 3 while keeping `.length === 2`; the load-bearing guard is
		// the invariant itself (see cli.ts). If a native must consume its
		// AbortSignal, it cannot go through this seam — see the comment in cli.ts.
		const fakeTransport = {
			comms: async () => ({}) as never,
			lifecycle: async () => ({}) as never,
			forge: async () => ({}) as never,
		};
		const natives = [
			...createCommsTools(new CommsBroker(fakeTransport)),
			...createLifecycleTools(new LifecycleBroker(fakeTransport)),
			...createForgeTools(new ForgeBroker(fakeTransport)),
		];
		expect(natives).toHaveLength(17);
		for (const tool of natives) {
			expect({ name: tool.name, arity: tool.execute.length }).toEqual({
				name: tool.name,
				arity: 2,
			});
		}
	});

	test("an UNCONFIGURED mount → skills [], no extension paths, no MCP tools, and main resolves", async () => {
		// A present-but-empty mount root (no current/). The default connectMcp runs
		// (empty configs → no dial, empty tools), so this exercises the real
		// connect path's empty branch too, not just a fake.
		const mount = scratch();
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await expect(
			main(
				{ HOME: scratch() },
				{
					configMount: mount,
					createSession: (options) => {
						seen.push({
							skills: options.skills,
							additionalExtensionPaths: options.additionalExtensionPaths,
							disableExtensionDiscovery: options.disableExtensionDiscovery,
							customTools: options.customTools,
							enableMCP: options.enableMCP,
						});
						return Promise.resolve({
							session: session as unknown as AgentSession,
						});
					},
					createTransport: () =>
						fakeCarrier(emptyLog(), { control: emptyControlStream }),
				},
			),
		).resolves.toBeUndefined();

		expect(seen).toHaveLength(1);
		// The unconfigured→none guarantee: an explicit empty skills array (skips
		// discovery), no extension paths, no MCP tools. Non-vacuity: a reader that
		// left skills undefined would let the SDK discover ambient skills → red.
		expect(seen[0].skills).toEqual([]);
		expect(seen[0].additionalExtensionPaths).toEqual([]);
		expect(seen[0].disableExtensionDiscovery).toBe(true);
		// No MCP tools (empty mount → empty connect), but the comms/lifecycle/forge
		// natives are ALWAYS merged in (SEA-1741/RIG-2672) — so customTools carries
		// exactly those, and never a discovered MCP tool.
		expect(toolNames(seen[0].customTools)).toContain("agents_spawn_peer");
		expect(seen[0].customTools).toHaveLength(17);
		expect(seen[0].enableMCP).toBe(false);
	});

	test("a partial mount (skills only) → only skills populated", async () => {
		const mount = scratch();
		writeMount(mount, "skills/only/SKILL.md", mountSkill("only"));
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch() },
			{
				configMount: mount,
				createSession: (options) => {
					seen.push({
						skills: options.skills,
						additionalExtensionPaths: options.additionalExtensionPaths,
						customTools: options.customTools,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(skillNames(seen[0].skills)).toEqual(["only"]);
		expect(seen[0].additionalExtensionPaths).toEqual([]);
		// No MCP tools from a skills-only mount, but the natives always merge in
		// (SEA-1741/RIG-2672) — so customTools is exactly the comms/lifecycle/forge
		// natives.
		expect(toolNames(seen[0].customTools)).toContain("comms_post_message");
		expect(seen[0].customTools).toHaveLength(17);
	});

	// ── SEA-1732 T10: COMPASS_ROLE → prompts/<role>/SYSTEM.md → customSystemPrompt ──
	//
	// The role selector delivers a per-role block-0 as `customSystemPrompt` (which
	// REPLACES OMP's default block-0), while persona STILL appends AFTER (record
	// §OQ-8). These pin the four compose states at the createSession seam; the
	// MP-1 render property (skills/rules/footer survival + read-tool gate) is the
	// SDK-render test that follows.
	test("COMPASS_ROLE with a shipped prompt → its text reaches customSystemPrompt", async () => {
		const mount = scratch();
		writeMount(
			mount,
			"prompts/manager/SYSTEM.md",
			"# Manager\nYou coordinate the fleet.\n",
		);
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch(), COMPASS_ROLE: "manager" },
			{
				configMount: mount,
				createSession: (options) => {
					seen.push({
						customSystemPrompt: options.customSystemPrompt,
						systemPrompt: options.systemPrompt,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		// The role prompt's TEXT (block-0 replacement) reached customSystemPrompt.
		// Non-vacuity: dropping the `customSystemPrompt: rolePrompt` spread leaves
		// this undefined → red.
		expect(seen[0].customSystemPrompt).toBe(
			"# Manager\nYou coordinate the fleet.\n",
		);
		// No persona set → no append customizer; behavior is role-replace only.
		expect(seen[0].systemPrompt).toBeUndefined();
	});

	test("COMPASS_ROLE set but NO prompt file → falls back to today's behavior (no customSystemPrompt)", async () => {
		// A role the operator set but the config bundle never shipped a
		// prompts/<role>/SYSTEM.md for. The reader returns undefined, so main must
		// OMIT customSystemPrompt entirely — never inject an empty replace, which
		// would still route through the block-0-replacing custom template.
		const mount = scratch();
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch(), COMPASS_ROLE: "ghost" },
			{
				configMount: mount,
				createSession: (options) => {
					seen.push({ customSystemPrompt: options.customSystemPrompt });
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		expect(seen[0].customSystemPrompt).toBeUndefined();
	});

	test("COMPASS_ROLE with a path-traversal label → rejected, no customSystemPrompt", async () => {
		// A role is a flat directory name; a label carrying a separator or `..`
		// must never traverse outside prompts/. The decoy sits at current/SYSTEM.md
		// — exactly where role="../" resolves (join(current, "prompts", "../",
		// "SYSTEM.md") = current/SYSTEM.md) — so WITHOUT the guard the traversal
		// would find it and inject it as block-0 (customSystemPrompt defined). The
		// guard rejects the label first, so main falls back to today's behavior.
		// This placement is what makes the test non-vacuous: drop the guard and it
		// fails. Defense in depth: role is store-set out-of-band today, but the
		// guard holds the moment a client-facing setter lands.
		const mount = scratch();
		writeMount(mount, "SYSTEM.md", "# Escaped\n");
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch(), COMPASS_ROLE: "../" },
			{
				configMount: mount,
				createSession: (options) => {
					seen.push({ customSystemPrompt: options.customSystemPrompt });
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		expect(seen[0].customSystemPrompt).toBeUndefined();
	});

	test("COMPASS_ROLE unset → no customSystemPrompt (exactly today's behavior)", async () => {
		const mount = scratch();
		writeMount(mount, "prompts/manager/SYSTEM.md", "# Manager\n");
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{ HOME: scratch() },
			{
				configMount: mount,
				createSession: (options) => {
					seen.push({ customSystemPrompt: options.customSystemPrompt });
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		// Non-vacuity: reading the role prompt unconditionally (not gated on a set
		// role) would surface the shipped manager prompt here → red.
		expect(seen[0].customSystemPrompt).toBeUndefined();
	});

	test("role + persona compose: role reaches customSystemPrompt AND persona appends after", async () => {
		// The OQ-8 composition: customSystemPrompt (role, REPLACE block-0) and the
		// systemPrompt append customizer (persona) are ORTHOGONAL keys, so both
		// apply. The customizer runs over whatever default array the SDK built —
		// which, with a role, already carries the role block-0 — so persona lands
		// LAST, after the role block. Drive the customizer with a fake default that
		// stands in for [role block-0, …skills/rules, project footer].
		const mount = scratch();
		writeMount(mount, "prompts/manager/SYSTEM.md", "# Manager block-0\n");
		const session = fakeSession();
		const seen: SeenConfig[] = [];
		await main(
			{
				HOME: scratch(),
				COMPASS_ROLE: "manager",
				COMPASS_PERSONA: "You are Ada.",
			},
			{
				configMount: mount,
				createSession: (options) => {
					seen.push({
						customSystemPrompt: options.customSystemPrompt,
						systemPrompt: options.systemPrompt,
					});
					return Promise.resolve({
						session: session as unknown as AgentSession,
					});
				},
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		expect(seen).toHaveLength(1);
		expect(seen[0].customSystemPrompt).toBe("# Manager block-0\n");
		const customizer = seen[0].systemPrompt;
		if (typeof customizer !== "function") {
			throw new Error("systemPrompt was not the append customizer function");
		}
		// Persona appends AFTER the role block (and everything else the SDK built).
		expect(
			customizer(["# Manager block-0", "skills+rules", "project footer"]),
		).toEqual([
			"# Manager block-0",
			"skills+rules",
			"project footer",
			"You are Ada.",
		]);
	});

	// ── MP-1 PROPERTY (frozen record §MP-1) ───────────────────────────────────
	//
	// Passing a role prompt as `customSystemPrompt` REPLACES OMP's block-0 — but
	// the SDK's custom-system-prompt template STILL auto-injects skills + rules,
	// and the project footer stays a separate block. Two pins the record names:
	//   (1) skills injection is GATED on the `read` tool being in the tool set
	//       (system-prompt.ts:819-820) — so the tool set MUST retain `read`;
	//   (2) the rendered prompt RETAINS skills + rules + the project footer even
	//       though block-0 is the role text, not the default.
	// This is a real SDK render (buildSystemPrompt), not a seam spy: it exercises
	// the actual template the SDK routes customSystemPrompt through.
	test("MP-1: a role prompt as customSystemPrompt REPLACES block-0 while skills, rules, and the project footer survive (read-tool gate held)", async () => {
		const roleBlock0 = "ROLE-BLOCK-0-SENTINEL: you are the manager.";
		const { systemPrompt } = await buildSystemPrompt({
			cwd: scratch(),
			// The role prompt injected as the block-0 replacement.
			resolvedCustomPrompt: roleBlock0,
			// The read tool is the skills-injection gate (system-prompt.ts:819).
			toolNames: ["read"],
			skills: [
				{
					name: "mp1-skill",
					path: "/mnt/skills/mp1-skill/SKILL.md",
					content: "# skill",
					level: "user",
					_source: {
						provider: "compass-config",
						path: "/mnt/skills/mp1-skill/SKILL.md",
						level: "user",
					},
				},
			] as never,
			rules: [
				{
					name: "mp1-rule",
					description: "MP1-RULE-SENTINEL constraint",
					path: "/mnt/rules/mp1-rule.md",
				},
			],
		});
		const rendered = systemPrompt.join("\n\n");
		// (1) block-0 is REPLACED: the role text is present…
		expect(rendered).toContain(roleBlock0);
		// …and the default block-0's opening sentinel is GONE.
		expect(rendered).not.toContain(
			"You are a helpful assistant the team trusts with load-bearing changes",
		);
		// (2) skills survived (the custom template's <skills> list) — this is the
		// read-tool gate holding: drop `read` from toolNames and this vanishes.
		expect(rendered).toContain("mp1-skill");
		expect(rendered).toContain("<skills>");
		// (2) rules survived (the custom template's <rules> list).
		expect(rendered).toContain("MP1-RULE-SENTINEL");
		expect(rendered).toContain("<rules>");
		// (2) the project footer survived as its own block (environment + cwd).
		expect(rendered).toContain("PROJECT");
		expect(rendered).toContain("current working directory");
		// The read tool stayed in the set (the gate's precondition).
		expect(systemPrompt.join("\n")).toContain("read");
	});

	test("MP-1 gate: WITHOUT the read tool the custom template drops the skills list (proves the gate is live)", async () => {
		// The non-vacuity companion to the property above: skills injection is
		// GATED on `read` (system-prompt.ts:819-820). With no read tool the same
		// role-as-customSystemPrompt render must NOT carry the skills list — so the
		// Compass tool set retaining `read` is load-bearing, not incidental.
		const { systemPrompt } = await buildSystemPrompt({
			cwd: scratch(),
			resolvedCustomPrompt: "ROLE-BLOCK-0-SENTINEL",
			toolNames: [],
			skills: [
				{
					name: "mp1-skill",
					path: "/mnt/skills/mp1-skill/SKILL.md",
					content: "# skill",
					level: "user",
					_source: {
						provider: "compass-config",
						path: "/mnt/skills/mp1-skill/SKILL.md",
						level: "user",
					},
				},
			] as never,
		});
		const rendered = systemPrompt.join("\n\n");
		expect(rendered).not.toContain("mp1-skill");
		expect(rendered).not.toContain("<skills>");
	});

	// The MCP manager teardown — what main() alone owns (the SDK never
	// disconnects a manager it did not build). disconnect must run on BOTH the
	// clean and error paths, or the container leaks every MCP subprocess/HTTP
	// session on exit.
	test("disconnects the MCP manager on the clean teardown path", async () => {
		const mount = scratch();
		writeMount(
			mount,
			"mcp/srv.json",
			JSON.stringify({ mcpServers: { db: { command: "db-mcp" } } }),
		);
		let disconnected = false;
		const session = fakeSession();
		await main(
			{ HOME: scratch() },
			{
				configMount: mount,
				connectMcp: () =>
					Promise.resolve({
						tools: [],
						disconnect: () => {
							disconnected = true;
							return Promise.resolve();
						},
					}),
				createSession: () =>
					Promise.resolve({ session: session as unknown as AgentSession }),
				createTransport: () =>
					fakeCarrier(emptyLog(), { control: emptyControlStream }),
			},
		);
		// Non-vacuity: dropping `await mcp.disconnect()` from the teardown leaves
		// this false → red.
		expect(disconnected).toBe(true);
	});

	test("disconnects the MCP manager on the error teardown path (and still propagates)", async () => {
		const mount = scratch();
		writeMount(
			mount,
			"mcp/srv.json",
			JSON.stringify({ mcpServers: { db: { command: "db-mcp" } } }),
		);
		const boom = new Error("SDK prompt failed mid-turn");
		let disconnected = false;
		const session = fakeSession({ promptError: boom });
		const carrier = fakeCarrier(emptyLog(), {
			control: async function* () {
				yield replayCompleteOp(1n);
				yield promptOp(2n, "go");
			},
		});
		await expect(
			main(
				{ HOME: scratch() },
				{
					configMount: mount,
					connectMcp: () =>
						Promise.resolve({
							tools: [],
							disconnect: () => {
								disconnected = true;
								return Promise.resolve();
							},
						}),
					createSession: () =>
						Promise.resolve({ session: session as unknown as AgentSession }),
					createTransport: () => carrier,
				},
			),
		).rejects.toBe(boom);
		// Non-vacuity: a disconnect that only ran on the clean path leaves this
		// false → red.
		expect(disconnected).toBe(true);
	});
});
