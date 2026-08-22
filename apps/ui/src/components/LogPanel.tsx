import { type Component, Show } from "solid-js";
import { useStore } from "../context";
import { foldSession } from "../session-events";
import type { Agent } from "../stub-data";
import { SessionTrace } from "./SessionTrace";

/** The trace observation pane: the selected agent's typed execution trace. The
 *  raw SessionEvent stream is folded (foldSession) into render-ready TraceItems
 *  and handed to SessionTrace. Observation-only — no composer. */
const TracePane: Component = () => {
	const store = useStore();
	const items = () => {
		const s = store.agentSession();
		return s ? foldSession(s.events) : undefined;
	};
	return (
		<Show
			when={items()}
			fallback={
				<div class="obs-empty muted">
					No session trace — this agent hasn't run yet.
				</div>
			}
		>
			{(traceItems) => (
				<Show
					when={traceItems().length > 0}
					fallback={<div class="obs-empty muted">Trace is empty.</div>}
				>
					<div class="obs-trace">
						<SessionTrace items={traceItems()} />
					</div>
				</Show>
			)}
		</Show>
	);
};

/** The log panel (design: architecture-lineage): a minimizable observation
 *  companion docked at the agent workspace's right edge, OUTSIDE the tab/split
 *  tree. The header carries the agent handle, a running dot, a Stop control and
 *  a minimize toggle; the body is the OMP-native session trace. Minimized, the
 *  panel collapses to a slim rail: the trace body is REMOVED from the DOM while
 *  the running dot stays visible for liveness at a glance. */
export const LogPanel: Component<{ agent: Agent }> = (props) => {
	const store = useStore();
	const running = () => store.agentSession()?.running ?? false;
	// A fixture-sourced session's id was never minted by a server, so Stop has
	// nothing it can honestly issue (store.ts `stopAgent` refuses it outright).
	// Render the control disabled and say why, the same shape the channel rail
	// uses for its not-yet-wired subscribe control (LeftSidebar.tsx:171-184) — a
	// visibly-dead button beats one that only reports into the console.
	const fixture = () => store.agentSession()?.fixture === true;
	return (
		<aside
			class={["log-panel", { minimized: !store.logOpen() }]}
			aria-label="Agent observation log"
		>
			<div class="obs-head">
				<Show when={store.logOpen()}>
					<span class="obs-title">@{props.agent.account.handle}</span>
				</Show>
				<span class="obs-spacer" />
				<span
					class={["obs-run", { live: running() }]}
					title={running() ? "Agent is running" : "Agent is idle"}
				>
					<span class="obs-run-dot" aria-hidden="true" />
					<Show when={store.logOpen()}>{running() ? "running" : "idle"}</Show>
				</span>
				<Show when={store.logOpen()}>
					{/* The one non-observational control: stop the running turn. You
					    steer via the channel, not here. */}
					<button
						type="button"
						class="obs-stop"
						disabled={!running() || fixture()}
						title={
							fixture()
								? "Can't stop: this session is fixture data, not a server-minted session."
								: `Stop @${props.agent.account.handle}`
						}
						onClick={() => void store.stopAgent()}
					>
						■ stop
					</button>
				</Show>
				{/* A refused stop (a server with no RunnerHub answers `Unavailable`,
				    and a fixture-sourced session is refused locally) resolves rather
				    than rejecting, so without this the click is indistinguishable
				    from a successful stop. Same shape as the ask block's refusal
				    (ChannelView.tsx:193-197). */}
				<Show when={store.stopError()}>
					{(msg) => (
						<span class="obs-error" role="alert">
							{msg()}
						</span>
					)}
				</Show>
				<button
					type="button"
					class="obs-min"
					aria-label={store.logOpen() ? "Minimize" : "Expand"}
					title={store.logOpen() ? "Minimize log panel" : "Expand log panel"}
					onClick={() => store.toggleLog()}
				>
					{store.logOpen() ? "⟩" : "⟨"}
				</button>
			</div>
			<Show when={store.logOpen()}>
				<div class="obs-body">
					<TracePane />
				</div>
			</Show>
		</aside>
	);
};
