// LifecycleBroker + the two native lifecycle tools (design: sealedsecurity/sealed
// docs/designs/product/compass-agent-spawn-despawn/design.md, T6).
// Each test defends an observable contract of the agent->Runner lifecycle call:
// the exact `LifecycleCallRequest` a tool `execute` puts on the wire (oneof
// case, spawn/despawn payload, call_id / client_request_id), and how a
// `LifecycleCallResult` renders back — a domain `error` case as a thrown Error
// (the OMP tool-failure contract), a success as text content.
//
// The transport is faked to the one method the broker consumes (`lifecycle`), so
// there is no socket, no Connect client, and no timing: a call in, a canned
// result out, and the captured request asserted verbatim.

import { describe, expect, test } from "bun:test";
import type { AgentTool, AgentToolResult } from "@oh-my-pi/pi-agent-core";
import { ArkErrors, type Type } from "arktype";
import {
	create,
	DespawnPeerResponseSchema,
	LifecycleCallErrorSchema,
	type LifecycleCallRequest,
	LifecycleCallRequestSchema,
	type LifecycleCallResult,
	LifecycleCallResultSchema,
	SpawnPeerResponseSchema,
} from "./compassv1";
import {
	createLifecycleTools,
	despawnParameters,
	LifecycleBroker,
	type LifecycleTransport,
	spawnParameters,
} from "./lifecycle";

// A fake of the one transport method the broker consumes. Records every request
// it is handed (so the wire shape is asserted) and returns a canned result.
class FakeTransport implements LifecycleTransport {
	readonly requests: LifecycleCallRequest[] = [];
	constructor(private readonly result: LifecycleCallResult) {}
	async lifecycle(req: LifecycleCallRequest): Promise<LifecycleCallResult> {
		this.requests.push(req);
		return this.result;
	}
}

function spawnResult(
	agentAccountId: string,
	containerName: string,
	sessionId: string,
): LifecycleCallResult {
	return create(LifecycleCallResultSchema, {
		callId: "call-1",
		result: {
			case: "spawn",
			value: create(SpawnPeerResponseSchema, {
				agentAccountId,
				containerName,
				sessionId,
			}),
		},
	});
}

function despawnResult(): LifecycleCallResult {
	return create(LifecycleCallResultSchema, {
		callId: "call-1",
		result: {
			case: "despawn",
			value: create(DespawnPeerResponseSchema, {}),
		},
	});
}

function errorResult(code: string, message: string): LifecycleCallResult {
	return create(LifecycleCallResultSchema, {
		callId: "call-1",
		result: {
			case: "error",
			value: create(LifecycleCallErrorSchema, { code, message }),
		},
	});
}

// Pull one tool out of the set by name, failing loudly if the set stops carrying
// it (so a rename reddens here rather than silently skipping the assertions).
function tool(broker: LifecycleBroker, name: string): AgentTool {
	const found = createLifecycleTools(broker).find((t) => t.name === name);
	if (!found) throw new Error(`no such tool: ${name}`);
	return found;
}

// `execute` is invoked exactly as the agent loop calls it: with params already
// validated against the tool's schema. The tests pass plain literals, so the
// parameter object is widened to a record at this one seam.
const exec = (
	t: AgentTool,
	id: string,
	params: Record<string, unknown>,
): Promise<AgentToolResult> => t.execute.call(t, id, params);

function textOf(result: AgentToolResult): string {
	const block = result.content[0];
	if (block?.type !== "text") throw new Error("expected a text content block");
	return block.text;
}

