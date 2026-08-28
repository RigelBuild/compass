// SEA-1678 T4 — the entrypoint's fleet-config passthrough (design
// compass-agent-config-passthrough §CP-1/CP-2/CP-4), object-injection variant.
//
// Matt's pivot: the Runner-mounted bundle stays the delivery vehicle, but the
// agent CONSUMES it by OBJECT INJECTION through the `createAgentSession` seams the
// runtime SDK (16.5.2, the installed .bun pkg) actually exposes — not the inert
// PI_CONFIG_FILES env path the original CP-1 named:
//
//   - settings/config.yml (CP-1) → `main` builds a `Settings` overlay
//     (`buildFleetSettings`) and passes it as `settingsManager`. The overlay
//     layer deep-merges AFTER global+project, so a fleet key beats the checkout's
//     project value — asserted directly on the built Settings, and end-to-end
//     through the SDK's resolution (a) via the createSession seam.
//   - rules (CP-4) → `main` composes: builds the fleet `Rule[]` and re-runs
//     the SDK's own rule discovery itself, passing `[...fleet, ...discovered]`
//     (fleet-first, both levels load).
//   - AGENTS.md (CP-2) → `main` composes: `contextFiles: [fleetGlobal,
//     ...discovered]` (fleet re-runs the cwd walk-up itself), ONLY when present.
//   - agents/ + models.yml (CP-4) → NO object seam, so they stay FILESYSTEM-based
//     via `ensureAgentDirLink` symlinks into $HOME/.omp/agent. The symlink
//     mechanics (d–f) are pinned directly; (g) is proven through real SDK
//     `discoverAgents` in a HOME-frozen subprocess.
//
// The createSession seam (MainDeps.createSession) captures the options `main`
// hands the SDK, so b–f assert on what reaches the session WITHOUT constructing a
// real one. Every fixture is a real tempdir, torn down after each test; no
// timers, no sleeps, no retries — deterministic FS fixtures only.

import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
	lstatSync,
	mkdirSync,
	mkdtempSync,
	readlinkSync,
	rmSync,
	symlinkSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { create } from "@bufbuild/protobuf";
import type {
	AgentSession,
	CreateAgentSessionOptions,
} from "@oh-my-pi/pi-coding-agent";
import { Settings } from "@oh-my-pi/pi-coding-agent";
import {
	buildFleetSettings,
	ensureAgentDirLink,
	type MainDeps,
	main,
} from "./cli";
import type { AgentControl as WireAgentControl } from "./compassv1";
import { PostConversationFrameResponseSchema } from "./gen/compass/v1/agent_gateway_pb";
import type { RunnerTransport } from "./transport/index";
import { createPublishSpine } from "./transport/publish-spine";

const tmpdirs: string[] = [];

function scratch(): string {
	const dir = mkdtempSync(join(tmpdir(), "compass-passthrough-"));
	tmpdirs.push(dir);
	return dir;
}

let savedHome: string | undefined;
beforeEach(() => {
	savedHome = process.env.HOME;
});

afterEach(() => {
	if (savedHome === undefined) delete process.env.HOME;
	else process.env.HOME = savedHome;
	for (const dir of tmpdirs.splice(0)) {
		rmSync(dir, { recursive: true, force: true });
	}
});

// A mount fixture: <mount>/current/<rel> holding `body`, parents created.
function writeMember(mount: string, rel: string, body: string): string {
	const path = join(mount, "current", rel);
	mkdirSync(join(path, ".."), { recursive: true });
	writeFileSync(path, body);
	return path;
}

function mkdirMember(mount: string, rel: string): string {
	const path = join(mount, "current", rel);
	mkdirSync(path, { recursive: true });
	return path;
}

// ── The createSession seam: capture the options `main` hands the SDK ──────────
//
// A minimal fake AgentSession + RunnerTransport, standing in for the two
// unfakeable constructors so `main` runs its full composition and its
// createAgentSession call lands on the recorder. The transport reuses the REAL
// PublishSpine (as createUnixSocketTransport does) so the sink/control wiring is
// production code; only the RPC handlers and the session are faked.

