// Coaching tooltip (RIG-2530) — the reusable label+chord Tooltip adopted across
// command-backed chrome. Built on the Kobalte v2-alpha `Tooltip` primitive
// (a11y-hard behavior: hover+focus reveal, open-delay timing, Escape dismiss,
// aria-describedby wiring — DL-150), styled by the shipped `.cx-tooltip` box.
// The chord is ALWAYS the keymap-resolved display string (via `shortcutFor`),
// never hand-authored (DL-234's single-derivation rule). Sequence-aware: a
// leader sequence ("G then B") renders as plain text in the chip's style, since
// ShortcutChip splits on "+" and would otherwise emit one giant <kbd> (A3).

import { Tooltip } from "@kobalte/core/tooltip";
import type { Component, ParentProps } from "solid-js";
import { Show } from "solid-js";
import "../design/components/tooltip.css";
import type { CommandId } from "../keyboard/commands";
import { detectPlatform } from "../keyboard/dispatch";
import { shortcutFor } from "../keyboard/keymap";
import { ShortcutChip } from "./ShortcutChip";

/** House open delay, mirrors --cx-tooltip-delay (tokens.css:227). */
export const COACH_TIP_DELAY_MS = 400;

/** Kobalte Tooltip root with openDelay defaulted to COACH_TIP_DELAY_MS;
 *  hover+focus reveal is Kobalte's default (triggerOnFocusOnly stays unset). */
export const CoachTip: Component<ParentProps<{ openDelay?: number }>> = (
	props,
) => (
	<Tooltip openDelay={props.openDelay ?? COACH_TIP_DELAY_MS}>
		{props.children}
	</Tooltip>
);

/** The trigger — Kobalte's polymorphic Trigger, re-exported so call sites author
 *  <CoachTipTrigger as="button" type="button" class=… onClick=…> with their
 *  existing attributes. */
export const CoachTipTrigger = Tooltip.Trigger;

/** Portal + Content(class="cx-tooltip") rendering `label`, then the chord:
 *  chord = props.chord ?? shortcutFor(props.command, detectPlatform());
 *  undefined → label only; contains " then " → plain-text sequence; otherwise
 *  <ShortcutChip chord={chord}>. Never destructures props. */
export const CoachTipContent: Component<{
	label: string;
	command?: CommandId;
	chord?: string;
	/** Fully REPLACES the `.cx-tooltip` box class (ShortcutChip parity) — it does
	 *  not augment it, so a caller passing this must re-include the row layout
	 *  (.cx-tooltip's flex + the .cx-tooltip-label / .cx-palette-shortcut parts). */
	class?: string;
}> = (props) => {
	const chord = () =>
		props.chord ??
		(props.command ? shortcutFor(props.command, detectPlatform()) : undefined);
	const isSequence = () => chord()?.includes(" then ") ?? false;
	return (
		<Tooltip.Portal>
			<Tooltip.Content class={props.class ?? "cx-tooltip"}>
				<span class="cx-tooltip-label">{props.label}</span>
				<Show when={chord()} keyed>
					{(resolved) => (
						<Show
							when={isSequence()}
							fallback={<ShortcutChip chord={resolved} />}
						>
							<span class="cx-palette-shortcut">{resolved}</span>
						</Show>
					)}
				</Show>
			</Tooltip.Content>
		</Tooltip.Portal>
	);
};
