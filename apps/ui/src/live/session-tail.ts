// The SubscribeAgentSession driver: tails one session's typed trace, handing each
// mapped event and lifecycle transition to the caller. The server sends no history,
// so a reconnect resumes the live tail only.

import {
	type AgentSessionFrame,
	AgentSessionState,
	Code,
	type CompassClient,
	ConnectError,
} from "@compass/client";
import type { BatchPending, SessionEvent } from "../session-events";
import { adaptSessionEvent } from "./adapt";
import { createReconnectBackoff, type ReconnectBackoff } from "./backoff";

/** One frame's payload: a trace event, batch-window state, or lifecycle update. */
export interface SessionFrameUpdate {
	readonly event?: SessionEvent;
	readonly batchPending?: BatchPending | null;
	readonly state?: AgentSessionState;
}

export interface SessionTailOptions {
	client: CompassClient;
	sessionId: string;
	onFrame: (update: SessionFrameUpdate) => void;
	signal?: AbortSignal;
	/** Observes a stream error. NotFound (unknown session or non-member) ends the
	 *  tail; any other error is reported before the driver resubscribes. */
	onError?: (error: unknown) => void;
	/** Defaults to the shared jittered backoff; tests inject an immediate one. */
	backoff?: ReconnectBackoff;
}

/** Why a tail stopped: a terminal state, a NotFound answer, or the abort signal. */
export type SessionTailEnd = "ended" | "notFound" | "aborted";

/** STOPPED and ERRORED end a session; the server closes the tail after one. */
export function isTerminalSessionState(state: AgentSessionState): boolean {
	return (
		state === AgentSessionState.STOPPED || state === AgentSessionState.ERRORED
	);
}

/** The most events kept per session; the oldest drop first, bounding a long tail. */
export const MAX_SESSION_EVENTS = 2000;

/** Append `event` to a session trace, merging a text delta into the previous event
 *  for the same kind and message id (as foldSession would), then apply the cap. */
export function appendSessionEvent(
	events: readonly SessionEvent[],
	event: SessionEvent,
): SessionEvent[] {
	const last = events.at(-1);
	if (
		last &&
		(event.kind === "assistant_text" || event.kind === "thinking") &&
		last.kind === event.kind &&
		last.messageId === event.messageId
	) {
		return [...events.slice(0, -1), { ...last, text: last.text + event.text }];
	}
	const next = [...events, event];
	return next.length > MAX_SESSION_EVENTS
		? next.slice(next.length - MAX_SESSION_EVENTS)
		: next;
}

/** Map one frame to an update, or undefined for a frame with nothing to hand
 *  on: the leading registration ack, or an event this UI has no mapping for. */
function frameUpdate(frame: AgentSessionFrame): SessionFrameUpdate | undefined {
	const mapped = frame.event ? adaptSessionEvent(frame.event) : undefined;
	const event =
		mapped && "kind" in mapped && mapped.kind !== "batch_pending"
			? mapped
			: undefined;
	const batchPending =
		mapped && "kind" in mapped && mapped.kind === "batch_pending"
			? mapped.count === 0
				? null
				: { count: mapped.count, firesAtMs: mapped.firesAtMs }
			: undefined;
	const state =
		frame.state === AgentSessionState.UNSPECIFIED ? undefined : frame.state;
	if (event === undefined && batchPending === undefined && state === undefined)
		return undefined;
	return {
		...(event ? { event } : {}),
		...(batchPending !== undefined ? { batchPending } : {}),
		...(state !== undefined ? { state } : {}),
	};
}

/** Drain one subscription. "ended" means a terminal state arrived; otherwise
 *  whether any frame beyond the registration ack was delivered. */
async function tailOnce(
	opts: SessionTailOptions,
	onProgress: () => void,
): Promise<"ended" | "progress" | "idle"> {
	const { client, sessionId, onFrame, signal } = opts;
	let outcome: "progress" | "idle" = "idle";
	for await (const frame of client.subscribeAgentSession(
		{ sessionId },
		{ signal },
	)) {
		const update = frameUpdate(frame);
		if (!update) continue;
		outcome = "progress";
		onProgress();
		onFrame(update);
		if (update.state !== undefined && isTerminalSessionState(update.state))
			return "ended";
	}
	return outcome;
}

/** Tail `sessionId` until a terminal state, a NotFound answer, or abort. Any other
 *  error or a clean non-terminal end resubscribes, backing off after an idle try. */
export async function runSessionTail(
	opts: SessionTailOptions,
): Promise<SessionTailEnd> {
	const { signal, onError } = opts;
	const backoff = opts.backoff ?? createReconnectBackoff(signal);

	while (!signal?.aborted) {
		try {
			const outcome = await tailOnce(opts, backoff.reset);
			if (outcome === "ended") return "ended";
			if (outcome === "idle") await backoff.wait();
		} catch (error) {
			if (signal?.aborted) return "aborted";
			onError?.(error);
			if (ConnectError.from(error).code === Code.NotFound) return "notFound";
			await backoff.wait();
		}
	}
	return "aborted";
}
