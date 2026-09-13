// The agent's lifecycle surface: a thin broker over the Runner transport, plus the two
// native spawn/despawn-peer tools (design compass-agent-spawn-despawn T6). Mirrors comms.ts:
// `AgentGateway.Lifecycle` is a Connect unary, so a result is the awaited return value —
// no pending map, no stdin pump.

// Cancellation is NOT plumbed (an aborted turn's in-flight spawn lands); the idempotency key
// dedupes a re-issued spawn. The broker exists so the tools depend on a narrow
// `LifecycleTransport`, not all of `RunnerTransport`.

// IDENTITY: the agent presents no token; the Runner owns which container a call arrived on
// and the Server resolves session -> account and executes under `WithActor`. Same-owner
// despawn authority is Server-side — an unauthorized target returns a `LifecycleCallError`
// in-band, not a transport teardown.

// The schema builder rides the SDK's own schema stack via its `/ark` compat facade — one
// schema implementation in the graph, so there is no two-copy mismatch to catch.
import { type } from "@oh-my-pi/omptype/ark";
import type { AgentTool } from "@oh-my-pi/pi-agent-core";
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
	// Scopes every idempotency key to this broker instance. The Server dedups on
	// `(author_account_id, client_request_id)`, an account outlives a session, and some
	// provider tool-call ids derive from turn position — so a bare id collides across two
	// sessions of the same account at the same turn position, silently returning the older result.
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
	// The non-blank bound on `handle`/`persona` is enforced at runtime but NOT expressible in
	// JSON Schema (arktype drops `.narrow`), so their descriptions carry the rule. `role` needs
	// no carry: a closed literal union DOES render, so an off-taxonomy label is rejected
	// structurally at the edge (the server re-validates as authority).
	handle: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("The new peer's account handle (unique); must not be blank"),
	role: type("'supervisor' | 'owner' | 'manager'").describe(
		"The Manager role for the spawned peer, from the closed taxonomy: " +
			"supervisor (owns the whole tree), owner (owns a product/service/domain), " +
			"or manager (owns one lane). Selects config/prompts/<role>/SYSTEM.md as the " +
			"peer's block-0 prompt. Set at creation only (a spawn onto an existing " +
			"handle keeps the stored role).",
	),
	persona: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe(
			"The peer's stable working context (the repos/projects/lanes it works " +
				"out of — NOT churning per-issue detail), applied as a system-prompt " +
				"append-overlay. Required; must not be blank. Set at creation only.",
		),
	"display_name?": type("string").describe(
		"Human-readable display name for the new peer",
	),
});

/** Exported so a test can validate the wire contract the agent loop enforces. */
export const despawnParameters = type({
	agent_handle: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("The peer's agent handle to tear down; must not be blank"),
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
		// The detail is server text that lands in the model's context as a tool failure — a
		// trusted position with no framing. A line break would forge a second line of
		// authoritative output, so it passes through the shared `flat`; the bound runs AFTER
		// the collapse so slicing cannot re-expose a removed break.
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
 * Wired into the container entrypoint by `cli.ts main()` (RIG-1741): the tools
 * are merged into the session's `customTools` and so register as `#withNatives`
 * natives. This package's tests also exercise the end-to-end contract directly.
 */
export function createLifecycleTools(broker: LifecycleBroker): AgentTool[] {
	const spawnPeer: AgentTool<typeof spawnParameters> = {
		name: "agents_spawn_peer",
		label: "Spawn peer agent",
		approval: "write",
		description:
			"Spawn a new peer agent owned by your owner. Provide a unique handle, " +
			"a role, and a persona; optionally a display name.",
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
							role: params.role,
							persona: params.persona,
							// Idempotency key, so a replayed spawn (a turn/model retry) dedupes at
							// the handler rather than double-spawning. Broker-scoped, never the
							// bare tool-call id. Spawn only — despawn is idempotent by semantics.
							clientRequestId: broker.idempotencyKey(toolCallId),
						}),
					},
				}),
			);
			if (result.result.case !== "spawn")
				throw lifecycleFailure(result, "agents_spawn_peer", "spawn");
			const spawned = result.result.value;
			// Names-only rendering: caller-supplied `handle` and server `dmChannelName` both
			// interpolate into text the model reads as authoritative, so each passes through
			// `attr` (a newline would forge an unattributed line). No id is ever rendered. An
			// empty `dmChannelName` means the DM open was deferred (recoverable), not a failure.
			const text =
				spawned.dmChannelName === ""
					? `Spawned peer ${attr(params.handle)}. (DM channel not yet open — use comms_open_dm to reach it.)`
					: `Spawned peer ${attr(params.handle)}; DM channel ${attr(spawned.dmChannelName)}.`;
			return {
				content: [
					{
						type: "text",
						text,
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
			"Tear down a peer agent your owner owns, by its agent handle. " +
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
							agentHandle: params.agent_handle,
						}),
					},
				}),
			);
			if (result.result.case !== "despawn")
				throw lifecycleFailure(result, "agents_despawn_peer", "despawn");
			// `agent_handle` is caller-supplied; guard it as a server value
			// would be, since it renders into authoritative tool output.
			return {
				content: [
					{
						type: "text",
						text: `Despawned peer ${attr(params.agent_handle)}.`,
					},
				],
			};
		},
	};

	return [spawnPeer, despawnPeer];
}