function fakeSession(): AgentSession {
	const gate = Promise.withResolvers<() => void>();
	const agent = {
		prompt: () => Promise.resolve(),
		steer: () => {},
		deliver: () => {},
		appendMessage: () => {},
		setSystemPrompt: () => {},
		setTools: () => {},
		getApiKey: undefined as
			| ((model: unknown) => Promise<string | undefined>)
			| undefined,
	};
	const session = {
		agent,
		// The boot-model-health belt reads these: a clean registry (no swallowed
		// config error) and a resolved model, so the belt is a no-op for every
		// config-passthrough fixture (none pin an unresolvable model).
		modelRegistry: { getError: () => undefined },
		model: { id: "resolved" },
		subscribe(fn: () => void): () => void {
			gate.resolve(fn);
			return () => {};
		},
	};
	return session as unknown as AgentSession;
}

function fakeCarrier(): RunnerTransport {
	const spine = createPublishSpine(async (stream) => {
		for await (const _ of stream) {
			// drain the publish stream so the spine's pump completes cleanly
		}
	});
	return {
		comms: () => Promise.reject(new Error("comms is not used by main")),
		lifecycle: () => Promise.reject(new Error("lifecycle is not used by main")),
		forge: () => Promise.reject(new Error("forge is not used by main")),
		publishSpine: () => spine,
		postConversationFrame: () =>
			Promise.resolve(create(PostConversationFrameResponseSchema, {})),
		control: () => (async function* (): AsyncGenerator<WireAgentControl> {})(),
		close: () => {},
	};
}

// Run `main` over a tempdir mount, capturing the createAgentSession options.
async function runMainOverMount(
	mount: string,
	env: Record<string, string | undefined> = {},
): Promise<CreateAgentSessionOptions> {
	const home = scratch();
	process.env.HOME = home;
	let captured: CreateAgentSessionOptions | undefined;
	const deps: MainDeps = {
		configMount: mount,
		createSession: (options) => {
			captured = options;
			return Promise.resolve({ session: fakeSession() });
		},
		createTransport: () => fakeCarrier(),
	};
	await main({ HOME: home, ...env }, deps);
	if (captured === undefined) {
		throw new Error("main never called createSession");
	}
	return captured;
}

// ── buildFleetSettings: CP-1 overlay build, parse-guarded (OQ-7) ──────────────

describe("buildFleetSettings", () => {
	// (a-unit) present + parseable ⇒ a Settings whose overlay layer beats the
	// checkout's project layer. This is the CP-1 acceptance at the unit: the fleet
	// key resolves OVER the project value the SDK would otherwise pick.
	test("builds a Settings whose fleet overlay beats the project value", async () => {
		const mount = scratch();
		const cwd = scratch();
		mkdirSync(join(cwd, ".omp"), { recursive: true });
		writeFileSync(
			join(cwd, ".omp", "config.yml"),
			"compaction:\n  keepRecentTokens: 111\n",
		);
		const settingsPath = writeMember(
			mount,
			"settings/config.yml",
			"compaction:\n  keepRecentTokens: 222\n",
		);
		const settings = await buildFleetSettings(cwd, scratch(), settingsPath);
		expect(settings).toBeDefined();
		// The overlay (222) wins over the project layer (111). Non-vacuity: a
		// build that ignored the overlay would resolve 111 → red.
		expect(settings?.get("compaction.keepRecentTokens")).toBe(222);
	});

	// undefined path ⇒ undefined (unconfigured boot, SDK inits its own default).
	test("returns undefined when no settings member is present", async () => {
		const settings = await buildFleetSettings(scratch(), scratch(), undefined);
		expect(settings).toBeUndefined();
	});

	// present but Bun-unparseable ⇒ undefined (fail-open, OQ-7): a member the Go
	// door admitted but Bun rejects must NOT crash the boot. Non-vacuity: a build
	// that fed the bad file straight to a strict Settings.loadIsolated would
	// throw → the test's toBeUndefined assertion never reached.
	test("returns undefined on an unparseable member (fail-open, OQ-7)", async () => {
		const mount = scratch();
		const settingsPath = writeMember(
			mount,
			"settings/config.yml",
			"compaction:\n\t- : : broken\n  : nope\n::\n",
		);
		const settings = await buildFleetSettings(
			scratch(),
			scratch(),
			settingsPath,
		);
		expect(settings).toBeUndefined();
	});
});

// ── main's object injection reaches createAgentSession (createSession seam) ────

