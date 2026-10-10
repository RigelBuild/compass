import { type Component, createEffect } from "solid-js";
import "../../design/components/tabs.css";
import "../../design/components/button.css";
import "../../design/components/input.css";
import { useStore } from "../../context";
import { openLink } from "../../open-link";
import {
	SETTINGS_SECTION_LABEL,
	SETTINGS_SECTIONS,
	type SettingsSection,
} from "../../view-route";
import { useView } from "../../view-scope";
import { SECTION_VIEW } from "./sections";
import "./settings.css";

export const SettingsView: Component = () => {
	const store = useStore();
	const view = useView();
	const section = () => {
		const route = view.route();
		return route.view === "settings" ? route.section : SETTINGS_SECTIONS[0];
	};
	// Only the focused view records; a background tab was not shown. Re-runs on
	// refocus, so returning to this view makes its section the last one again.
	createEffect(
		() => (store.focusedView().id === view.id ? section() : undefined),
		(next) => {
			if (next !== undefined) store.setSettingsSection(next);
		},
	);

	return (
		<section class="settings-view" aria-label="Settings">
			<nav
				class="cx-tabs settings-nav"
				aria-label="Settings sections"
				data-orientation="v"
			>
				{SETTINGS_SECTIONS.map((id: SettingsSection) => {
					const path = `/settings/${id}`;
					const open = openLink(
						store,
						() => path,
						() => view.navigate(path),
					);
					return (
						<a
							class="cx-tab"
							href={`#${path}`}
							// Opts out of router link claims, which would overwrite aria-current.
							rel="external"
							data-selected={section() === id ? "" : undefined}
							aria-current={section() === id ? "page" : undefined}
							// The layout owns navigation; the browser must not follow the href.
							onClick={(event) => {
								event.preventDefault();
								open.onClick(event);
							}}
							onAuxClick={open.onAuxClick}
							onMouseDown={open.onMouseDown}
						>
							{SETTINGS_SECTION_LABEL[id]}
						</a>
					);
				})}
			</nav>
			<div class="settings-body">{SECTION_VIEW[section()]()}</div>
		</section>
	);
};
