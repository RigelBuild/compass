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
import { SplitPane } from "./components/SplitPane";
import { TabStrip } from "./components/TabStrip";
import { TopBarSearch } from "./components/TopBarSearch";
import { useStore } from "./context";
import type { CommandId } from "./keyboard/commands";
import { detectPlatform, installKeymap } from "./keyboard/dispatch";
import { shortcutForAria } from "./keyboard/keymap";
import type { LiveClients } from "./live/client";
import { routeTitle } from "./route-title";
import { focusViewPanel } from "./view-panel";
import { focusedPane, focusedViewOf, shownViewIds } from "./window-layout";

// Compass shell: routed center view with persistent navigation chrome.

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
	// SplitPane renders them, so they are found under main by view id.
	let main: HTMLElement | undefined;
	const panels = (): Map<string, HTMLElement> =>
		new Map(
			[
				...(main?.querySelectorAll<HTMLElement>(".view-panel[data-view-id]") ??
					[]),
			].map((el) => [el.dataset.viewId ?? "", el]),
		);
	const lastFocus = new Map<string, HTMLElement>();
	// The view focus was last in, kept even after a close removes its panel.
	let focusedPanelView: string | undefined;
	// The splitter focus sat on: removing it drops focus to the body even
	// though its tab's panes survive.
	let focusedSplitter: HTMLElement | undefined;
	// The splitter sits outside both panels; focus on it counts as its tab's
	// focused view, so removing or hiding it still lands focus in a pane.
	const viewIdOf = (target: HTMLElement): string | undefined => {
		const panelView = target.closest<HTMLElement>(".view-panel[data-view-id]")
			?.dataset.viewId;
		if (panelView !== undefined) return panelView;
		const tabId = target.closest<HTMLElement>(".cx-split-pane[data-tab-id]")
			?.dataset.tabId;
		const tab = store.layout().tabs.find((item) => item.id === tabId);
		return tab ? focusedPane(tab.layout).id : undefined;
	};
	const rememberFocus = (event: FocusEvent): void => {
		const target = event.target;
		if (!(target instanceof HTMLElement)) return;
		const viewId = viewIdOf(target);
		if (viewId === undefined) return;
		focusedPanelView = viewId;
		focusedSplitter = target.closest(".view-panel") ? undefined : target;
		if (focusedSplitter) return;
		lastFocus.set(viewId, target);
		store.focusViewId(viewId);
	};
	// Focus moving to another real target outside main ends the claim; a null
	// target is a removal, which the close path below handles.
	const forgetFocus = (event: FocusEvent): void => {
		const next = event.relatedTarget;
		if (!(next instanceof Node)) return;
		if (!main?.contains(next)) {
			focusedPanelView = undefined;
			focusedSplitter = undefined;
		}
	};
	// Focus is leaving if it sits in a box or panel about to hide, or it was in
	// a panel a close just removed (the browser has already dropped it to body).
	const focusLeaving = (
		shown: Map<string, HTMLElement>,
		toggles: readonly [HTMLElement, boolean][],
	): boolean => {
		const active = document.activeElement;
		if (active === null || active === document.body)
			return (
				focusedPanelView !== undefined &&
				(focusedSplitter?.isConnected === false || !shown.has(focusedPanelView))
			);
		return toggles.some(([el, show]) => !show && el.contains(active));
	};
	const pruneFocus = (ids: readonly string[]): void => {
		for (const id of [...lastFocus.keys()]) {
			if (!ids.includes(id)) lastFocus.delete(id);
		}
	};
	// Each tab's box and every panel, paired with whether it should show.
	const visibility = (
		current: Map<string, HTMLElement>,
		shownIds: readonly string[],
		activeTabId: string,
	): [HTMLElement, boolean][] => [
		...[...(main?.querySelectorAll<HTMLElement>(".cx-split-pane") ?? [])].map(
			(box): [HTMLElement, boolean] => [box, box.dataset.tabId === activeTabId],
		),
		...[...current].map(([id, el]): [HTMLElement, boolean] => [
			el,
			shownIds.includes(id),
		]),
	];
	createEffect(
		() => ({
			shownIds: shownViewIds(store.layout()),
			focusedId: focusedViewOf(store.layout()).id,
			activeTabId: store.layout().activeTabId,
			ids: store.viewScopes().map((scope) => scope.id),
		}),
		({ shownIds, focusedId, activeTabId, ids }) => {
			pruneFocus(ids);
			const current = panels();
			const focused = current.get(focusedId);
			if (!focused) return;
			const toggles = visibility(current, shownIds, activeTabId);
			for (const [el, show] of toggles) if (show) el.hidden = false;
			const activePanelId = [...current].find(([, panel]) =>
				panel.contains(document.activeElement),
			)?.[0];
			if (
				focusLeaving(current, toggles) ||
				(activePanelId !== undefined && activePanelId !== focusedId)
			) {
				focusInto(focusedId, focused);
			}
			for (const [el, show] of toggles) el.hidden = !show;
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

			<main
				class="main"
				ref={(el) => {
					main = el;
				}}
				onFocusIn={rememberFocus}
				onFocusOut={forgetFocus}
			>
				<For each={store.layout().tabs} keyed={(tab) => tab.id}>
					{(tab) => <SplitPane tabId={untrack(tab).id} />}
				</For>
			</main>

			<Show when={store.rightOpen()}>
				<RightSidebar />
			</Show>

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