describe("main injects fleet config as objects into createAgentSession", () => {
	// (a) settings: a parseable member ⇒ a Settings overlay reaches the session as
	// settingsManager, and it resolves the fleet key over the project value.
	test("(a) a settings member reaches the session as an overlay that beats project", async () => {
		const mount = scratch();
		const cwd = scratch();
		mkdirSync(join(cwd, ".omp"), { recursive: true });
		writeFileSync(
			join(cwd, ".omp", "config.yml"),
			"compaction:\n  keepRecentTokens: 111\n",
		);
		writeMember(
			mount,
			"settings/config.yml",
			"compaction:\n  keepRecentTokens: 222\n",
		);
		const options = await runMainOverMount(mount, { COMPASS_WORKDIR: cwd });
		const settingsManager = options.settingsManager;
		expect(settingsManager).toBeDefined();
		const resolved = await settingsManager;
		expect(resolved).toBeInstanceOf(Settings);
		expect((resolved as Settings).get("compaction.keepRecentTokens")).toBe(222);
	});

	// (b) no settings member ⇒ no settingsManager key (SDK inits its own default).
	test("(b) an absent settings member leaves settingsManager unset", async () => {
		const mount = scratch();
		mkdirMember(mount, "skills"); // a populated-but-settings-less mount
		const options = await runMainOverMount(mount);
		expect(options.settingsManager).toBeUndefined();
	});

	// (c) rules COMPOSE (Matt-decided): the fleet rules are a GLOBAL/user set that
	// must COMPOSE with — not replace — the checkout's own discovered rules. With
	// BOTH fleet members AND a project .omp/rules/ rule in cwd, the session's
	// `rules` carries BOTH, fleet entries FIRST (least prominent).
	test("(c) mounted rules compose with the checkout's discovered rules, fleet first", async () => {
		const mount = scratch();
		const cwd = scratch();
		writeMember(
			mount,
			"rules/alpha.md",
			"---\ndescription: alpha\n---\nalpha body\n",
		);
		writeMember(mount, "rules/beta.mdc", "---\ndescription: beta\n---\nbeta\n");
		// The checkout's own project-level rule (builtin discovery: .omp/rules/).
		mkdirSync(join(cwd, ".omp", "rules"), { recursive: true });
		writeFileSync(
			join(cwd, ".omp", "rules", "checkout.md"),
			"---\ndescription: checkout\n---\ncheckout body\n",
		);
		const options = await runMainOverMount(mount, { COMPASS_WORKDIR: cwd });
		const names = options.rules?.map((r) => r.name) ?? [];
		// Both present — compose, not replace. Non-vacuity: replace semantics
		// (rules = mounted.rules only) drops the checkout rule → this fails.
		expect(names).toContain("alpha");
		expect(names).toContain("beta");
		expect(names).toContain("checkout");
		// Fleet entries come FIRST (least prominent).
		expect(names.slice(0, 2)).toEqual(["alpha", "beta"]);
		expect(names.indexOf("checkout")).toBeGreaterThan(names.indexOf("beta"));
	});

	test("(c) an unconfigured mount still composes the checkout's discovered rules", async () => {
		const mount = scratch();
		mkdirMember(mount, "skills");
		const cwd = scratch();
		mkdirSync(join(cwd, ".omp", "rules"), { recursive: true });
		writeFileSync(
			join(cwd, ".omp", "rules", "checkout.md"),
			"---\ndescription: checkout\n---\ncheckout body\n",
		);
		const options = await runMainOverMount(mount, { COMPASS_WORKDIR: cwd });
		// Empty fleet set still composes cleanly: the checkout rule is present.
		expect(options.rules?.map((r) => r.name)).toContain("checkout");
	});

	// (c) AGENTS.md COMPOSE (Matt-decided): the fleet AGENTS.md is a GLOBAL/user
	// file that must COMPOSE with — not replace — the checkout's project AGENTS.md.
	// With BOTH a fleet member AND a project .omp/AGENTS.md in cwd, the session's
	// contextFiles carries BOTH, fleet global FIRST (least prominent). Absent
	// fleet ⇒ key omitted ⇒ SDK discovers the project file itself.
	test("(c) a fleet AGENTS.md composes with the checkout's project AGENTS.md, fleet first", async () => {
		const mount = scratch();
		const cwd = scratch();
		// The checkout's own project-level AGENTS.md (builtin discovery: .omp/).
		mkdirSync(join(cwd, ".omp"), { recursive: true });
		const projectAgents = join(cwd, ".omp", "AGENTS.md");
		writeFileSync(projectAgents, "# project conventions\n");
		const fleetAgents = writeMember(
			mount,
			"AGENTS.md",
			"# fleet conventions\n",
		);
		const options = await runMainOverMount(mount, { COMPASS_WORKDIR: cwd });
		const paths = (options.contextFiles ?? []).map((f) => f.path);
		// Both present — compose, not replace. Non-vacuity: passing only [fleet]
		// (no compose) drops the project entry → this fails.
		expect(paths).toContain(fleetAgents);
		expect(paths).toContain(projectAgents);
		// The fleet global is FIRST (least prominent; closer-to-cwd sorts later).
		expect(paths[0]).toBe(fleetAgents);
	});

	test("(c) an absent fleet AGENTS.md leaves contextFiles unset (SDK discovers project itself)", async () => {
		const mount = scratch();
		mkdirMember(mount, "skills");
		const options = await runMainOverMount(mount);
		expect(options.contextFiles).toBeUndefined();
	});
});

