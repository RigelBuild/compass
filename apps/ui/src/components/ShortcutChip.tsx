// Render keymap chords as individual key caps; hosts may override the class.

import type { Component } from "solid-js";
import { For } from "solid-js";

export const ShortcutChip: Component<{ chord: string; class?: string }> = (
	props,
) => {
	const keys = () => props.chord.split("+");
	return (
		<span class={props.class ?? "cx-palette-shortcut"}>
			<For each={keys()}>{(key) => <kbd>{key}</kbd>}</For>
		</span>
	);
};
