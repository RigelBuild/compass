// Open modes for a navigation link: a plain click navigates the focused view
// in place; Mod+click and middle-click open the path in a new tab.

import { detectPlatform } from "./keyboard/dispatch";
import type { AppStore } from "./store";

export interface OpenLinkHandlers {
	onClick: (event: MouseEvent) => void;
	onAuxClick: (event: MouseEvent) => void;
	onMouseDown: (event: MouseEvent) => void;
}

/** Click handlers for a link to `path`. `inPlace` keeps the link's own
 *  navigation, so its side effects (closing the shortcuts sheet) stay put. */
export function openLink(
	store: Pick<AppStore, "dispatchLayout">,
	path: () => string | undefined,
	inPlace: () => void,
): OpenLinkHandlers {
	const openInTab = (): void => {
		const target = path();
		if (target !== undefined)
			store.dispatchLayout({ kind: "open", path: target });
	};
	return {
		onClick: (event) => {
			const mod = detectPlatform() === "mac" ? event.metaKey : event.ctrlKey;
			if (mod) openInTab();
			else inPlace();
		},
		onAuxClick: (event) => {
			if (event.button !== 1) return;
			event.preventDefault();
			openInTab();
		},
		// Cancelled on mousedown: by auxclick the browser may already autoscroll.
		onMouseDown: (event) => {
			if (event.button === 1) event.preventDefault();
		},
	};
}
