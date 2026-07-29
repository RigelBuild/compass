// CompassAgent — the first-party in-container agent (design §T5).
//
// Drives an `AgentSession` from @oh-my-pi/pi-coding-agent: subscribes its session
// event stream, maps each event to a compass.v1 `AgentFrame` (via EventMapper) and
// writes it to stdout through the FrameSink; consumes decoded `AgentControl` ops
// from the ControlSource and drives the session (`prompt`/`steer`/`setTools`/
// `setSystemPrompt`), gated by the replay barrier. One container = one agent = one
// session. The Runner starts this over ExecStreaming and relays its stdout up
// PublishEvents.
//
// Why the session, not the bare `Agent`: the typed session renderer (compass-0.8)
// needs the `AgentSessionEvent` superset — `todo_reminder`/`todo_auto_clear`/
// `notice` and the compaction/retry orchestration events that the session emits
// but the core `Agent` event stream (`AgentEvent`, 10 cases) does not. Only
// `AgentSession.subscribe` yields that union, so the emitter subscribes at the
// session level.
//
// Why control drives `session.agent`, not the session: the control contract
// (prompt/steer/config/replay) is frozen at compass-0.6 §T5 and its wire payload
// representation is a parked follow-up (control.ts). Driving the inner `Agent`
// directly preserves that frozen contract byte-for-byte — the session-level
// prompt/steer add streaming-queue mediation the Runner already owns (it holds
// live input behind the replay barrier), and session-level steer takes plain text
// while the frozen `SteerControl` carries an `AgentMessage`. So this change swaps
// only the ruled event-stream source; the control path is unchanged.
//
// The wire envelopes (`AgentFrame` out, `AgentControl` in) are isolated behind
// FrameSink / ControlSource: this class produces/consumes typed domain values and
// never touches wire bytes, so the pending gen of those internal proto messages
// changes only the sink/source impls, not this class.

import type { AgentTool } from "@oh-my-pi/pi-agent-core";
import type { AgentSession } from "@oh-my-pi/pi-coding-agent";
import {
	AgentSessionState,
	create,
	type SessionFrame,
	SessionFrameSchema,
} from "./compassv1";
import type { AgentControl, ControlSource } from "./control";
import type { FrameSink } from "./frame";
import { EventMapper, type UnmappedEvent } from "./mapping";

export interface CompassAgentOptions {
	// The session to drive. Constructed by the caller (container entrypoint) via
	// `createAgentSession` with its model/tools/system-prompt, so this class stays
	// IO-focused and the session's configuration is a separate concern. Required:
	// there is no no-arg `AgentSession` constructor (it needs an
	// `AgentSessionConfig`), so unlike the former bare-`Agent` default the caller
	// always supplies it.
	readonly session: AgentSession;
	// Outbound frame sink (stdout). The wire envelope lives behind it.
	readonly sink: FrameSink;
	// Inbound control source (stdin). Yields decoded AgentControl frames.
	readonly control: ControlSource;
	// Sink for frames the mapper could not map — logged + counted, never
	// dropped. Defaults to a no-op counter-less logger via console.
	readonly onUnmapped?: (u: UnmappedEvent) => void;
}

export class CompassAgent {
	readonly #session: AgentSession;
	readonly #sink: FrameSink;
	readonly #control: ControlSource;
	readonly #mapper: EventMapper;
	readonly #onUnmapped: (u: UnmappedEvent) => void;
	// The tools the session was constructed with (container entrypoint) — the
	// native set a control frame may never drop (see #withNatives). Snapshotted
	// as a COPY: `agent.state` hands back the live state object, so holding its
	// array by reference would let a later setTools mutate this out from under
	// us — reintroducing the very revocation this field exists to prevent.
	readonly #natives: AgentTool[];
	// Runner holds live prompt/steer until the agent acks ReplayComplete, but the
	// agent also guards locally: control frames that arrive before replay settles
	// are applied as replay (context), and live prompt/steer are refused until
	// `#replayComplete` — a belt-and-suspenders on the frozen replay barrier.
	#replayComplete = false;

	constructor(opts: CompassAgentOptions) {
		this.#session = opts.session;
		// Tolerate a session that exposes no `state`/`state.tools`: this is a
		// read for a safety net, and a constructor that throws on a shape it
		// merely inspects is worse than one that degrades to no natives.
		this.#natives = [...(opts.session.agent?.state?.tools ?? [])];
		this.#sink = opts.sink;
		this.#control = opts.control;
		this.#mapper = new EventMapper();
		this.#onUnmapped =
			opts.onUnmapped ??
			((u) =>
				console.error(
					`[compass-agent] unmapped: ${u.eventType} — ${u.reason}`,
				));
	}

