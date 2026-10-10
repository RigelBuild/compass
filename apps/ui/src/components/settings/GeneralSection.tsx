import { type Component, createUniqueId } from "solid-js";
import { useStore } from "../../context";
import type { ShellMode } from "../../shell-globals";
import { shellMode } from "../../shell-globals";
import { SettingsRow } from "./SettingsRow";

export function modeLabel(
	mode: ShellMode | undefined,
): "Browser" | "Embedded server" | "Remote server" {
	switch (mode) {
		case undefined:
			return "Browser";
		case "embedded":
			return "Embedded server";
		case "client":
		case "setup":
		case "reopen":
			return "Remote server";
	}
	const exhaustive: never = mode;
	return exhaustive;
}
export const GeneralSection: Component = () => {
	const store = useStore();
	const uid = createUniqueId();

	return (
		<section class="settings-content" aria-labelledby={`${uid}-general-title`}>
			<header class="settings-content-head">
				<h2 class="settings-heading" id={`${uid}-general-title`}>
					General
				</h2>
				<p class="settings-description">View connection and account details.</p>
			</header>
			<section class="settings-group" aria-label="General settings">
				<SettingsRow label="Server URL">
					<span>{store.serverUrl() ?? "Not connected"}</span>
				</SettingsRow>
				<SettingsRow label="Mode">
					<span>{modeLabel(shellMode())}</span>
				</SettingsRow>
				<SettingsRow label="Server version">
					{store.daemon().live ? (
						<span>
							{[
								store.daemon().version,
								store.daemon().apiVersion,
								store.daemon().rev,
							]
								.filter((part) => part !== "")
								.join(" · ")}
						</span>
					) : (
						<span>Not connected</span>
					)}
				</SettingsRow>
				<SettingsRow label="Signed in as">
					<span>
						{store.caller().handle} · {store.caller().displayName}
					</span>
				</SettingsRow>
				<SettingsRow label="Keyboard shortcuts">
					<button
						type="button"
						class="cx-btn"
						aria-label="Open keyboard shortcuts"
						onClick={() => store.toggleShortcuts()}
					>
						Open
					</button>
				</SettingsRow>
			</section>
		</section>
	);
};
