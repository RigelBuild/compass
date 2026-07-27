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

/** The log panel (design compass-0.7 T4 §547-576): a minimizable observation
 *  companion docked at the agent workspace's right edge, OUTSIDE the tab/split
 *  tree. The header carries the agent handle, a running dot, a Stop control and
 *  a minimize toggle; the body is the OMP-native session trace. Minimized, the
 *  panel collapses to a slim rail: the trace body is REMOVED from the DOM while
 *  the running dot stays visible for liveness at a glance. */
export const LogPanel: Component<{ agent: Agent }> = (props) => {
	const store = useStore();
	const running = () => store.agentSession()?.running ?? false;
	return (
		<aside
			class="log-panel"
			classList={{ minimized: !store.logOpen() }}
			aria-label="Agent observation log"
		>
			<div class="obs-head">
				<Show when={store.logOpen()}>
					<span class="obs-title">@{props.agent.account.handle}</span>
				</Show>
				<span class="obs-spacer" />
				<span
					class="obs-run"
					classList={{ live: running() }}
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
						disabled={!running()}
						title={`Stop @${props.agent.account.handle}`}
						onClick={() => store.stopAgent()}
					>
						■ stop
					</button>
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