// ── ensureAgentDirLink: CP-4 filesystem members (agents, models.yml) ──────────
//
// Only the two members with NO object seam still symlink into $HOME/.omp/agent.
// The link mechanics (d–f) are pinned directly against lstat/readlink.

describe("ensureAgentDirLink (agents/models.yml — the filesystem members)", () => {
	// (d) target set ⇒ a symlink at $HOME/.omp/agent/<entry> pointing through the
	// mount's current/-relative member. Covers a dir entry (agents) and a file
	// entry (models.yml) — one code path.
	test("(d) target set links a directory entry (agents) through current/", async () => {
		const home = scratch();
		const mount = scratch();
		const target = mkdirMember(mount, "agents");
		await ensureAgentDirLink(home, "agents", target);
		const link = join(home, ".omp", "agent", "agents");
		expect(lstatSync(link).isSymbolicLink()).toBe(true);
		expect(readlinkSync(link)).toBe(target);
		expect(readlinkSync(link)).toContain(join("current"));
	});

	test("(d) target set links a file entry (models.yml) through current/", async () => {
		const home = scratch();
		const mount = scratch();
		const target = writeMember(mount, "models.yml", "providers: {}\n");
		await ensureAgentDirLink(home, "models.yml", target);
		const link = join(home, ".omp", "agent", "models.yml");
		expect(lstatSync(link).isSymbolicLink()).toBe(true);
		expect(readlinkSync(link)).toBe(target);
	});

	// Idempotent: a second call with the same target keeps one correct link.
	test("is idempotent — re-running with the same target keeps one correct link", async () => {
		const home = scratch();
		const mount = scratch();
		const target = writeMember(mount, "models.yml", "providers: {}\n");
		await ensureAgentDirLink(home, "models.yml", target);
		await ensureAgentDirLink(home, "models.yml", target);
		const link = join(home, ".omp", "agent", "models.yml");
		expect(lstatSync(link).isSymbolicLink()).toBe(true);
		expect(readlinkSync(link)).toBe(target);
	});

	// A stale Compass-owned symlink is repointed at the new target.
	test("repoints a stale Compass-owned symlink at the new target", async () => {
		const home = scratch();
		const mount = scratch();
		const dir = join(home, ".omp", "agent");
		mkdirSync(dir, { recursive: true });
		symlinkSync("/some/old/agents", join(dir, "agents"));
		const target = mkdirMember(mount, "agents");
		await ensureAgentDirLink(home, "agents", target);
		expect(readlinkSync(join(dir, "agents"))).toBe(target);
	});

	// (e) member removed on a re-run ⇒ the Compass-owned link is removed.
	test("(e) target unset removes a Compass-owned symlink", async () => {
		const home = scratch();
		const mount = scratch();
		const target = mkdirMember(mount, "agents");
		await ensureAgentDirLink(home, "agents", target);
		const link = join(home, ".omp", "agent", "agents");
		expect(lstatSync(link).isSymbolicLink()).toBe(true);
		await ensureAgentDirLink(home, "agents", undefined);
		expect(() => lstatSync(link)).toThrow();
	});

	// (f) a pre-existing REAL DIRECTORY at a dir entry (agents) survives BOTH the
	// create path (target set) and the remove path (target unset) — a user-placed
	// dir always wins, and is never clobbered nor deleted.
	test("(f) a pre-existing real directory (agents) survives create and remove paths", async () => {
		const home = scratch();
		const mount = scratch();
		const dir = join(home, ".omp", "agent");
		const userAgents = join(dir, "agents");
		mkdirSync(userAgents, { recursive: true });
		writeFileSync(join(userAgents, "local.md"), "local agent\n");
		const target = mkdirMember(mount, "agents");
		await ensureAgentDirLink(home, "agents", target);
		expect(lstatSync(userAgents).isDirectory()).toBe(true);
		expect(lstatSync(userAgents).isSymbolicLink()).toBe(false);
		await ensureAgentDirLink(home, "agents", undefined);
		expect(lstatSync(userAgents).isDirectory()).toBe(true);
		expect(lstatSync(join(userAgents, "local.md")).isFile()).toBe(true);
	});

	// (f, file variant) a pre-existing REGULAR FILE at models.yml survives both.
	test("(f) a pre-existing regular file (models.yml) survives create and remove paths", async () => {
		const home = scratch();
		const mount = scratch();
		const dir = join(home, ".omp", "agent");
		mkdirSync(dir, { recursive: true });
		const userFile = join(dir, "models.yml");
		writeFileSync(userFile, "user models\n");
		const target = writeMember(mount, "models.yml", "providers: {}\n");
		await ensureAgentDirLink(home, "models.yml", target);
		expect(lstatSync(userFile).isFile()).toBe(true);
		expect(lstatSync(userFile).isSymbolicLink()).toBe(false);
		await ensureAgentDirLink(home, "models.yml", undefined);
		expect(lstatSync(userFile).isFile()).toBe(true);
	});

	// target unset with NO existing entry is a clean no-op (unconfigured boot).
	test("target unset with nothing present is a no-op", async () => {
		const home = scratch();
		const link = join(home, ".omp", "agent", "agents");
		await ensureAgentDirLink(home, "agents", undefined);
		expect(() => lstatSync(link)).toThrow();
	});
});

