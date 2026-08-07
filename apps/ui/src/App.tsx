import type { RouteSectionProps } from "@solidjs/router";
import { useLocation, useNavigate } from "@solidjs/router";
import { type Component, Show } from "solid-js";
import "./design/tokens.css";
import "./design/base.css";
import "./app.css";
import { LeftSidebar } from "./components/LeftSidebar";
import { RightSidebar } from "./components/RightSidebar";
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

// App is the router ROOT LAYOUT (record A1): the shell chrome (topbar,
// sidebars, UsageBar) stays outside the routed region, and the `<main>` center
// renders `props.children` — the surface the matched route mounts. App lives
// inside the router tree, so it wires the store's router seam here: it feeds
// `useNavigate()` + a reactive `useLocation().pathname` into `bindRouter`,
// which installs the single-writer route-sync effect (store.ts applyRoute).
const App: Component<RouteSectionProps> = (props) => {
	const store = useStore();
	const navigate = useNavigate();
	const location = useLocation();
	store.bindRouter({
		navigate: (path) => navigate(path),
		currentPath: () => location.pathname,
	});
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

			<main class="main">{props.children}</main>

			<Show when={store.rightOpen()}>
				<RightSidebar />
			</Show>

			<UsageBar />
		</div>
	);
};

export default App;