describe("LifecycleBroker", () => {
	test("delegates the call verbatim to the transport and returns its result", async () => {
		const result = spawnResult("acct-1", "cont-1", "sess-1");
		const transport = new FakeTransport(result);
		const broker = new LifecycleBroker(transport);
		const req: LifecycleCallRequest = create(LifecycleCallRequestSchema, {
			callId: "abc",
		});

		await expect(broker.call(req)).resolves.toBe(result);
		expect(transport.requests).toEqual([req]);
	});

	// The Server dedups spawns on (account, client_request_id) and the account
	// outlives the session, so two brokers must never mint the same key for the
	// same tool-call id — a collision is silently swallowed by the dedup join.
	test("two brokers mint different idempotency keys for the same tool call id", () => {
		const transport = new FakeTransport(spawnResult("a", "c", "s"));
		const a = new LifecycleBroker(transport).idempotencyKey("tc-1");
		const b = new LifecycleBroker(transport).idempotencyKey("tc-1");

		expect(a).not.toBe(b);
		expect(a).toEndWith(":tc-1");
	});

	test("one broker is stable for one tool call id", () => {
		const broker = new LifecycleBroker(new FakeTransport(despawnResult()));

		expect(broker.idempotencyKey("tc-1")).toBe(broker.idempotencyKey("tc-1"));
		expect(broker.idempotencyKey("tc-1")).not.toBe(
			broker.idempotencyKey("tc-2"),
		);
	});
});

describe("createLifecycleTools", () => {
	test("exposes exactly the two lifecycle tools", () => {
		const tools = createLifecycleTools(
			new LifecycleBroker(new FakeTransport(despawnResult())),
		);
		expect(tools.map((t) => t.name)).toEqual([
			"agents_spawn_peer",
			"agents_despawn_peer",
		]);
		expect(tools.every((t) => t.label.length > 0)).toBe(true);
		// `approval` decides which modes auto-approve the call. Both are writes; a
		// silent flip to `read` would broaden auto-approval, and nothing else here
		// would redden.
		const byName = (n: string) => {
			const t = tools.find((x) => x.name === n);
			if (t === undefined) throw new Error(`no tool ${n}`);
			return t;
		};
		expect(byName("agents_spawn_peer").approval).toBe("write");
		expect(byName("agents_despawn_peer").approval).toBe("write");
		// Each tool carries its own schema — a crossed wiring would otherwise only
		// surface as a confusing validation failure at call time.
		expect(byName("agents_spawn_peer").parameters).toBe(spawnParameters);
		expect(byName("agents_despawn_peer").parameters).toBe(despawnParameters);
	});
});

describe("agents_spawn_peer", () => {
	test("puts the exact SpawnPeerRequest on the wire incl. a minted clientRequestId", async () => {
		const transport = new FakeTransport(
			spawnResult("acct-9", "cont-9", "sess-9"),
		);
		const broker = new LifecycleBroker(transport);
		const t = tool(broker, "agents_spawn_peer");

		await exec(t, "tc-42", {
			handle: "worker-a",
			display_name: "Worker A",
			initial_prompt: "do the thing",
		});

		expect(transport.requests).toHaveLength(1);
		const req = transport.requests[0];
		expect(req.callId).toBe("tc-42");
		expect(req.call.case).toBe("spawn");
		if (req.call.case !== "spawn") throw new Error("expected spawn case");
		const spawn = req.call.value;
		expect(spawn.handle).toBe("worker-a");
		expect(spawn.displayName).toBe("Worker A");
		expect(spawn.initialPrompt).toBe("do the thing");
		expect(spawn.clientRequestId.length).toBeGreaterThan(0);
		expect(spawn.clientRequestId).toEndWith(":tc-42");
		expect(spawn.clientRequestId).toBe(broker.idempotencyKey("tc-42"));
	});

	test("defaults optional params to empty strings on the wire", async () => {
		const transport = new FakeTransport(
			spawnResult("acct-9", "cont-9", "sess-9"),
		);
		const t = tool(new LifecycleBroker(transport), "agents_spawn_peer");

		await exec(t, "tc-1", { handle: "worker-a" });

		const req = transport.requests[0];
		if (req.call.case !== "spawn") throw new Error("expected spawn case");
		expect(req.call.value.displayName).toBe("");
		expect(req.call.value.initialPrompt).toBe("");
	});

	test("renders the spawned peer's server values as a text block", async () => {
		const transport = new FakeTransport(
			spawnResult("acct-9", "cont-9", "sess-9"),
		);
		const t = tool(new LifecycleBroker(transport), "agents_spawn_peer");

		const result = await exec(t, "tc-1", { handle: "worker-a" });

		expect(textOf(result)).toBe(
			"Spawned peer acct-9 (container cont-9, session sess-9).",
		);
	});

	test("a malformed server value degrades rather than forging output", async () => {
		const transport = new FakeTransport(
			spawnResult('acct"9\ninjected', "cont-9", "sess-9"),
		);
		const t = tool(new LifecycleBroker(transport), "agents_spawn_peer");

		const result = await exec(t, "tc-1", { handle: "worker-a" });

		expect(textOf(result)).toBe(
			"Spawned peer (malformed) (container cont-9, session sess-9).",
		);
	});

	test("an in-band error result throws the tool-failure text shape", async () => {
		const transport = new FakeTransport(
			errorResult("not_found", "no such owner"),
		);
		const t = tool(new LifecycleBroker(transport), "agents_spawn_peer");

		await expect(exec(t, "tc-1", { handle: "worker-a" })).rejects.toThrow(
			"agents_spawn_peer failed: not_found: no such owner",
		);
	});

	test("a wrong result case throws a protocol violation", async () => {
		const transport = new FakeTransport(despawnResult());
		const t = tool(new LifecycleBroker(transport), "agents_spawn_peer");

		await expect(exec(t, "tc-1", { handle: "worker-a" })).rejects.toThrow(
			"agents_spawn_peer: protocol violation — expected a spawn result, got despawn",
		);
	});
});

