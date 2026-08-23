// Shortcut chip (RIG-2483) — the shared point-of-use key hint. Splits a
// resolved chord ("Ctrl+K", "Shift+Enter") on "+" and renders the
// `.cx-palette-shortcut` chip. The chord it renders is ALWAYS the
// resolveChord-resolved display string (via `shortcutFor`); it never resolves
// `Mod` itself. The `class` prop lets a future `.cx-menu-item`/`.cx-tooltip`
// host (RIG-2530) restyle the box while reusing the same split rendering.
//
// OQ-5: chords render as resolved text ("Ctrl+K"); mapping to "⌘K" on mac is a
// one-component change here later.

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
