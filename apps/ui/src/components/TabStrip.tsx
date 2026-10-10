import {
	type Component,
	createSignal,
	For,
	onCleanup,
	onSettled,
	Show,
} from "solid-js";
import "../design/components/tabs.css";
import "../design/components/tab-strip.css";
import { useStore } from "../context";
import type { CommandId } from "../keyboard/commands";
import { detectPlatform } from "../keyboard/dispatch";
import { createRovingGroup, type Stop } from "../keyboard/roving";
import { routeTitle } from "../route-title";
import { viewPanelId, viewTabId } from "../view-panel";
import { parseRoute } from "../view-route";
import { focusedPane, type ViewTab } from "../window-layout";
import { Glyph } from "./Glyph";
import { StateDot } from "./StateDot";

/** An agent tab leads with the agent's state, as the sidebar tree does. */
const AgentMark: Component<{ agentId: string }> = (props) => {
	const store = useStore();
	return (
		<Show when={store.agentById(props.agentId)}>
			{(a) => <StateDot state={a().lifecycle ?? "idle"} />}
		</Show>
	);
};

/** Step the roving cursor over the tabs; Left/Right wrap like an ARIA tablist. */
function rovingTarget(
	ids: readonly string[],
	cursor: string,
	command: CommandId,
): string | undefined {
	const at = Math.max(ids.indexOf(cursor), 0);
	switch (command) {
		case "list.moveLeft":
			return ids[(at - 1 + ids.length) % ids.length];
		case "list.moveRight":
			return ids[(at + 1) % ids.length];
		case "list.moveFirst":
			return ids[0];
		case "list.moveLast":
			return ids[ids.length - 1];
		default:
			return undefined;
	}
}

/** The topbar view-tab strip: one tab per layout tab. Click focuses a tab, its
 *  close button (or Delete) closes it, and a drag reorders it. */
export const TabStrip: Component = () => {
	const store = useStore();
	const tabs = (): ViewTab[] => store.layout().tabs;
	// Refs fire after the stop list settles, so a version signal makes the
	// element map observable to the roving effect (Bridge's pattern).
	const els = new Map<string, HTMLElement>();
	const [elsVersion, bumpElsVersion] = createSignal(0, { equals: false });
	const setStopEl = (id: string) => (el: HTMLElement | undefined) => {
		if (el) els.set(id, el);
		else els.delete(id);
		bumpElsVersion(0);
	};
	const stops = (): Stop[] => {
		elsVersion();
		return tabs().flatMap((tab) => {
			const el = els.get(tab.id);
			return el ? [{ id: tab.id, el }] : [];
		});
	};
	// The roving cursor rests on the active tab until arrow keys move it.
	const [cursor, setCursor] = createSignal(() => store.layout().activeTabId);
	const group = createRovingGroup({
		group: { zone: "topbar", id: "view-tabs" },
		stops,
		cursor,
		setCursor,
		onCommand: (command) => {
			const next = rovingTarget(
				tabs().map((tab) => tab.id),
				cursor(),
				command,
			);
			if (next === undefined) return false;
			setCursor(next);
			return true;
		},
	});
	store.keyboard.registerGroup(group);
	onCleanup(() => store.keyboard.unregisterGroup(group));

	let dragging: string | undefined;
	const dropOn = (targetId: string): void => {
		const from = dragging;
		dragging = undefined;
		if (from === undefined || from === targetId) return;
		const toIndex = tabs().findIndex((tab) => tab.id === targetId);
		if (toIndex >= 0)
			store.dispatchLayout({ kind: "move", tabId: from, toIndex });
	};

	return (
		<div
			class="cx-tabs cx-tab-strip"
			data-orientation="h"
			role="tablist"
			aria-label="View tabs"
			data-tour="view-tabs"
		>
			<For each={tabs()} keyed={(tab) => tab.id}>
				{(tab) => <TabItem tab={tab()} />}
			</For>
		</div>
	);

	function TabItem(props: { tab: ViewTab }) {
		const id = props.tab.id;
		const view = () => focusedPane(props.tab.layout);
		const match = () => parseRoute(view().path);
		const title = () => routeTitle(match(), store);
		const agentId = () => {
			const m = match();
			return m.view === "agent" ? m.agentId : undefined;
		};
		const isActive = () => store.layout().activeTabId === id;
		// Keep the strip's roving cursor aligned after shared close handling.
		const close = () => {
			store.closeTab(id);
			onSettled(() => {
				setCursor(store.layout().activeTabId);
			});
		};
		const closeKey = (key: string): boolean =>
			key === "Delete" || (key === "Backspace" && detectPlatform() === "mac");
		return (
			// biome-ignore lint/a11y/noStaticElementInteractions: pointer drag-reorder only; the tab and close buttons inside carry the keyboard semantics.
			<div
				class="cx-tab-strip-item"
				draggable="true"
				onDragStart={(event) => {
					dragging = id;
					event.dataTransfer?.setData("text/plain", id);
					if (event.dataTransfer) event.dataTransfer.effectAllowed = "move";
				}}
				onDragOver={(event) => {
					if (dragging !== undefined) event.preventDefault();
				}}
				onDrop={(event) => {
					event.preventDefault();
					dropOn(id);
				}}
				onDragEnd={() => {
					dragging = undefined;
				}}
			>
				<button
					type="button"
					role="tab"
					class="cx-tab"
					id={viewTabId(id)}
					aria-selected={isActive() ? "true" : "false"}
					data-selected={isActive() ? "" : undefined}
					aria-controls={viewPanelId(view().id)}
					ref={setStopEl(id)}
					onClick={() => store.dispatchLayout({ kind: "focusTab", tabId: id })}
					onKeyDown={(event) => {
						if (closeKey(event.key)) {
							event.preventDefault();
							close();
						}
					}}
				>
					<Show when={agentId()}>{(a) => <AgentMark agentId={a()} />}</Show>
					<span class="cx-tab-strip-label">{title()}</span>
				</button>
				<button
					type="button"
					class="cx-tab-strip-close"
					tabindex={-1}
					aria-label={`Close ${title()}`}
					onClick={close}
				>
					<Glyph name="close" />
				</button>
			</div>
		);
	}
};
