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
import { Glyph } from "./components/Glyph";
import { LeftSidebar } from "./components/LeftSidebar";
import { Palette } from "./components/Palette";
import { RightSidebar } from "./components/RightSidebar";
import { RuntimeMarker } from "./components/RuntimeMarker";
import { ShortcutsOverlay } from "./components/ShortcutsOverlay";
import { StateDot } from "./components/StateDot";
import { TopBarSearch } from "./components/TopBarSearch";
import { UsageBar } from "./components/UsageBar";
import { ViewHost } from "./components/ViewHost";
import { useStore } from "./context";
import type { CommandId } from "./keyboard/commands";
import { detectPlatform, installKeymap } from "./keyboard/dispatch";
import { shortcutForAria } from "./keyboard/keymap";
import type { LiveClients } from "./live/client";

// Compass shell: routed center view with persistent navigation and usage chrome.

// App owns the shell layout; the matched route renders in its center region.
// Bind the store router seam to the active router location and navigation.
const App: Component<
	RouteSectionProps & { clients?: Pick<LiveClients, "comms" | "compass"> }
> = (props) => {
	const store = useStore();
	const navigate = useNavigate();
	const location = useLocation();
	store.bindRouter({
		navigate: (path, options) => navigate(path, options),
		currentPath: () => location.pathname,
		currentState: () => location.state,
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
						<Glyph name="logo" />
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
								<Glyph name="status" />
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
								<Show when={agent().runtime}>
									{(m) => <RuntimeMarker marker={m()} />}
								</Show>
								{agent().account.displayName}
							</button>
						)}
					</Show>
				</nav>

				<TopBarSearch clients={props.clients} />
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
							<Glyph name="panel-left" />
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
							<Glyph name="panel-right" />
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

			<main class="main">
				{/* Keyed: a context provider's value is read once, so a new focused
				    view needs a fresh ViewHost. */}
				<Show when={store.focusedView()} keyed>
					{(scope) => <ViewHost scope={scope} />}
				</Show>
			</main>

			<Show when={store.rightOpen()}>
				<RightSidebar />
			</Show>

			<UsageBar />

			<Show when={store.shortcutsOpen()}>
				<ShortcutsOverlay />
			</Show>

			<Show when={store.paletteOpen()}>
				<Palette clients={props.clients} />
			</Show>
		</div>
	);
};

export default App;
