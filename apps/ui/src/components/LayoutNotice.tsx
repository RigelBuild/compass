import { type Component, onCleanup, Show } from "solid-js";
import { useStore } from "../context";
import "../design/components/button.css";
import "../design/components/toast.css";
import { viewTabId } from "../view-panel";

/** The tab-cap refusal, inline in the topbar beside the strip so it never
 *  covers view chrome. The status region stays mounted while empty so a screen
 *  reader registers it before the first announcement. */
export const LayoutNotice: Component = () => {
	const store = useStore();
	return (
		<div class="layout-notice-region" role="status">
			<Show when={store.layoutNotice()}>
				{(notice) => {
					let el: HTMLDivElement | undefined;
					// Focus or hover inside the notice pauses its timeout.
					const held = { focused: false, hovered: false };
					const holdNotice = (change: Partial<typeof held>): void => {
						Object.assign(held, change);
						store.holdLayoutNotice(held.focused || held.hovered);
					};
					// Dismissed or expired with focus inside: hand focus to the strip
					// rather than letting it fall to the body.
					onCleanup(() => {
						const active = document.activeElement;
						const dropped = active === null || active === document.body;
						if (!el?.contains(active) && !(held.focused && dropped)) return;
						const tabId = store.layout().activeTabId;
						document.getElementById(viewTabId(tabId))?.focus();
					});
					return (
						// biome-ignore lint/a11y/noStaticElementInteractions: focus/hover only pause the timeout; the Dismiss button carries the interaction.
						<div
							ref={(node) => {
								el = node;
							}}
							class="cx-toast layout-notice"
							data-kind="warn"
							title={notice().text}
							onFocusIn={() => holdNotice({ focused: true })}
							onFocusOut={(e) => {
								const next = e.relatedTarget;
								if (next instanceof Node && e.currentTarget.contains(next))
									return;
								// No target and no window focus: the window blurred, so focus
								// will come back here; keep holding.
								if (next === null && !document.hasFocus()) return;
								holdNotice({ focused: false });
							}}
							onMouseEnter={() => holdNotice({ hovered: true })}
							onMouseLeave={() => holdNotice({ hovered: false })}
						>
							<span class="layout-notice-text">{notice().text}</span>
							<Show when={notice().count > 1}>
								<span class="layout-notice-count">
									{` (repeated ${notice().count} times)`}
								</span>
							</Show>
							<button
								type="button"
								class="cx-btn"
								data-size="sm"
								data-variant="ghost"
								aria-label="Dismiss"
								onClick={() => store.dismissLayoutNotice()}
							>
								×
							</button>
						</div>
					);
				}}
			</Show>
		</div>
	);
};
