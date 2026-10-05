import type { RouteSectionProps } from "@solidjs/router";
import { useLocation, useNavigate } from "@solidjs/router";
import {
	type Component,
	createEffect,
	For,
	onCleanup,
	Show,
	untrack,
} from "solid-js";
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
import { ShortcutsOverlay } from "./components/ShortcutsOverlay";
import { TabStrip, viewPanelId, viewTabId } from "./components/TabStrip";
import { TopBarSearch } from "./components/TopBarSearch";
import { UsageBar } from "./components/UsageBar";
import { ViewHost } from "./components/ViewHost";
import { useStore } from "./context";
import type { CommandId } from "./keyboard/commands";
import { detectPlatform, installKeymap } from "./keyboard/dispatch";
import { shortcutForAria } from "./keyboard/keymap";
import type { LiveClients } from "./live/client";
import { routeTitle } from "./route-title";
import { focusedViewOf, tabViews } from "./window-layout";

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
	// Panels toggle `hidden` imperatively so a switch can move focus into the
	// shown view before hiding the old one (record A4); a hidden element drops focus.
	const panels = new Map<string, HTMLElement>();
	const lastFocus = new Map<string, HTMLElement>();
	const rememberFocus = (event: FocusEvent): void => {
		const target = event.target;
		if (!(target instanceof HTMLElement)) return;
		const panel = target.closest<HTMLElement>('[role="tabpanel"]');
		const viewId = [...panels].find(([, el]) => el === panel)?.[0];
		if (viewId !== undefined) lastFocus.set(viewId, target);
	};
	const tabIdOf = (viewId: string): string | undefined => {
		const tab = store
			.layout()
			.tabs.find((t) => tabViews(t.layout).some((v) => v.id === viewId));
		return tab ? viewTabId(tab.id) : undefined;
	};
	createEffect(
		() => ({
			shownId: focusedViewOf(store.layout()).id,
			ids: store.viewScopes().map((scope) => scope.id),
		}),
		({ shownId, ids }) => {
			for (const id of [...panels.keys()]) {
				if (ids.includes(id)) continue;
				panels.delete(id);
				lastFocus.delete(id);
			}
			const shown = panels.get(shownId);
			if (!shown) return;
			shown.hidden = false;
			const active = document.activeElement;
			const leaving = [...panels].some(
				([id, el]) => id !== shownId && active !== null && el.contains(active),
			);
			if (leaving) focusInto(shownId, shown);
			for (const [id, el] of panels) {
				if (id !== shownId) el.hidden = true;
			}
		},
	);
	const focusInto = (viewId: string, panel: HTMLElement): void => {
		const remembered = lastFocus.get(viewId);
		const target =
			remembered?.isConnected && panel.contains(remembered)
				? remembered
				: (panel.querySelector<HTMLElement>(
						'a[href], button:not([disabled]), input:not([disabled]), [tabindex]:not([tabindex="-1"])',
					) ?? panel);
		target.focus();
	};
	createEffect(
		() => routeTitle(store.focusedView().route(), store),
		(title) => {
			document.title = title;
		},
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

				<TabStrip />

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

			<main class="main" onFocusIn={rememberFocus}>
				<For each={store.viewScopes()} keyed={(scope) => scope.id}>
					{(scope) => (
						<div
							class="view-panel"
							role="tabpanel"
							id={viewPanelId(untrack(scope).id)}
							aria-labelledby={tabIdOf(scope().id)}
							hidden
							ref={(el) => panels.set(untrack(scope).id, el)}
						>
							<ViewHost scope={scope()} />
						</div>
					)}
				</For>
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
