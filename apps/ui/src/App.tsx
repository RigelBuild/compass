import { type Component, Match, Show, Switch } from "solid-js";
import "./app.css";
import { AgentView } from "./components/AgentView";
import { BacklogView } from "./components/BacklogView";
import { Bridge } from "./components/Bridge";
import { ChannelView } from "./components/ChannelView";
import { DoneView } from "./components/DoneView";
import { LeftSidebar } from "./components/LeftSidebar";
import { RightSidebar } from "./components/RightSidebar";
import { SettingsView } from "./components/SettingsView";
import { StateDot } from "./components/StateDot";
import { UsageBar } from "./components/UsageBar";
import { useStore } from "./context";

// The Compass ADE shell — an Orca-inspired layout over the compass.v1 surface
// (docs/specs/product/compass.md). A CSS grid: a topbar, a left agent-folder
// tree, a central Bridge (swimlane board) / agent (ACP + terminals) view, a
// right sidebar (fleet conversations + files/VCS/PR), and a bottom usage bar.
// The board is primary; the "channel" and "agent" surfaces render as center
// matches within the same shell (single Switch), reached via the store.
//
// This is the dev walking-skeleton made fully explorable: every surface reads
// the in-memory stub (stub-data.ts) through one store (store.ts), so it renders
// and is clickable in `vite dev` with no daemon and no Tauri IPC. When the
// daemon grows the real board / agent / ACP / audit streams, the store's
// accessors swap the fixture for the generated @compass/client and the
// components stay as-is.

const App: Component = () => {
	const store = useStore();
	return (
		<div class="app">
			<header class="topbar">
				<div class="brand">
					<span class="logo" aria-hidden="true">
						◇
					</span>
					<span class="title">Compass</span>
					<span class="subtitle">ADE</span>
				</div>

				<div class="topbar-sep" />

				<nav class="view-tabs" aria-label="View">
					<button
						type="button"
						class="view-tab"
						classList={{ active: store.view() === "bridge" }}
						onClick={() => store.showBridge()}
					>
						<span class="tab-glyph" aria-hidden="true">
							▦
						</span>
						Bridge
					</button>
					<Show when={store.selectedAgent()}>
						{(agent) => (
							<button
								type="button"
								class="view-tab"
								classList={{ active: store.view() === "agent" }}
								onClick={() => store.openAgent(agent().account.id)}
							>
								<StateDot state={agent().lifecycle ?? "idle"} />
								{agent().account.displayName}
							</button>
						)}
					</Show>
				</nav>

				<span class="topbar-spacer" />

				<div class="daemon" classList={{ live: store.daemon().live }}>
					<span class="dot" aria-hidden="true" />
					<span>
						{store.daemon().live ? "daemon connected" : "stub data — no daemon"}
					</span>
					<span class="daemon-ver">
						{store.daemon().version} · {store.daemon().apiVersion}
					</span>
				</div>

				<div class="pane-toggles">
					<button
						type="button"
						class="pane-toggle"
						classList={{ active: store.leftOpen() }}
						title="Toggle left sidebar"
						onClick={() => store.toggleLeft()}
					>
						▐
					</button>
					<button
						type="button"
						class="pane-toggle"
						classList={{ active: store.rightOpen() }}
						title="Toggle right sidebar"
						onClick={() => store.toggleRight()}
					>
						▌
					</button>
				</div>
			</header>

			<Show when={store.leftOpen()}>
				<LeftSidebar />
			</Show>

			<main class="main">
				<Switch fallback={<AgentView />}>
					<Match when={store.view() === "bridge"}>
						<Bridge />
					</Match>
					<Match when={store.view() === "channel"}>
						<ChannelView />
					</Match>
					<Match when={store.view() === "backlog"}>
						<BacklogView />
					</Match>
					<Match when={store.view() === "done"}>
						<DoneView />
					</Match>
					<Match when={store.view() === "settings"}>
						<SettingsView />
					</Match>
				</Switch>
			</main>

			<Show when={store.rightOpen()}>
				<RightSidebar />
			</Show>

			<UsageBar />
		</div>
	);
};

export default App;