	// Wire the session event stream to the frame sink, then consume control frames
	// until stdin closes. Emits a terminal status — STOPPED on a clean
	// control-stream close, ERRORED on an exception — then resolves (clean) or
	// re-throws (error).
	async run(): Promise<void> {
		const unsubscribe = this.#session.subscribe((event) => {
			for (const out of this.#mapper.map(event)) {
				if (out.kind === "unmapped") {
					this.#onUnmapped(out);
				} else {
					this.#sink.emit(out);
				}
			}
		});
		// Announce STARTING immediately so the board shows the session coming up
		// before the first session event.
		this.#emitStatus(AgentSessionState.STARTING);
		try {
			for await (const control of this.#control) {
				await this.#applyControl(control);
			}
			// Clean control-stream close → STOPPED (the normal terminal state).
			this.#emitStatus(AgentSessionState.STOPPED);
		} catch (err) {
			// An unexpected exit (control decode / SDK op / stream error) →
			// ERRORED, distinct from a clean STOPPED (compass.proto:141). Emit it
			// as the terminal status, then re-propagate — a swallowed crash would
			// be a silent failure.
			this.#emitStatus(AgentSessionState.ERRORED);
			throw err;
		} finally {
			unsubscribe();
		}
	}

	// Emit a board lifecycle transition as a session frame carrying only the
	// state (SessionFrame.typed_event empty — no trace event). The former typed
	// AgentSessionStatus frame is gone under the spine-inversion; the Runner
	// extracts the state into an AgentSessionStatus server-side, stamping the
	// session_id it owns.
	#emitStatus(state: AgentSessionState): void {
		const value: SessionFrame = create(SessionFrameSchema, { state });
		this.#sink.emit({ kind: "session", value });
	}

	// Apply one decoded control frame. Discriminated on the frozen AgentControl
	// oneof: replay applies to context (never live input); replay_complete lifts
	// the barrier; prompt/steer/ask_answer/config drive the session once replay has
	// settled. Control drives the inner `Agent` (`this.#session.agent`) to preserve
	// the frozen control contract (see the file header).
	async #applyControl(control: AgentControl): Promise<void> {
		switch (control.kind) {
			case "replay":
				// TranscriptReplay → seed context, never execute as live input.
				this.#session.agent.appendMessage(control.message);
				return;
			case "replayComplete":
				this.#replayComplete = true;
				return;
			case "config":
				if (control.systemPrompt !== undefined)
					this.#session.agent.setSystemPrompt(control.systemPrompt);
				if (control.tools !== undefined)
					this.#session.agent.setTools(this.#withNatives(control.tools));
				return;
			case "prompt":
				// Live input: the Runner holds these until ReplayComplete; the
				// local barrier is a backstop. A control that slips through early
				// is surfaced, not silently dropped.
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:prompt",
						reason:
							"live prompt arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				await this.#session.agent.prompt(control.input);
				return;
			case "steer":
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:steer",
						reason:
							"live steer arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				this.#session.agent.steer(control.message);
				return;
			case "askAnswer":
				// Live input (a structured answer to an in-flight ask): the Runner
				// holds these behind the replay barrier like prompt/steer; a frame
				// slipping through early is surfaced, not dropped.
				if (!this.#replayComplete) {
					this.#onUnmapped({
						kind: "unmapped",
						eventType: "control:ask_answer",
						reason:
							"live ask_answer arrived before ReplayComplete — refused by replay barrier",
					});
					return;
				}
				// STAGED: wiring the answer into the SDK needs the SEA-1310 ask
				// correlation key (askId → the in-flight ask). Until then, surface a
				// counted "staged" unmapped op — the variant is faithfully enumerated
				// + applied (never a missing arm), but the answer is not yet
				// delivered.
				this.#onUnmapped({
					kind: "unmapped",
					eventType: "control:ask_answer",
					reason:
						"ask_answer delivery staged — awaiting SEA-1310 ask correlation key",
				});
				return;
		}
	}

	// Union the control's tool list with any construction-time native missing
	// from it, keyed by name: a control may add tools and reorder freely, but it
	// can never drop a native. Comms is not a grantable capability — it is what
	// makes this process an agent rather than a compute job; an agent silently
	// stripped of it cannot even report that it lost it. Restricting what an
	// account may say or see is a server-side authorization decision (visibility
	// and channel membership, enforced in SQL), so a control frame omitting a
	// tool must not be able to make that decision here, in a layer with no
	// authorization code at all. Control order is preserved and the surviving
	// natives follow; the input array passes through unchanged when nothing is
	// missing, so the common case allocates nothing.
	#withNatives(tools: AgentTool[]): AgentTool[] {
		if (this.#natives.length === 0) return tools;
		const names = new Set(tools.map((tool) => tool.name));
		let merged: AgentTool[] | undefined;
		for (const native of this.#natives) {
			if (names.has(native.name)) continue;
			merged ??= tools.slice();
			merged.push(native);
			names.add(native.name);
		}
		return merged ?? tools;
	}
}
