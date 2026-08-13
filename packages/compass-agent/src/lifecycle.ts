// The agent's lifecycle surface: a thin broker over the Runner transport, plus
// the two native tools an agent registers on its Agent to spawn / despawn a peer
// (design sealedsecurity/sealed
// docs/designs/product/compass-agent-spawn-despawn/design.md, T6 — design
// records live in sealed, not this repo).
//
// This mirrors comms.ts exactly, one leg over: `AgentGateway.Lifecycle` is a
// Connect **unary** over the per-container Unix socket (transport/index.ts), so
// correlation and deadlines belong to the RPC and a result is just the awaited
// return value — no pending map, no stdin pump, no deadlock. Cancellation is NOT
// plumbed: `execute`'s `AbortSignal` is not forwarded, so an aborted turn does
// not cancel an in-flight spawn — it lands. The idempotency key means a re-issue
// of the same spawn dedupes rather than double-spawning. What is left for the
// broker is one delegation. It exists so the tools depend on a narrow one-method
// surface (`LifecycleTransport`) rather than the whole `RunnerTransport`.
//
// IDENTITY. The agent presents no token and asserts no account: the Runner owns
// which container (hence which session) a call arrived on, and the Server
// resolves session -> account and executes under `WithActor`. Same-owner despawn
// authority is enforced Server-side — an unauthorized target comes back as a
// `LifecycleCallError`, in-band, not as a transport teardown.

import type { AgentTool } from "@oh-my-pi/pi-agent-core";
// `arktype` is pinned exact in package.json to whatever the SDK resolves — see
// the comms.ts note on this pin; a mismatch resolves two @ark/schema copies and
// `tsc` catches it.
import { type } from "arktype";
import {
	create,
	DespawnPeerRequestSchema,
	type LifecycleCallRequest,
	LifecycleCallRequestSchema,
	type LifecycleCallResult,
	SpawnPeerRequestSchema,
} from "./compassv1";
import { attr, flat } from "./render-guard";

/**
 * The one transport method the lifecycle tools consume — a structural subset of
 * `RunnerTransport` (transport/index.ts), so `createUnixSocketTransport()`'s
 * result satisfies it directly while a unit test fakes a single method.
 */
export interface LifecycleTransport {
	lifecycle(call: LifecycleCallRequest): Promise<LifecycleCallResult>;
}

/**
 * A thin adapter over the lifecycle leg of the Runner transport. `call`
 * delegates straight to `transport.lifecycle(req)`; the Connect unary owns
 * correlation and deadlines. Cancellation is not plumbed — see the file header.
 */
export class LifecycleBroker {
	readonly #transport: LifecycleTransport;
	// Scopes every idempotency key this broker mints to this one broker
	// instance. The Server dedups on `(author_account_id, client_request_id)`
	// and an account outlives any single session, while some provider tool-call
	// ids are derived from turn position rather than randomness (the OpenAI
	// fallback hashes `messageIndex:toolCallIndex:toolName`). A bare tool-call
	// id therefore collides across two sessions of the same account at the same
	// turn position, and the collision is silent: the spawn dedup returns the
	// older result, so the tool reports success for a spawn that never ran.
	readonly #idempotencyNonce = crypto.randomUUID();

	constructor(transport: LifecycleTransport) {
		this.#transport = transport;
	}

	/** The account-safe idempotency key for a spawn made under `toolCallId`. */
	idempotencyKey(toolCallId: string): string {
		return `${this.#idempotencyNonce}:${toolCallId}`;
	}

	call(req: LifecycleCallRequest): Promise<LifecycleCallResult> {
		return this.#transport.lifecycle(req);
	}
}

/** Exported so a test can validate the wire contract the agent loop enforces. */
export const spawnParameters = type({
	// The non-blank bound is enforced at runtime but is NOT expressible in JSON
	// Schema — arktype drops the `.narrow` predicate from the wire schema the
	// model is shown, so the description carries the rule instead (see the
	// comms.ts `postParameters` note).
	handle: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("The new peer's account handle (unique); must not be blank"),
	"display_name?": type("string").describe(
		"Human-readable display name for the new peer",
	),
});

/** Exported so a test can validate the wire contract the agent loop enforces. */
export const despawnParameters = type({
	agent_account_id: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("The peer's agent account id to tear down; must not be blank"),
});

