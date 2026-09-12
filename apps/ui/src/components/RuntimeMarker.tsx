import { type Component, Show } from "solid-js";
import type { RuntimeMarker as RuntimeMarkerModel } from "../stub-data";
import "../design/components/runtime-marker.css";

/** The session runtime marker: a small mono text token beside the state dot
 * naming which runner backend the session runs on (`podman` / `microvm` /
 * `apple-container` / `host` / `unknown`). Where egress is `unenforced` the
 * marker additionally reads as uncontained — a warn-colored `!` suffix plus a
 * `data-uncontained` hook — so a user glancing at the roster can tell a
 * host-network session from a contained one. An `unknown` tier or posture
 * renders as such and never as contained. Color/sizing live in
 * `runtime-marker.css`. */
export const RuntimeMarker: Component<{ marker: RuntimeMarkerModel }> = (
	props,
) => {
	const uncontained = () => props.marker.posture === "unenforced";
	const label = () =>
		`runtime ${props.marker.tier}, egress ${props.marker.posture}`;
	return (
		<span
			class="cx-runtime-marker"
			data-tier={props.marker.tier}
			data-posture={props.marker.posture}
			data-uncontained={uncontained() ? "1" : undefined}
			title={label()}
			// role="img" so the label replaces the glyphs for assistive tech: the
			// bare tier text and the "!" do not say "egress unenforced" on their own.
			role="img"
			aria-label={label()}
		>
			{props.marker.tier}
			<Show when={uncontained()}>
				<span class="cx-runtime-uncontained" aria-hidden="true">
					!
				</span>
			</Show>
		</span>
	);
};
