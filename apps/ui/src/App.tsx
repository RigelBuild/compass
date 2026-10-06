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
import { LayoutNotice } from "./components/LayoutNotice";
import { LeftSidebar } from "./components/LeftSidebar";
import { Palette } from "./components/Palette";
import { RightSidebar } from "./components/RightSidebar";
import { ShortcutsOverlay } from "./components/ShortcutsOverlay";
import { TabStrip } from "./components/TabStrip";
import { TopBarSearch } from "./components/TopBarSearch";
import { UsageBar } from "./components/UsageBar";
import { ViewHost } from "./components/ViewHost";
import { useStore } from "./context";
import type { CommandId } from "./keyboard/commands";
import { detectPlatform, installKeymap } from "./keyboard/dispatch";
import { shortcutForAria } from "./keyboard/keymap";
import type { LiveClients } from "./live/client";
import { routeTitle } from "./route-title";
import { focusViewPanel, viewPanelId, viewTabId } from "./view-panel";
import { focusedViewOf, shownViewIds, tabViews } from "./window-layout";

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
	// shown view before hiding the old one; a hidden element drops focus.
	const panels = new Map<string, HTMLElement>();
	const lastFocus = new Map<string, HTMLElement>();
	// The view focus was last in, kept even after a close removes its panel.
	let focusedPanelView: string | undefined;
	const rememberFocus = (event: FocusEvent): void => {
		const target = event.target;
		if (!(target instanceof HTMLElement)) return;
		const panel = target.closest<HTMLElement>('[role="tabpanel"]');
		const viewId = [...panels].find(([, el]) => el === panel)?.[0];
		if (viewId === undefined) return;
		lastFocus.set(viewId, target);
		focusedPanelView = viewId;
		store.focusViewId(viewId);
	};
	// Focus moving to another real target outside every panel ends the claim;
	// a null target is a removal, which the close path below still handles.
	const forgetFocus = (event: FocusEvent): void => {
		const next = event.relatedTarget;
		if (!(next instanceof Node)) return;
		if (![...panels.values()].some((el) => el.contains(next)))
			focusedPanelView = undefined;
	};
	const tabIdOf = (viewId: string): string | undefined => {
		const tab = store
			.layout()
			.tabs.find((t) => tabViews(t.layout).some((v) => v.id === viewId));
		return tab ? viewTabId(tab.id) : undefined;
	};
	// Focus is leaving if it sits in a panel about to hide, or it was in a panel
	// a close just removed (the browser has already dropped it to the body).
	const focusLeaving = (shownIds: readonly string[]): boolean => {
		const active = document.activeElement;
		if (active === null || active === document.body)
			return focusedPanelView !== undefined && !panels.has(focusedPanelView);
		return [...panels].some(
			([id, el]) => !shownIds.includes(id) && el.contains(active),
		);
	};
	const prunePanels = (ids: readonly string[]): void => {
		for (const id of [...panels.keys()]) {
			if (ids.includes(id)) continue;
			panels.delete(id);
			lastFocus.delete(id);
		}
	};
	createEffect(
		() => ({
			shownIds: shownViewIds(store.layout()),
			focusedId: focusedViewOf(store.layout()).id,
			ids: store.viewScopes().map((scope) => scope.id),
		}),
		({ shownIds, focusedId, ids }) => {
			prunePanels(ids);
			const focused = panels.get(focusedId);
			if (!focused) return;
			for (const id of shownIds) panels.get(id)?.removeAttribute("hidden");
			const activePanelId = [...panels].find(([, panel]) =>
				panel.contains(document.activeElement),
			)?.[0];
			if (
				focusLeaving(shownIds) ||
				(activePanelId !== undefined && activePanelId !== focusedId)
			) {
				focusInto(focusedId, focused);
			}
			for (const [id, el] of panels) el.hidden = !shownIds.includes(id);
		},
	);
	const focusInto = (viewId: string, panel: HTMLElement): void => {
		const remembered = lastFocus.get(viewId);
		if (remembered && !panel.contains(remembered)) lastFocus.delete(viewId);
		const target = focusViewPanel(viewId, lastFocus.get(viewId));
		if (target) lastFocus.set(viewId, target);
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
				<LayoutNotice />

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

			<main class="main" onFocusIn={rememberFocus} onFocusOut={forgetFocus}>
				<For each={store.viewScopes()} keyed={(scope) => scope.id}>
					{(scope) => (
						<div
							class="view-panel"
							role="tabpanel"
							id={viewPanelId(untrack(scope).id)}
							aria-labelledby={tabIdOf(scope().id)}
							tabindex={-1}
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
