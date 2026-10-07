import { type Component, createMemo, For, Show } from "solid-js";
import "../design/components/split-pane.css";
import { useStore } from "../context";
import { viewPanelId, viewTabId } from "../view-panel";
import {
	MAX_RATIO,
	MIN_RATIO,
	type TabLayout,
	tabViews,
} from "../window-layout";
import { ViewHost } from "./ViewHost";

type Split = Extract<TabLayout, { kind: "split" }>;

const KEY_STEP = 0.05;

/** One view tab's box: its single view, or both panes of a split with a
 *  splitter between them. App toggles `hidden` on the box and its panels. */
export const SplitPane: Component<{ tabId: string }> = (props) => {
	const store = useStore();
	const layout = createMemo(
		() => store.layout().tabs.find((tab) => tab.id === props.tabId)?.layout,
	);
	const split = createMemo((): Split | undefined => {
		const tab = layout();
		return tab?.kind === "split" ? tab : undefined;
	});
	const views = createMemo(() => {
		const tab = layout();
		return tab ? tabViews(tab) : [];
	});
	let root: HTMLDivElement | undefined;
	let dragging: number | undefined;

	const onKeyDown = (event: KeyboardEvent, current: Split): void => {
		const [back, forward] =
			current.direction === "row"
				? ["ArrowLeft", "ArrowRight"]
				: ["ArrowUp", "ArrowDown"];
		const step =
			event.key === forward ? KEY_STEP : event.key === back ? -KEY_STEP : 0;
		if (step === 0) return;
		event.preventDefault();
		// Rounded so repeated steps land on whole percents, not float drift.
		const ratio = Math.round((current.ratio + step) * 100) / 100;
		store.dispatchLayout({ kind: "resize", ratio });
	};
	const onPointerMove = (event: PointerEvent, current: Split): void => {
		// A release that never reached us leaves `dragging` set; `buttons` and
		// the active tab say whether this is still a live drag of this tab.
		if (
			dragging !== event.pointerId ||
			(event.buttons & 1) === 0 ||
			store.layout().activeTabId !== props.tabId ||
			!root
		)
			return;
		const box = root.getBoundingClientRect();
		const ratio =
			current.direction === "row"
				? (event.clientX - box.left) / box.width
				: (event.clientY - box.top) / box.height;
		store.dispatchLayout({ kind: "resize", ratio });
	};

	return (
		<div
			class="cx-split-pane"
			data-tab-id={props.tabId}
			data-direction={split()?.direction}
			hidden
			ref={(el) => {
				root = el;
			}}
		>
			<For each={views()} keyed={(view) => view.id}>
				{(view, index) => {
					const pane = (): "first" | "second" =>
						index() === 0 ? "first" : "second";
					const scope = createMemo(() =>
						store.viewScopes().find((item) => item.id === view().id),
					);
					const share = (): number | undefined => {
						const current = split();
						if (!current) return undefined;
						return pane() === "first" ? current.ratio : 1 - current.ratio;
					};
					return (
						<>
							<Show when={index() === 1 && split()}>
								{(current) => (
									// biome-ignore lint/a11y/useSemanticElements: an <hr> cannot take focus or a value; this is the ARIA window-splitter pattern.
									// biome-ignore lint/a11y/useFocusableInteractive: Solid 2 types admit only lowercase `tabindex`, which this rule does not read.
									<div
										class="cx-split-pane-splitter"
										role="separator"
										tabindex={0}
										aria-label="Resize panes"
										aria-orientation={
											current().direction === "row" ? "vertical" : "horizontal"
										}
										aria-controls={viewPanelId(current().first.id)}
										aria-valuemin={MIN_RATIO * 100}
										aria-valuemax={MAX_RATIO * 100}
										aria-valuenow={Math.round(current().ratio * 100)}
										onKeyDown={(event) => onKeyDown(event, current())}
										onPointerDown={(event) => {
											if (event.button !== 0) return;
											event.preventDefault();
											dragging = event.pointerId;
											event.currentTarget.setPointerCapture(event.pointerId);
										}}
										onPointerMove={(event) => onPointerMove(event, current())}
										onPointerUp={() => {
											dragging = undefined;
										}}
										onPointerCancel={() => {
											dragging = undefined;
										}}
										onLostPointerCapture={() => {
											dragging = undefined;
										}}
									/>
								)}
							</Show>
							<div
								class="view-panel"
								role="tabpanel"
								id={viewPanelId(view().id)}
								data-view-id={view().id}
								aria-labelledby={viewTabId(props.tabId)}
								tabindex={-1}
								hidden
								data-focused={split()?.focused === pane() ? "" : undefined}
								style={{ "flex-grow": share() }}
							>
								<Show when={scope()} keyed>
									{(item) => <ViewHost scope={item} />}
								</Show>
							</div>
						</>
					);
				}}
			</For>
		</div>
	);
};