/**
 * The `Error` a non-matching `LifecycleCallResult` deserves — both shapes are
 * tool failures under the OMP contract ("throw an error when a tool fails"):
 *   - `error` — an in-band domain failure (unknown target, other-owner target).
 *     The code and detail go into the message so the model can act on them.
 *   - anything else — the Server answered a spawn with a despawn, or set no case
 *     at all. That is a protocol violation; succeeding silently would hand the
 *     model a fabricated empty result.
 */
function lifecycleFailure(
	result: LifecycleCallResult,
	toolName: string,
	expected: string,
): Error {
	const outcome = result.result;
	if (outcome.case === "error") {
		// The detail is server text that lands in the model's context as a tool
		// failure — a position at least as trusted as the transcript, with no
		// framing and no author. A line break in it would forge a second line of
		// authoritative output, so it passes through the shared `flat` (never a
		// second copy of its regex — see render-guard.ts). The bound runs AFTER
		// the collapse, so slicing cannot re-expose a break the collapse removed.
		const detail = flat(outcome.value.message).slice(0, 500);
		return new Error(
			`${toolName} failed: ${attr(outcome.value.code)}: ${detail}`,
		);
	}
	return new Error(
		`${toolName}: protocol violation — expected a ${expected} result, got ${outcome.case ?? "none"}`,
	);
}

/**
 * The native lifecycle tool set. Exactly two tools: spawn and despawn a peer.
 *
 * Wired into the container entrypoint by `cli.ts main()` (SEA-1741): the tools
 * are merged into the session's `customTools` and so register as `#withNatives`
 * natives. This package's tests also exercise the end-to-end contract directly.
 */
export function createLifecycleTools(broker: LifecycleBroker): AgentTool[] {
	const spawnPeer: AgentTool<typeof spawnParameters> = {
		name: "agents_spawn_peer",
		label: "Spawn peer agent",
		approval: "write",
		description:
			"Spawn a new peer agent owned by your owner. Provide a unique handle; " +
			"optionally a display name.",
		parameters: spawnParameters,
		execute: async (toolCallId, params) => {
			const result = await broker.call(
				create(LifecycleCallRequestSchema, {
					callId: toolCallId,
					call: {
						case: "spawn",
						value: create(SpawnPeerRequestSchema, {
							handle: params.handle,
							displayName: params.display_name ?? "",
							// Idempotency key, so a replayed spawn (an agent-turn/model
							// retry of the same tool call) dedupes at the lifecycle handler
							// rather than double-spawning. Broker-scoped, never the bare
							// tool-call id — see `LifecycleBroker.idempotencyKey`. Spawn
							// only — despawn is idempotent by semantics and carries no field.
							clientRequestId: broker.idempotencyKey(toolCallId),
						}),
					},
				}),
			);
			if (result.result.case !== "spawn")
				throw lifecycleFailure(result, "agents_spawn_peer", "spawn");
			const spawned = result.result.value;
			// Server values interpolated into text the model reads as authoritative
			// harness output — a newline in any turns one line into two, the second
			// unattributed. Each passes through the shared `attr`.
			return {
				content: [
					{
						type: "text",
						text: `Spawned peer ${attr(spawned.agentAccountId)} (container ${attr(spawned.containerName)}, session ${attr(spawned.sessionId)}).`,
					},
				],
			};
		},
	};

	const despawnPeer: AgentTool<typeof despawnParameters> = {
		name: "agents_despawn_peer",
		label: "Despawn peer agent",
		approval: "write",
		description:
			"Tear down a peer agent your owner owns, by its agent account id. " +
			"Idempotent: despawning an already-absent peer succeeds.",
		parameters: despawnParameters,
		execute: async (toolCallId, params) => {
			const result = await broker.call(
				create(LifecycleCallRequestSchema, {
					callId: toolCallId,
					call: {
						case: "despawn",
						// No clientRequestId: despawn is idempotent by semantics
						// (removing an absent peer succeeds), so the message carries no
						// dedup field.
						value: create(DespawnPeerRequestSchema, {
							agentAccountId: params.agent_account_id,
						}),
					},
				}),
			);
			if (result.result.case !== "despawn")
				throw lifecycleFailure(result, "agents_despawn_peer", "despawn");
			// `agent_account_id` is caller-supplied; guard it as a server value
			// would be, since it renders into authoritative tool output.
			return {
				content: [
					{
						type: "text",
						text: `Despawned peer ${attr(params.agent_account_id)}.`,
					},
				],
			};
		},
	};

	return [spawnPeer, despawnPeer];
}
