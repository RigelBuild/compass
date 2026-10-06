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
import type { SessionEvent } from "../session-events";
import { adaptSessionEvent } from "./adapt";
import { createReconnectBackoff } from "./backoff";

/** One frame's payload: the mapped trace event, the lifecycle transition, or
 *  both. A field is absent when the frame did not carry it. */
export interface SessionFrameUpdate {
	readonly event?: SessionEvent;
	readonly state?: AgentSessionState;
}

export interface SessionTailOptions {
	client: CompassClient;
	sessionId: string;
	onFrame: (update: SessionFrameUpdate) => void;
	signal?: AbortSignal;
	/** Observes a stream error. NotFound (unknown session, or the caller is
	 *  not a member) is reported here and ends the tail; any other error is
	 *  reported before the driver resubscribes. */
	onError?: (error: unknown) => void;
}

/** STOPPED and ERRORED end a session; the server closes the tail after one. */
export function isTerminalSessionState(state: AgentSessionState): boolean {
	return (
		state === AgentSessionState.STOPPED || state === AgentSessionState.ERRORED
	);
}

/** Map one frame to its update, or undefined for a frame with nothing to hand
 *  on: the leading registration ack, or an event this UI has no mapping for. */
function frameUpdate(frame: AgentSessionFrame): SessionFrameUpdate | undefined {
	const event = frame.event ? adaptSessionEvent(frame.event) : undefined;
	const state =
		frame.state === AgentSessionState.UNSPECIFIED ? undefined : frame.state;
	if (!event && state === undefined) return undefined;
	return {
		...(event ? { event } : {}),
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

/** Tail `sessionId` until it reaches a terminal state, the server answers
 *  NotFound, or `signal` aborts. A clean non-terminal end (the server drops a
 *  lagging subscriber the same way) or an error resubscribes, backing off when
 *  the previous attempt delivered nothing. */
export async function runSessionTail(opts: SessionTailOptions): Promise<void> {
	const { signal, onError } = opts;
	const backoff = createReconnectBackoff(signal);

	while (!signal?.aborted) {
		try {
			const outcome = await tailOnce(opts, backoff.reset);
			if (outcome === "ended") return;
			if (outcome === "idle") await backoff.wait();
		} catch (error) {
			if (signal?.aborted) return;
			onError?.(error);
			if (ConnectError.from(error).code === Code.NotFound) return;
			await backoff.wait();
		}
	}
}