// ── (g): the SDK actually discovers a mounted subagent (launch-frozen HOME) ────
//
// Subagent defs are the ONE fleet member with no object seam AND a home-anchored
// discovery: `discoverAgents` reads the user-level agent dir off os.homedir(),
// which Bun freezes at module load. Only a fresh process with HOME set at launch
// makes discovery hermetic — so this spawns config-passthrough-probe.ts, which
// runs `main`'s exact agents symlink effect then queries the SDK by name.

interface ProbeResult {
	subagentFound: boolean;
}

async function runProbe(env: Record<string, string>): Promise<ProbeResult> {
	const proc = Bun.spawn(
		["bun", join(import.meta.dir, "config-passthrough-probe.ts")],
		{
			env: { ...process.env, ...env },
			stdout: "pipe",
			stderr: "pipe",
		},
	);
	const [stdout, stderr] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
	]);
	await proc.exited;
	const line = stdout.split("\n").find((l) => l.startsWith("PROBE_RESULT:"));
	if (!line) {
		throw new Error(
			`probe produced no PROBE_RESULT (exit ${proc.exitCode})\nstdout:\n${stdout}\nstderr:\n${stderr}`,
		);
	}
	return JSON.parse(line.slice("PROBE_RESULT:".length)) as ProbeResult;
}

function probeFixture(): { home: string; mount: string; cwd: string } {
	return { home: scratch(), mount: scratch(), cwd: scratch() };
}