describe("agents_despawn_peer", () => {
	test("maps agent_account_id and sends NO clientRequestId", async () => {
		const transport = new FakeTransport(despawnResult());
		const t = tool(new LifecycleBroker(transport), "agents_despawn_peer");

		await exec(t, "tc-7", { agent_account_id: "acct-3" });

		expect(transport.requests).toHaveLength(1);
		const req = transport.requests[0];
		expect(req.callId).toBe("tc-7");
		expect(req.call.case).toBe("despawn");
		if (req.call.case !== "despawn") throw new Error("expected despawn case");
		const despawn = req.call.value;
		expect(despawn.agentAccountId).toBe("acct-3");
		// The despawn message carries no dedup field at all — assert nothing named
		// clientRequestId leaked onto it.
		expect("clientRequestId" in despawn).toBe(false);
	});

	test("renders a success text block", async () => {
		const transport = new FakeTransport(despawnResult());
		const t = tool(new LifecycleBroker(transport), "agents_despawn_peer");

		const result = await exec(t, "tc-1", { agent_account_id: "acct-3" });

		expect(textOf(result)).toBe("Despawned peer acct-3.");
	});

	test("an in-band error result throws the tool-failure text shape", async () => {
		const transport = new FakeTransport(
			errorResult("not_found", "other owner"),
		);
		const t = tool(new LifecycleBroker(transport), "agents_despawn_peer");

		await expect(
			exec(t, "tc-1", { agent_account_id: "acct-3" }),
		).rejects.toThrow("agents_despawn_peer failed: not_found: other owner");
	});

	test("a wrong result case throws a protocol violation", async () => {
		const transport = new FakeTransport(spawnResult("a", "c", "s"));
		const t = tool(new LifecycleBroker(transport), "agents_despawn_peer");

		await expect(
			exec(t, "tc-1", { agent_account_id: "acct-3" }),
		).rejects.toThrow(
			"agents_despawn_peer: protocol violation — expected a despawn result, got spawn",
		);
	});
});

describe("lifecycle parameter schemas", () => {
	const rejects = (schema: Type<object>, params: unknown): boolean =>
		schema(params) instanceof ArkErrors;

	test("spawn rejects an empty or whitespace-only handle", () => {
		expect(rejects(spawnParameters, {})).toBe(true);
		expect(rejects(spawnParameters, { handle: "" })).toBe(true);
		expect(rejects(spawnParameters, { handle: "  " })).toBe(true);
		expect(rejects(spawnParameters, { handle: "worker-a" })).toBe(false);
	});

	test("despawn rejects an empty or whitespace-only agent_account_id", () => {
		expect(rejects(despawnParameters, {})).toBe(true);
		expect(rejects(despawnParameters, { agent_account_id: "" })).toBe(true);
		expect(rejects(despawnParameters, { agent_account_id: "  " })).toBe(true);
		expect(rejects(despawnParameters, { agent_account_id: "acct-3" })).toBe(
			false,
		);
	});
});
