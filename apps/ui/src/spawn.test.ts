import { describe, expect, test } from "bun:test";
import { AgentSessionState } from "@compass/client";
import {
	applySessionStatus,
	applySpawnError,
	applySpawned,
	applyStopped,
	beginSpawn,
	beginStop,
	bindingDotState,
	type SessionBinding,
	type SpawnPhase,
	type SpawnSpec,
} from "./spawn";
import type { AgentState } from "./stub-data";

// spawn.ts is the store-internal spawn/stop phase machine (design T1). These
// tests pin every transition the reducers own — the legal source phase per
// reducer, both failure sites, and `bindingDotState`'s totality — so a wrong
// transition or a knowably-wrong optimistic dot reddens here, not in the store.

const spec: SpawnSpec = {
	agentAccountId: "agent-1",
	initialPrompt: "do the thing",
	workstreamId: "issue-1",
};

// A binding forced into an arbitrary phase, for exercising rejection arms and
// the total dot mapping without walking the whole machine to get there.
function bindingAt(
	phase: SpawnPhase,
	overrides: Partial<SessionBinding> = {},
): SessionBinding {
	return {
		workstreamId: "issue-1",
		agentAccountId: "agent-1",
		clientRequestId: "req-1",
		initialPrompt: "do the thing",
		phase,
		...overrides,
	};
}

const ALL_PHASES: readonly SpawnPhase[] = [
	"spawning",
	"running",
	"spawn-failed",
	"stopping",
	"stop-failed",
	"stopped",
];

describe("beginSpawn", () => {
	test("mints a spawning binding capturing the spec", () => {
		const b = beginSpawn(spec, "req-1");
		expect(b.phase).toBe("spawning");
		expect(b.agentAccountId).toBe("agent-1");
		expect(b.workstreamId).toBe("issue-1");
		expect(b.clientRequestId).toBe("req-1");
		expect(b.initialPrompt).toBe("do the thing");
		expect(b.sessionId).toBeUndefined();
		expect(b.error).toBeUndefined();
	});

	test("captures an empty initialPrompt verbatim (start idle)", () => {
		const b = beginSpawn({ ...spec, initialPrompt: "" }, "req-2");
		expect(b.initialPrompt).toBe("");
	});
});

describe("applySpawned", () => {
	test("spawning → running, sets sessionId", () => {
		const b = applySpawned(beginSpawn(spec, "req-1"), "sess-1");
		expect(b.phase).toBe("running");
		expect(b.sessionId).toBe("sess-1");
	});

	test("rejects every non-spawning source phase", () => {
		for (const phase of ALL_PHASES) {
			if (phase === "spawning") continue;
			expect(() => applySpawned(bindingAt(phase), "sess-1")).toThrow();
		}
	});
});

describe("beginStop", () => {
	test("running → stopping", () => {
		expect(beginStop(bindingAt("running")).phase).toBe("stopping");
	});

	test("stop-failed → stopping (retry on a still-held session)", () => {
		expect(beginStop(bindingAt("stop-failed")).phase).toBe("stopping");
	});

	test("rejects every other source phase", () => {
		for (const phase of ALL_PHASES) {
			if (phase === "running" || phase === "stop-failed") continue;
			expect(() => beginStop(bindingAt(phase))).toThrow();
		}
	});
});

describe("applySpawnError", () => {
	test("at spawn → spawn-failed with error", () => {
		const b = applySpawnError(bindingAt("spawning"), "spawn", "boom");
		expect(b.phase).toBe("spawn-failed");
		expect(b.error).toBe("boom");
	});

	test("at stopping → stop-failed with error", () => {
		const b = applySpawnError(bindingAt("stopping"), "stopping", "nope");
		expect(b.phase).toBe("stop-failed");
		expect(b.error).toBe("nope");
	});
});

describe("applyStopped", () => {
	test("accepts running / stopping / stop-failed → stopped", () => {
		for (const phase of ["running", "stopping", "stop-failed"] as const) {
			expect(applyStopped(bindingAt(phase)).phase).toBe("stopped");
		}
	});

	test("rejects every other source phase", () => {
		for (const phase of ALL_PHASES) {
			if (
				phase === "running" ||
				phase === "stopping" ||
				phase === "stop-failed"
			)
				continue;
			expect(() => applyStopped(bindingAt(phase))).toThrow();
		}
	});
});

describe("applySessionStatus", () => {
	test("leaves SpawnPhase at running, ignoring the live state", () => {
		const b = applySessionStatus(
			bindingAt("running"),
			AgentSessionState.WORKING,
		);
		expect(b.phase).toBe("running");
	});

	// A non-running live state pins the invariant WORKING alone cannot: a
	// wrong impl that derived phase from the live state (STOPPED → "stopped")
	// passes the WORKING case by coincidence but fails here. The reducer must
	// discard `_state` and never widen SpawnPhase (DL-167 / Board-state model).
	test("discards a non-running live state, holding phase at running", () => {
		const b = applySessionStatus(
			bindingAt("running"),
			AgentSessionState.STOPPED,
		);
		expect(b.phase).toBe("running");
	});
});

describe("bindingDotState", () => {
	test("is total over every phase", () => {
		const expected: Record<SpawnPhase, AgentState> = {
			spawning: "working",
			running: "working",
			"spawn-failed": "error",
			stopping: "working",
			"stop-failed": "error",
			stopped: "stopped",
		};
		for (const phase of ALL_PHASES) {
			// running is prompt-dependent; asserted separately below.
			if (phase === "running") continue;
			expect(bindingDotState(bindingAt(phase))).toBe(expected[phase]);
		}
	});

	test("running with a non-empty prompt → working", () => {
		expect(bindingDotState(bindingAt("running", { initialPrompt: "go" }))).toBe(
			"working",
		);
	});

	test("running with an empty prompt → idle (start idle)", () => {
		expect(bindingDotState(bindingAt("running", { initialPrompt: "" }))).toBe(
			"idle",
		);
	});
});