describe("(g) the SDK resolves a mounted subagent by name (subprocess, HOME-frozen)", () => {
	test("with the agents symlink in place, discoverAgents finds the mounted subagent", async () => {
		const { home, mount, cwd } = probeFixture();
		writeMember(
			mount,
			"agents/probeagent.md",
			"---\nname: probeagent\ndescription: a probe subagent\n---\nbody\n",
		);
		const result = await runProbe({
			HOME: home,
			PROBE_MOUNT: mount,
			PROBE_CWD: cwd,
			PROBE_SUBAGENT_NAME: "probeagent",
		});
		expect(result.subagentFound).toBe(true);
		// The probe forks a subprocess that cold-imports the SDK to run the real
		// discoverAgents walk (no object seam for agents/, per §CP-4). Warm that
		// is ~1.3s, but a cold, contended CI runner blows the 5s default; a
		// generous hard bound absorbs the cold-start variance without a retry.
	}, 20_000);

	// The remove path, end-to-end: an unconfigured mount leaves nothing for
	// discovery to find (no dangling link, no stale content).
	test("an unconfigured mount leaves the SDK discovering no fleet subagent", async () => {
		const { home, mount, cwd } = probeFixture();
		mkdirSync(join(mount, "current"), { recursive: true });
		const result = await runProbe({
			HOME: home,
			PROBE_MOUNT: mount,
			PROBE_CWD: cwd,
			PROBE_SUBAGENT_NAME: "probeagent",
		});
		expect(result.subagentFound).toBe(false);
	}, 20_000);
});

// ── SEA-1678 T6: the Reload RE-READ (the record's load-bearing acceptance) ─────
//
// The record's acceptance is that the update path is proven on the agent's
// OBSERVED value, not just a flipped symlink: after a ConfigVersion Reload the
// re-exec'd `main()` must build its createAgentSession objects from the NEW mount
// content — a fresh Settings overlay resolving the updated value, recomposed
// rules carrying the updated body, updated contextFiles. A Reload re-execs
// `main()`, and the reader reads through the mount's `current/` view, so the
// faithful model is: boot once over the mount, capture the injected objects,
// CHANGE a member's content in `current/` (the re-materialized new version the
// Runner flips `current` to), re-run `main()` over the SAME mount + HOME + cwd,
// and assert the SECOND createAgentSession call carries the new content. A stale
// object (a cached read, a boot-once assumption) would carry the old value → red.

// Run `main` twice over the same mount/home/cwd, applying `mutate` to the mount
// between the two boots (the ConfigVersion re-materialize). Returns both captured
// createAgentSession option sets so a test can assert the second observed the
// mutation. HOME + cwd are stable across both boots — a Reload keeps the
// container (and its scoped HOME) and re-execs the agent against the same cwd.
async function runMainAcrossReload(
	mount: string,
	mutate: () => void,
	env: Record<string, string | undefined> = {},
): Promise<{
	before: CreateAgentSessionOptions;
	after: CreateAgentSessionOptions;
}> {
	const home = scratch();
	const boot = async (): Promise<CreateAgentSessionOptions> => {
		process.env.HOME = home;
		let captured: CreateAgentSessionOptions | undefined;
		const deps: MainDeps = {
			configMount: mount,
			createSession: (options) => {
				captured = options;
				return Promise.resolve({ session: fakeSession() });
			},
			createTransport: () => fakeCarrier(),
		};
		await main({ HOME: home, ...env }, deps);
		if (captured === undefined) {
			throw new Error("main never called createSession");
		}
		return captured;
	};
	const before = await boot();
	mutate();
	const after = await boot();
	return { before, after };
}

