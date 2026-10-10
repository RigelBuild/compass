import type { JSX } from "@solidjs/web";

export function SettingsRow(props: {
	label: string;
	help?: string;
	for?: string;
	children: JSX.Element;
}): JSX.Element {
	const helpId = props.for ? `${props.for}-description` : undefined;
	return (
		<div class="settings-row">
			<div class="settings-row-label">
				{/* A read-only row has no control, so its name is plain text. */}
				{props.for ? (
					<label for={props.for}>{props.label}</label>
				) : (
					<span class="settings-row-name">{props.label}</span>
				)}
				{props.help && (
					<span id={helpId} class="settings-row-help">
						{props.help}
					</span>
				)}
			</div>
			<div class="settings-row-control">{props.children}</div>
		</div>
	);
}
