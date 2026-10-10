import { type Component, createUniqueId } from "solid-js";
import { useStore } from "../../context";
import "../../design/components/button.css";
import { SettingsRow } from "./SettingsRow";

export const AppearanceSection: Component = () => {
	const store = useStore();
	const uid = createUniqueId();

	return (
		<section
			class="settings-content"
			aria-labelledby={`${uid}-appearance-title`}
		>
			<header class="settings-content-head">
				<h2 class="settings-heading" id={`${uid}-appearance-title`}>
					Appearance
				</h2>
				<p class="settings-description">
					Configure motion settings for this device.
				</p>
			</header>
			<section class="settings-group" aria-label="Appearance settings">
				<SettingsRow label="Reduce motion">
					{/* biome-ignore lint/a11y/useSemanticElements: the required contract is a named button group. */}
					<div role="group" aria-label="Reduce motion" class="settings-actions">
						<button
							type="button"
							class="cx-btn"
							aria-pressed={
								store.reduceMotion() === "system" ? "true" : "false"
							}
							data-selected={store.reduceMotion() === "system" ? "" : undefined}
							onClick={() => store.setReduceMotion("system")}
						>
							Follow system
						</button>
						<button
							type="button"
							class="cx-btn"
							aria-pressed={store.reduceMotion() === "on" ? "true" : "false"}
							data-selected={store.reduceMotion() === "on" ? "" : undefined}
							onClick={() => store.setReduceMotion("on")}
						>
							Always
						</button>
					</div>
				</SettingsRow>
			</section>
		</section>
	);
};