describe("main re-reads the mount on a ConfigVersion Reload (SEA-1678 T6)", () => {
	// The load-bearing acceptance: after a Reload the agent OBSERVES the updated
	// settings value. Boot resolves the fleet overlay (222) over the project
	// value (111); a version flip changes the fleet member to 333; the re-exec'd
	// session's settingsManager must now resolve 333, not the stale 222.
	test("a changed settings member is observed on the next boot's overlay", async () => {
		const mount = scratch();
		const cwd = scratch();
		mkdirSync(join(cwd, ".omp"), { recursive: true });
		writeFileSync(
			join(cwd, ".omp", "config.yml"),
			"compaction:\n  keepRecentTokens: 111\n",
		);
		writeMember(
			mount,
			"settings/config.yml",
			"compaction:\n  keepRecentTokens: 222\n",
		);
		const { before, after } = await runMainAcrossReload(
			mount,
			() => {
				// The ConfigVersion re-materialize: current/ now serves 333.
				writeMember(
					mount,
					"settings/config.yml",
					"compaction:\n  keepRecentTokens: 333\n",
				);
			},
			{ COMPASS_WORKDIR: cwd },
		);
		const resolvedBefore = (await before.settingsManager) as Settings;
		expect(resolvedBefore.get("compaction.keepRecentTokens")).toBe(222);
		const resolvedAfter = (await after.settingsManager) as Settings;
		// The re-read observes the NEW value. Non-vacuity: a boot-once/stale
		// object would carry 222 here → red.
		expect(resolvedAfter.get("compaction.keepRecentTokens")).toBe(333);
	});

	// A changed rule body is recomposed on the next boot: the fleet rule's
	// content is re-read, so the session's `rules` carries the updated body while
	// still composing with the checkout's discovered rule.
	test("a changed rule member is recomposed on the next boot", async () => {
		const mount = scratch();
		const cwd = scratch();
		writeMember(
			mount,
			"rules/alpha.md",
			"---\ndescription: alpha\n---\noriginal alpha body\n",
		);
		mkdirSync(join(cwd, ".omp", "rules"), { recursive: true });
		writeFileSync(
			join(cwd, ".omp", "rules", "checkout.md"),
			"---\ndescription: checkout\n---\ncheckout body\n",
		);
		const { before, after } = await runMainAcrossReload(
			mount,
			() => {
				writeMember(
					mount,
					"rules/alpha.md",
					"---\ndescription: alpha\n---\nupdated alpha body\n",
				);
			},
			{ COMPASS_WORKDIR: cwd },
		);
		const alphaBefore = before.rules?.find((r) => r.name === "alpha");
		expect(alphaBefore?.content).toContain("original alpha body");
		const alphaAfter = after.rules?.find((r) => r.name === "alpha");
		// The re-read observes the NEW rule body, and still composes with the
		// checkout rule. Non-vacuity: a stale rule set carries the original body.
		expect(alphaAfter?.content).toContain("updated alpha body");
		expect(alphaAfter?.content).not.toContain("original alpha body");
		expect(after.rules?.map((r) => r.name)).toContain("checkout");
	});

	// A rule ADDED on the flip is discovered on the next boot: the fleet rule set
	// is re-enumerated, not frozen at first boot.
	test("a rule added on the flip appears on the next boot", async () => {
		const mount = scratch();
		const cwd = scratch();
		writeMember(
			mount,
			"rules/alpha.md",
			"---\ndescription: alpha\n---\nalpha\n",
		);
		const { before, after } = await runMainAcrossReload(
			mount,
			() => {
				writeMember(
					mount,
					"rules/beta.md",
					"---\ndescription: beta\n---\nbeta\n",
				);
			},
			{ COMPASS_WORKDIR: cwd },
		);
		expect(before.rules?.map((r) => r.name)).not.toContain("beta");
		expect(after.rules?.map((r) => r.name)).toContain("beta");
	});

	// A changed AGENTS.md body is re-read into contextFiles on the next boot: the
	// fleet global's content is re-read (still first/least-prominent), composing
	// with the checkout's project AGENTS.md.
	test("a changed AGENTS.md is re-read into contextFiles on the next boot", async () => {
		const mount = scratch();
		const cwd = scratch();
		mkdirSync(join(cwd, ".omp"), { recursive: true });
		const projectAgents = join(cwd, ".omp", "AGENTS.md");
		writeFileSync(projectAgents, "# project conventions\n");
		const fleetAgents = writeMember(
			mount,
			"AGENTS.md",
			"# fleet conventions v1\n",
		);
		const { before, after } = await runMainAcrossReload(
			mount,
			() => {
				writeMember(mount, "AGENTS.md", "# fleet conventions v2\n");
			},
			{ COMPASS_WORKDIR: cwd },
		);
		const fleetBefore = (before.contextFiles ?? []).find(
			(f) => f.path === fleetAgents,
		);
		expect(fleetBefore?.content).toBe("# fleet conventions v1\n");
		const fleetAfter = (after.contextFiles ?? []).find(
			(f) => f.path === fleetAgents,
		);
		// The re-read observes the NEW AGENTS.md body. Non-vacuity: a stale
		// context file carries v1 here. Compose survives: project file still present.
		expect(fleetAfter?.content).toBe("# fleet conventions v2\n");
		expect((after.contextFiles ?? []).map((f) => f.path)).toContain(
			projectAgents,
		);
		expect((after.contextFiles ?? [])[0]?.path).toBe(fleetAgents);
	});
});
