import type { RouteSectionProps } from "@solidjs/router";
import { useLocation, useNavigate } from "@solidjs/router";
import { type Component, onCleanup, Show } from "solid-js";
import "./design/tokens.css";
import "./design/base.css";
import "./design/components/badge-glyph.css";
import "./design/components/card.css";
import "./design/components/menu.css";
import "./design/components/shortcuts.css";
import "./design/components/state-dot.css";
import "./app.css";
import {
	CoachTip,
	CoachTipContent,
	CoachTipTrigger,
} from "./components/CoachTip";
import { LeftSidebar } from "./components/LeftSidebar";
import { Palette } from "./components/Palette";
import { RightSidebar } from "./components/RightSidebar";
import { ShortcutsOverlay } from "./components/ShortcutsOverlay";
import { StateDot } from "./components/StateDot";
import { UsageBar } from "./components/UsageBar";
import { useStore } from "./context";
import type { CommandId } from "./keyboard/commands";
import { detectPlatform, installKeymap } from "./keyboard/dispatch";
import { shortcutForAria } from "./keyboard/keymap";

// The Compass ADE shell — an Orca-inspired layout over the compass.v1 surface
// (docs/specs/product/compass.md). A CSS grid: a topbar, a left agent-folder
// tree, a central Bridge (swimlane board) / agent (ACP + terminals) view, a
// right sidebar (fleet conversations + files/VCS/PR), and a bottom usage bar.
// The board is primary; the "channel" and "agent" surfaces render as center
// matches within the same shell (single Switch), reached via the store.
//
// This is the dev walking-skeleton made fully explorable: every surface reads
// the in-memory stub (stub-data.ts) through one store (store.ts), so it renders
// and is clickable in `vite dev` with no daemon and no Wails IPC. When the
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
	// Install the single production window keymap listener over the store's
	// keyboard spine (RIG-2456): registry + focus-gated active-group/zone
	// accessors. `onCleanup` keeps the harness's repeated render/dispose cycles
	// from stacking listeners (dispatch returns the exact uninstaller).
	onCleanup(
		installKeymap(
			store.keyboard.registry,
			store.keyboard.activeGroup,
			store.keyboard.activeZone,
		),
	);
	// Point-of-use coaching (RIG-2530): the topbar Bridge tab announces its chord
	// via aria-keyshortcuts + a CoachTip tooltip, resolved from the keymap through
	// shortcutFor (D4) — matching the LeftSidebar view buttons.
	const bridgeAria = shortcutForAria(
		"view.bridge" as CommandId,
		detectPlatform(),
	);
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
					<CoachTip>
						<CoachTipTrigger
							as="button"
							type="button"
							class={["view-tab", { active: store.view() === "bridge" }]}
							onClick={() => store.showBridge()}
							aria-keyshortcuts={bridgeAria}
						>
							<span class="tab-glyph" aria-hidden="true">
								▦
							</span>
							Bridge
						</CoachTipTrigger>
						<CoachTipContent
							label="Bridge"
							command={"view.bridge" as CommandId}
						/>
					</CoachTip>
					<Show when={store.selectedAgent()}>
						{(agent) => (
							<button
								type="button"
								class={["view-tab", { active: store.view() === "agent" }]}
								onClick={() => store.openAgent(agent().account.id)}
							>
								<StateDot state={agent().lifecycle ?? "idle"} />
								{agent().account.displayName}
							</button>
						)}
					</Show>
				</nav>

				<span class="topbar-spacer" />

				<div class={["daemon", { live: store.daemon().live }]}>
					<span class="dot" aria-hidden="true" />
					<span>
						{store.daemon().live ? "daemon connected" : "stub data — no daemon"}
					</span>
					<span class="daemon-ver">
						{store.daemon().version} · {store.daemon().apiVersion}
					</span>
				</div>

				<div class="pane-toggles">
					<CoachTip>
						<CoachTipTrigger
							as="button"
							type="button"
							class={["pane-toggle", { active: store.leftOpen() }]}
							aria-label="Toggle left sidebar"
							aria-keyshortcuts={shortcutForAria(
								"sidebar.toggleLeft" as CommandId,
								detectPlatform(),
							)}
							onClick={() => store.toggleLeft()}
						>
							▐
						</CoachTipTrigger>
						<CoachTipContent
							label="Toggle left sidebar"
							command={"sidebar.toggleLeft" as CommandId}
						/>
					</CoachTip>
					<CoachTip>
						<CoachTipTrigger
							as="button"
							type="button"
							class={["pane-toggle", { active: store.rightOpen() }]}
							aria-label="Toggle right sidebar"
							aria-keyshortcuts={shortcutForAria(
								"sidebar.toggleRight" as CommandId,
								detectPlatform(),
							)}
							onClick={() => store.toggleRight()}
						>
							▌
						</CoachTipTrigger>
						<CoachTipContent
							label="Toggle right sidebar"
							command={"sidebar.toggleRight" as CommandId}
						/>
					</CoachTip>
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

			<Show when={store.shortcutsOpen()}>
				<ShortcutsOverlay />
			</Show>

			<Show when={store.paletteOpen()}>
				<Palette />
			</Show>
		</div>
	);
};

export default App;
